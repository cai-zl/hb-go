package s3_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"hg"
	"hg/s3"
)

func msgOf(t *testing.T, err error) string {
	t.Helper()
	var e *hg.Error
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("want *hg.Error, got %v", err)
	}
	return e.Message
}

func codeOf(t *testing.T, err error) int {
	t.Helper()
	var e *hg.Error
	if err == nil || !errors.As(err, &e) {
		t.Fatalf("want *hg.Error, got %v", err)
	}
	return e.Code
}

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
		if err == nil || !strings.Contains(msgOf(t, err), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
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
	key := "hg-test/roundtrip.txt"

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
	if _, err := s.Get("hg-test/missing.txt"); err == nil || codeOf(t, err) != 404 {
		t.Fatalf("missing err = %v, want 404 object not found", err)
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
		if _, err := s.PresignGet("k", secs); err == nil || !strings.Contains(msgOf(t, err), "presign") {
			t.Fatalf("PresignGet(%v) err = %v, want limit error", secs, err)
		}
		if _, err := s.PresignPut("k", secs, "text/plain"); err == nil || !strings.Contains(msgOf(t, err), "presign") {
		}
	}
}
