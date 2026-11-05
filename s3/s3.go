// Package s3 提供 S3 兼容对象存储访问(minio-go/v7)。启动时校验连通与桶存在;
// 对象不存在统一返回可 errors.Is 判断的 ErrNotFound;密钥不落日志。
package s3

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"hg"
)

// maxPresignTTL 预签名有效期上限:7 天,与 AWS SigV4 上限一致。
const maxPresignTTL = 7 * 24 * time.Hour

// ErrNotFound 是对象不存在时的哨兵错误(信封 {"code":404,"message":"object not found"})。
var ErrNotFound = hg.Err(404, "object not found")

// Config 对应 TOML [s3] 段,endpoint/bucket/access_key_id/secret_access_key 必填。
type Config struct {
	Endpoint        string `toml:"endpoint"`
	Bucket          string `toml:"bucket"`
	AccessKeyID     string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
	Region          string `toml:"region"`
	PathStyle       bool   `toml:"path_style"`
}

// S3 包装 minio 客户端与目标桶。
type S3 struct {
	client *minio.Client
	bucket string
}

// Connect 建立客户端并校验桶存在(失败启动即报错)。
// endpoint 形如 "localhost:9000" 走 http,"https://..." 走 https。
func Connect(cfg Config) (*S3, error) {
	switch {
	case cfg.Endpoint == "":
		return nil, hg.Err(500, "s3 config: endpoint is required")
	case cfg.Bucket == "":
		return nil, hg.Err(500, "s3 config: bucket is required")
	case cfg.AccessKeyID == "" || cfg.SecretAccessKey == "":
		return nil, hg.Err(500, "s3 config: access_key_id and secret_access_key are required")
	}
	secure := false
	endpoint := cfg.Endpoint
	if u, err := url.Parse(cfg.Endpoint); err == nil && u.Scheme != "" {
		endpoint = u.Host
		secure = u.Scheme == "https"
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	bucketLookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		bucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: bucketLookup,
	})
	if err != nil {
		return nil, hg.Internal(err)
	}
	s := &S3{client: client, bucket: cfg.Bucket}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := client.BucketExists(ctx, s.bucket)
	if err != nil {
		return nil, hg.Internal(err)
	}
	if !ok {
		return nil, hg.Errf(500, "s3: bucket %q not found", cfg.Bucket)
	}
	return s, nil
}

// isNotFound 把 minio 错误归一为「对象/桶不存在」。
func isNotFound(err error) bool {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket"
	}
	return false
}

// Put 上传对象,contentType 为空时用 application/octet-stream。
func (s *S3) Put(key string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(context.Background(), s.bucket, key,
		strings.NewReader(string(data)), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return hg.Internal(err)
	}
	return nil
}

// Get 读取对象;不存在返回 ErrNotFound(errors.Is 判断)。
func (s *S3) Get(key string) ([]byte, error) {
	obj, err := s.client.GetObject(context.Background(), s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, hg.Internal(err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, hg.Internal(err)
	}
	return data, nil
}

// Delete 删除对象;对象不存在同样视为成功(幂等)。
func (s *S3) Delete(key string) error {
	if err := s.client.RemoveObject(context.Background(), s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return hg.Internal(err)
	}
	return nil
}

// Exists 判断对象是否存在。
func (s *S3) Exists(key string) (bool, error) {
	_, err := s.client.StatObject(context.Background(), s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, hg.Internal(err)
}

// PresignGet 生成下载预签名 URL,有效期上限 7 天。
func (s *S3) PresignGet(key string, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", hg.Errf(500, "presign: ttl must be 1..%s", maxPresignTTL)
	}
	u, err := s.client.PresignedGetObject(context.Background(), s.bucket, key, ttl, nil)
	if err != nil {
		return "", hg.Internal(err)
	}
	return u.String(), nil
}

// PresignPut 生成上传预签名 URL,有效期上限 7 天。
func (s *S3) PresignPut(key string, ttl time.Duration, contentType string) (string, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", hg.Errf(500, "presign: ttl must be 1..%s", maxPresignTTL)
	}
	// shortcut: minio-go 预签名不签 Content-Type,客户端上传时自带该头;参数仅为 API 对齐
	u, err := s.client.PresignedPutObject(context.Background(), s.bucket, key, ttl)
	if err != nil {
		return "", hg.Internal(err)
	}
	return u.String(), nil
}
