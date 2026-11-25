package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	// sqlite 驱动仅在测试里注册,不泄漏给 hb 的消费者
	_ "modernc.org/sqlite"
)

func sqlitePool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := Connect(Config{Driver: "sqlite", URL: ":memory:", MaxConns: 1, AcquireTimeout: time.Second})
	if err != nil {
		t.Fatalf("connect sqlite: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func TestConnectValidation(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{URL: "postgres://x/db"}, "driver is required"},
		{Config{Driver: "oracle", URL: "x"}, `unknown driver "oracle"`},
		{Config{Driver: "sqlite"}, "url is required"},
	}
	for _, tc := range cases {
		if _, err := Connect(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("Connect(%+v): got %v, want error containing %q", tc.cfg, err, tc.want)
		}
	}
}

func TestInvalidPGURL(t *testing.T) {
	cfg := Config{Driver: "pg", URL: "postgres://user:fake-secret@127.0.0.1:badport/db"}
	// 解析 DSN 就失败,还没轮到拨号;错误里不能出现连接串
	pool, err := Connect(cfg)
	if pool != nil || err == nil || err.Error() != "db config: invalid pg url" {
		t.Fatalf("Connect(%+v) = %v, %v; want nil pool and a redacted invalid pg url error", cfg, pool, err)
	}
	if err := Migrate(cfg, nil, nil); err == nil || err.Error() != "db config: invalid pg url" {
		t.Fatalf("Migrate(%+v) = %v; want redacted invalid pg url error", cfg, err)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	pool := sqlitePool(t)
	files := fstest.MapFS{
		// 两个语句一个文件:验证多语句迁移原样执行
		"0001_init.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE t_user (id INTEGER PRIMARY KEY, name TEXT NOT NULL);\nINSERT INTO t_user (name) VALUES ('alice');\n")},
		"0002_order.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t_order (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL);\n")},
	}
	cfg := Config{Driver: "sqlite"}
	for i := 0; i < 2; i++ {
		if err := Migrate(cfg, pool, files); err != nil {
			t.Fatalf("migrate pass %d: %v", i+1, err)
		}
	}
	var n, sumLen int
	if err := pool.QueryRow(`SELECT COUNT(*), LENGTH(checksum) FROM hb_migrations`).Scan(&n, &sumLen); err != nil {
		t.Fatal(err)
	}
	if n != 2 || sumLen != 32 {
		t.Fatalf("ledger rows=%d checksum len=%d, want 2 rows / 32 bytes", n, sumLen)
	}
	var seeded int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM t_user WHERE name = 'alice'`).Scan(&seeded); err != nil || seeded != 1 {
		t.Fatalf("seed row lost: n=%d err=%v", seeded, err)
	}
}

func TestMigrateBadFileName(t *testing.T) {
	pool := sqlitePool(t)
	err := Migrate(Config{Driver: "sqlite"}, pool, fstest.MapFS{
		"notes.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	})
	if err == nil || !strings.Contains(err.Error(), `bad file name "notes.sql"`) {
		t.Fatalf("got %v, want bad file name error", err)
	}
}

func TestMigrateDuplicateNumber(t *testing.T) {
	pool := sqlitePool(t)
	err := Migrate(Config{Driver: "sqlite"}, pool, fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte("CREATE TABLE a (id INTEGER);")},
		"0001_b.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (id INTEGER);")},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate number 1") {
		t.Fatalf("got %v, want duplicate number error", err)
	}
}

func TestMigrateFailureIsAtomic(t *testing.T) {
	pool := sqlitePool(t)
	files := fstest.MapFS{
		"0001_ok.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE t_user (id INTEGER PRIMARY KEY);\n")},
		"0002_bad.sql": &fstest.MapFile{Data: []byte("CREATE TABLE broken (id INTEGER;\n")},
	}
	err := Migrate(Config{Driver: "sqlite"}, pool, files)
	if err == nil || !strings.Contains(err.Error(), "migration 0002_bad.sql") {
		t.Fatalf("got %v, want migration failure", err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM hb_migrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ledger rows=%d err=%v, want 1 (0001 committed, 0002 rolled back)", n, err)
	}
	var tables int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('t_user', 'broken')`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("tables found=%d err=%v, want 1 (t_user only)", tables, err)
	}
}

func TestMysqlRequiresMultiStatements(t *testing.T) {
	cfg := Config{Driver: "mysql", URL: "user:pass@tcp(127.0.0.1:3306)/db"}
	err := Migrate(cfg, nil, fstest.MapFS{})
	if err == nil || !strings.Contains(err.Error(), "multiStatements=true") {
		t.Fatalf("got %v, want multiStatements hint", err)
	}
}

func TestMigratePg(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	files := fstest.MapFS{
		"0001_demo.sql": &fstest.MapFile{Data: []byte("CREATE TABLE hb_demo (id BIGSERIAL PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL DEFAULT now());\nINSERT INTO hb_demo DEFAULT VALUES;\n")},
	}
	cfg := Config{Driver: "pg", URL: url}
	// pg 迁移走库内专用连接,pool 参数不参与执行
	for i := 0; i < 2; i++ {
		if err := Migrate(cfg, nil, files); err != nil {
			t.Fatalf("migrate pass %d: %v", i+1, err)
		}
	}
	pool, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM hb_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("ledger rows=%d err=%v, want 2", n, err)
	}
	var demo int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM hb_demo`).Scan(&demo); err != nil || demo != 1 {
		t.Fatalf("seed rows=%d err=%v, want 1", demo, err)
	}
	for _, ddl := range []string{`DROP TABLE IF EXISTS hb_demo`, `DROP TABLE IF EXISTS hb_migrations`} {
		if _, err := pool.Exec(ddl); err != nil {
			t.Fatalf("cleanup %q: %v", ddl, err)
		}
	}
}
