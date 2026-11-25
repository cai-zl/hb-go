// Package hb 提供 HTTP 响应信封、分页、错误工具和 Gin 引擎初始化。
package hb

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"

	"github.com/gin-gonic/gin"
)

// Envelope 是所有业务响应的统一形状。
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Response 解析上游信封(调用方侧),Data 为 nil 表示失败。
type Response[T any] struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    *T     `json:"data"`
}

// PageData 是构造和解析分页信封 data 共用的类型，T 为列表元素类型。
type PageData[T any] struct {
	List     []T    `json:"list"`
	Total    uint64 `json:"total"`
	Page     uint64 `json:"page"`
	PageSize uint64 `json:"pageSize"`
}

// Error 是业务/基础设施错误的统一类型,实现 error 接口。
type Error struct {
	Code    int
	Message string
	origin  error
}

func (e *Error) Error() string {
	if e.origin != nil {
		return fmt.Sprintf("%d %s: %v", e.Code, e.Message, e.origin)
	}
	return fmt.Sprintf("%d %s", e.Code, e.Message)
}

// Unwrap 暴露被掩码的原始错误(仅 Internal 构造的才有)。
func (e *Error) Unwrap() error { return e.origin }

// Err 构造业务错误(如参数校验、凭据错误),信息会原样写给调用方。
// code 为 0 时归一为 500。
func Err(code int, msg string) error {
	return &Error{Code: normCode(code), Message: msg}
}

// Errf 同 Err,消息走 fmt.Sprintf。
func Errf(code int, format string, args ...any) error {
	return Err(code, fmt.Sprintf(format, args...))
}

// mask 把任意 error 变成 *Error:*Error 原样保留,其余掩码为 internal
// error,原文 warn 进日志。
func mask(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	slog.Warn("internal error", "err", err)
	return &Error{Code: 500, Message: "internal error", origin: err}
}

// Internal 掩码基础设施错误:写给调用方固定 internal error,原文可经
// errors.As/Unwrap 取回(仅日志/测试用)。
func Internal(err error) error {
	if err == nil {
		return nil
	}
	return mask(err)
}

func normCode(code int) int {
	if code == 0 {
		return 500
	}
	return code
}

// Write 使用调用者给定的 HTTP 状态写信封。
func Write(c *gin.Context, status int, env Envelope) {
	c.JSON(status, env)
}

// Page 构造分页信封的 data。T 是列表元素类型，通常从 list 推断。
// 不复制切片；nil 切片编码为 null，非 nil 空切片编码为 []。
func Page[T any](list []T, total, page, pageSize uint64) *PageData[T] {
	return &PageData[T]{List: list, Total: total, Page: page, PageSize: pageSize}
}

// New 返回预装框架级错误信封的 gin 引擎:未匹配路由 404、方法不允许 405,
// 保留真实状态码。Recovery 仍需显式 e.Use(hb.Recovery())。
func New() *gin.Engine {
	e := gin.New()
	e.NoRoute(func(c *gin.Context) {
		c.JSON(404, Envelope{Code: 404, Message: "Not Found"})
	})
	e.HandleMethodNotAllowed = true
	e.NoMethod(func(c *gin.Context) {
		c.JSON(405, Envelope{Code: 405, Message: "Method Not Allowed"})
	})
	return e
}

// Recovery 把 panic 转成 internal error 信封,堆栈进日志。可选中间件,默认不装。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 64<<10)
				buf = buf[:runtime.Stack(buf, false)]
				slog.Error("panic recovered", "err", fmt.Sprint(r), "stack", string(buf))
				if !c.Writer.Written() {
					c.JSON(200, Envelope{Code: 500, Message: "internal error"})
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}
