// Package db 提供多方言数据库连接(pg / mysql / sqlite)与只上行、幂等的文件迁移。
//
// 连接池统一为 database/sql 的 *sql.DB:驱动由业务在 main.go 里 blank import
// (pg → github.com/jackc/pgx/v5/stdlib,mysql → github.com/go-sql-driver/mysql,
// sqlite → modernc.org/sqlite),选型由业务配置 [db].driver 定死,不做运行时切换。
// 迁移文件按所选方言书写,库原样执行、不做翻译。查询属于业务,hb 只提供池与迁移。
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"hb"
)

// Config 对应 TOML [db] 段,driver 与 url 必填,其余有缺省值。
type Config struct {
	Driver         string        `toml:"driver"` // pg | mysql | sqlite
	URL            string        `toml:"url"`
	MaxConns       int           `toml:"max_connections"`
	AcquireTimeout time.Duration `toml:"acquire_timeout"` // TOML 写法 "5s"
}

// sqlDrivers 方言 → database/sql 驱动名。驱动须由业务 import,否则
// sql.Open 报 unknown driver(pg 驱动由 pgx/v5/stdlib 注册,名 "pgx")。
var sqlDrivers = map[string]string{"pg": "pgx", "mysql": "mysql", "sqlite": "sqlite"}

func (cfg Config) validate() error {
	switch cfg.Driver {
	case "pg", "mysql", "sqlite":
	case "":
		return hb.Err(500, "db config: driver is required (pg, mysql or sqlite)")
	default:
		return hb.Errf(500, "db config: unknown driver %q (want pg, mysql or sqlite)", cfg.Driver)
	}
	if cfg.URL == "" {
		return hb.Err(500, "db config: url is required")
	}
	return nil
}

// Connect 建立 *sql.DB 连接池并立即 Ping 校验连通性(失败启动即报错)。
// AcquireTimeout 的语义收缩为"启动连通超时"——database/sql 没有按次获取超时。
func Connect(cfg Config) (*sql.DB, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	pool, err := sql.Open(sqlDrivers[cfg.Driver], cfg.URL)
	if err != nil {
		return nil, hb.Internal(fmt.Errorf("db connect: %w (import the driver in main.go)", err))
	}
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = 10
	}
	pool.SetMaxOpenConns(maxConns)
	timeout := cfg.AcquireTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, hb.Internal(err)
	}
	return pool, nil
}

// dialect 各方言的记账表 DDL 与记账行 INSERT(占位符、当前时间函数按方言)。
// 记账 id 恒为迁移文件序号,故无需自增列。
type dialect struct {
	create string
	insert string
}

var dialects = map[string]dialect{
	"pg": {
		create: `CREATE TABLE IF NOT EXISTS hb_migrations (
	id          BIGINT PRIMARY KEY,
	name        TEXT NOT NULL,
	checksum    BYTEA NOT NULL,
	applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	duration_ms BIGINT NOT NULL
)`,
		insert: `INSERT INTO hb_migrations (id, name, checksum, duration_ms) VALUES ($1, $2, $3, $4)`,
	},
	"mysql": {
		create: `CREATE TABLE IF NOT EXISTS hb_migrations (
	id          BIGINT PRIMARY KEY,
	name        VARCHAR(255) NOT NULL,
	checksum    VARBINARY(32) NOT NULL,
	applied_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
	duration_ms BIGINT NOT NULL
)`,
		insert: `INSERT INTO hb_migrations (id, name, checksum, applied_at, duration_ms) VALUES (?, ?, ?, CURRENT_TIMESTAMP(6), ?)`,
	},
	"sqlite": {
		create: `CREATE TABLE IF NOT EXISTS hb_migrations (
	id          INTEGER PRIMARY KEY,
	name        TEXT NOT NULL,
	checksum    BLOB NOT NULL,
	applied_at  TEXT NOT NULL DEFAULT (datetime('now')),
	duration_ms INTEGER NOT NULL
)`,
		insert: `INSERT INTO hb_migrations (id, name, checksum, applied_at, duration_ms) VALUES (?, ?, ?, datetime('now'), ?)`,
	},
}

// Migrate 应用 fsys 中文件名形如 "<序号>_<描述>.sql" 的迁移:只上行、幂等,
// 按序号升序执行未应用者,一条迁移一条事务。记账表 hb_migrations 记录
// 序号、文件名、内容 sha256、应用时间与耗时。
//
// pg 经一次性专用连接执行:pgx 默认扩展协议一条 Exec 只能一条语句,
// simple protocol 支持多语句,且专用连接不拖累业务池的预编译缓存。
// mysql 要求 DSN 带 multiStatements=true(库不代改 DSN)。
func Migrate(cfg Config, pool *sql.DB, fsys fs.FS) error {
	d, ok := dialects[cfg.Driver]
	if !ok {
		return hb.Errf(500, "db config: unknown driver %q (want pg, mysql or sqlite)", cfg.Driver)
	}
	exec := pool
	switch cfg.Driver {
	case "pg":
		connCfg, err := pgx.ParseConfig(cfg.URL)
		if err != nil {
			return hb.Internal(fmt.Errorf("db config: %w", err))
		}
		connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
		exec = stdlib.OpenDB(*connCfg)
		exec.SetMaxOpenConns(1)
		defer exec.Close()
	case "mysql":
		if !strings.Contains(cfg.URL, "multiStatements=true") {
			return hb.Err(500, "db config: mysql migrations need multiStatements=true in the DSN")
		}
	}
	if _, err := exec.ExecContext(context.Background(), d.create); err != nil {
		return hb.Internal(err)
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return hb.Internal(err)
	}
	type mig struct {
		id   int64
		name string
		sql  []byte
		sum  []byte
	}
	var pending []mig
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		id, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return hb.Errf(500, "migration: bad file name %q, want \"<number>_<desc>.sql\"", e.Name())
		}
		if prev, dup := seen[id]; dup {
			return hb.Errf(500, "migration: duplicate number %d in %q and %q", id, prev, e.Name())
		}
		seen[id] = e.Name()
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return hb.Internal(err)
		}
		sum := sha256.Sum256(data)
		pending = append(pending, mig{id: id, name: e.Name(), sql: data, sum: sum[:]})
	}
	if len(pending) == 0 {
		return nil
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].id < pending[j].id })

	applied := map[int64]bool{}
	rows, err := exec.QueryContext(context.Background(), `SELECT id FROM hb_migrations`)
	if err != nil {
		return hb.Internal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return hb.Internal(err)
		}
		applied[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return hb.Internal(err)
	}

	for _, m := range pending {
		if applied[m.id] {
			continue
		}
		start := time.Now()
		ctx := context.Background()
		tx, err := exec.BeginTx(ctx, nil)
		if err != nil {
			return hb.Internal(err)
		}
		if _, err := tx.ExecContext(ctx, string(m.sql)); err != nil {
			_ = tx.Rollback()
			return hb.Internal(fmt.Errorf("migration %s: %w", m.name, err))
		}
		if _, err := tx.ExecContext(ctx, d.insert,
			m.id, m.name, m.sum, time.Since(start).Milliseconds()); err != nil {
			_ = tx.Rollback()
			return hb.Internal(err)
		}
		if err := tx.Commit(); err != nil {
			return hb.Internal(err)
		}
		slog.Info("migration applied", "file", m.name)
	}
	return nil
}
