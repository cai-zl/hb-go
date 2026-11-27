package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/auth"
	"github.com/cai-zl/hb-go/internal/api"
	"github.com/cai-zl/hb-go/redis"
)

type sessionData struct {
	UID   int    `json:"uid"`
	Role  string `json:"role"`
	Extra string `json:"extra"`
}

type testAuthChecker struct{}

func (testAuthChecker) Check(req auth.LoginReq) (json.RawMessage, error) {
	if req.Username == "alice" && req.Password == "wonderland" {
		return json.Marshal(sessionData{UID: 7, Role: "admin", Extra: "x"})
	}
	return nil, hb.Err(401, "invalid username or password")
}

type authCheckFunc func(auth.LoginReq) (json.RawMessage, error)

func (f authCheckFunc) Check(req auth.LoginReq) (json.RawMessage, error) { return f(req) }

type testClientChecker struct{}

func (testClientChecker) Check(req auth.ClientReq) (json.RawMessage, error) {
	if req.ClientID == "svc" && req.ClientSecret == "s3cret" {
		return json.Marshal(sessionData{UID: 9, Role: "client"})
	}
	return nil, hb.Err(401, "invalid client credentials")
}

type clientCheckFunc func(auth.ClientReq) (json.RawMessage, error)

func (f clientCheckFunc) Check(req auth.ClientReq) (json.RawMessage, error) { return f(req) }

func newEngine(t *testing.T, sessions *auth.Sessions, check auth.AuthChecker, clientCheck auth.ClientChecker) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api.MountAuth(&r.RouterGroup, sessions, check, clientCheck)
	return r
}

// 用 RawMessage 区分 data 字段缺失与 JSON null。
type authRouteResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func do(t *testing.T, r http.Handler, method, path, authorization, body string) (int, authRouteResponse) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env authRouteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: HTTP %d, expected JSON envelope: %v", method, path, w.Code, err)
	}
	return w.Code, env
}

func TestBearerThreeStates(t *testing.T) {
	r := newEngine(t, nil, testAuthChecker{}, nil)
	for _, header := range []string{"", "Basic abc", "Bearer "} {
		status, env := do(t, r, "GET", "/auth/me", header, "")
		if status != 401 || env.Code != 401 || env.Message != "unauthorized" || env.Data != nil {
			t.Fatalf("Authorization %q: HTTP %d, %+v; want 401 unauthorized without data", header, status, env)
		}
	}
}

func TestAuthMalformedBodies(t *testing.T) {
	check := authCheckFunc(func(auth.LoginReq) (json.RawMessage, error) {
		t.Fatal("checker called for malformed JSON")
		return nil, nil
	})
	r := newEngine(t, nil, check, nil)
	for _, path := range []string{"/auth/token", "/auth/refresh", "/auth/revoke"} {
		status, env := do(t, r, "POST", path, "", "{oops")
		if status != 200 || env.Code != 400 || env.Message != "invalid request body" || env.Data != nil {
			t.Fatalf("POST %s: HTTP %d, %+v; want HTTP 200, code 400, invalid request body without data", path, status, env)
		}
	}
}

func TestTokenGrantType(t *testing.T) {
	const input = `{"grant_type":"client_credentials","client_id":"svc","client_secret":"bad"}`
	seen := clientCheckFunc(func(req auth.ClientReq) (json.RawMessage, error) {
		if req.ClientID != "svc" || req.ClientSecret != "bad" {
			t.Fatalf("client checker received %+v; want bound client credentials", req)
		}
		return nil, hb.Err(401, "invalid client credentials")
	})
	never := authCheckFunc(func(auth.LoginReq) (json.RawMessage, error) {
		t.Fatal("user checker called for a non-user grant")
		return nil, nil
	})
	pwd := authCheckFunc(func(req auth.LoginReq) (json.RawMessage, error) {
		if req.Username != "alice" || req.Password != "wonderland" {
			t.Fatalf("user checker received %+v; want bound user credentials", req)
		}
		return nil, hb.Err(401, "invalid username or password")
	})
	for _, tc := range []struct {
		name        string
		clientCheck auth.ClientChecker
		userCheck   auth.AuthChecker
		body        string
		code        int
		message     string
	}{
		{"client credentials", seen, never, input, 401, "invalid client credentials"},
		{"client login not configured", nil, never, input, 400, "unsupported grant type"},
		{"password grant", nil, pwd, `{"grant_type":"password","username":"alice","password":"wonderland"}`, 401, "invalid username or password"},
		{"unknown grant type", testClientChecker{}, never, `{"grant_type":"user","username":"alice","password":"wonderland"}`, 400, "unsupported grant type"},
	} {
		r := newEngine(t, nil, tc.userCheck, tc.clientCheck)
		status, env := do(t, r, "POST", "/auth/token", "", tc.body)
		if status != 200 || env.Code != tc.code || env.Message != tc.message || env.Data != nil {
			t.Fatalf("%s: HTTP %d, %+v; want HTTP 200, code %d, %q without data", tc.name, status, env, tc.code, tc.message)
		}
	}
}

func TestLoginCheckErrors(t *testing.T) {
	const input = `{"username":"alice","password":"wrong"}`
	for _, tc := range []struct {
		name    string
		err     error
		code    int
		message string
	}{
		{"wrapped business error", fmt.Errorf("check: %w", hb.Err(401, "invalid username or password")), 401, "invalid username or password"},
		{"zero code", &hb.Error{Code: 0, Message: "check failed"}, 500, "check failed"},
		{"unknown error", errors.New("backing store refused connection"), 500, "internal error"},
	} {
		check := authCheckFunc(func(req auth.LoginReq) (json.RawMessage, error) {
			if req.Username != "alice" || req.Password != "wrong" {
				t.Fatal("checker did not receive bound credentials")
			}
			return nil, tc.err
		})
		r := newEngine(t, nil, check, nil)
		status, env := do(t, r, "POST", "/auth/token", "", input)
		if status != 200 || env.Code != tc.code || env.Message != tc.message || env.Data != nil {
			t.Fatalf("%s: HTTP %d, %+v; want HTTP 200, code %d, %q without data", tc.name, status, env, tc.code, tc.message)
		}
	}
}

func TestLoginRejectsInvalidSessionJSON(t *testing.T) {
	check := authCheckFunc(func(auth.LoginReq) (json.RawMessage, error) { return json.RawMessage("{"), nil })
	r := newEngine(t, auth.NewSessions(nil, auth.Config{}), check, nil)
	status, env := do(t, r, "POST", "/auth/token", "", `{"username":"alice","password":"wonderland"}`)
	if status != 200 || env.Code != 500 || env.Message != "internal error" || env.Data != nil {
		t.Fatalf("invalid session JSON: HTTP %d, %+v; want HTTP 200, code 500, internal error without data", status, env)
	}
}

func requireTokenResponse(t *testing.T, status int, env authRouteResponse, sessions *auth.Sessions) auth.TokenResponse {
	t.Helper()
	var tokens auth.TokenResponse
	if status != 200 || env.Code != 0 || env.Message != "ok" || json.Unmarshal(env.Data, &tokens) != nil ||
		tokens.TokenType != "Bearer" || tokens.ExpiresIn != int64(sessions.AccessTTL().Seconds()) ||
		len(tokens.AccessToken) != 64 || len(tokens.RefreshToken) != 64 {
		t.Fatalf("want HTTP 200, code 0, ok, and OAuth2 snake_case tokens; got HTTP %d, code %d, message %q, data %s", status, env.Code, env.Message, env.Data)
	}
	return tokens
}

func TestAuthFlow(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping integration test")
	}
	client, err := redis.Connect(redis.Config{URL: url})
	if err != nil {
		t.Fatalf("redis connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sessions := auth.NewSessions(client, auth.Config{})
	r := newEngine(t, sessions, testAuthChecker{}, testClientChecker{})
	r.GET("/private/who", auth.Guard(sessions), func(c *gin.Context) {
		data, err := auth.From[sessionData](c)
		if err != nil {
			t.Fatalf("auth.From: %v", err)
		}
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: data})
	})
	r.GET("/private/kind", auth.Guard(sessions), func(c *gin.Context) {
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: auth.Kind(c)})
	})

	status, env := do(t, r, "POST", "/auth/token", "", `{"username":"alice","password":"wrong"}`)
	if status != 200 || env.Code != 401 || env.Message != "invalid username or password" || env.Data != nil {
		t.Fatalf("wrong password: HTTP %d, %+v; want HTTP 200, code 401 without data", status, env)
	}
	login := func() auth.TokenResponse {
		status, env := do(t, r, "POST", "/auth/token", "", `{"username":"alice","password":"wonderland"}`)
		tokens := requireTokenResponse(t, status, env, sessions)
		t.Cleanup(func() { _, _ = sessions.Revoke(tokens.AccessToken) })
		return tokens
	}
	tokens := login()
	other := login()
	if tokens.AccessToken == other.AccessToken || tokens.RefreshToken == other.RefreshToken {
		t.Fatal("tokens should differ across logins")
	}

	const wantData = `{"uid":7,"role":"admin","extra":"x"}`
	for _, path := range []string{"/auth/me", "/private/who"} {
		status, env = do(t, r, "GET", path, "Bearer "+tokens.AccessToken, "")
		if status != 200 || env.Code != 0 || env.Message != "ok" || string(env.Data) != wantData {
			t.Fatalf("GET %s: HTTP %d, code %d, message %q, data %s; want session object %s", path, status, env.Code, env.Message, env.Data, wantData)
		}
	}
	if status, _ = do(t, r, "GET", "/private/who", "Bearer garbage-token", ""); status != 401 {
		t.Fatalf("Guard with invalid token: HTTP %d, want 401", status)
	}

	body, _ := json.Marshal(auth.RefreshReq{RefreshToken: tokens.RefreshToken})
	status, env = do(t, r, "POST", "/auth/refresh", "", string(body))
	fresh := requireTokenResponse(t, status, env, sessions)
	t.Cleanup(func() { _, _ = sessions.Revoke(fresh.AccessToken) })
	if fresh.AccessToken == tokens.AccessToken || fresh.RefreshToken == tokens.RefreshToken {
		t.Fatal("refresh must rotate both tokens")
	}
	if status, _ = do(t, r, "GET", "/auth/me", "Bearer "+tokens.AccessToken, ""); status != 401 {
		t.Fatalf("old access after refresh: HTTP %d, want 401", status)
	}
	status, env = do(t, r, "GET", "/auth/me", "Bearer "+fresh.AccessToken, "")
	if status != 200 || env.Code != 0 || env.Message != "ok" || string(env.Data) != wantData {
		t.Fatalf("rotated session: HTTP %d, code %d, message %q, data %s; want %s", status, env.Code, env.Message, env.Data, wantData)
	}
	status, env = do(t, r, "POST", "/auth/refresh", "", string(body))
	if status != 200 || env.Code != 401 || env.Message != "invalid refresh token" || env.Data != nil {
		t.Fatalf("old refresh token: HTTP %d, %+v; want HTTP 200, code 401, invalid refresh token without data", status, env)
	}

	body, _ = json.Marshal(auth.RevokeReq{AccessToken: fresh.AccessToken})
	for i := range 2 {
		status, env = do(t, r, "POST", "/auth/revoke", "", string(body))
		if status != 200 || env.Code != 0 || env.Message != "ok" || env.Data != nil {
			t.Fatalf("revoke #%d: HTTP %d, %+v; want HTTP 200, code 0, ok without data", i+1, status, env)
		}
	}
	if status, _ = do(t, r, "GET", "/auth/me", "Bearer "+fresh.AccessToken, ""); status != 401 {
		t.Fatalf("revoked access: HTTP %d, want 401", status)
	}
	body, _ = json.Marshal(auth.RefreshReq{RefreshToken: fresh.RefreshToken})
	status, env = do(t, r, "POST", "/auth/refresh", "", string(body))
	if status != 200 || env.Code != 401 || env.Message != "invalid refresh token" || env.Data != nil {
		t.Fatalf("revoked refresh token: HTTP %d, %+v; want HTTP 200, code 401, invalid refresh token without data", status, env)
	}

	if ok, err := sessions.Touch(other.AccessToken); err != nil || !ok {
		t.Fatalf("touch: %v %v", ok, err)
	}
	if status, _ = do(t, r, "GET", "/auth/me", "Bearer "+other.AccessToken, ""); status != 200 {
		t.Fatalf("touched token: HTTP %d, want 200", status)
	}

	// 客户端登录:grant_type 分流到 ClientChecker,会话带 kind,refresh 轮换后保持
	status, env = do(t, r, "POST", "/auth/token", "", `{"grant_type":"client_credentials","client_id":"svc","client_secret":"bad"}`)
	if status != 200 || env.Code != 401 || env.Message != "invalid client credentials" || env.Data != nil {
		t.Fatalf("wrong client secret: HTTP %d, %+v; want HTTP 200, code 401 without data", status, env)
	}
	status, env = do(t, r, "POST", "/auth/token", "", `{"grant_type":"client_credentials","client_id":"svc","client_secret":"s3cret"}`)
	clientTokens := requireTokenResponse(t, status, env, sessions)
	t.Cleanup(func() { _, _ = sessions.Revoke(clientTokens.AccessToken) })
	for _, tc := range []struct{ token, kind string }{{other.AccessToken, auth.KindUser}, {clientTokens.AccessToken, auth.KindClient}} {
		if status, env = do(t, r, "GET", "/private/kind", "Bearer "+tc.token, ""); status != 200 || string(env.Data) != `"`+tc.kind+`"` {
			t.Fatalf("GET /private/kind with %s token: HTTP %d, %+v; want data %q", tc.kind, status, env, tc.kind)
		}
	}
	body, _ = json.Marshal(auth.RefreshReq{RefreshToken: clientTokens.RefreshToken})
	status, env = do(t, r, "POST", "/auth/refresh", "", string(body))
	rotated := requireTokenResponse(t, status, env, sessions)
	t.Cleanup(func() { _, _ = sessions.Revoke(rotated.AccessToken) })
	if status, env = do(t, r, "GET", "/private/kind", "Bearer "+rotated.AccessToken, ""); status != 200 || string(env.Data) != `"client"` {
		t.Fatalf("client kind after refresh: HTTP %d, %+v; want data \"client\"", status, env)
	}
}
