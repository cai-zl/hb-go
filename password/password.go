// Package password 提供密码哈希与校验,输出格式与 Rust 版 hb 的 crypto
// 模块逐字节兼容(存量哈希可直接校验、新哈希可被 Rust 校验)。
package password

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// HashAlg 是密码哈希算法标识。
type HashAlg string

const (
	AlgArgon2id     HashAlg = "argon2id"
	AlgBcrypt       HashAlg = "bcrypt"
	AlgPBKDF2SHA256 HashAlg = "pbkdf2"
)

// 参数与 Rust 版 hb 一致,保证跨语言互通。
const (
	argon2MemoryKiB  uint32 = 19456
	argon2Time       uint32 = 2
	argon2Threads    uint8  = 1
	argon2KeyLen     uint32 = 32
	argon2SaltLen           = 16
	bcryptCost              = 12 // Rust bcrypt crate DEFAULT_COST
	pbkdf2Iterations        = 600000
	pbkdf2KeyLen            = 32
	pbkdf2SaltLen           = 16
)

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// Hash 用默认算法 Argon2id 生成 PHC 串。
func Hash(plain string) (string, error) { return HashWith(plain, AlgArgon2id) }

// HashWith 按指定算法生成:
//   - argon2id: $argon2id$v=19$m=19456,t=2,p=1$<b64盐>$<b64哈希>
//   - bcrypt:   $2b$12$...(与 Rust bcrypt crate 同前缀)
//   - pbkdf2:   $pbkdf2$600000$<盐hex>$<派生hex>(PBKDF2-HMAC-SHA256,32 字节派生/16 字节盐)
func HashWith(plain string, alg HashAlg) (string, error) {
	switch alg {
	case AlgArgon2id:
		salt := randomBytes(argon2SaltLen)
		key := argon2.IDKey([]byte(plain), salt, argon2Time, argon2MemoryKiB, argon2Threads, argon2KeyLen)
		enc := base64.RawStdEncoding.EncodeToString
		return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
			argon2MemoryKiB, argon2Time, argon2Threads, enc(salt), enc(key)), nil
	case AlgBcrypt:
		h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
		if err != nil {
			return "", err
		}
		s := string(h)
		if strings.HasPrefix(s, "$2a$") {
			s = "$2b$" + s[4:] // x/crypto 生成 $2a$,统一为 $2b$;两者算法一致
		}
		return s, nil
	case AlgPBKDF2SHA256:
		salt := randomBytes(pbkdf2SaltLen)
		dk, err := pbkdf2.Key(sha256.New, plain, salt, pbkdf2Iterations, pbkdf2KeyLen)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("$pbkdf2$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(dk)), nil
	default:
		return "", errors.New("crypto: unknown algorithm")
	}
}

// Algorithm 识别存量哈希的算法;不识别返回空串。
func Algorithm(stored string) string {
	switch {
	case strings.HasPrefix(stored, "$argon2id$"), strings.HasPrefix(stored, "$argon2i$"), strings.HasPrefix(stored, "$argon2d$"):
		return "argon2id"
	case strings.HasPrefix(stored, "$2a$"), strings.HasPrefix(stored, "$2b$"), strings.HasPrefix(stored, "$2y$"):
		return "bcrypt"
	case strings.HasPrefix(stored, "$pbkdf2$"):
		return "pbkdf2"
	default:
		return ""
	}
}

// Verify 校验密码,按前缀自动分派;非法/不识别的存量串一律 false,不 panic。
func Verify(plain, stored string) bool {
	switch {
	case strings.HasPrefix(stored, "$argon2"):
		return verifyArgon2(plain, stored)
	case strings.HasPrefix(stored, "$2a$"), strings.HasPrefix(stored, "$2b$"), strings.HasPrefix(stored, "$2y$"):
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(plain)) == nil
	case strings.HasPrefix(stored, "$pbkdf2$"):
		return verifyPBKDF2(plain, stored)
	default:
		return false
	}
}

func verifyArgon2(plain, stored string) bool {
	// $argon2id$v=19$m=19456,t=2,p=1$<盐>$<哈希>
	parts := strings.Split(stored, "$")
	if len(parts) != 6 {
		return false
	}
	var m, t uint32 = argon2MemoryKiB, argon2Time
	var p uint8 = argon2Threads
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return false
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return false
		}
		switch k {
		case "m":
			m = uint32(n)
		case "t":
			t = uint32(n)
		case "p":
			p = uint8(n)
		}
	}
	dec := base64.RawStdEncoding.DecodeString
	salt, err := dec(parts[4])
	if err != nil {
		return false
	}
	want, err := dec(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	var got []byte
	switch parts[1] {
	case "argon2id":
		got = argon2.IDKey([]byte(plain), salt, t, m, p, uint32(len(want)))
	case "argon2i", "argon2d":
		got = argon2.Key([]byte(plain), salt, t, m, p, uint32(len(want)))
	default:
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func verifyPBKDF2(plain, stored string) bool {
	// $pbkdf2$600000$<盐hex>$<派生hex>
	parts := strings.Split(stored, "$")
	if len(parts) != 5 {
		return false
	}
	iters, err := strconv.Atoi(parts[2])
	if err != nil || iters < 1 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[3])
	dk, err2 := hex.DecodeString(parts[4])
	if err1 != nil || err2 != nil || len(dk) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, plain, salt, iters, len(dk))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, dk) == 1
}
