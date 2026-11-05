# hb

Go 生态风格的统一 HTTP 响应信封库 + 一键启动套件。业务 handler 只写 `(any, error)`,
框架负责把一切归一成统一 JSON 信封。

```
{"code": 0, "message": "ok", "data": {...}}        // 成功
{"code": 404, "message": "user not found"}         // 业务失败(HTTP 仍是 200)
```

HTTP 响应**永远 200**(错误只体现在 `code`);仅框架级错误(404/405、WS 握手失败)
保留真实状态码并带信封 body。

## 包一览
module 路径 `github.com/cai-zl/hb-go`,下表包名为简写。

| 包 | 内容 |
| --- | --- |
| `hb` | 信封、`Wrap((any, error))`、`Err/Errf/Internal`、`Page`、`Recovery`、`New()` 引擎 |
| `hb/boot` | 配置加载、Nacos 接入、依赖组装、`Serve/ServeWith`、健康检查、热更 |
| `hb/db` | 多方言连接池(pg/mysql/sqlite)+ 序号迁移器(`hb_migrations` 记账) |
| `hb/redis` | go-redis 客户端 |
| `hb/s3` | minio 客户端(上传/下载/删除/预签名) |
| `hb/nacos` | 配置中心 + 服务注册 |
| `hb/ws` | WebSocket Hub(广播/定向/心跳) |
| `hb/auth` | Redis 会话 + OAuth2 风格四端点 |
| `hb/crypto` | AES-256-GCM、SHA256/HMAC、随机数 |
| `hb/password` | argon2id/bcrypt/pbkdf2 哈希与校验 |

包间依赖只有两个方向:所有子包 → `hb` 根包;`hb/boot` → 各子包(仅做组装)。
`db/redis/s3/nacos/ws/password` 等叶子互不依赖;`auth → crypto`(令牌随机数)是
唯一的叶子间依赖。

## 快速开始

`go get github.com/cai-zl/hb-go` 后,在**业务 module 根目录**直接写 `main.go`
(与 `go.mod` 同级,不需要 `cmd/` 目录):

```
yourapp/
├── go.mod               // module yourapp;require github.com/cai-zl/hb-go
├── main.go              // ← 与 go.mod 同级
├── migrations/
│   └── 0001_init.sql    // 序号_描述.sql,经 go:embed 交给迁移器
└── routes/
    ├── user.go          // var User boot.Mount(闭包捕获 app)
    └── order.go         // 每个业务模块一个文件
```

`main.go`:

```go
package main

import (
	"embed"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib" // 选定 pg:驱动由业务 import

	"hb"
	"github.com/cai-zl/hb-go/boot"
)

//go:embed migrations/*.sql
var migrations embed.FS

func main() {
	err := boot.Serve([]boot.Mount{userRoutes},
		boot.WithMigrator(migrations),
		boot.WithHook(pushTicker),
	)
	if err != nil {
		panic(err)
	}
}

// userRoutes 形状即 gin 社区的 register-function 惯例:
// 在这里用闭包捕获 app,注册具体 handler。
func userRoutes(app *boot.App, rg *gin.RouterGroup) {
	rg.GET("/users/:id", hb.Wrap(func(c *gin.Context) (any, error) {
		var name string
		err := app.DB.QueryRowContext(c, `SELECT name FROM t_user WHERE id = $1`,
			c.Param("id")).Scan(&name)
		if err != nil {
			return nil, hb.Err(404, "user not found") // 业务错误:code 原样给调用方
		}
		return gin.H{"id": c.Param("id"), "name": name}, nil
	}))

	rg.GET("/users", hb.Wrap(func(c *gin.Context) (any, error) {
		rows, err := app.DB.QueryContext(c, `SELECT id, name FROM t_user`)
		if err != nil {
			return nil, err // 裸 error:Wrap 自动掩码为 500 internal error
		}
		defer rows.Close()
		var users []gin.H
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				return nil, err
			}
			users = append(users, gin.H{"id": id, "name": name})
		}
		return hb.Page(users, uint64(len(users)), 1, 20), nil // 分页信封
	}))
}

// 启动钩子:常驻任务自行 go 启动;返回 error 则整体启动失败。
func pushTicker(app *boot.App) error {
	go func() {
		for range time.Tick(5 * time.Second) {
			app.Hub.Broadcast(map[string]any{"seq": time.Now().Unix()})
		}
	}()
	return nil
}
```

## 多模块组织

业务长大以后,每个模块一个文件(或包),各自声明一个 `boot.Mount`(Mount 的
第一个参数就是组装好的 `App`,handler 闭包直接引用),main.go 只做组装:
```go
// routes/user.go —— package routes
var User boot.Mount = func(app *boot.App, rg *gin.RouterGroup) {
	rg.GET("/users/:id", hb.Wrap(func(c *gin.Context) (any, error) {
		var name string
		err := app.DB.QueryRowContext(c, `SELECT name FROM t_user WHERE id = $1`,
			c.Param("id")).Scan(&name)
		if err != nil {
			return nil, hb.Err(404, "user not found")
		}
		return gin.H{"id": c.Param("id"), "name": name}, nil
	}))
}

// routes/order.go 同理:var Order boot.Mount = func(app *boot.App, rg *gin.RouterGroup) { ... }
```

```go
// main.go —— 组装即一行
err := boot.Serve([]boot.Mount{routes.User, routes.Order},
	boot.WithMigrator(migrations))
```

带鉴权的模块在各自 Mount 里挂 `auth.Guard(app.Sessions)` 子组;WebSocket 模块
经 `boot.WithWS("/ws", handler)` 挂接,不占 Mount。若拆成多个 go.mod(独立仓库/
共享库),用 `go.work` 把 hb 与业务模块放进同一 workspace 即可,代码不变。

配置(TOML,来自 Nacos 配置中心或内联文件)——配置段即开关:

```toml
app_name = "demo"
port = 3000
log_level = "info"

[db]
[db]
driver = "pg"               # 必填:pg | mysql | sqlite
max_connections = 10
acquire_timeout = "5s"      # time.Duration 字符串;整数则按纳秒

[redis]
url = "redis://127.0.0.1:6379"

[extra]                      # 业务配置只写 [extra]
rate_limit = 100
```

## 信封与错误(hb 包)

```go
// handler 惯用法:唯一出口 (any, error)
e := hb.New() // 预装 404/405 框架信封的 gin 引擎(Recovery 需显式 e.Use)
e.Use(hb.Recovery())
e.GET("/hi", hb.Wrap(func(c *gin.Context) (any, error) {
	return gin.H{"hello": "world"}, nil        // → {"code":0,"message":"ok","data":{...}}
}))

// 业务错误:code/message 原样给调用方,HTTP 仍 200
return nil, hb.Err(404, "user not found")
return nil, hb.Errf(400, "bad page size %d", n)

// 裸 error(db 驱动错等):Wrap 自动掩码 500 "internal error",原文 warn 进日志
return nil, err

// 需要 HTTP 语义/自定义形状时自行写出,返回 (nil, nil) 即可,Wrap 不覆盖
c.Data(200, "text/plain", []byte("raw")); return nil, nil

// 分页:JSON 形如 data:{"list":[...],"total":42,"page":2,"pageSize":20}
return hb.Page(rows, total, page, pageSize), nil

// 中间件写框架级错误(Guard、401 等)
hb.Write(c, http.StatusUnauthorized, hb.Envelope{Code: 401, Message: "unauthorized"})
c.Abort()
```

错误是标准 `error`:`hb.Err` 返回 `error`,需要 code 时用 `errors.As` 取回
`*hb.Error`(字段 `Code`/`Message`,`Unwrap()` 暴露掩码原文):

```go
var e *hb.Error
if errors.As(err, &e) && e.Code == 404 { ... }
```

解析上游服务响应(客户端侧):

```go
var r hb.Response[User]        // {"code":..,"message":..,"data":User}
json.Unmarshal(body, &r)
if r.Code != 0 { /* r.Data == nil */ }

var p hb.PageData[User]        // 解析分页信封的 data
```

## boot

两种启动:

```go
boot.Serve(mounts, opts...)                      // 全环境:NACOS_* 环境变量 → 配置中心,注册+热更
boot.ServeWith(cfg, mounts, opts...)             // 显式配置;配 WithNacos 则仅注册,无热更
```

- `Mount = func(app *App, rg *gin.RouterGroup)`;`App{Config, DB, Redis, S3, Nacos, Hub, Sessions}`
  只配置了的依赖字段非 nil。
- 选项:`WithMigrator(embed.FS)`、`WithHook(func(*App) error)`(可多次,顺序执行)、
  `WithWS(path, onMessage)`、`WithWSClose(path, onMessage, onClose)`、
  `WithLogging(false)`(自带 slog 时关掉内置初始化)、`WithNacos(n)`。
- 启动顺序:日志(log_level > LOG_LEVEL > info)→ 读配置 → 按段连接依赖 →
  迁移 → 钩子 → 路由 + `/healthz` `/readyz`(实测 db/redis,不含 s3)→
  监听 → 注册 → 热更监听 → SIGINT/SIGTERM 优雅停机 + 反注册。
- 业务配置写 `[extra]`,类型化快照读取,推送后自动刷新:

```go
type appCfg struct {
	RateLimit int `toml:"rate_limit"`
}

cfg, err := boot.Snapshot[appCfg](app.Config) // 首调解析并缓存,之后查表
if err != nil {
	return nil, err
}
_ = cfg.RateLimit // 推送后重新调用 Snapshot 得到新值
```

- 日志级别与 `[extra]` 支持热更;坏 TOML 推送整体保持旧值(warn)。
- 未知顶层键仅 warn 不拦截启动;`[auth]` 缺 `[redis]` 启动报错。

## auth

Redis 双键会话(`{prefix}a:{token}` / `{prefix}r:{token}` 互存对端),access
有效期由记录内 exp 判定,键 TTL = refresh TTL。

```go
// check:业务凭据校验,返回会话数据;凭据错返回业务错误(HTTP 200 + 业务码)
check := func(req auth.LoginReq) (any, error) {
	stored, err := getUser(req.Username)
	if err != nil {
		return nil, hb.Err(401, "invalid username or password")
	}
	if !password.Verify(req.Password, stored.Hash) {
		return nil, hb.Err(401, "invalid username or password")
	}
	return sessionData{UID: stored.ID, Role: stored.Role}, nil
}

mount := auth.Routes(app.Sessions, check) // POST /auth/token|refresh|revoke + GET /auth/me
mount(rg)

// 受保护路由
protected := rg.Group("/private", auth.Guard(app.Sessions))
protected.GET("/who", func(c *gin.Context) {
	data, err := auth.From[sessionData](c)
	if err != nil {
		return nil, err
	}
	return data, nil
})
```

响应为 OAuth2 风格 snake_case:`{"access_token":..,"refresh_token":..,"token_type":"Bearer","expires_in":7200}`。
token 缺失/失效 → HTTP 401 + 信封;凭据错 → HTTP 200 + 业务码 401。

```go
auth.NewSessions(redisClient, auth.Config{
	TokenTTL:   2 * time.Hour,       // toml: token_ttl = "2h"
	RefreshTTL: 30 * 24 * time.Hour, // toml: refresh_ttl = "720h"
	Prefix:     "hb:auth:",          // Redis 键前缀
})
// Sessions.Create/Get/Refresh/Revoke/Touch,AccessTTL()/RefreshTTL() time.Duration
```

## ws

```go
boot.WithWS("/ws", echo) // 或 WithWSClose("/ws", echo, onClose func(id string))

func echo(app *boot.App, conn *ws.Conn, msg ws.Message) {
	switch msg.Kind {
	case ws.Text, ws.Binary, ws.Pong:
		conn.Send(map[string]any{"echo": string(msg.Data)}) // 带 conn 元数据
	}
	app.Hub.Broadcast(map[string]any{"n": app.Hub.Count()})
	app.Hub.SendTo(conn.ID, "hi, just you") // 定向发送须持有 conn.ID
	conn.ID; conn.IP; conn.ForwardedFor; conn.UserAgent; conn.Header // ID 为 32 字符随机 hex
}
```

写缓冲 256 条,慢连接丢弃并 warn(不阻塞);Ping 间隔 `ws.Heartbeat = 30s`;
消息按到达顺序回调;pong 载荷同样透传给 onMessage。

## s3

```go
s, err := s3.Connect(s3.Config{
	Endpoint: "127.0.0.1:9000", Bucket: "demo",
	AccessKeyID: "...", SecretAccessKey: "...", // 永不落日志
})
s.Put("a/b.txt", data, "text/plain")
data, err := s.Get("a/b.txt")        // 未命中:errors.Is(err, s3.ErrNotFound)
ok, _ := s.Exists("a/b.txt")
_ = s.Delete("a/b.txt")              // 幂等
url, _ := s.PresignGet("a/b.txt", 10*time.Minute)      // 上限 7*24h
url, _ = s.PresignPut("a/b.txt", 10*time.Minute, "text/plain")
```

## db

连接池统一为 `database/sql` 的 `*sql.DB`;**选型由业务定死**:配置 `[db].driver`
必填(pg | mysql | sqlite),驱动在业务 main.go 里 blank import,hb 不代引:

```go
import (
	_ "github.com/jackc/pgx/v5/stdlib" // pg;mysql → go-sql-driver/mysql;sqlite → modernc.org/sqlite
)

pool, err := db.Connect(db.Config{Driver: "pg", URL: "postgres://...", MaxConns: 10, AcquireTimeout: 5 * time.Second})
err = db.Migrate(dbCfg, pool, migrationsFS) // 业务仓库 //go:embed migrations/*.sql
```

| driver | 驱动包(业务自引) | URL 形态 |
| --- | --- | --- |
| `pg` | `github.com/jackc/pgx/v5/stdlib` | `postgres://user:pass@host/db` |
| `mysql` | `github.com/go-sql-driver/mysql` | `user:pass@tcp(host)/db?multiStatements=true` |
| `sqlite` | `modernc.org/sqlite`(纯 Go 免 CGO) | 文件路径 `app.db` 或 `:memory:` |

- 迁移文件名 `序号_描述.sql`:只上行、幂等、一条迁移一条事务,记账表
  `hb_migrations`(name、checksum、applied_at、duration_ms)。
- 迁移 SQL **直接用所选方言书写**,库原样执行、不做翻译:pg 走一次性专用连接
  (多语句文件可用,不拖累业务池);mysql 必须 DSN 带 `multiStatements=true`
  (库只校验提示,不代改 DSN);sqlite 同样支持多语句文件。
- `AcquireTimeout` 是启动连通超时(`database/sql` 无按次获取超时)。
- 未查到行直接判 `errors.Is(err, sql.ErrNoRows)`(database/sql 惯例,库不做转换)。

### sqlc

查询代码推荐业务侧用 [sqlc](https://sqlc.dev) 生成,生成代码直接持有 `app.DB`:

```yaml
# sqlc.yaml —— database/sql 目标,三种引擎通吃
sql_package: "database/sql"
```

选型已锁死 pg 的业务可改 `sql_package: "pgx/v5"`(性能更好,代价是放弃换库可能)。

## nacos

```go
cfg, _ := nacos.FromEnv() // NACOS_SERVER_ADDR 必填;NAMESPACE/USERNAME/PASSWORD/DATA_ID/GROUP
n, err := nacos.ConnectAuth(cfg.ServerAddr, cfg.Namespace, cfg.Username, cfg.Password)
text, found, _ := n.GetConfig(dataID, group)
var c appCfg
found, _ = n.LoadConfig(dataID, group, &c)   // 坏 TOML:config: 前缀错误
_ = n.ListenConfig(dataID, group, func(content string) { /* 推送 */ })
_ = n.Register("svc", "10.0.0.1", 3000, "DEFAULT_GROUP")   // 临时实例
_ = n.Deregister("svc", "10.0.0.1", 3000, "DEFAULT_GROUP")
```

## crypto / password

```go
sum := crypto.SHA256(data)                    // 小写 hex
mac := crypto.HMACSHA256(key, data)
n, _ := crypto.RandomHex(32)                  // 64 字符随机 hex
enc, _ := crypto.Encrypt("口令", data)        // base64(nonce‖ct‖tag)
dec, err := crypto.Decrypt("口令", enc)       // 失败一律 "crypto: decrypt failed"

h, _ := password.Hash("hunter2")              // argon2id(默认)
h, _ = password.HashWith("x", password.AlgBcrypt)
ok := password.Verify("hunter2", h)           // 前缀分派;垃圾输入一律 false
alg := password.Algorithm(h)                  // "argon2id" / "bcrypt" / "pbkdf2-sha256"
```

三算法互相兼容(可与 Rust hb 生成的哈希互通,已双向验证)。

## 测试

单元测试零外部依赖;集成测试按环境变量门控:

| 变量 | 覆盖 |
| --- | --- |
| `TEST_DATABASE_URL` | 迁移幂等/原子性/记账 |
| `TEST_REDIS_URL` | 登录/轮换/吊销全流程 |
| `TEST_S3_ENDPOINT`(含 BUCKET/ACCESS_KEY/SECRET_KEY) | 上传下载/404/预签名 |
| `TEST_NACOS_ADDR` | 注册/反注册/配置读取 |

## 与 Rust hb 的差异

功能对齐,API 按 Go 惯例重塑:

1. `Result<T, HbError>` → `error`;`HbError` → `*hb.Error`(errors.As 可取回)。
2. 直写响应(`Hb::ok(c, ..)`)→ handler 返回 `(any, error)`,`Wrap` 统一出口。
3. `merge_all` → 删除;`hb.New()` 返回预装 404/405 信封的 `*gin.Engine`。
4. `*_secs` 整数配置 → `time.Duration`(TOML 写 `"5s"`/`"2h"`;整数按纳秒)。
5. `database/sql` 统一池,三方言 pg/mysql/sqlite(`[db].driver` 必填,选型由业务
   定死、驱动业务 import;`hb_migrations` 记账表,格式与 sqlx 不兼容——记账独立,
   已应用的 SQL 内容仍以 checksum 比对)。
6. 密码哈希默认 argon2id(与 hb 相同参数),`$pbkdf2$600000$..` 格式互通。
7. 示例以 README 承载(本页),不维护 examples/ 目录;业务服务 `main.go`
   与 `go.mod` 同级。
