package auth_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

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
	var e *hb.Error
	if pair != (auth.TokenPair{}) || !errors.As(err, &e) || e.Code != 500 || e.Message != "internal error" {
		t.Fatalf(`Create(json.RawMessage("{")) = %+v, %v; want zero tokens and *hb.Error with code 500 and message "internal error"`, pair, err)
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
