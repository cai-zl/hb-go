package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// MountHealth 挂载存活和就绪探针。
func MountHealth(rg *gin.RouterGroup, pool *sql.DB, client *goredis.Client) {
	rg.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	rg.GET("/readyz", func(c *gin.Context) {
		if allReady(pool, client) {
			c.String(http.StatusOK, "ok")
		} else {
			c.String(http.StatusServiceUnavailable, "not ready")
		}
	})
}

func allReady(pool *sql.DB, client *goredis.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if pool != nil {
		if err := pool.PingContext(ctx); err != nil {
			zap.S().Warnw("readiness: db not reachable", "err", err.Error())
			return false
		}
	}
	if client != nil {
		if err := client.Ping(ctx).Err(); err != nil {
			zap.S().Warnw("readiness: redis not reachable", "err", err.Error())
			return false
		}
	}
	return true
}
