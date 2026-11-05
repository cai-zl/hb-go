// Package auth 提供会话认证:不透明 token + Redis 存储(复用 redis 与 crypto),
// 登录/刷新/吊销/个人信息四端点与整组保护中间件。
//
// 语义约定(与 Rust 版一致):登录凭据错误 = HTTP 200 + {"code":401,...} 业务信封;
// token 缺失/失效 = HTTP 401 + 信封;Redis 等基础设施故障保留 500 信封,不冒充 401。
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"

	"hg"
	"hg/crypto"
)

// Config 对应 TOML [auth] 段,全部有缺省值。
type Config struct {
	TokenTTL   time.Duration `toml:"token_ttl"`   // TOML 写法 "2h"
	RefreshTTL time.Duration `toml:"refresh_ttl"` // TOML 写法 "720h"
	Prefix     string        `toml:"prefix"`
}

const (
	defaultTokenTTL   = 2 * time.Hour
	defaultRefreshTTL = 30 * 24 * time.Hour
	defaultPrefix     = "hg:auth:"
)

// TokenPair 一对新旧会话令牌。
type TokenPair struct {
	AccessToken  string
	RefreshToken string
}

// TokenResponse 登录/刷新端点的响应体,OAuth2 规范 snake_case
// (全库唯一非 camelCase 例外,与 Rust 版一致)。
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

// LoginReq 登录请求体;check 闭包在 /auth/token 时收到它。
type LoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// SessionRecord Redis 里的一条会话记录。两个键互存对方(peer),
// 吊销/轮换才能从任一 token 找到另一半。
type SessionRecord struct {
	Data json.RawMessage `json:"data"`
	Peer string          `json:"peer"`
	Exp  int64           `json:"exp"` // access 的过期 unix 秒;refresh 记录恒 0(不判定)
}

// Sessions 会话存储。token 为 32 随机字节 hex;Redis 双键 {prefix}a:{token} /
// {prefix}r:{token} 互存 peer;键 TTL = RefreshTTL,access 的有效期由记录内
// exp 判定 —— access token 过期后键还在,revoke 仍能顺着 peer 删掉 refresh。
type Sessions struct {
	client     *goredis.Client
	tokenTTL   time.Duration
	refreshTTL time.Duration
	prefix     string
}

// NewSessions 建立会话存储并应用配置缺省值。
func NewSessions(client *goredis.Client, cfg Config) *Sessions {
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = defaultTokenTTL
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = defaultRefreshTTL
	}
	if cfg.Prefix == "" {
		cfg.Prefix = defaultPrefix
	}
	return &Sessions{
		client:     client,
		tokenTTL:   cfg.TokenTTL,
		refreshTTL: cfg.RefreshTTL,
		prefix:     cfg.Prefix,
	}
}

func (s *Sessions) atKey(token string) string { return s.prefix + "a:" + token }
func (s *Sessions) rtKey(token string) string { return s.prefix + "r:" + token }

func newToken() string { return crypto.RandomHex(32) }

func recordJSON(data any, peer string, exp int64) (string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return "", hg.Internal(err)
	}
	rec, err := json.Marshal(SessionRecord{Data: raw, Peer: peer, Exp: exp})
	if err != nil {
		return "", hg.Internal(err)
	}
	return string(rec), nil
}

func parseRecord(raw string) (SessionRecord, error) {
	var rec SessionRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return rec, hg.Errf(500, "auth: session record corrupted: %v", err)
	}
	return rec, nil
}

// Create 签发一对新 token,会话数据为登录时业务写入的任意可序列化值。
func (s *Sessions) Create(data any) (TokenPair, error) {
	at, rt := newToken(), newToken()
	exp := time.Now().Add(s.tokenTTL).Unix()
	recAT, err := recordJSON(data, rt, exp)
	if err != nil {
		return TokenPair{}, err
	}
	recRT, err := recordJSON(data, at, 0)
	if err != nil {
		return TokenPair{}, err
	}
	ctx := context.Background()
	if err := s.client.Set(ctx, s.atKey(at), recAT, s.refreshTTL).Err(); err != nil {
		return TokenPair{}, hg.Internal(err)
	}
	if err := s.client.Set(ctx, s.rtKey(rt), recRT, s.refreshTTL).Err(); err != nil {
		return TokenPair{}, hg.Internal(err)
	}
	return TokenPair{AccessToken: at, RefreshToken: rt}, nil
}

// Get 按 access token 取会话记录;键不存在或已过 exp 返回 ok=false
// (基础设施错误才返回 err)。
func (s *Sessions) Get(token string) (SessionRecord, bool, error) {
	raw, gerr := s.client.Get(context.Background(), s.atKey(token)).Result()
	if gerr == goredis.Nil {
		return SessionRecord{}, false, nil
	}
	if gerr != nil {
		return SessionRecord{}, false, hg.Internal(gerr)
	}
	rec, err := parseRecord(raw)
	if err != nil {
		return SessionRecord{}, false, err
	}
	if rec.Exp != 0 && time.Now().Unix() > rec.Exp {
		return SessionRecord{}, false, nil
	}
	return rec, true, nil
}

// Refresh 用 refresh token 轮换出新对:写新删旧,旧 access 顺 peer 删除。
// refresh token 无效返回 ok=false。
func (s *Sessions) Refresh(refreshToken string) (TokenPair, bool, error) {
	rk := s.rtKey(refreshToken)
	raw, gerr := s.client.Get(context.Background(), rk).Result()
	if gerr == goredis.Nil {
		return TokenPair{}, false, nil
	}
	if gerr != nil {
		return TokenPair{}, false, hg.Internal(gerr)
	}
	rec, err := parseRecord(raw)
	if err != nil {
		return TokenPair{}, false, err
	}
	at, rt := newToken(), newToken()
	exp := time.Now().Add(s.tokenTTL).Unix()
	recAT, err := recordJSON(json.RawMessage(rec.Data), rt, exp)
	if err != nil {
		return TokenPair{}, false, err
	}
	recRT, err := recordJSON(json.RawMessage(rec.Data), at, 0)
	if err != nil {
		return TokenPair{}, false, err
	}
	ctx := context.Background()
	if err := s.client.Set(ctx, s.atKey(at), recAT, s.refreshTTL).Err(); err != nil {
		return TokenPair{}, false, hg.Internal(err)
	}
	if err := s.client.Set(ctx, s.rtKey(rt), recRT, s.refreshTTL).Err(); err != nil {
		return TokenPair{}, false, hg.Internal(err)
	}
	// 旧 access = r: 记录里的 peer;旧 refresh = 当前键
	if err := s.client.Del(ctx, s.atKey(rec.Peer), rk).Err(); err != nil {
		return TokenPair{}, false, hg.Internal(err)
	}
	return TokenPair{AccessToken: at, RefreshToken: rt}, true, nil
}

// Revoke 吊销会话:删 access 键与顺 peer 的 refresh 键,幂等,返回是否存在过。
func (s *Sessions) Revoke(accessToken string) (bool, error) {
	raw, gerr := s.client.Get(context.Background(), s.atKey(accessToken)).Result()
	if gerr == goredis.Nil {
		return false, nil
	}
	if gerr != nil {
		return false, hg.Internal(gerr)
	}
	rec, err := parseRecord(raw)
	if err != nil {
		return false, err
	}
	ctx := context.Background()
	keys := []string{s.atKey(accessToken)}
	if rec.Peer != "" {
		keys = append(keys, s.rtKey(rec.Peer))
	}
	if err := s.client.Del(ctx, keys...).Err(); err != nil {
		return false, hg.Internal(err)
	}
	return true, nil
}

// Touch 双续期:把两个键的 TTL 都重置为 RefreshTTL,返回 token 是否存在。
func (s *Sessions) Touch(accessToken string) (bool, error) {
	rec, ok, err := s.Get(accessToken)
	if err != nil || !ok {
		return false, err
	}
	ctx := context.Background()
	if err := s.client.Expire(ctx, s.atKey(accessToken), s.refreshTTL).Err(); err != nil {
		return false, hg.Internal(err)
	}
	if rec.Peer != "" {
		if err := s.client.Expire(ctx, s.rtKey(rec.Peer), s.refreshTTL).Err(); err != nil {
			return false, hg.Internal(err)
		}
	}
	return true, nil
}

// AccessTTL 返回解析后的 access 有效期。
func (s *Sessions) AccessTTL() time.Duration { return s.tokenTTL }

// RefreshTTL 返回解析后的 refresh 有效期。
func (s *Sessions) RefreshTTL() time.Duration { return s.refreshTTL }

// bearer 解析 Authorization: Bearer <token>;缺头/前缀不对/空 token 返回空串。
func bearer(header string) string {
	const p = "Bearer "
	if strings.HasPrefix(header, p) && len(header) > len(p) {
		return header[len(p):]
	}
	return ""
}

func unauthorized(c *gin.Context, msg string) {
	hg.Write(c, http.StatusUnauthorized, hg.Envelope{Code: 401, Message: msg})
	c.Abort()
}

// Guard 整组路由保护中间件:校验通过后把会话记录注入 gin context
// (key "hg.session"),handler 内用 auth.From[T] 取会话数据。
func Guard(sessions *Sessions) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearer(c.Request.Header.Get("Authorization"))
		if token == "" {
			unauthorized(c, "unauthorized")
			return
		}
		rec, ok, err := sessions.Get(token)
		if err != nil {
			// Redis 故障等基础设施错误:保留 500 信封,不冒充 401
			var e *hg.Error
			if !errors.As(err, &e) {
				e = &hg.Error{Code: 500, Message: "internal error"}
			}
			hg.Write(c, http.StatusOK, hg.Envelope{Code: e.Code, Message: e.Message})
			c.Abort()
			return
		}
		if !ok {
			unauthorized(c, "unauthorized")
			return
		}
		c.Set("hg.session", rec)
		c.Set("hg.token", token)
		c.Next()
	}
}

const (
	ctxKeySession = "hg.session"
	ctxKeyToken   = "hg.token"
)

// From 取当前请求的会话数据(T 为登录时写入的类型);须配合 Guard 使用。
func From[T any](c *gin.Context) (T, error) {
	var zero T
	v, exists := c.Get(ctxKeySession)
	if !exists {
		return zero, hg.Err(401, "unauthorized")
	}
	rec, _ := v.(SessionRecord)
	var data T
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		return zero, hg.Errf(500, "auth: session data type mismatch: %v", err)
	}
	return data, nil
}

// Token 可拿到本次请求的 access token(Guard 注入)。
func Token(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyToken); ok {
		s, _ := v.(string)
		return s
	}
	return ""
}

// RefreshReq / RevokeReq 请求体。
type RefreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

type RevokeReq struct {
	AccessToken string `json:"access_token"`
}

func tokenResponse(pair TokenPair, sessions *Sessions) TokenResponse {
	return TokenResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(sessions.AccessTTL().Seconds()),
	}
}

// Routes 挂载四端点:POST /auth/token|refresh|revoke、GET /auth/me。
// check 是业务唯一要写的东西 —— 仅在登录瞬间被调用:查库验密码后返回会话数据,
// 返回错误时原样透传成 200+业务码信封(凭据错误语义)。
func Routes(sessions *Sessions, check func(LoginReq) (any, error)) func(*gin.RouterGroup) {
	return func(rg *gin.RouterGroup) {
		rg.POST("/auth/token", hg.Wrap(func(c *gin.Context) (any, error) {
			var req LoginReq
			if err := c.ShouldBindJSON(&req); err != nil {
				return nil, hg.Err(400, "invalid request body")
			}
			data, err := check(req)
			if err != nil {
				return nil, err
			}
			pair, err := sessions.Create(data)
			if err != nil {
				return nil, err
			}
			return tokenResponse(pair, sessions), nil
		}))
		rg.POST("/auth/refresh", hg.Wrap(func(c *gin.Context) (any, error) {
			var req RefreshReq
			if err := c.ShouldBindJSON(&req); err != nil {
				return nil, hg.Err(400, "invalid request body")
			}
			pair, ok, err := sessions.Refresh(req.RefreshToken)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, hg.Err(401, "invalid refresh token")
			}
			return tokenResponse(pair, sessions), nil
		}))
		rg.POST("/auth/revoke", hg.Wrap(func(c *gin.Context) (any, error) {
			var req RevokeReq
			if err := c.ShouldBindJSON(&req); err != nil {
				return nil, hg.Err(400, "invalid request body")
			}
			if _, err := sessions.Revoke(req.AccessToken); err != nil {
				return nil, err
			}
			return nil, nil // 空成功信封
		}))
		rg.GET("/auth/me", func(c *gin.Context) {
			token := bearer(c.Request.Header.Get("Authorization"))
			if token == "" {
				unauthorized(c, "unauthorized")
				return
			}
			rec, ok, err := sessions.Get(token)
			if err != nil {
				var e *hg.Error
				if !errors.As(err, &e) {
					e = &hg.Error{Code: 500, Message: "internal error"}
				}
				hg.Write(c, http.StatusOK, hg.Envelope{Code: e.Code, Message: e.Message})
				return
			}
			if !ok {
				unauthorized(c, "unauthorized")
				return
			}
			hg.Write(c, http.StatusOK, hg.Envelope{Code: 0, Message: "ok", Data: json.RawMessage(rec.Data)})
		})
	}
}
