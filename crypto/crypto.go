// Package crypto 提供摘要、HMAC 与 AES-GCM 加密,
// 输出格式与 Rust 版 hb 的 crypto 模块逐字节兼容。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"

	"hb"
)

// SHA256 返回 64 字符小写 hex。
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HMACSHA256 返回 64 字符小写 hex。
func HMACSHA256(key, data []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// RandomHex 返回 n 字节 CSPRNG 随机数的 hex(2n 字符)。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read 永不失败(Go 文档保证)
	return hex.EncodeToString(b)
}

func deriveKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// Encrypt 用口令字符串加密:SHA-256 派生 32 字节 AES-256-GCM 密钥,
// 随机 12 字节 nonce,输出 base64(nonce‖ciphertext‖tag)。
func Encrypt(key string, plaintext []byte) (string, error) {
	gcm, err := gcmFor(key)
	if err != nil {
		return "", hb.Internal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密 Encrypt 的输出;口令错/串坏统一报 "crypto: decrypt failed",
// 原始原因仅 debug 级日志。
func Decrypt(key, encoded string) ([]byte, error) {
	fail := func(err error) ([]byte, error) {
		slog.Debug("decrypt failed", "err", err)
		return nil, hb.Err(500, "crypto: decrypt failed")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fail(err)
	}
	gcm, err := gcmFor(key)
	if err != nil {
		slog.Debug("aes init failed", "err", err)
		return fail(err)
	}
	if len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return fail(errors.New("ciphertext too short"))
	}
	out, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return fail(err)
	}
	return out, nil
}

// 返回纯 error 而非 *hb.Error:typed-nil 具体类型装进接口后 err != nil 恒真,
// 会把每次成功路径误判为失败。
func gcmFor(key string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
