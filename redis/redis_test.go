package redis_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/redis"
)

func TestConnectRejectsEmptyURL(t *testing.T) {
	client, err := redis.Connect(redis.Config{})
	var e *hb.Error
	if client != nil || err == nil || err.Error() != "redis config: url is required" || errors.As(err, &e) {
		t.Fatalf("Connect(Config{}) = %v, %v; want nil client and a plain url-required error", client, err)
	}
}

func TestConnectRejectsBadURL(t *testing.T) {
	for _, raw := range []string{"http://nope", "redis://user:fake-secret@127.0.0.1:badport/0"} {
		client, err := redis.Connect(redis.Config{URL: raw})
		if client != nil || err == nil || err.Error() != "redis config: invalid url" || strings.Contains(err.Error(), "fake-secret") {
			t.Fatalf("Connect(%q) = %v, %v; want nil client and a redacted invalid-url error", raw, client, err)
		}
	}
}

func TestConnectPing(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping integration test")
	}
	client, err := redis.Connect(redis.Config{URL: url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	if err := client.Set(t.Context(), "hb_test", "ok", 0).Err(); err != nil {
		t.Fatal(err)
	}
	v, verr := client.Get(t.Context(), "hb_test").Result()
	if verr != nil || v != "ok" {
		t.Fatalf("get = %q (err %v), want ok", v, verr)
	}
}
