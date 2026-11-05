package password_test

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"hg"
	"hg/password"

	"golang.org/x/crypto/argon2"
)

// ---- 密码哈希:Rust hr 侧生成的参考向量(跨语言兼容) ----

var rustVectors = map[string]string{
	"argon2id": "$argon2id$v=19$m=19456,t=2,p=1$AQIDBAUGBwgJCgsMDQ4PEA$XjJz8yJ75yKOuio9hmmWUSoO4c2Ktym5oVhKj7zY6NY",
	"bcrypt":   "$2b$12$znqGO/XEg8XS2J2Kt6x3t.fKoj6X7yMt7fUbBSkUgKJT/Yw13794.",
	"pbkdf2":   "$pbkdf2$600000$0102030405060708090a0b0c0d0e0f10$f4288e32dfd9caac7df0222ae7ccf8b2109c4afea5bad0cf24b5aeb2d6da721d",
}

func TestRustVectorsVerify(t *testing.T) {
	for alg, stored := range rustVectors {
		if !password.Verify("hunter2", stored) {
			t.Fatalf("%s: rust-generated hash not verified", alg)
		}
		if password.Verify("wrong-password", stored) {
			t.Fatalf("%s: wrong password accepted", alg)
		}
		if got := password.Algorithm(stored); got == "" {
			t.Fatalf("%s: Algorithm() unrecognized", alg)
		}
	}
}

func TestHashRoundTripAllAlgs(t *testing.T) {
	for _, alg := range []password.HashAlg{password.AlgArgon2id, password.AlgBcrypt, password.AlgPBKDF2SHA256} {
		stored, err := password.HashWith("hunter2", alg)
		if err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		if !password.Verify("hunter2", stored) {
			t.Fatalf("%s: roundtrip failed (%s)", alg, stored)
		}
		if password.Verify("nope", stored) {
			t.Fatalf("%s: wrong password accepted", alg)
		}
	}
}

func TestArgon2Shape(t *testing.T) {
	stored, _ := password.Hash("hunter2")
	if !strings.HasPrefix(stored, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("prefix wrong: %s", stored)
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 6 {
		t.Fatalf("parts = %d", len(parts))
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	key, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt) != 16 || len(key) != 32 {
		t.Fatalf("salt/key shape wrong: %d %d %v %v", len(salt), len(key), err1, err2)
	}
}

func TestArgon2NonDefaultParams(t *testing.T) {
	// 非默认参数(m=16384,t=3,p=2)也能被解析校验:直接经 x/crypto 复算构造
	saltB64 := base64.RawStdEncoding.EncodeToString([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	key := argon2.IDKey([]byte("hunter2"), []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, 3, 16384, 2, 32)
	full := fmt.Sprintf("$argon2id$v=19$m=16384,t=3,p=2$%s$%s", saltB64, base64.RawStdEncoding.EncodeToString(key))
	if !password.Verify("hunter2", full) {
		t.Fatal("non-default params not verified")
	}
	if password.Verify("hunter2", strings.Replace(full, "m=16384", "m=16383", 1)) {
		t.Fatal("tampered params accepted")
	}
}

func TestBcryptPrefixes(t *testing.T) {
	stored, _ := password.HashWith("hunter2", password.AlgBcrypt)
	if !strings.HasPrefix(stored, "$2b$12$") {
		t.Fatalf("prefix/cost wrong: %s", stored)
	}
	// $2y$ 变体同样接受(bcrypt 家族语义一致)
	y := "$2y$" + strings.TrimPrefix(stored, "$2b$")
	if !password.Verify("hunter2", y) {
		t.Fatal("$2y$ variant not accepted")
	}
}

func TestPbkdf2Shape(t *testing.T) {
	stored, _ := password.HashWith("hunter2", password.AlgPBKDF2SHA256)
	parts := strings.Split(stored, "$")
	if len(parts) != 5 || parts[1] != "pbkdf2" || parts[2] != "600000" {
		t.Fatalf("shape wrong: %s", stored)
	}
	salt, _ := hex.DecodeString(parts[3])
	dk, _ := hex.DecodeString(parts[4])
	if len(salt) != 16 || len(dk) != 32 {
		t.Fatalf("salt=%d dk=%d", len(salt), len(dk))
	}
}

func TestVerifyNeverPanics(t *testing.T) {
	for _, s := range []string{"", "garbage", "$argon2$", "$argon2id$", "$argon2id$v=19$x$y$z", "$pbkdf2$", "$pbkdf2$x$y$z", "$2b$", "$2b$abc", "hunter2"} {
		if password.Verify("hunter2", s) {
			t.Fatalf("garbage %q accepted", s)
		}
	}
	if password.Algorithm("") != "" {
		t.Fatal("empty should be unrecognized")
	}
}

func TestUnknownAlgorithm(t *testing.T) {
	_, err := password.HashWith("hunter2", "md5")
	var e *hg.Error
	if err == nil || !errors.As(err, &e) || e.Code != 500 {
		t.Fatalf("unknown alg: %v", err)
	}
}
