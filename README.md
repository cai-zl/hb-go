# hb

Go 生态风格的 HTTP 响应信封库 + 一键启动套件。业务路由直接注册原生 Gin handler，
查询、校验、授权和响应策略由业务项目负责。

```
{"code": 0, "message": "ok", "data": {...}}        // 成功
{"code": 404, "message": "user not found"}         // 业务失败(HTTP 仍是 200)
```

上例沿用 hb 的业务协议：常规成功和失败均为 HTTP 200，通过 `code` 表达业务结果。
库内置鉴权、404/405、WS 握手和就绪端点保留各自的 HTTP 状态规则。

## 包一览
module 路径 `github.com/cai-zl/hb-go`,下表包名为简写。

| 包 | 内容 |
| --- | --- |
| `hb` | `Envelope` / `Write`、`Err/Errf/Internal`、`Page`、`Recovery`、`New()` 引擎 |
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

完整 API 清单见文末 [API 速查](#api-速查)。

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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib" // 选定 pg:驱动由业务 import

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/boot"
)

//go:embed migrations/*.sql
var migrations embed.FS

func main() {
	err := boot.Serve([]boot.Mount{userRoutes},
		boot.WithMigrator(migrations),
		boot.WithHook(pushTicker),
		boot.WithAuth(loginChecker{}), // 实现见下方 auth 小节;需要 [auth] 和 [redis]
	)
	if err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

// userRoutes 形状即 gin 社区的 register-function 惯例:
// 在这里用闭包捕获 app,注册具体 handler。
func userRoutes(app *boot.App, rg *gin.RouterGroup) {
	rg.GET("/users/:id", func(c *gin.Context) {
		var name string
		err := app.DB.QueryRowContext(c, `SELECT name FROM t_user WHERE id = $1`,
			c.Param("id")).Scan(&name)
		if err != nil {
			writeBusinessError(c, hb.Err(404, "user not found"))
			return
		}
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: gin.H{"id": c.Param("id"), "name": name}})
	})

	rg.GET("/users", func(c *gin.Context) {
		rows, err := app.DB.QueryContext(c, `SELECT id, name FROM t_user`)
		if err != nil {
			writeBusinessError(c, err)
			return
		}
		defer rows.Close()
		var users []gin.H
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				writeBusinessError(c, err)
				return
			}
			users = append(users, gin.H{"id": id, "name": name})
		}
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: hb.Page(users, uint64(len(users)), 1, 20)})
	})
}

// 业务项目统一的错误出口；已有等价的响应层时直接复用。
func writeBusinessError(c *gin.Context, err error) {
	var e *hb.Error
	errors.As(hb.Internal(err), &e)
	env := hb.Envelope{Code: 500, Message: "internal error"}
	if e != nil {
		env.Message = e.Message
		if e.Code != 0 {
			env.Code = e.Code
		}
	}
	hb.Write(c, http.StatusOK, env)
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

不需要登录端点时，省略 `boot.WithAuth`；业务路由不手动注册 `/auth/*`。

## 多模块组织

业务长大以后,每个模块一个文件(或包),各自声明一个 `boot.Mount`(Mount 的
第一个参数就是组装好的 `App`,handler 闭包直接引用),main.go 只做组装:
各路由模块复用同一业务 HTTP 层；把快速开始的响应与错误策略放到路由共同可用的位置。

```go
// routes/user.go —— package routes
var User boot.Mount = func(app *boot.App, rg *gin.RouterGroup) {
	rg.GET("/users/:id", func(c *gin.Context) {
		var name string
		err := app.DB.QueryRowContext(c, `SELECT name FROM t_user WHERE id = $1`,
			c.Param("id")).Scan(&name)
		if err != nil {
			writeBusinessError(c, hb.Err(404, "user not found"))
			return
		}
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: gin.H{"id": c.Param("id"), "name": name}})
	})
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

[auth]                       # 组装 app.Sessions;此示例用 WithAuth 暴露四个端点

[extra]                      # 业务配置只写 [extra]
rate_limit = 100
```

## 信封与错误(hb 包)

**旧版迁移：** 将 `hb.Wrap` 调用改成原生 `func(c *gin.Context)`。复用业务项目已有的统一响应和错误出口；没有时在项目 HTTP 层定义一处标准，不在每条 handler 重写。

采用 hb 信封时，用 `hb.Write` / `hb.Envelope` 写响应，`hb.Page` 构造分页，`hb.Err` / `hb.Errf` 表达可公开的业务错误。沿用旧协议时，常规响应使用 HTTP 200，由 `code` 表达业务结果。未知错误先用 `hb.Internal` 掩码，只写识别出的 `*hb.Error.Code` / `.Message`，不把底层原因或凭据写给客户端。

配置、启动与依赖操作（`boot`、`db`、`redis`、`s3`、`nacos`、`auth.Sessions`、`crypto`、`password`、`ws`）返回普通 `error`，日志由消费错误的一方处理；非 HTTP 调用不使用 `hb.Err` / `hb.Errf` / `hb.Internal`，只有 HTTP 层才把错误变成可公开的信封。

```go
e := hb.New() // 预装 404/405 框架信封的 gin 引擎(Recovery 需显式 e.Use)
e.Use(hb.Recovery())
e.GET("/hi", func(c *gin.Context) {
	hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: gin.H{"hello": "world"}})
})

// 文件、头部和自定义响应由 Gin 控制，写完直接返回。
e.GET("/raw", func(c *gin.Context) {
	c.Data(http.StatusOK, "text/plain", []byte("raw"))
	return
})

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
  `WithAuth(checker)`、`WithWS(path, onMessage)`、`WithWSClose(path, onMessage, onClose)`、
  `WithLogging(false)`(自带 slog 时关掉内置初始化)、`WithNacos(n)`。
- 启动顺序:日志(log_level > LOG_LEVEL > info)→ 读配置 → 按段连接依赖 →
  迁移 → 钩子 → auth 端点(有 `WithAuth` 时) → 业务及 WS 路由 + `/healthz` `/readyz`(实测 db/redis,不含 s3)→
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

配置 `[auth]` 和 `[redis]` 后，boot 会组装 `app.Sessions`。只有向 `boot.Serve` 或
`boot.ServeWith` 传入 `boot.WithAuth(checker)`，boot 才会自动挂载 `auth` 包实现的
`POST /auth/token`、`POST /auth/refresh`、`POST /auth/revoke`、`GET /auth/me`。
传入该选项时两段配置均必需，不需要在业务 `Mount` 中注册这些端点。

```go
// 使用 encoding/json 和 hb-go 的 auth、boot、password 包。
type loginChecker struct{}

type sessionData struct {
	UID  int64  `json:"uid"`
	Role string `json:"role"`
}

// 实现 auth.AuthChecker。getUser 是业务查询；账号不存在时返回业务码 401 的错误。
func (loginChecker) Check(req auth.LoginReq) (json.RawMessage, error) {
	stored, err := getUser(req.Username)
	if err != nil {
		return nil, err
	}
	if !password.Verify(req.Password, stored.Hash) {
		return nil, hb.Err(401, "invalid username or password")
	}
	return json.Marshal(sessionData{UID: stored.ID, Role: stored.Role})
}

func serveWithAuth(cfg *boot.Config) error {
	return boot.ServeWith(cfg, []boot.Mount{mountPrivate}, boot.WithAuth(loginChecker{}))
}

// 业务 Mount 只注册自己的受保护路由；writeBusinessError 是业务的错误出口。
func mountPrivate(app *boot.App, rg *gin.RouterGroup) {
	protected := rg.Group("/private", auth.Guard(app.Sessions))
	protected.GET("/who", func(c *gin.Context) {
		data, err := auth.From[sessionData](c)
		if err != nil {
			writeBusinessError(c, err)
			return
		}
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: data})
	})
}
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
	conn.Send(map[string]string{"echo": string(msg.Data)})
	app.Hub.Broadcast(map[string]int{"n": app.Hub.Count()})
	app.Hub.SendTo(conn.ID, "hi, just you") // 线上是带引号的 JSON 字符串
}
```

握手时采集 `conn.IP`、`conn.ForwardedFor`、`conn.UserAgent`、`conn.Header`；`conn.ID` 为 32 字符随机 hex。

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

## API 速查

业务层只用下表 API,不要自写:信封/错误/分页、404/405/panic 兜底、启动/信号/停机/健康检查、
配置解析、会话与鉴权中间件、连接池/迁移器、WebSocket Hub、各客户端封装。
包名为简写(见[包一览](#包一览)),用法见上文各节。

### hb

| API | 说明 |
| --- | --- |
| `Err(code int, msg string) error`、`Errf(code int, format string, args ...any) error` | 业务错误,code/message 原样给调用方;code 0 归一为 500 |
| `Internal(err error) error` | 掩码基础设施错误:调用方只见 500 internal error,原文进日志、可 `Unwrap` |
| `Error` | `struct { Code int; Message string }`,方法 `Error() string`、`Unwrap() error`(取回被掩码的原文);用 `errors.As` 取回 |
| `Envelope` | `struct { Code int; Message string; Data any }`,统一响应形状 |
| `Write(c *gin.Context, status int, env Envelope)` | 使用调用者给定的 HTTP 状态写信封 |
| `Page[T any](list []T, total, page, pageSize uint64) *PageData[T]` | 从切片构造分页 data，不复制切片；nil 切片编码为 `null`，非 nil 空切片编码为 `[]` |
| `Response[T]`、`PageData[T]` | 信封解析模型 `struct { Code int; Message string; Data *T }`；分页构造、解析共用 `struct { List []T; Total, Page, PageSize uint64 }` |
| `New() *gin.Engine` | 预装 404/405 框架信封的引擎 |
| `Recovery() gin.HandlerFunc` | panic → 500 internal error 信封;可选,需显式 `Use` |

**旧版迁移：** 显式的 `*hb.Paged` 改用 `*hb.PageData[元素类型]`。传切片可推断类型；裸 `nil` 写作 `hb.Page[元素类型](nil, ...)`，数组先切片。非切片值或有自定义编码的命名切片不要用 `Page`。

### boot

| API | 说明 |
| --- | --- |
| `Serve(mounts []Mount, opts ...Option) error` | 全环境启动:`NACOS_*` 环境变量 → 配置中心,注册 + 热更 |
| `ServeWith(cfg *Config, mounts []Mount, opts ...Option) error` | 显式配置启动;不配 `WithNacos` 即 standalone(不注册、无热更) |
| `Mount` | `type Mount func(app *App, rg *gin.RouterGroup)`,业务路由挂载函数 |
| `Hook` | `type Hook func(app *App) error`,启动钩子;常驻任务在这里自行 `go` 启动 |
| `Option` | `type Option func(*Boot)`,`WithX` 选项的类型;业务只传选项,不直接构造 `Boot` |
| `App` | `struct { Config *Config; DB *sql.DB; Redis *goredis.Client; S3 *s3.S3; Nacos *nacos.Nacos; Hub *ws.Hub; Sessions *auth.Sessions }`,只配置了的依赖非 nil(`Hub` 恒有) |
| `WithMigrator(fsys fs.FS) Option` | 启动时跑迁移;FS 根下直接是 `序号_描述.sql`(`//go:embed migrations/*.sql` 须传 `fs.Sub(migrations, "migrations")`) |
| `WithHook(h Hook) Option` | 追加启动钩子,可多次,按调用顺序执行 |
| `WithAuth(check auth.AuthChecker) Option` | 自动挂载四个 auth 端点;需要 `[auth]` 和 `[redis]`,多次调用时后者覆盖前者 |
| `WithWS(path string, onMessage func(*App, *ws.Conn, ws.Message)) Option` | 挂一个 WebSocket 端点,可多次 |
| `WithWSClose(path string, onMessage func(*App, *ws.Conn, ws.Message), onClose func(id string)) Option` | 同 `WithWS`,另带断连回调(每 Hub 一个,后者覆盖前者) |
| `WithLogging(enabled bool) Option` | `false` 关闭内置 slog 初始化,同时失去热更监听 |
| `WithNacos(n *nacos.Nacos) Option` | `ServeWith` 下接注册与停机反注册 |
| `ParseConfig(text string) (*Config, error)` | 解析 TOML 配置:应用缺省值、校验 port、收集未知顶层键 |
| `Config` | `struct { AppName, Host, Group, LogLevel string; Port int; AdvertiseIP string; Unknown []string; Db *db.Config; Redis *redis.Config; S3 *s3.Config; Auth *auth.Config }` |
| `Snapshot[T any](c *Config) (*T, error)` | `[extra]` 类型化快照:首调解析并缓存,之后查表,推送后自动刷新 |
| `Config.Extra(v any) error` | 每次都把 `[extra]` 解析进 `v`,不缓存 |
| `Config.ReloadExtraFrom(table map[string]any)` | 用给定表刷新 `[extra]` 与全部快照;业务测试里模拟推送用 |
| `DefaultAppName() string` | 二进制名,`app_name` 与 `NACOS_DATA_ID` 的缺省值 |
| `/healthz`、`/readyz` | 内置探针:存活恒 200 `ok`;就绪实测 db/redis(不含 s3),未就绪 503 |

### auth

| API | 说明 |
| --- | --- |
| `AuthChecker` | `interface { Check(LoginReq) (json.RawMessage, error) }`,业务校验凭据后返回已编码的会话 JSON |
| `Guard(sessions *Sessions) gin.HandlerFunc` | 整组路由保护:校验 Bearer token,通过后注入会话 |
| `From[T any](c *gin.Context) (T, error)` | 取登录时写入的会话数据,须配 `Guard` |
| `Token(c *gin.Context) string` | 取本次请求的 access token(`Guard` 注入) |
| `NewSessions(client *goredis.Client, cfg Config) *Sessions` | 建会话存储并应用缺省值 |
| `Sessions.Create(data any) (TokenPair, error)` | 签发一对新 token |
| `Sessions.Get(token string) (SessionRecord, bool, error)` | 按 access token 取会话记录;键不存在或已过期 `ok=false` |
| `Sessions.Refresh(refreshToken string) (TokenPair, bool, error)` | 轮换出新对,旧对随即失效 |
| `Sessions.Revoke(accessToken string) (bool, error)` | 吊销会话,幂等,返回是否存在过 |
| `Sessions.Touch(accessToken string) (bool, error)` | 把两个键的 TTL 重置为 refresh 有效期 |
| `Sessions.AccessTTL() time.Duration`、`Sessions.RefreshTTL() time.Duration` | 解析后的有效期 |
| `Config` | `struct { TokenTTL, RefreshTTL time.Duration; Prefix string }`,全部有缺省值 |
| `TokenResponse` | `struct { AccessToken, RefreshToken, TokenType string; ExpiresIn int64 }`,登录/刷新响应体(snake_case) |
| `LoginReq`、`RefreshReq`、`RevokeReq` | 请求体:`struct { Username, Password string }`、`struct { RefreshToken string }`、`struct { AccessToken string }` |
| `TokenPair`、`SessionRecord` | `struct { AccessToken, RefreshToken string }`、`struct { Data json.RawMessage; Peer string; Exp int64 }` |

### ws

| API | 说明 |
| --- | --- |
| `Upgrade(hub *Hub, w http.ResponseWriter, r *http.Request, onMessage func(*Conn, Message)) error` | 升级握手并注册进 Hub;`boot.WithWS` 已代劳 |
| `NewHub() *Hub` | 建连接注册表;走 boot 时用 `app.Hub` |
| `Hub.Broadcast(msg any)` | 全员广播 |
| `Hub.SendTo(id string, msg any) bool` | 按连接 ID 定向发送，返回是否入队 |
| `Hub.Count() int`、`Hub.Conn(id string) *Conn` | 在线连接数 / 按 id 查连接 |
| `Hub.SetOnDisconnect(fn func(id string))` | 断连回调,每 Hub 一个 |
| `Conn.Send(msg any) bool` | 回发一条消息，返回是否入队 |
| `Conn` | `struct { ID string; IP, ForwardedFor net.IP; UserAgent string; Header http.Header }`,握手期采集;ID 为 32 字符随机 hex |
| `Message` | `struct { Conn *Conn; Data []byte }`;Text/Binary/Pong 载荷都只以 `Data` 到达,没有类型字段 |
| `Heartbeat` | `const Heartbeat = 30 * time.Second`,服务端 Ping 周期 |

`Send`、`SendTo` 和 `Broadcast` 将业务值自动编码为 JSON 文本帧，不添加信封；`nil` 编成 `null`。显式传入的 `hb.Envelope` 也只是普通业务值。返回的 `bool` 只表示是否入队，不保证送达。

**旧版迁移：** `BroadcastRaw`、`SendRaw` 改用 `Broadcast`、`SendTo`；不再需要 `OKMsg` 或 `ErrMsg` 预编码，直接传业务值或显式构造的错误信封。已有完整 JSON 时用 `json.RawMessage`，保留 JSON 含义而非逐字节透传（例如空白可能被压缩、HTML 字符可能被转义）。旧的非 JSON 文本改用 `string` 会在线上变成带引号的 JSON 字符串，不再有发送非 JSON 帧的入口。

### db

| API | 说明 |
| --- | --- |
| `Connect(cfg Config) (*sql.DB, error)` | 建连接池并立即 Ping 校验连通性,失败即报错 |
| `Migrate(cfg Config, pool *sql.DB, fsys fs.FS) error` | 只上行、幂等的文件迁移,一条迁移一条事务,记账表 `hb_migrations` |
| `Config` | `struct { Driver, URL string; MaxConns int; AcquireTimeout time.Duration }`;`driver`、`url` 必填 |

### redis

| API | 说明 |
| --- | --- |
| `Connect(cfg Config) (*goredis.Client, error)` | 建客户端并立即 Ping 校验连通性 |
| `Config` | `struct { URL string; PoolSize int; WaitTimeout time.Duration }`;`url` 必填 |

### s3

| API | 说明 |
| --- | --- |
| `Connect(cfg Config) (*S3, error)` | 建客户端并校验桶存在,失败即报错 |
| `S3.Put(key string, data []byte, contentType string) error` | 上传;contentType 为空用 application/octet-stream |
| `S3.Get(key string) ([]byte, error)` | 读取;对象不存在返回 `ErrNotFound` |
| `S3.Delete(key string) error` | 删除,对象不存在同样视为成功 |
| `S3.Exists(key string) (bool, error)` | 对象是否存在 |
| `S3.PresignGet(key string, ttl time.Duration) (string, error)`、`S3.PresignPut(key string, ttl time.Duration, contentType string) (string, error)` | 预签名 URL,有效期须大于 0 且不超过 7 天(`contentType` 目前未参与签名) |
| `ErrNotFound` | 哨兵错误:`errors.Is(err, s3.ErrNotFound)` |
| `Config` | `struct { Endpoint, Bucket, AccessKeyID, SecretAccessKey, Region string; PathStyle bool }`;前四项必填 |

### nacos

| API | 说明 |
| --- | --- |
| `FromEnv() (Config, error)` | 解析 `NACOS_*` 环境变量;`NACOS_SERVER_ADDR` 必填 |
| `Connect(serverAddr, namespace string) (*Nacos, error)`、`ConnectAuth(serverAddr, namespace, username, password string) (*Nacos, error)` | 建配置中心 + 命名服务客户端(无鉴权 / 带鉴权) |
| `Nacos.GetConfig(dataID, group string) (string, bool, error)` | 读配置文本;配置不存在 `found=false` |
| `Nacos.LoadConfig(dataID, group string, target any) (bool, error)` | 读取并解析 TOML 到 `target` |
| `ParseConfig(text string, target any) error` | 解析 TOML 配置文本 |
| `Nacos.ListenConfig(dataID, group string, onChange func(content string)) error` | 监听配置变更,推送时以完整新内容回调 |
| `Nacos.Register(service, ip string, port uint64, group string) error`、`Nacos.Deregister(service, ip string, port uint64, group string) error` | 注册 / 反注册临时实例 |
| `Config` | `struct { ServerAddr, Namespace, Username, Password, DataID, Group string }` |

### crypto / password

| API | 说明 |
| --- | --- |
| `crypto.SHA256(data []byte) string`、`crypto.HMACSHA256(key, data []byte) string` | 64 字符小写 hex |
| `crypto.RandomHex(n int) string` | n 字节 CSPRNG 随机数的 hex(2n 字符);登录令牌即 `RandomHex(32)`,业务生成随机 ID 直接复用 |
| `crypto.Encrypt(key string, plaintext []byte) (string, error)`、`crypto.Decrypt(key, encoded string) ([]byte, error)` | AES-256-GCM,`base64(nonce‖ct‖tag)`;解密失败一律 `crypto: decrypt failed` |
| `password.Hash(plain string) (string, error)` | 密码哈希,默认 argon2id |
| `password.HashWith(plain string, alg HashAlg) (string, error)` | 指定算法:`password.AlgArgon2id`、`password.AlgBcrypt`、`password.AlgPBKDF2SHA256` |
| `password.Verify(plain, stored string) bool` | 按前缀分派校验;垃圾输入一律 false |
| `password.Algorithm(stored string) string` | 识别算法:`"argon2id"`、`"bcrypt"`、`"pbkdf2"`;不识别返回空串 |

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
2. 直写响应(`Hb::ok(c, ..)`) → 原生 Gin handler；业务项目统一响应与错误策略，可用 `hb.Write` 写信封。
3. `merge_all` → 删除;`hb.New()` 返回预装 404/405 信封的 `*gin.Engine`。
4. `*_secs` 整数配置 → `time.Duration`(TOML 写 `"5s"`/`"2h"`;整数按纳秒)。
5. `database/sql` 统一池,三方言 pg/mysql/sqlite(`[db].driver` 必填,选型由业务
   定死、驱动业务 import;`hb_migrations` 记账表,格式与 sqlx 不兼容——记账独立,
   已应用的 SQL 内容仍以 checksum 比对)。
6. 密码哈希默认 argon2id(与 hb 相同参数),`$pbkdf2$600000$..` 格式互通。
7. 示例以 README 承载(本页),不维护 examples/ 目录;业务服务 `main.go`
   与 `go.mod` 同级。
