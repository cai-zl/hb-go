// Package redis 提供 Redis 客户端(go-redis/v9)。池由 go-redis 内建,
// 连接只由 boot 启动时建立并 Ping 校验。
package redis

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"hg"
)

// Config 对应 TOML [redis] 段,url 必填。
type Config struct {
	URL         string        `toml:"url"`
	PoolSize    int           `toml:"pool_size"`
	WaitTimeout time.Duration `toml:"wait_timeout"` // TOML 写法 "3s"
}

// Connect 建立客户端并立即 Ping 校验连通性。
func Connect(cfg Config) (*goredis.Client, error) {
	if cfg.URL == "" {
		return nil, hg.Err(500, "redis config: url is required")
	}
	opts, err := goredis.ParseURL(cfg.URL)
	if err != nil {
		return nil, hg.Internal(err)
	}
	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	} else {
		opts.PoolSize = 10
	}
	if cfg.WaitTimeout > 0 {
		opts.PoolTimeout = cfg.WaitTimeout
	}
	client := goredis.NewClient(opts)
	timeout := opts.PoolTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, hg.Internal(err)
	}
	return client, nil
}
