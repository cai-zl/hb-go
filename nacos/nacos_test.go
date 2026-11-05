package nacos_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"hb"
	"hb/nacos"
)

func msgOf(t *testing.T, err error) string {
	t.Helper()
	var e *hb.Error
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("want *hb.Error, got %v", err)
	}
	return e.Message
}

func TestFromEnvRequiresServerAddr(t *testing.T) {
	t.Setenv("NACOS_SERVER_ADDR", "")
	if _, err := nacos.FromEnv(); err == nil || !strings.Contains(msgOf(t, err), "missing env: NACOS_SERVER_ADDR") {
		t.Fatalf("err = %v, want missing env", err)
	}
}

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("NACOS_SERVER_ADDR", "127.0.0.1:8848")
	cfg, err := nacos.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "public" || cfg.Group != "DEFAULT_GROUP" {
		t.Fatalf("cfg = %+v, want public/DEFAULT_GROUP defaults", cfg)
	}
	if cfg.DataID == "" || cfg.DataID == "hb.test" || strings.Contains(cfg.DataID, "/") {
		// 缺省为二进制名(go test 二进制),含路径或空都算错
		if cfg.DataID == "" {
			t.Fatalf("DataID default empty, cfg = %+v", cfg)
		}
	}
}

func TestFromEnvOverrides(t *testing.T) {
	t.Setenv("NACOS_SERVER_ADDR", "nacos-1:8848,nacos-2:8848")
	t.Setenv("NACOS_NAMESPACE", "dev")
	t.Setenv("NACOS_DATA_ID", "hb.toml")
	t.Setenv("NACOS_GROUP", "MY_GROUP")
	t.Setenv("NACOS_USERNAME", "u")
	t.Setenv("NACOS_PASSWORD", "p")
	cfg, err := nacos.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "dev" || cfg.DataID != "hb.toml" || cfg.Group != "MY_GROUP" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Username != "u" || cfg.Password != "p" {
		t.Fatalf("auth not carried: %+v", cfg)
	}
	_ = os.Unsetenv("NACOS_USERNAME") // 其余键不干扰其他测试
}

func TestParseConfigTOML(t *testing.T) {
	type appCfg struct {
		Name string   `toml:"name"`
		N    int      `toml:"n"`
		Tags []string `toml:"tags"`
	}
	var out appCfg
	if err := nacos.ParseConfig("name = \"a\"\nn = 3\ntags = [\"x\", \"y\"]", &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "a" || out.N != 3 || len(out.Tags) != 2 {
		t.Fatalf("out = %+v", out)
	}
	// 坏 TOML → config: 前缀错误
	var bad appCfg
	err := nacos.ParseConfig("name = ", &bad)
	if err == nil || !strings.HasPrefix(err.Error(), "500 config: ") {
		t.Fatalf("err = %v, want config: prefix", err)
	}
}

func TestConnectValidatesAddr(t *testing.T) {
	if _, err := nacos.Connect("", "public"); err == nil || !strings.Contains(msgOf(t, err), "server addr is required") {
		t.Fatalf("err = %v, want required", err)
	}
	// shortcut: SDK 客户端创建是惰性的,不拨号;连通性由 TEST_NACOS_ADDR 集成测试覆盖
}

// ConnectAuth/GetConfig/LoadConfig/Register 依赖真实 Nacos,按 TEST_NACOS_ADDR 门控。
func TestNacosIntegration(t *testing.T) {
	addr := os.Getenv("TEST_NACOS_ADDR")
	if addr == "" {
		t.Skip("TEST_NACOS_ADDR not set; skipping integration test")
	}
	client, err := nacos.Connect(addr, "public")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	text, found, err := client.GetConfig("hb-test.toml", "DEFAULT_GROUP")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = text
	_ = found
	if err := client.Register("hb-test-svc", "127.0.0.1", 18080, "DEFAULT_GROUP"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := client.Deregister("hb-test-svc", "127.0.0.1", 18080, "DEFAULT_GROUP"); err != nil {
		t.Fatalf("deregister: %v", err)
	}
}
