package auth_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/auth"
)

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

func TestCreateRejectsInvalidJSON(t *testing.T) {
	sessions := auth.NewSessions(nil, auth.Config{})
	pair, err := sessions.Create(json.RawMessage("{"))
	var me *json.MarshalerError
	var e *hb.Error
	if pair != (auth.TokenPair{}) || !errors.As(err, &me) || errors.As(err, &e) {
		t.Fatalf(`Create(json.RawMessage("{")) = %+v, %v; want zero tokens and a plain *json.MarshalerError, not *hb.Error`, pair, err)
	}
}

func TestGuardMasksSessionStoreError(t *testing.T) {
	// 已关闭的客户端不拨号,每条命令都返回 goredis.ErrClosed
	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	if err := client.Close(); err != nil {
		t.Fatalf("client.Close() = %v, want nil", err)
	}
	sessions := auth.NewSessions(client, auth.Config{})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/private", auth.Guard(sessions), func(*gin.Context) {
		t.Fatal("Guard allowed a request when the session store failed")
	})

	// 原因只进日志不进响应:捕获 zap 全局 Logger,断言后恢复
	var logs bytes.Buffer
	restore := zap.ReplaceGlobals(zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewDevelopmentEncoderConfig()),
		zapcore.AddSync(&logs),
		zapcore.DebugLevel,
	)))
	t.Cleanup(restore)

	req := httptest.NewRequest(http.MethodGet, "/private", nil)
	req.Header.Set("Authorization", "Bearer fake")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Code != 500 || env.Message != "internal error" || env.Data != nil {
		t.Fatalf("Guard with failing store: HTTP %d, body %s; want HTTP 200, code 500, internal error without data", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), goredis.ErrClosed.Error()) {
		t.Fatalf("response leaks the store error: %s", w.Body)
	}
	var entry struct {
		Level string `json:"L"`
		Msg   string `json:"M"`
		Err   string `json:"err"`
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &entry) != nil || entry.Level != "WARN" || entry.Msg != "internal error" || !strings.Contains(entry.Err, goredis.ErrClosed.Error()) {
		t.Fatalf("log = %q; want exactly one WARN internal error entry carrying the store error", logs.String())
	}
}

func TestGuardRejectsMissingToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/private", auth.Guard(nil), func(*gin.Context) {
		t.Fatal("Guard allowed a request without a token")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/private", nil))
	var env hb.Envelope
	if w.Code != http.StatusUnauthorized || json.Unmarshal(w.Body.Bytes(), &env) != nil || env.Code != 401 || env.Message != "unauthorized" {
		t.Fatalf("Guard without token: HTTP %d, body %s; want 401 unauthorized", w.Code, w.Body)
	}
}
