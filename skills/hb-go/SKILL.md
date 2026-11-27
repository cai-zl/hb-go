---
name: hb-go
description: 在使用 github.com/cai-zl/hb-go 的 Go/Gin 业务仓库开发或评审代码时，查找并复用框架已有的 HTTP 信封、分页、启动装配、配置、日志、鉴权、迁移、Redis、S3、Nacos、WebSocket 和密码哈希 API，避免业务层重复封装。
---

# hb-go 业务开发

## 先查 API，再写业务逻辑

1. 确认目标项目使用 Go 模块 `github.com/cai-zl/hb-go`，根包名为 `hb`。在消费方 Go 模块目录用 `go list -mod=readonly -m -json github.com/cai-zl/hb-go` 核对选中的 `Version` 和 `Replace`；若依赖未引入，不为使用本 Skill 而安装它。
2. 按下面的任务表选入口。导入根包用 `github.com/cai-zl/hb-go`；子包在该路径后加 `/boot`、`/auth`、`/db`、`/redis`、`/s3`、`/nacos`、`/ws`、`/crypto`。精确签名以当前依赖版本为准，例如在消费方模块目录运行 `go doc github.com/cai-zl/hb-go Page` 或 `go doc github.com/cai-zl/hb-go/boot ServeWith`；也可直接读实际依赖源码（以 `go list -m -json` 的 `Dir` 为准，若有 `Replace` 则核对替换来源）。若命令失败且没有可读源码，用任务表提供参考建议并注明未验证版本；不要据此修改代码或自动安装、升级工具链。
3. 先搜业务仓库已有的挂载点、依赖和封装。框架或业务已有等价能力就复用，不加第二套 handler 包装、会话中间件、客户端或迁移器；只补校验、授权、查询和业务数据等业务逻辑。

## 必须遵守的约束

- **先查证再动手。** 新增或修改通用能力前，找目标项目的调用点，核对该项目实际依赖版本的 `go doc` 或源码。已有适用 API 或业务适配层就复用。只有两处都没有满足明确需求的实现时，才补最小业务逻辑，并说明缺口。缺少版本或源码证据时，报告缺项，暂停依赖该事实的改动。
- **保留业务职责和协议。** 直接注册原生 `func(c *gin.Context)`；凭据核验、角色/资源授权、数据查询、校验及业务码仍由业务负责。沿用旧 hb 协议时，常规成功和失败均为 HTTP 200，通过信封的 `code` 表达结果。头部、文件下载和自定义协议由 Gin 控制，写完直接返回；内置鉴权、404/405、就绪端点保留各自的状态规则。使用可选依赖前核对对应配置，缺项时报告，不在请求中临时补连。
- **交付前核对。** 确认改动调用了对应框架入口且没有新增等价实现；对受影响的状态码、信封字段或业务授权跑定向测试。不把未运行的检查报为通过。

## 禁止项

| 禁止 | 改用或处理方式 |
| --- | --- |
| 猜签名、假定示例适配所有版本；为了凑 API 擅自改框架、引入或升级 hb-go。 | 以业务项目实际选中的版本为准；无法确认时说明缺失证据。 |
| 在各 handler 重复手拼信封、自建等价分页或上游响应结构，或向调用方暴露底层错误、凭据或令牌。 | 优先复用业务项目已有的统一响应和错误出口；没有时在项目 HTTP 层定义一处标准。采用 hb 信封时用 `hb.Write` / `hb.Envelope`。 |
| 在采用 `boot.Serve` / `boot.ServeWith` 的项目再写启动/停机逻辑、重复注册 `/healthz` 或 `/readyz`、重建已有客户端/连接池/Hub，或在 handler 内重连。 | 走 `boot.Mount`，复用 `boot.App` 已装配的依赖，新增业务探针须有不同职责和路由。 |
| 在业务 `Mount` 手动挂载 `/auth/token`、`/auth/refresh`、`/auth/revoke` 或 `/auth/me`，或调用已移除的手动挂载 API；自写 Bearer 解析或直接读 `hb.session` / `hb.token`；把 Redis 故障报成 HTTP 401，或把通过 `Guard` 当成已获资源授权。 | 用 `boot.WithAuth(checker)` / `boot.WithClientAuth(clientCheck)` 自动挂载固定端点，业务路由仍用 `auth.Guard`、`auth.From[T]` / `auth.Token`；HTTP 401 只用于令牌缺失或失效等认证失败，保留业务授权检查。 |
| 就地修改 `boot.Snapshot[T]` 返回的共享值；自写等价迁移器或将含 `migrations/` 子目录的原始 embed FS 直接传给 `boot.WithMigrator`。 | 快照只读；迁移用 `boot.WithMigrator` 或 `db.Migrate`，按需先 `fs.Sub` 并处理错误。 |
| 访问不存在的 `ws.Message.Kind`，默认套 HTTP 信封，或用 `WithWS` 代替握手鉴权与 Origin 限制。 | 用 `msg.Data`；直接传业务值给 `Send`、`SendTo` 或 `Broadcast`，只有已编码的 JSON 才用 `json.RawMessage`。复用业务的握手校验。 |
| 自制等价密码哈希，用普通 SHA-256 存储密码，或把密码、密钥、令牌写进日志。 | 用 `crypto.Hash` / `crypto.Verify`；日志只记录不会暴露秘密的诊断信息。 |
| 自建第二套日志系统：自己跑 `zap.New` / `zap.ReplaceGlobals` / `log.New`，或引入 logrus、zerolog 等其他日志库。 | 复用 boot 装配的 zap：业务代码一律用 `app.Logger`（`*zap.SugaredLogger`）或启动后的全局 `zap.S()`，级别统一走配置 `log_level`；boot 初始化之前的启动失败直接打 stderr。 |

## 按任务选现有入口

| 要做的事 | 先用 |
| --- | --- |
| 统一 HTTP 响应、业务错误、分页 | 原生 Gin handler 调用业务项目统一出口；可公开的业务错误用 `hb.Err` / `hb.Errf`，未知错误先用 `hb.Internal` 掩码，只写识别出的 `*hb.Error.Code` / `.Message`。分页用 `hb.Page`；解析上游信封用 `hb.Response[T]` / `hb.PageData[T]`，不自定义同形结构。 |
| Gin 引擎与服务启动 | Nacos 模式用 `boot.Serve`，显式配置用 `boot.ServeWith`；业务路由写成 `boot.Mount`，启动任务用 `boot.WithHook`。启动器已经装配 `/healthz` 和 `/readyz`；启动日志的 `commit` 由 boot 自动打（`go build` 嵌入的主模块 VCS 提交，缺 `.git` 的构建用 `-ldflags "-X github.com/cai-zl/hb-go/boot.Commit=<hash>"` 注入），业务侧零代码，不要自写 git 采集。独立建引擎用 `hb.New()`，需要 panic 信封时显式装 `hb.Recovery()`。 |
| 应用配置、客户端 | 用 `boot.ParseConfig` 解析 TOML，业务设置放 `[extra]`，用 `boot.Snapshot[T](app.Config)` 读，勿修改返回的共享快照。走 boot 时复用已配置的 `app.DB`、`app.Redis`、`app.S3`、`app.Nacos`，不在 handler 中重连；独立使用才查各包的 `Connect` / `ConnectAuth`。S3 `Get` 未命中用 `errors.Is(err, s3.ErrNotFound)`。 |
| 日志 | 用 boot 初始化的 zap（console 编码、stderr、带 caller）：业务代码用 `app.Logger`（`*zap.SugaredLogger`，`WithWS`/`WithHook` 回调同样适用）或全局 `zap.S()`，如 `app.Logger.Infow("order paid", "orderID", id)`；级别走配置 `log_level`（优先级 `log_level` > `LOG_LEVEL` > `info`，Nacos 推送热更），不自建 logger。 |
| 登录、会话与密码 | 实现 `auth.AuthChecker`：`Check(auth.LoginReq) (json.RawMessage, error)` 校验凭据，在业务实现内用 `json.Marshal` 编码具体的会话类型。向 `boot.Serve` / `boot.ServeWith` 传 `boot.WithAuth(checker)` 自动挂载四个端点，需要 `[auth]` 和 `[redis]`；配置这两段但不传选项时，只组装 `app.Sessions`，不暴露端点。客户端登录实现 `auth.ClientChecker`（`Check(auth.ClientReq) (json.RawMessage, error)`，校验 `client_id`/`client_secret`）并传 `boot.WithClientAuth`，可与 `WithAuth` 只用其一；`/auth/token` 按 `grant_type` 分流：缺失/空或 `"password"` 走用户 Checker，`"client_credentials"` 走客户端 Checker，其他值返回业务码 400 `unsupported grant type`。两类会话共用同一套 token 对与 refresh/revoke/TTL，业务用 `auth.Kind(c)`（`auth.KindUser`/`auth.KindClient`）区分身份并自行限制路由。受保护路由组挂 `auth.Guard(app.Sessions)`，原生 Gin handler 用 `auth.From[T](c)` 取数据并检查错误；需要 access token 时用 `auth.Token(c)`。密码用 `crypto.Hash` / `crypto.Verify`，不拿普通 SHA-256 当密码哈希。 |
| DB 迁移 | 启动时用 `boot.WithMigrator`，独立场景用 `db.Migrate`；迁移文件放在传入 FS 的根目录，命名如 `0001_init.sql`。用 `//go:embed migrations/*.sql` 时，先取 `fs.Sub(migrations, "migrations")`，处理错误后再传子 FS。 |
| WebSocket | 用 `boot.WithWS` / `boot.WithWSClose` 和 `app.Hub`，不另建连接表或写循环。`ws.Message` 只有 `Conn` / `Data`；业务值直接传给 `Send`、`SendTo` 或 `Broadcast`，自动编码 JSON、不套 HTTP 信封。只有已编码的 JSON 才用 `json.RawMessage`。鉴权和 Origin 限制仍由业务决定并实施。 |
| 摘要、随机标识、兼容加解密 | 查 `crypto.SHA256`、`crypto.HMACSHA256`、`crypto.RandomHex`、`crypto.Encrypt` / `crypto.Decrypt`，不重写框架已有格式。 |

## 最小示例：列表分页

包内独立示例：把 `mountUsers` 加入服务现有的 `[]boot.Mount`，业务只替换列表、总数和分页参数。

```go
package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/boot"
)

func mountUsers(_ *boot.App, rg *gin.RouterGroup) {
	rg.GET("/users", func(c *gin.Context) {
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: hb.Page([]int{1, 2}, 42, 2, 20)})
	})
}
```

HTTP 200 参考响应：

```json
{"code":0,"message":"ok","data":{"list":[1,2],"total":42,"page":2,"pageSize":20}}
```
