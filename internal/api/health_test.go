package api_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	_ "modernc.org/sqlite"

	"github.com/cai-zl/hb-go/internal/api"
)

func TestHealthProbeResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mount := func(pool *sql.DB, client *goredis.Client) *gin.Engine {
		r := gin.New()
		api.MountHealth(&r.RouterGroup, pool, client)
		return r
	}
	check := func(name string, r *gin.Engine, readyStatus int, readyBody string) {
		t.Helper()
		for _, want := range []struct {
			path   string
			status int
			body   string
		}{
			{"/healthz", http.StatusOK, "ok"},
			{"/readyz", readyStatus, readyBody},
		} {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, want.path, nil))
			if w.Code != want.status || w.Body.String() != want.body {
				t.Fatalf("%s %s = %d %q, want %d %q", name, want.path, w.Code, w.Body.String(), want.status, want.body)
			}
			if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("%s %s Content-Type = %q, want text/plain; charset=utf-8", name, want.path, got)
			}
		}
	}

	check("no dependencies", mount(nil, nil), http.StatusOK, "ok")

	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	r := mount(pool, nil)
	check("open SQLite pool", r, http.StatusOK, "ok")
	if err := pool.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	check("closed SQLite pool", r, http.StatusServiceUnavailable, "not ready")

	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	if err := client.Close(); err != nil {
		t.Fatalf("close redis: %v", err)
	}
	check("closed Redis client", mount(nil, client), http.StatusServiceUnavailable, "not ready")
}
