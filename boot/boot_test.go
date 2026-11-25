package boot_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/auth"
	"github.com/cai-zl/hb-go/boot"
)

func mustParse(t *testing.T, text string) *boot.Config {
	t.Helper()
	cfg, err := boot.ParseConfig(text)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

func TestParseConfigDefaults(t *testing.T) {
	cfg := mustParse(t, "port = 8080")
	if cfg.Port != 8080 || cfg.Host != "0.0.0.0" || cfg.Group != "DEFAULT_GROUP" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.AppName == "" {
		t.Fatal("app_name default empty")
	}
	if len(cfg.Unknown) != 0 || cfg.Db != nil || cfg.Redis != nil {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestParseConfigExplicit(t *testing.T) {
	cfg := mustParse(t, `
app_name = "demo"
host = "127.0.0.1"
group = "G1"
log_level = "debug"
port = 3000
advertise_ip = "10.0.0.1"
[db]
url = "postgres://u:p@127.0.0.1/db"
[redis]
url = "redis://127.0.0.1:6379"
`)
	if cfg.AppName != "demo" || cfg.Host != "127.0.0.1" || cfg.Group != "G1" ||
		cfg.LogLevel != "debug" || cfg.Port != 3000 || cfg.AdvertiseIP != "10.0.0.1" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Db == nil || cfg.Db.URL != "postgres://u:p@127.0.0.1/db" {
		t.Fatalf("db = %+v", cfg.Db)
	}
	if cfg.Redis == nil || cfg.Redis.URL != "redis://127.0.0.1:6379" {
		t.Fatalf("redis = %+v", cfg.Redis)
	}
}

func TestParseConfigUnknownKeys(t *testing.T) {
	cfg := mustParse(t, "port = 1\nfoo = 2\nbar = \"x\"")
	if len(cfg.Unknown) != 2 {
		t.Fatalf("unknown = %v", cfg.Unknown)
	}
	seen := map[string]bool{}
	for _, k := range cfg.Unknown {
		seen[k] = true
	}
	if !seen["foo"] || !seen["bar"] {
		t.Fatalf("unknown = %v", cfg.Unknown)
	}
}

func hbErr(t *testing.T, err error) *hb.Error {
	t.Helper()
	var e *hb.Error
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("want *hb.Error, got %v", err)
	}
	return e
}

func TestParseConfigRequiresPort(t *testing.T) {
	_, err := boot.ParseConfig("app_name = \"x\"")
	if hbErr(t, err).Message != "boot config: port is required" {
		t.Fatalf("err = %v", err)
	}
	_, err = boot.ParseConfig("port = 99999")
	if hbErr(t, err).Message != "boot config: port out of range: 99999" {
		t.Fatalf("err = %v", err)
	}
}

func TestParseConfigBadTOML(t *testing.T) {
	_, err := boot.ParseConfig("port =")
	msg := hbErr(t, err).Message
	if len(msg) < 8 || msg[:8] != "config: " {
		t.Fatalf("err = %v, want config: prefix", err)
	}
}

type appExtra struct {
	Name string `toml:"name"`
	N    int    `toml:"n"`
}

func TestExtraAndSnapshot(t *testing.T) {
	cfg := mustParse(t, "port = 1\n[extra]\nname = \"a\"\nn = 3\n")
	var e appExtra
	if err := cfg.Extra(&e); err != nil {
		t.Fatal(err)
	}
	if e.Name != "a" || e.N != 3 {
		t.Fatalf("extra = %+v", e)
	}
	// 快照:首次解析,二次查表(同指针)
	s1, err := boot.Snapshot[appExtra](cfg)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := boot.Snapshot[appExtra](cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("snapshot should be cached (same pointer)")
	}
	// 推送刷新:新值新指针;旧快照保持旧值
	cfg.ReloadExtraFrom(map[string]any{"name": "b", "n": 4})
	s3, err := boot.Snapshot[appExtra](cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s3 == s1 || s3.Name != "b" || s3.N != 4 {
		t.Fatalf("s3 = %+v", s3)
	}
	if s1.Name != "a" {
		t.Fatalf("old snapshot mutated: %+v", s1)
	}
	// 坏推送:类型解析失败 → 保留旧值
	cfg.ReloadExtraFrom(map[string]any{"name": 123, "n": 4})
	s4, err := boot.Snapshot[appExtra](cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s4.Name != "b" || s4.N != 4 {
		t.Fatalf("bad push should keep old value, got %+v", s4)
	}
}

func TestExtraBadShape(t *testing.T) {
	cfg := mustParse(t, "port = 1\n[extra]\nn = \"not-an-int\"\n")
	var e appExtra
	err := cfg.Extra(&e)
	if err == nil || !strings.HasPrefix(err.Error(), "extra config: ") {
		t.Fatalf("err = %v, want extra config: prefix", err)
	}
}

func TestSnapshotBeforeExtraMissing(t *testing.T) {
	cfg := mustParse(t, "port = 1") // 无 [extra]
	// 空表解析成零值 struct 也应成功
	s, err := boot.Snapshot[appExtra](cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "" || s.N != 0 {
		t.Fatalf("s = %+v", s)
	}
}

// freePort 找一个可用端口(测后归还,存在极小竞态,测试可接受)。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func waitFor(t *testing.T, url string, timeout time.Duration) *http.Response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			return resp
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server not reachable at %s within %v", url, timeout)
	return nil
}

// 全链路:standalone 起服(无任何依赖段)→ 探针/信封/405 → SIGTERM 优雅停机。
func TestServeStandaloneLifecycle(t *testing.T) {
	port := freePort(t)
	cfg := mustParse(t, fmt.Sprintf("port = %d\nhost = \"127.0.0.1\"\nadvertise_ip = \"127.0.0.1\"\nlog_level = \"warn\"\n", port))
	mounts := []boot.Mount{
		func(app *boot.App, rg *gin.RouterGroup) {
			rg.GET("/only-get", func(c *gin.Context) { c.String(200, "hi") })
		},
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	errCh := make(chan error, 1)
	go func() {
		errCh <- boot.ServeWith(cfg, mounts, boot.WithHook(func(app *boot.App) error {
			if app.Config.AppName == "" {
				t.Error("hook got empty app_name")
			}
			return nil
		}))
	}()

	resp := waitFor(t, base+"/healthz", 5*time.Second)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}

	resp = waitFor(t, base+"/readyz", 3*time.Second)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("readyz = %d, want 200 (no deps configured)", resp.StatusCode)
	}

	resp, err := http.Get(base + "/definitely-missing")
	if err != nil {
		t.Fatal(err)
	}
	var env hb.Envelope
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || json.Unmarshal(raw, &env) != nil || env.Code != 404 || env.Message != "Not Found" {
		t.Fatalf("404 = %d %q", resp.StatusCode, raw)
	}

	resp, err = http.Post(base+"/only-get", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("405 = %d %q", resp.StatusCode, raw)
	}
	var env405 hb.Envelope
	if json.Unmarshal(raw, &env405) != nil || env405.Code != 405 {
		t.Fatalf("405 body = %q", raw)
	}

	// 给 srv.Serve goroutine 与 signal.Notify 留出就位时间
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ServeWith = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeWith did not return after SIGTERM")
	}
}

// 钩子失败:不监听、直接返回错误。
func TestHookFailurePreventsStart(t *testing.T) {
	port := freePort(t)
	cfg := mustParse(t, fmt.Sprintf("port = %d\nhost = \"127.0.0.1\"\nadvertise_ip = \"127.0.0.1\"\n", port))
	boom := hb.Err(500, "hook boom")
	err := boot.ServeWith(cfg, nil, boot.WithHook(func(app *boot.App) error { return boom }))
	if hbErr(t, err).Message != "hook boom" {
		t.Fatalf("err = %v", err)
	}
}

// auth 段无 redis 段 → 启动报错。
func TestAuthRequiresRedis(t *testing.T) {
	port := freePort(t)
	cfg := mustParse(t, fmt.Sprintf("port = %d\nhost = \"127.0.0.1\"\nadvertise_ip = \"127.0.0.1\"\n[auth]\n", port))
	err := boot.ServeWith(cfg, nil, boot.WithLogging(false))
	if hbErr(t, err).Message != "boot config: [auth] requires a [redis] section" {
		t.Fatalf("err = %v", err)
	}
}

type unusedAuthChecker struct{}

func (unusedAuthChecker) Check(auth.LoginReq) (json.RawMessage, error) {
	panic("checker called during startup")
}

func TestWithAuthRequiresAuthConfig(t *testing.T) {
	cfg := mustParse(t, "port = 1\nadvertise_ip = \"127.0.0.1\"")
	mounts := []boot.Mount{func(*boot.App, *gin.RouterGroup) {
		t.Fatal("mounted business routes without [auth]")
	}}
	err := boot.ServeWith(cfg, mounts, boot.WithAuth(unusedAuthChecker{}), boot.WithLogging(false))
	if hbErr(t, err).Message != "boot config: WithAuth requires an [auth] section" {
		t.Fatalf("err = %v", err)
	}
}

// Serve(env 模式)缺 NACOS_SERVER_ADDR → 报错。
func TestServeRequiresNacosEnv(t *testing.T) {
	t.Setenv("NACOS_SERVER_ADDR", "")
	err := boot.Serve(nil)
	if hbErr(t, err).Message != "missing env: NACOS_SERVER_ADDR" {
		t.Fatalf("err = %v", err)
	}
	_ = os.Unsetenv("NACOS_SERVER_ADDR")
}

// Duration 配置解码:TOML 字符串 "5s"/"2h" → time.Duration;
// 缺省值在 Connect/NewSessions 应用(解析期零值是正常态)。
func TestDurationDecode(t *testing.T) {
	cfg := mustParse(t, `
port = 1
[auth]
token_ttl = "1h"
refresh_ttl = "168h"
`)
	if cfg.Auth.TokenTTL != time.Hour || cfg.Auth.RefreshTTL != 7*24*time.Hour {
		t.Fatalf("auth ttl = %v / %v", cfg.Auth.TokenTTL, cfg.Auth.RefreshTTL)
	}
	cfg2 := mustParse(t, "port = 1\n[db]\nurl = \"x\"\nacquire_timeout = \"9s\"\n")
	if cfg2.Db.AcquireTimeout != 9*time.Second {
		t.Fatalf("AcquireTimeout = %v, want 9s", cfg2.Db.AcquireTimeout)
	}
	// 缺省值:零值 Config → NewSessions 应用默认 2h / 30d
	s := auth.NewSessions(nil, auth.Config{})
	if s.AccessTTL() != 2*time.Hour || s.RefreshTTL() != 30*24*time.Hour {
		t.Fatalf("auth defaults = %v / %v", s.AccessTTL(), s.RefreshTTL())
	}
	// 坏时长字符串 → config: 错误
	if _, err := boot.ParseConfig("port = 1\n[auth]\ntoken_ttl = \"oops\"\n"); err == nil ||
		!strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("err = %v", err)
	}
}
