// Package ws 提供 WebSocket 连接管理(gorilla/websocket):Hub 注册表、定向/广播
// 发送、30 秒服务端 Ping 探活与断连回调。消息回调按连接内串行执行(保序);
// 客户端 Pong 载荷同 Text/Binary 一样透传给回调(可用于心跳 RTT 测量)。
package ws

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cai-zl/hb-go"
)

// Heartbeat 服务端 Ping 周期。
const Heartbeat = 30 * time.Second

// Message 已擦除类型的入站消息:Pong 载荷也会以 Message 形式进入回调。
type Message struct {
	Conn *Conn
	Data []byte
}

// Conn 一条已升级连接的信息:升级握手时采集,之后经 Hub.Conn(id) 随时可查。
type Conn struct {
	ID           string
	IP           net.IP // TCP 直连地址;反代部署时是代理地址,真实客户端看 ForwardedFor
	ForwardedFor net.IP // X-Forwarded-For 首段
	UserAgent    string
	Header       http.Header

	socket    *websocket.Conn
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// Send 将 msg 编码为 JSON 文本回发，不添加信封。返回值只表示是否入队，不保证送达。
func (c *Conn) Send(msg any) bool {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Warn("ws send serialize failed; skipped", "conn", c.ID, "err", err)
		return false
	}
	return c.enqueue(data)
}

func (c *Conn) enqueue(data []byte) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.send <- data:
		return true
	case <-c.done:
		return false
	default:
		// 慢消费者队列满:丢弃避免阻塞调用方
		slog.Warn("ws send queue full; dropped", "conn", c.ID)
		return false
	}
}

func (c *Conn) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// Hub 连接注册表:ID 为随机不透明标识,断连回调启动前设置、每个 Hub 一个。
type Hub struct {
	mu           sync.RWMutex
	conns        map[string]*Conn
	onDisconnect func(id string)
}

// NewHub 创建空注册表。
func NewHub() *Hub {
	return &Hub{conns: make(map[string]*Conn)}
}

// SetOnDisconnect 设置断连回调(连接关闭或网络错误时以 id 调用)。
func (h *Hub) SetOnDisconnect(fn func(id string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onDisconnect = fn
}

// Count 当前在线连接数。
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// Conn 按 id 查连接,不存在返回 nil。
func (h *Hub) Conn(id string) *Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[id]
}

// Broadcast 将 msg 编码为 JSON 文本广播，不添加信封。编码失败仅记录告警并跳过。
func (h *Hub) Broadcast(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Warn("broadcast serialize failed; skipped", "err", err)
		return
	}
	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		if !c.enqueue(data) {
			h.remove(c.ID)
		}
	}
}

// SendTo 将 msg 编码为 JSON 文本发往 id，不添加信封。返回值只表示是否入队，不保证送达。
func (h *Hub) SendTo(id string, msg any) bool {
	c := h.Conn(id)
	if c == nil {
		return false
	}
	data, err := json.Marshal(msg)
	if err != nil {
		slog.Warn("ws send serialize failed; skipped", "conn", id, "err", err)
		return false
	}
	if !c.enqueue(data) {
		h.remove(id)
		return false
	}
	return true
}

// randomID 生成 32 字符随机 hex 连接 ID(128 位熵)。熵源读失败视为系统故障,
// 直接 panic(与 Go 1.24+ rand.Read 忽略 error 的语义一致)。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (h *Hub) add(c *Conn) {
	for {
		id := randomID()
		h.mu.Lock()
		if _, taken := h.conns[id]; taken {
			h.mu.Unlock()
			continue // 128 位熵下几乎不可能;纯防御
		}
		c.ID = id
		h.conns[id] = c
		h.mu.Unlock()
		return
	}
}

func (h *Hub) remove(id string) {
	h.mu.Lock()
	_, ok := h.conns[id]
	delete(h.conns, id)
	fn := h.onDisconnect
	h.mu.Unlock()
	if ok {
		slog.Info("websocket closed", "conn", id)
		if fn != nil {
			fn(id)
		}
	}
}

var upgrader = websocket.Upgrader{
	// 浏览器跨域由业务自行约束;与 Rust axum 一样不在库层拦截 Origin
	CheckOrigin: func(*http.Request) bool { return true },
}

// Upgrade 升级 HTTP 连接并注册进 Hub;握手失败返回错误。握手期采集
// IP / X-Forwarded-For 首段 / UA / 头。每连接一个读循环 goroutine 串行执行
// onMessage,一个写循环 goroutine 负责 30s Ping 与出站队列。
func Upgrade(hub *Hub, w http.ResponseWriter, r *http.Request, onMessage func(*Conn, Message)) error {
	socket, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return hb.Internal(err)
	}
	c := &Conn{
		IP:        remoteIP(r),
		UserAgent: r.UserAgent(),
		Header:    r.Header,
		socket:    socket,
		send:      make(chan []byte, 256),
		done:      make(chan struct{}),
	}
	if ff := r.Header.Get("X-Forwarded-For"); ff != "" {
		first, _, _ := strings.Cut(ff, ",")
		c.ForwardedFor = net.ParseIP(strings.TrimSpace(first))
	}
	hub.add(c)
	slog.Info("websocket connected", "conn", c.ID, "ip", c.IP)

	go writeLoop(c)
	go readLoop(c, hub, onMessage)
	return nil
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func readLoop(c *Conn, hub *Hub, onMessage func(*Conn, Message)) {
	defer func() {
		c.close()
		hub.remove(c.ID)
	}()
	// gorilla 把 Pong 单独交给 PongHandler:同 Text/Binary 一样透传给回调
	c.socket.SetPongHandler(func(appData string) error {
		onMessage(c, Message{Conn: c, Data: []byte(appData)})
		return nil
	})
	for {
		typ, data, err := c.socket.ReadMessage()
		if err != nil {
			return // Close 帧或网络错误:清理 + 断连回调
		}
		if typ == websocket.TextMessage || typ == websocket.BinaryMessage {
			onMessage(c, Message{Conn: c, Data: data})
		}
	}
}

func writeLoop(c *Conn) {
	ticker := time.NewTicker(Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			_ = c.socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.socket.WriteMessage(websocket.TextMessage, msg); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			_ = c.socket.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.socket.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.close()
				return
			}
		}
	}
}
