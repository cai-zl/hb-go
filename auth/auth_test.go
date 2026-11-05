package auth_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/auth"
	"github.com/cai-zl/hb-go/redis"
)

type sessionData struct {
	UID   int    `json:"uid"`
	Role  string `json:"role"`
	Extra string `json:"extra"`
}

func testSessions(t *testing.T) *auth.Sessions {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping integration test")
	}
	client, err := redis.Connect(redis.Config{URL: url})
	if err != nil {
		t.Fatalf("redis connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return auth.NewSessions(client, auth.Config{})
}

func newEngine(sessions *auth.Sessions) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	check := func(req auth.LoginReq) (any, error) {
		if req.Username == "alice" && req.Password == "wonderland" {
			return sessionData{UID: 7, Role: "admin", Extra: "x"}, nil
		}
		return nil, hb.Err(401, "invalid username or password")
	}
	auth.Routes(sessions, check)(r.Group(""))
	// 受保护组:Guard + From 取会话数据
	protected := r.Group("/private", auth.Guard(sessions))
	protected.GET("/who", func(c *gin.Context) {
		data, err := auth.From[sessionData](c)
		if err != nil {
			var e *hb.Error
			if errors.As(err, &e) {
				c.JSON(200, hb.Envelope{Code: e.Code, Message: e.Message})
			} else {
				c.JSON(200, hb.Envelope{Code: 500, Message: "internal error"})
			}
			return
		}
		c.JSON(200, hb.Envelope{Code: 0, Message: "ok", Data: data})
	})
	return r
}

func do(t *testing.T, r http.Handler, method, path, token string, body string) (*http.Response, hb.Envelope) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req, _ = http.NewRequest(method, path, nil)
	} else {
		req, _ = http.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env hb.Envelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Result(), env
}

// ---- 单元部分(不依赖 Redis):配置缺省值、Bearer 三态、错误信封形状 ----

func TestSessionsConfigDefaults(t *testing.T) {
	s := auth.NewSessions(nil, auth.Config{})
	if s.AccessTTL() != 2*time.Hour || s.RefreshTTL() != 30*24*time.Hour {
		t.Fatalf("ttl = %v/%v, want 2h/30d", s.AccessTTL(), s.RefreshTTL())
	}
	s = auth.NewSessions(nil, auth.Config{TokenTTL: time.Minute, RefreshTTL: 2 * time.Minute})
	if s.AccessTTL() != time.Minute || s.RefreshTTL() != 2*time.Minute {
		t.Fatalf("ttl override = %v/%v, want 1m/2m", s.AccessTTL(), s.RefreshTTL())
	}
}

func TestBearerThreeStates(t *testing.T) {
	r := newEngine(auth.NewSessions(nil, auth.Config{}))

	// 缺头 → HTTP 401 + 信封
	resp, env := do(t, r, "GET", "/auth/me", "", "")
	if resp.StatusCode != 401 || env.Code != 401 || env.Message != "unauthorized" {
		t.Fatalf("no header: %d %+v, want 401 envelope", resp.StatusCode, env)
	}
	// 前缀不对 → 401
	resp, env = do(t, r, "GET", "/auth/me", "", "")
	req, _ := http.NewRequest("GET", "/auth/me", nil)
	req.Header.Set("Authorization", "Basic abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("wrong scheme: %d, want 401", w.Code)
	}
	if resp.StatusCode != 401 || env.Code != 401 {
		t.Fatal("expected 401")
	}
	// 空 token → 401
	req, _ = http.NewRequest("GET", "/auth/me", nil)
	req.Header.Set("Authorization", "Bearer ")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("empty token: %d, want 401", w.Code)
	}
	// 坏 JSON 登录体 → 200 + 400 业务信封(不触碰 Redis)
	resp, env = do(t, r, "POST", "/auth/token", "", "{oops")
	if resp.StatusCode != 200 || env.Code != 400 {
		t.Fatalf("bad body: %d %+v, want 200/400", resp.StatusCode, env)
	}
}

// ---- 集成部分(TEST_REDIS_URL 门控):登录/轮换/吊销全流程 ----

func login(t *testing.T, r http.Handler, user, pass string) (auth.TokenResponse, int, hb.Envelope) {
	t.Helper()
	body, _ := json.Marshal(auth.LoginReq{Username: user, Password: pass})
	resp, env := do(t, r, "POST", "/auth/token", "", string(body))
	var tr auth.TokenResponse
	if env.Data != nil {
		raw, _ := json.Marshal(env.Data)
		_ = json.Unmarshal(raw, &tr)
	}
	return tr, resp.StatusCode, env
}

func TestAuthFlow(t *testing.T) {
	sessions := testSessions(t)
	r := newEngine(sessions)

	// 凭据错误 = HTTP 200 + 业务码 401 信封
	tr, status, env := login(t, r, "alice", "wrong")
	if status != 200 || env.Code != 401 {
		t.Fatalf("bad login: %d %+v, want 200/401", status, env)
	}

	// 登录成功:OAuth2 snake_case 键 + 形状
	tr, status, env = login(t, r, "alice", "wonderland")
	if status != 200 || env.Code != 0 {
		t.Fatalf("login: %d %+v, want 200/0", status, env)
	}
	if tr.TokenType != "Bearer" || tr.ExpiresIn != int64(sessions.AccessTTL().Seconds()) ||
		len(tr.AccessToken) != 64 || len(tr.RefreshToken) != 64 {
		t.Fatalf("token response = %+v, want Bearer/7200/64hex", tr)
	}

	// token 随机性:两次登录 token 不同
	tr2, _, _ := login(t, r, "alice", "wonderland")
	if tr2.AccessToken == tr.AccessToken {
		t.Fatal("access tokens should differ across logins")
	}

	// /auth/me 携带 access token → 会话数据原样
	resp, env := do(t, r, "GET", "/auth/me", tr.AccessToken, "")
	if resp.StatusCode != 200 || env.Code != 0 {
		t.Fatalf("me: %d %+v", resp.StatusCode, env)
	}
	raw, _ := json.Marshal(env.Data)
	var data sessionData
	if err := json.Unmarshal(raw, &data); err != nil || data.UID != 7 || data.Role != "admin" {
		t.Fatalf("me data = %s (%v), want uid 7 admin", raw, err)
	}

	// Guard + From:受保护路由
	resp, env = do(t, r, "GET", "/private/who", tr.AccessToken, "")
	if resp.StatusCode != 200 || env.Code != 0 {
		t.Fatalf("private: %d %+v", resp.StatusCode, env)
	}
	// Guard 下垃圾 token → HTTP 401
	resp, _ = do(t, r, "GET", "/private/who", "garbage-token", "")
	if resp.StatusCode != 401 {
		t.Fatalf("private garbage: %d, want 401", resp.StatusCode)
	}

	// Refresh 轮换:新对可用,旧 access 立即失效
	body, _ := json.Marshal(auth.RefreshReq{RefreshToken: tr.RefreshToken})
	resp, env = do(t, r, "POST", "/auth/refresh", "", string(body))
	if resp.StatusCode != 200 || env.Code != 0 {
		t.Fatalf("refresh: %d %+v", resp.StatusCode, env)
	}
	raw, _ = json.Marshal(env.Data)
	var fresh auth.TokenResponse
	if err := json.Unmarshal(raw, &fresh); err != nil || fresh.AccessToken == "" {
		t.Fatalf("refresh data = %s", raw)
	}
	if fresh.AccessToken == tr.AccessToken {
		t.Fatal("refresh should rotate access token")
	}
	if resp, _ = do(t, r, "GET", "/auth/me", tr.AccessToken, ""); resp.StatusCode != 401 {
		t.Fatalf("old access after refresh: %d, want 401", resp.StatusCode)
	}
	if resp, _ = do(t, r, "GET", "/auth/me", fresh.AccessToken, ""); resp.StatusCode != 200 {
		t.Fatalf("new access after refresh: %d, want 200", resp.StatusCode)
	}

	// 垃圾 refresh token → 200 + 业务 401
	body, _ = json.Marshal(auth.RefreshReq{RefreshToken: "no-such-token"})
	resp, env = do(t, r, "POST", "/auth/refresh", "", string(body))
	if resp.StatusCode != 200 || env.Code != 401 {
		t.Fatalf("bad refresh: %d %+v, want 200/401", resp.StatusCode, env)
	}

	// Revoke:双键删除、幂等
	body, _ = json.Marshal(auth.RevokeReq{AccessToken: fresh.AccessToken})
	resp, env = do(t, r, "POST", "/auth/revoke", "", string(body))
	if resp.StatusCode != 200 || env.Code != 0 {
		t.Fatalf("revoke: %d %+v", resp.StatusCode, env)
	}
	if resp, _ = do(t, r, "GET", "/auth/me", fresh.AccessToken, ""); resp.StatusCode != 401 {
		t.Fatal("revoked token should be invalid")
	}
	resp, env = do(t, r, "POST", "/auth/revoke", "", string(body))
	if resp.StatusCode != 200 || env.Code != 0 {
		t.Fatalf("revoke twice must be idempotent: %d %+v", resp.StatusCode, env)
	}

	// Touch 续期不改变有效性
	tr3, _, _ := login(t, r, "alice", "wonderland")
	if ok, err := sessions.Touch(tr3.AccessToken); err != nil || !ok {
		t.Fatalf("touch: %v %v", ok, err)
	}
	if resp, _ = do(t, r, "GET", "/auth/me", tr3.AccessToken, ""); resp.StatusCode != 200 {
		t.Fatal("token should stay valid after touch")
	}
}
