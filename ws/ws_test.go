package ws_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/cai-zl/hb-go/ws"
)

// echoServer 回发包含原载荷的不带信封的 JSON，跳过空载荷。
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
			c.Send(map[string]string{"echo": string(m.Data)})
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

func readText(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("read frame type = %d, want TextMessage (%d)", kind, websocket.TextMessage)
	}
	return string(data)
}

func TestUpgradeEchoAndMetadata(t *testing.T) {
	connCh := make(chan *ws.Conn, 1)
	hub, srv := echoServer(t, func(c *ws.Conn) { connCh <- c })
	c := dial(t, srv, http.Header{"User-Agent": []string{"hb-test"}})

	if err := c.WriteMessage(websocket.TextMessage, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, c); got != `{"echo":"hi"}` {
		t.Fatalf("echo of hi = %q, want %s", got, `{"echo":"hi"}`)
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
	if conn.UserAgent != "hb-test" || conn.IP == nil {
		t.Fatalf("conn metadata = %+v, want UA hb-test + IP", conn)
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

	hub.Broadcast(map[string]int{"n": 1})
	if got := readText(t, c1); got != `{"n":1}` {
		t.Fatalf("c1 after Broadcast(map[n:1]) = %q, want %s", got, `{"n":1}`)
	}
	if got := readText(t, c2); got != `{"n":1}` {
		t.Fatalf("c2 after Broadcast(map[n:1]) = %q, want %s", got, `{"n":1}`)
	}

	if hub.SendTo(id1, make(chan int)) {
		t.Fatal("SendTo(id1, chan int) = true, want false")
	}
	if !hub.SendTo(id1, "hi") {
		t.Fatal(`SendTo(id1, "hi") = false, want true`)
	}
	if got := readText(t, c1); got != `"hi"` {
		t.Fatalf(`c1 after SendTo(id1, "hi") = %q, want %s`, got, `"hi"`)
	}
	if hub.SendTo("no-such-id", "x") {
		t.Fatal(`SendTo("no-such-id", "x") = true, want false`)
	}
	const direct = `{"direct":true}`
	if !hub.SendTo(id1, json.RawMessage(direct)) {
		t.Fatalf("SendTo(id1, json.RawMessage(%s)) = false, want true", direct)
	}
	if got := readText(t, c1); got != direct {
		t.Fatalf("c1 after SendTo(id1, json.RawMessage(%s)) = %q, want %s", direct, got, direct)
	}

	hub.Broadcast("after")
	if got := readText(t, c1); got != `"after"` {
		t.Fatalf(`c1 after Broadcast("after") = %q, want %s`, got, `"after"`)
	}
	if got := readText(t, c2); got != `"after"` {
		t.Fatalf(`c2 after Broadcast("after") = %q, want %s`, got, `"after"`)
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
