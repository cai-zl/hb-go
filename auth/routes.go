package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cai-zl/hb-go"
	"github.com/cai-zl/hb-go/internal/bootonly"
)

// MountRoutes 挂载固定的鉴权端点,仅供 boot 装配。业务使用 boot.WithAuth。
func MountRoutes(_ bootonly.AuthRoutes, rg *gin.RouterGroup, sessions *Sessions, check AuthChecker) {
	rg.POST("/auth/token", func(c *gin.Context) {
		var req LoginReq
		if err := c.ShouldBindJSON(&req); err != nil {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 400, Message: "invalid request body"})
			return
		}
		data, err := check.Check(req)
		if err != nil {
			writeAuthRouteError(c, err)
			return
		}
		pair, err := sessions.Create(data)
		if err != nil {
			writeAuthRouteError(c, err)
			return
		}
		if !c.Writer.Written() {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: tokenResponse(pair, sessions)})
		}
	})
	rg.POST("/auth/refresh", func(c *gin.Context) {
		var req RefreshReq
		if err := c.ShouldBindJSON(&req); err != nil {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 400, Message: "invalid request body"})
			return
		}
		pair, ok, err := sessions.Refresh(req.RefreshToken)
		if err != nil {
			writeAuthRouteError(c, err)
			return
		}
		if !ok {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 401, Message: "invalid refresh token"})
			return
		}
		if !c.Writer.Written() {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: tokenResponse(pair, sessions)})
		}
	})
	rg.POST("/auth/revoke", func(c *gin.Context) {
		var req RevokeReq
		if err := c.ShouldBindJSON(&req); err != nil {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 400, Message: "invalid request body"})
			return
		}
		if _, err := sessions.Revoke(req.AccessToken); err != nil {
			writeAuthRouteError(c, err)
			return
		}
		if !c.Writer.Written() {
			hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok"})
		}
	})
	rg.GET("/auth/me", Guard(sessions), func(c *gin.Context) {
		data, err := From[json.RawMessage](c)
		if err != nil {
			writeAuthRouteError(c, err)
			return
		}
		hb.Write(c, http.StatusOK, hb.Envelope{Code: 0, Message: "ok", Data: data})
	})
}

func tokenResponse(pair TokenPair, sessions *Sessions) TokenResponse {
	return TokenResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(sessions.AccessTTL().Seconds()),
	}
}

func writeAuthRouteError(c *gin.Context, err error) {
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
