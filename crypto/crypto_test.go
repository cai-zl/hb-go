package crypto_test

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"hg/crypto"
)

// ---- 摘要 ----

func TestSHA256Vector(t *testing.T) {
	got := crypto.SHA256([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("SHA256 = %s, want %s", got, want)
	}
}

func TestHMACVector(t *testing.T) {
	got := crypto.HMACSHA256([]byte("key"), []byte("The quick brown fox jumps over the lazy dog"))
	want := "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if got != want {
		t.Fatalf("HMACSHA256 = %s, want %s", got, want)
	}
}

func TestRandomHex(t *testing.T) {
	a := crypto.RandomHex(32)
	if len(a) != 64 {
		t.Fatalf("len = %d, want 64", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("not hex: %v", err)
	}
	if a == crypto.RandomHex(32) {
		t.Fatal("random repeated")
	}
}

// ---- AES-GCM ----

func TestAESRoundTrip(t *testing.T) {
	for _, plain := range []string{"", "hi", "中文内容 with spaces", strings.Repeat("x", 4096)} {
		enc, err := crypto.Encrypt("pass-phrase", []byte(plain))
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		out, err := crypto.Decrypt("pass-phrase", enc)
		if err != nil || string(out) != plain {
			t.Fatalf("roundtrip %q: err=%v out=%q", plain, err, out)
		}
	}
}

func TestDecryptUniformFailure(t *testing.T) {
	enc, _ := crypto.Encrypt("right-key", []byte("secret"))
	raw, _ := base64.StdEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xff // 篡改 tag
	cases := map[string]string{
		"tampered":  base64.StdEncoding.EncodeToString(raw),
		"garbage":   "not-base64!!",
		"too short": base64.StdEncoding.EncodeToString([]byte("short")),
		"empty":     "",
	}
	for name, input := range cases {
		out, err := crypto.Decrypt("right-key", input)
		if err == nil || err.Error() != "500 crypto: decrypt failed" {
			t.Fatalf("%s: err = %v, want uniform 500 crypto: decrypt failed", name, err)
		}
		if out != nil {
			t.Fatalf("%s: out should be nil", name)
		}
	}
	if _, err := crypto.Decrypt("other-key", enc); err == nil || err.Error() != "500 crypto: decrypt failed" {
		t.Fatal("wrong key should fail uniformly")
	}
}
