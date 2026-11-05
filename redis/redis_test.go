package redis_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"hb"
	"hb/redis"
)

func msgOf(t *testing.T, err error) string {
	t.Helper()
	var e *hb.Error
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("want *hb.Error, got %v", err)
	}
	return e.Message
}

func TestConnectRejectsEmptyURL(t *testing.T) {
	if _, err := redis.Connect(redis.Config{}); err == nil || !strings.Contains(msgOf(t, err), "url is required") {
		t.Fatalf("err = %v, want url required", err)
	}
}

func TestConnectRejectsBadURL(t *testing.T) {
	if _, err := redis.Connect(redis.Config{URL: "http://nope"}); err == nil {
		t.Fatal("want parse failure for non-redis scheme")
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
	if err := client.Set(t.Context(), "hg_test", "ok", 0).Err(); err != nil {
		t.Fatal(err)
	}
	v, verr := client.Get(t.Context(), "hg_test").Result()
	if verr != nil || v != "ok" {
		t.Fatalf("get = %q (err %v), want ok", v, verr)
	}
}
