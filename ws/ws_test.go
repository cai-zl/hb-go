package ws_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"hg"
	"hg/ws"
)

// echoServer: 收到文本回发信封 {code:0,message:"echo",data:<原文>};
// 收到 Pong 时回发 {code:0,message:"pong"}。
func echoServer(t *testing.T, observe ...func(*ws.Conn)) (*ws.Hub, *httptest.Server) {
	t.Helper()
	hub := ws.NewHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := ws.Upgrade(hub, w, r, func(c *ws.Conn, m ws.Message) {
			for _, f := range observe {
				f(c)
			}
			if len(m.Data) == 0 { // pong(空载荷无法区分,用 ping 往返测)
				return
			}
			c.Send(hg.Envelope{Message: "echo", Data: string(m.Data)})
		}); err != nil {
			t.Errorf("upgrade: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { /* 等待清理完成由各测试自行保证 */ })
	return hub, srv
}

func dial(t *testing.T, srv *httptest.Server, header http.Header) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.DefaultDialer.Dial(u, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readEnv(t *testing.T, c *websocket.Conn) hg.Envelope {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env hg.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return env
}

func TestUpgradeEchoAndMetadata(t *testing.T) {
	connCh := make(chan *ws.Conn, 1)
	hub, srv := echoServer(t, func(c *ws.Conn) { connCh <- c })
	c := dial(t, srv, http.Header{"User-Agent": []string{"hg-test"}})

	if err := c.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	env := readEnv(t, c)
	if env.Code != 0 || env.Message != "echo" || env.Data != "hi" {
		t.Fatalf("env = %+v, want code 0 echo hi", env)
	}
	if hub.Count() != 1 {
		t.Fatalf("count = %d, want 1", hub.Count())
	}
	conn := waitConn(t, connCh)
	if hub.Conn(conn.ID) != conn {
		t.Fatalf("hub.Conn(%q) != conn", conn.ID)
	}
	if len(conn.ID) != 32 || !allHex(conn.ID) {
		t.Fatalf("conn.ID = %q, want 32-char hex", conn.ID)
	}
	if conn.UserAgent != "hg-test" || conn.IP == nil {
		t.Fatalf("conn metadata = %+v, want UA hg-test + IP", conn)
	}
}

func TestForwardedForFirstSegment(t *testing.T) {
	connCh := make(chan *ws.Conn, 1)
	_, srv := echoServer(t, func(c *ws.Conn) { connCh <- c })
	c := dial(t, srv, http.Header{"X-Forwarded-For": []string{"203.0.113.7, 10.0.0.1"}})
	_ = c.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
	var got *ws.Conn
	select {
	case got = <-connCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not record conn")
	}
	if got == nil || got.ForwardedFor == nil || got.ForwardedFor.String() != "203.0.113.7" {
		t.Fatalf("forwarded_for = %v, want 203.0.113.7", got.ForwardedFor)
	}
}

func TestBroadcastAndSendTo(t *testing.T) {
	connCh := make(chan *ws.Conn, 2)
	hub, srv := echoServer(t, func(c *ws.Conn) { connCh <- c })
	c1 := dial(t, srv, nil)
	c2 := dial(t, srv, nil)
	// 逐条拨号 + 发 Pong 触发 observe(PongHandler):先连者必先被捕获,id1 对应 c1
	_ = c1.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
	id1 := waitConn(t, connCh).ID
	_ = c2.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
	id2 := waitConn(t, connCh).ID
	if id1 == id2 || len(id1) != 32 || len(id2) != 32 || !allHex(id1) || !allHex(id2) {
		t.Fatalf("conn ids = %q, %q; want distinct 32-char hex", id1, id2)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hub.Count() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Count() != 2 {
		t.Fatalf("count = %d, want 2", hub.Count())
	}

	hub.Broadcast(hg.Envelope{Data: "bcast"})
	env := readEnv(t, c1)
	if env.Data != "bcast" {
		t.Fatalf("c1 env = %+v", env)
	}
	env = readEnv(t, c2)
	if env.Data != "bcast" {
		t.Fatalf("c2 env = %+v", env)
	}

	if !hub.SendTo(id1, hg.Envelope{Message: "direct"}) {
		t.Fatal("SendTo(id1) returned false")
	}
	env = readEnv(t, c1)
	if env.Message != "direct" {
		t.Fatalf("direct env = %+v", env)
	}
	if hub.SendTo("no-such-id", hg.Envelope{Data: "x"}) {
		t.Fatal(`SendTo("no-such-id") should be false`)
	}
	if !hub.SendRaw(id1, "raw-text") {
		t.Fatal("SendRaw(id1) returned false")
	}
	_ = c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := c1.ReadMessage()
	if err != nil || string(data) != "raw-text" {
		t.Fatalf("raw = %q (err %v), want raw-text", data, err)
	}
}

func TestDisconnectCallback(t *testing.T) {
	connCh := make(chan *ws.Conn, 1)
	hub, srv := echoServer(t, func(c *ws.Conn) { connCh <- c })
	closed := make(chan string, 1)
	hub.SetOnDisconnect(func(id string) { closed <- id })

	c := dial(t, srv, nil)
	_ = c.WriteControl(websocket.PongMessage, nil, time.Now().Add(time.Second))
	conn := waitConn(t, connCh)
	deadline := time.Now().Add(2 * time.Second)
	for hub.Count() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = c.Close() // 客户端主动断开 → 服务端触发回调并清理
	select {
	case id := <-closed:
		if id != conn.ID {
			t.Fatalf("closed id = %q, want %q", id, conn.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect callback not fired")
	}
	deadline = time.Now().Add(2 * time.Second)
	for hub.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Count() != 0 {
		t.Fatalf("count after close = %d, want 0", hub.Count())
	}
}

func TestPongForwardedToCallback(t *testing.T) {
	hub := ws.NewHub()
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = ws.Upgrade(hub, w, r, func(_ *ws.Conn, m ws.Message) {
			select {
			case got <- m.Data:
			default:
			}
		})
	}))
	defer srv.Close()

	c := dial(t, srv, nil)
	deadline := time.Now().Add(2 * time.Second)
	for hub.Count() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.WriteControl(websocket.PongMessage, []byte("rtt-payload"), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-got:
		if string(data) != "rtt-payload" {
			t.Fatalf("pong payload = %q, want rtt-payload", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pong not forwarded to onMessage")
	}
}

func TestErrMsgNormalizesZeroCode(t *testing.T) {
	var env hg.Envelope
	if err := json.Unmarshal([]byte(ws.ErrMsg(0, "boom")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != 500 || env.Message != "boom" {
		t.Fatalf("env = %+v, want code 500 boom", env)
	}
	if err := json.Unmarshal([]byte(ws.ErrMsg(404, "nope")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != 404 {
		t.Fatalf("env = %+v, want 404", env)
	}
}

func waitConn(t *testing.T, ch chan *ws.Conn) *ws.Conn {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("observe not fired")
		return nil
	}
}

func allHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
