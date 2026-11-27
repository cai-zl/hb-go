// Package boot 一键启动:配置 → 连接依赖 → 迁移 → 钩子 → 路由+健康检查 → 监听
// → 注册 Nacos → 配置热更监听 → 停机反注册。
//
// 两种启动方式:
//
//	Serve(mounts, opts...)          // 全环境模式:NACOS_* 环境变量 → 配置中心,注册+热更
//	ServeWith(cfg, mounts, opts...) // 显式模式:自带 Config;WithNacos 可选接注册(无热更)
//
// 配置段即开关:[db]/[redis]/[s3]/[auth] 写了才连接与检查,不写即关闭该依赖;
// 写了但缺必填项(url 等)启动报错。业务配置只写 [extra]。
package boot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pelletier/go-toml/v2"
	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/auth"
	"github.com/cai-zl/hb-go/db"
	"github.com/cai-zl/hb-go/internal/api"
	"github.com/cai-zl/hb-go/nacos"
	hbredis "github.com/cai-zl/hb-go/redis"
	"github.com/cai-zl/hb-go/s3"
	"github.com/cai-zl/hb-go/ws"
)

// Mount 业务路由挂载:App 组装完成后、监听前调用——请求期经 app 取
// DB/Hub/Sessions 等。形状即 gin 社区的 register-function 惯例。
type Mount func(app *App, rg *gin.RouterGroup)

// Hook 启动钩子:按挂接顺序执行,任一返回错误即整体失败(不监听、不注册)。
// 常驻任务在钩子里自行 go 启动。
type Hook func(app *App) error

// Config 服务配置(来自 Nacos 配置中心或调用方)。
// log_level 与 [extra] 支持热更新(env 模式推送即生效)。
type Config struct {
	AppName     string // app_name,缺省二进制名
	Host        string // host,缺省 0.0.0.0
	Group       string // Nacos 注册分组,缺省 DEFAULT_GROUP
	LogLevel    string // log_level,空=未配置(优先级 log_level > LOG_LEVEL > info)
	Port        int    // port,必填
	AdvertiseIP string // advertise_ip,空=自动探测,失败启动报错
	Unknown     []string

	Db    *db.Config      `toml:"db"`    // [db] 段,写了才连
	Redis *hbredis.Config `toml:"redis"` // [redis] 段,写了才连
	S3    *s3.Config      `toml:"s3"`    // [s3] 段,写了才连(启动即校验桶)
	Auth  *auth.Config    `toml:"auth"`  // [auth] 段,依赖 [redis]

	mu    sync.RWMutex
	extra map[string]any
	snaps map[reflect.Type]*snapEntry
}

// snapEntry 类型化快照缓存项:解析闭包 + 当前值(*T)。解析只发生在首次
// Snapshot 调用与推送时刻,之后命中是查表。
type snapEntry struct {
	parse func(table map[string]any) (any, error)
	value any
}

// knownKeys 顶层固定键;其余打 warn 不拦截(业务配置请写 [extra])。
var knownKeys = map[string]bool{
	"app_name": true, "host": true, "group": true, "log_level": true,
	"port": true, "advertise_ip": true,
	"db": true, "redis": true, "s3": true, "auth": true, "extra": true,
}

// durationKeys 各配置段里允许写成字符串时长的键("5s"/"2h"/"168h"),
// 解码前归一成纳秒整数(time.Duration 的底层表示);整数原样按纳秒解。
var durationKeys = map[string]map[string]bool{
	"db":    {"acquire_timeout": true},
	"redis": {"wait_timeout": true},
	"auth":  {"token_ttl": true, "refresh_ttl": true},
}

// Commit 启动日志里提交信息的注入覆盖值(默认自动读 go build 嵌入的主模块 VCS 提交):
//	go build -ldflags "-X github.com/cai-zl/hb-go/boot.Commit=<hash>"
// 注入后原样打印;适用于源码不含 .git 的构建(如容器镜像)。
var Commit string

// normalizeDurations 就地改写 raw,失败返回 config: 错误。
func normalizeDurations(raw map[string]any) error {
	for section, keys := range durationKeys {
		st, _ := raw[section].(map[string]any)
		for k := range keys {
			s, ok := st[k].(string)
			if !ok {
				continue
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return fmt.Errorf("config: %s.%s: invalid duration %q", section, k, s)
			}
			st[k] = int64(d)
		}
	}
	return nil
}

// App 全家桶服务状态:Serve 组装后交给 mounts 与钩子;
// 只配置了的依赖字段非 nil。
type App struct {
	Config   *Config
	Logger   *zap.SugaredLogger // zap 全局 Logger 的 Sugared 视图;业务直接用它,WithLogging(false) 时为 nil
	DB       *sql.DB
	Redis    *goredis.Client
	S3       *s3.S3
	Nacos    *nacos.Nacos
	Hub      *ws.Hub
	Sessions *auth.Sessions
}

// ParseConfig 解析 TOML 配置:应用缺省值、校验 port、收集未知顶层键。
func ParseConfig(text string) (*Config, error) {
	var raw map[string]any
	if err := toml.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := normalizeDurations(raw); err != nil {
		return nil, err
	}
	norm, err := toml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var rc struct {
		Port        int             `toml:"port"`
		AppName     string          `toml:"app_name"`
		Host        string          `toml:"host"`
		Group       string          `toml:"group"`
		LogLevel    string          `toml:"log_level"`
		AdvertiseIP string          `toml:"advertise_ip"`
		Db          *db.Config      `toml:"db"`
		Redis       *hbredis.Config `toml:"redis"`
		S3          *s3.Config      `toml:"s3"`
		Auth        *auth.Config    `toml:"auth"`
		Extra       map[string]any  `toml:"extra"`
	}
	if err := toml.Unmarshal(norm, &rc); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if rc.Port == 0 {
		return nil, errors.New("boot config: port is required")
	}
	if rc.Port < 0 || rc.Port > 65535 {
		return nil, fmt.Errorf("boot config: port out of range: %d", rc.Port)
	}
	appName := rc.AppName
	if appName == "" {
		appName = DefaultAppName()
	}
	host := rc.Host
	if host == "" {
		host = "0.0.0.0"
	}
	group := rc.Group
	if group == "" {
		group = "DEFAULT_GROUP"
	}
	var unknown []string
	for k := range raw {
		if !knownKeys[k] {
			unknown = append(unknown, k)
		}
	}
	extra := rc.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	return &Config{
		AppName: appName, Host: host, Group: group, LogLevel: rc.LogLevel,
		Port: rc.Port, AdvertiseIP: rc.AdvertiseIP, Unknown: unknown,
		Db: rc.Db, Redis: rc.Redis, S3: rc.S3, Auth: rc.Auth,
		extra: extra, snaps: map[reflect.Type]*snapEntry{},
	}, nil
}

// DefaultAppName 运行时二进制名(去路径),app_name 与 NACOS_DATA_ID 的缺省值。
func DefaultAppName() string {
	if len(os.Args) == 0 {
		return "hb-app"
	}
	return filepath.Base(os.Args[0])
}

// Extra 把 [extra] 段解析进业务自定义配置类型(热更新后读到的是新值;
// 按请求调用无妨,表很小)。
func (c *Config) Extra(v any) error {
	c.mu.RLock()
	table := c.extra
	c.mu.RUnlock()
	return extraInto(table, v)
}

// extraInto 表 → 目标(经 TOML 往返)。
func extraInto(table map[string]any, v any) error {
	b, err := toml.Marshal(table)
	if err != nil {
		return fmt.Errorf("extra config: %v", err)
	}
	if err := toml.Unmarshal(b, v); err != nil {
		return fmt.Errorf("extra config: %v", err)
	}
	return nil
}

// Snapshot 类型化配置快照:首次调用解析并缓存,之后每次都是查表(零解析);
// 推送热更或 ReloadExtraFrom 时自动重解析刷新。返回的 *T 是快照——推送后旧
// *T 仍是旧值,重新调用即得新值。同一 T 全程共享一份缓存(双检防并发首调)。
func Snapshot[T any](c *Config) (*T, error) {
	key := reflect.TypeOf((*T)(nil))
	c.mu.RLock()
	e, ok := c.snaps[key]
	c.mu.RUnlock()
	if ok {
		return e.value.(*T), nil
	}
	var v T
	if err := c.Extra(&v); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.snaps[key]; ok {
		return e.value.(*T), nil
	}
	c.snaps[key] = &snapEntry{
		parse: func(table map[string]any) (any, error) {
			var parsed T
			if err := extraInto(table, &parsed); err != nil {
				return nil, err
			}
			return &parsed, nil
		},
		value: &v,
	}
	return &v, nil
}

// ReloadExtraFrom 用给定 [extra] 表刷新配置与全部快照——env 模式由推送自动
// 调用,公开是为了在业务测试里模拟推送。单个类型解析失败保留旧值(warn)。
func (c *Config) ReloadExtraFrom(table map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.extra = table
	for ty, e := range c.snaps {
		v, err := e.parse(table)
		if err != nil {
			zap.S().Warnw("snapshot reload kept old value", "type", ty.String(), "err", err.Error())
			continue
		}
		e.value = v
	}
	zap.S().Infow("extra config reloaded")
}

// warnUnknown 未知顶层键打 warn 提示,不拦截启动。
func (c *Config) warnUnknown() {
	for _, k := range c.Unknown {
		zap.S().Warnw("unknown top-level config key; business config belongs in [extra]", "key", k)
	}
}

// ---- 启动 ----

type wsRoute struct {
	path      string
	onMessage func(*App, *ws.Conn, ws.Message) // 业务回调,可访问 App(hub 等)
}

// Boot 一次启动的全部选项。
type Boot struct {
	mounts      []Mount
	migrator    fs.FS
	hooks       []Hook
	logging     bool
	nacos       *nacos.Nacos
	authCheck   auth.AuthChecker
	clientCheck auth.ClientChecker
	wsRoutes    []wsRoute
	onClose     func(id string)
}

// Option 函数式选项(WithMigrator/WithHook/WithAuth/WithClientAuth/WithWS/WithWSClose/WithLogging/WithNacos)。
type Option func(*Boot)

// WithMigrator 挂接内嵌迁移(业务仓库 //go:embed migrations/*.sql 后传入),
// 不调用则不跑迁移。需要配置 [db]。
func WithMigrator(fsys fs.FS) Option { return func(b *Boot) { b.migrator = fsys } }

// WithLogging 日志开关:默认 boot 自动初始化 zap(console 编码,stderr,级别优先级
// log_level > LOG_LEVEL > info,全局 zap.S() 与 app.Logger 可用);自带日志时传 false 关闭。
func WithLogging(enabled bool) Option { return func(b *Boot) { b.logging = enabled } }

// WithHook 追加启动钩子,可多次调用,按调用顺序执行。
func WithHook(h Hook) Option { return func(b *Boot) { b.hooks = append(b.hooks, h) } }

// WithAuth 让 boot 自动挂载四个 /auth/* 端点,需要配置 [auth] 和 [redis]。
// 业务只负责校验登录凭据并返回已编码的会话 JSON。多次调用时后者覆盖前者。
func WithAuth(check auth.AuthChecker) Option { return func(b *Boot) { b.authCheck = check } }

// WithClientAuth 让 boot 自动挂载四个 /auth/* 端点并启用客户端登录
// (POST /auth/token 带 grant_type = "client_credentials"),需要配置 [auth] 和 [redis]。
// 业务只负责校验 client_id/client_secret 并返回已编码的会话 JSON。
// 可与 WithAuth 只用其一;多次调用时后者覆盖前者。
func WithClientAuth(check auth.ClientChecker) Option {
	return func(b *Boot) { b.clientCheck = check }
}

// WithWS 挂一个 WebSocket 端点,可多次调用。消息回调收 Conn(握手期采集的
// IP/请求头)与客户端消息,按到达顺序保序执行。
func WithWS(path string, onMessage func(*App, *ws.Conn, ws.Message)) Option {
	return func(b *Boot) { b.wsRoutes = append(b.wsRoutes, wsRoute{path, onMessage}) }
}

// WithWSClose WithWS + 断连回调:连接关闭(客户端 Close 或网络错误)时以
// 连接 id 调用。回调每 Hub 仅一个,多次调用后者覆盖前者,须在启动前挂好。
func WithWSClose(path string, onMessage func(*App, *ws.Conn, ws.Message), onClose func(id string)) Option {
	return func(b *Boot) {
		b.wsRoutes = append(b.wsRoutes, wsRoute{path, onMessage})
		b.onClose = onClose
	}
}

// WithNacos 接上已连好的 Nacos 句柄(仅 ServeWith 需要:注册/停机反注册生效;
// 配置热更只在 env 模式 Serve 下启用)。
func WithNacos(n *nacos.Nacos) Option { return func(b *Boot) { b.nacos = n } }

func newBoot(mounts []Mount, opts []Option) *Boot {
	b := &Boot{mounts: mounts, logging: true}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Serve 全环境启动:NACOS_* 环境变量 → Nacos 配置中心读 TOML → 按配置段
// 连接依赖 → 迁移 → 钩子 → 监听 → 注册 → 热更监听 → 停机反注册。
func Serve(mounts []Mount, opts ...Option) error {
	nc, err := nacos.FromEnv()
	if err != nil {
		return err
	}
	client, err := nacos.ConnectAuth(nc.ServerAddr, nc.Namespace, nc.Username, nc.Password)
	if err != nil {
		return err
	}
	text, found, err := client.GetConfig(nc.DataID, nc.Group)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("nacos config not found: %s/%s", nc.DataID, nc.Group)
	}
	cfg, err := ParseConfig(text)
	if err != nil {
		return err
	}
	b := newBoot(mounts, opts)
	// 热更监听与日志初始化绑定(与 Rust 一致:日志关闭则无监听)
	return b.run(cfg, client, &hotReload{dataID: nc.DataID, group: nc.Group})
}

// ServeWith 显式启动:调用方自带 Config。不传 WithNacos 即 standalone
// (不注册、无热更);接上句柄则注册/停机反注册生效。
func ServeWith(cfg *Config, mounts []Mount, opts ...Option) error {
	b := newBoot(mounts, opts)
	return b.run(cfg, b.nacos, nil)
}

// hotReload env 模式的配置热更参数。
type hotReload struct {
	dataID string
	group  string
}

func (b *Boot) run(cfg *Config, nc *nacos.Nacos, hot *hotReload) error {
	if (b.authCheck != nil || b.clientCheck != nil) && cfg.Auth == nil {
		return errors.New("boot config: WithAuth/WithClientAuth requires an [auth] section")
	}

	var applyLog func(string)
	var logger *zap.SugaredLogger
	if b.logging {
		logger, applyLog = initLogging(resolveLevel(cfg))
	}
	zap.S().Infow("starting", "app", cfg.AppName, "commit", gitCommit())
	cfg.warnUnknown()

	advertise := cfg.AdvertiseIP
	if advertise == "" {
		ip, err := detectIP()
		if err != nil {
			return fmt.Errorf("cannot detect local ip; set advertise_ip in config: %w", err)
		}
		advertise = ip
	}

	// 按配置段连接依赖(段缺失=关闭该依赖;缺必填项已在 ParseConfig/Connect 报错)
	app := &App{Config: cfg, Hub: ws.NewHub(), Logger: logger}
	if cfg.Db != nil {
		pool, err := db.Connect(*cfg.Db)
		if err != nil {
			return err
		}
		app.DB = pool
		if b.migrator != nil {
			if err := db.Migrate(*cfg.Db, pool, b.migrator); err != nil {
				return err
			}
			zap.S().Infow("migrations applied")
		}
	}
	if cfg.Redis != nil {
		client, err := hbredis.Connect(*cfg.Redis)
		if err != nil {
			return err
		}
		app.Redis = client
	}
	if cfg.S3 != nil {
		client, err := s3.Connect(*cfg.S3)
		if err != nil {
			return err
		}
		app.S3 = client
	}
	if cfg.Auth != nil {
		if app.Redis == nil {
			return errors.New("boot config: [auth] requires a [redis] section")
		}
		app.Sessions = auth.NewSessions(app.Redis, *cfg.Auth)
	}
	if nc != nil {
		app.Nacos = nc
	}
	if b.onClose != nil {
		app.Hub.SetOnDisconnect(b.onClose)
	}
	for i, h := range b.hooks {
		zap.S().Debugw("running startup hook", "hook", i)
		if err := h(app); err != nil {
			return err
		}
	}

	// 路由:hb.New 预装 404/405 框架信封;auth 端点 + 业务挂载 + WS 端点 + 健康检查
	e := hb.New()
	if b.authCheck != nil || b.clientCheck != nil {
		api.MountAuth(&e.RouterGroup, app.Sessions, b.authCheck, b.clientCheck)
	}
	for _, m := range b.mounts {
		m(app, &e.RouterGroup)
	}
	for _, wr := range b.wsRoutes {
		path, onMessage := wr.path, wr.onMessage
		e.GET(path, func(c *gin.Context) {
			if err := ws.Upgrade(app.Hub, c.Writer, c.Request, func(conn *ws.Conn, msg ws.Message) {
				onMessage(app, conn, msg)
			}); err != nil {
				zap.S().Warnw("websocket handshake failed", "err", err)
				if !c.Writer.Written() {
					hb.Write(c, http.StatusOK, hb.Envelope{Code: 500, Message: "handshake failed"})
				}
			}
		})
	}
	api.MountHealth(&e.RouterGroup, app.DB, app.Redis)

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
	if err != nil {
		return fmt.Errorf("bind %s:%d failed: %w", cfg.Host, cfg.Port, err)
	}
	zap.S().Infow("listening", "host", cfg.Host, "port", cfg.Port)

	// 先监听后注册:注册失败不占着端口
	if nc != nil {
		if err := nc.Register(cfg.AppName, advertise, uint64(cfg.Port), cfg.Group); err != nil {
			return err
		}
	} else {
		zap.S().Infow("nacos disabled, running standalone (no service registry)")
	}

	// 配置热更监听(仅 env 模式;日志关闭则无监听)
	if hot != nil && applyLog != nil {
		change := func(content string) { onConfigPush(cfg, applyLog, content) }
		if err := nc.ListenConfig(hot.dataID, hot.group, change); err != nil {
			zap.S().Warnw("config listener failed; hot reload disabled", "err", err.Error())
		}
	}

	srv := &http.Server{Handler: e}
	go func() { _ = srv.Serve(ln) }() // 关闭/出错经 Shutdown 与信号路径收尾

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	zap.S().Infow("shutdown signal received")
	if err := srv.Shutdown(context.Background()); err != nil {
		zap.S().Warnw("graceful shutdown failed", "err", err.Error())
	}
	// 临时实例,注销失败也会被 Nacos 自动摘除
	if nc != nil {
		if err := nc.Deregister(cfg.AppName, advertise, uint64(cfg.Port), cfg.Group); err != nil {
			zap.S().Errorw("deregister failed (ephemeral instance will expire)", "err", err.Error())
		}
	}
	return nil
}

// onConfigPush 同一份推送驱动两类热更新:日志级别与 [extra](含快照)。
// 推送的 TOML 解析失败时整体保持旧值(warn)。
func onConfigPush(cfg *Config, applyLog func(string), content string) {
	fresh, err := ParseConfig(content)
	if err != nil {
		zap.S().Warnw("config reload ignored (bad toml)", "err", err.Error())
		return
	}
	level := resolveLevel(fresh)
	applyLog(level)
	zap.S().Infow("log level reloaded", "level", level)
	cfg.ReloadExtraFrom(fresh.extra)
}

// resolveLevel 日志级别解析:log_level > LOG_LEVEL > "info"(初始化与热更共用)。
func resolveLevel(cfg *Config) string {
	if cfg.LogLevel != "" {
		return cfg.LogLevel
	}
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		return env
	}
	return "info"
}

// initLogging 初始化 zap(console 编码输出到 stderr,级别挂 AtomicLevel,带 caller)
// 并设为全局 Logger;返回 SugaredLogger 与热更闭包(改 log_level 无需重启)。
func initLogging(level string) (*zap.SugaredLogger, func(string)) {
	lv := zap.NewAtomicLevel()
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv.SetLevel(zapcore.InfoLevel)
	}
	cfg := zap.Config{
		Level:            lv,
		Development:      false,
		Encoding:         "console",
		EncoderConfig:    zap.NewDevelopmentEncoderConfig(),
		OutputPaths:      []string{"stderr"},
		ErrorOutputPaths: []string{"stderr"},
	}
	logger, err := cfg.Build()
	if err != nil {
		logger = zap.NewNop()
	}
	zap.ReplaceGlobals(logger)
	return logger.Sugar(), func(l string) {
		if nl, err := zapcore.ParseLevel(l); err == nil {
			lv.SetLevel(nl)
		}
	}
}

// detectIP 探测本机对外 IP(UDP connect 8.8.8.8:80,不实际发包)。
func detectIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// gitCommit 启动日志里的提交信息:Commit 注入值 > go build 嵌入的主模块 VCS 提交
// (形如 "a1b2c3d 2026-11-25T10:00:00Z",工作区有改动加 "(dirty)");都没有记 unknown,不影响启动。
func gitCommit() string {
	if Commit != "" {
		return Commit
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev, stamp string
	dirty := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			stamp = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += " (dirty)"
	}
	if stamp != "" {
		rev += " " + stamp
	}
	return rev
}
