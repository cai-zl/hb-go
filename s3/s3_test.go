package s3_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cai-zl/hb-go/s3"
)

func TestConnectValidatesRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  s3.Config
		want string
	}{
		{"endpoint", s3.Config{}, "endpoint is required"},
		{"bucket", s3.Config{Endpoint: "localhost:9000"}, "bucket is required"},
		{"keys", s3.Config{Endpoint: "localhost:9000", Bucket: "b"}, "access_key_id and secret_access_key are required"},
	}
	for _, tc := range cases {
		_, err := s3.Connect(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestConnectRejectsBadEndpoint(t *testing.T) {
	// 坏端口让 minio.New 直接失败:不回显端点(可能含凭据),也不发请求
	client, err := s3.Connect(s3.Config{Endpoint: "http://user:fake-secret@127.0.0.1:badport/", Bucket: "b", AccessKeyID: "key", SecretAccessKey: "secret"})
	if client != nil || err == nil || err.Error() != "s3 config: failed to initialize client" {
		t.Fatalf("Connect(bad endpoint) = %v, %v; want nil client and a redacted init failure", client, err)
	}
}

func integrationS3(t *testing.T) *s3.S3 {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_S3_ENDPOINT not set; skipping integration test")
	}
	client, err := s3.Connect(s3.Config{
		Endpoint:        endpoint,
		Bucket:          os.Getenv("TEST_S3_BUCKET"),
		AccessKeyID:     os.Getenv("TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("TEST_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return client
}

func TestS3RoundTrip(t *testing.T) {
	s := integrationS3(t)
	key := "hb-test/roundtrip.txt"

	if err := s.Put(key, []byte("hello s3"), "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(key)
	if err != nil || string(got) != "hello s3" {
		t.Fatalf("get = %q (err %v), want hello s3", got, err)
	}
	ok, err := s.Exists(key)
	if err != nil || !ok {
		t.Fatalf("exists = %v (err %v), want true", ok, err)
	}
	if _, err := s.Get("hb-test/missing.txt"); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
	u, err := s.PresignGet(key, 60)
	if err != nil || !strings.Contains(u, "X-Amz-Signature") {
		t.Fatalf("presign url = %q (err %v), want signed URL", u, err)
	}
	if err := s.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(key); err != nil { // 幂等
		t.Fatalf("delete twice: %v", err)
	}
	ok, err = s.Exists(key)
	if err != nil || ok {
		t.Fatalf("exists after delete = %v (err %v), want false", ok, err)
	}
}

func TestPresignLimits(t *testing.T) {
	s := integrationS3(t)
	for _, secs := range []time.Duration{0, 604801 * time.Second} {
		if _, err := s.PresignGet("k", secs); err == nil || !strings.Contains(err.Error(), "presign") {
			t.Fatalf("PresignGet(%v) err = %v, want limit error", secs, err)
		}
		if _, err := s.PresignPut("k", secs, "text/plain"); err == nil || !strings.Contains(err.Error(), "presign") {
			t.Fatalf("PresignPut(%v) err = %v, want limit error", secs, err)
		}
	}
}
