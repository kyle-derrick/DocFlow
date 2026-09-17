package http

import (
	"io"
	"net/http"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/mcp"
	"github.com/gin-gonic/gin"
)

// mcpRateLimitPerMin 为 /mcp 端点的独立按 IP 轻限流（每分钟 60 次；
// MCP 客户端一次会话内的请求频率远低于此，主要防御异常客户端刷接口）。
const mcpRateLimitPerMin = 60

// mcpMaxBodyBytes 为单次 JSON-RPC 请求体上限（64MB，覆盖 base64 编码的
// 大内容写入；实际内容大小仍由上传管线的单文件上限与配额约束）。
const mcpMaxBodyBytes = 64 << 20

// registerMCP 挂载 MCP（Model Context Protocol）端点：根级 /mcp，不经
// /api/v1（无 CSRF 中间件——MCP 客户端为非浏览器 Bearer 凭证调用方），
// 认证复用 PAT/access token 公共鉴权函数，独立按 IP 限流。
// POST 为单次 JSON-RPC 请求-响应（Streamable HTTP 简化版，无 session/SSE）；
// GET 返回 405（不支持服务端推送流）。
func (h *Handler) registerMCP(r *gin.Engine, jwtSecret string) {
	deps := h.mcpDeps
	if deps == nil {
		deps = &mcp.Deps{}
	}
	if deps.Storage == nil {
		deps.Storage = h.storage
	}
	// search 为可选能力（SetSearch 注入；未注入时 df_search_files 不可用）。
	deps.Search = h.search
	h.mcpServer = mcp.NewServer(deps)

	// PAT 认证路径与 api 组一致：h.auth 未注入（契约测试）时 dfpat_ 一律 401。
	var patVerifier auth.AccessTokenVerifier
	if h.auth != nil {
		patVerifier = h.auth
	}
	limiter := publicLimiter(NewRateLimiter(mcpRateLimitPerMin))
	r.POST("/mcp", limiter, mcpAuthMiddleware(jwtSecret, patVerifier), h.mcpPost)
	r.GET("/mcp", limiter, func(c *gin.Context) {
		c.Header("Allow", "POST")
		c.JSON(http.StatusMethodNotAllowed, gin.H{"error": "method not allowed: send JSON-RPC 2.0 requests by POST"})
	})
}

// mcpAuthMiddleware 认证 Bearer 凭证（PAT dfpat_ 或 access token，复用
// auth.VerifyBearer 公共鉴权函数）；失败以 JSON-RPC error -32001 拒绝
// （HTTP 401）。成功注入 user_id / PAT scopes / auth_kind 上下文。
func mcpAuthMiddleware(jwtSecret string, pat auth.AccessTokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, scopes, kind, err := auth.VerifyBearer(jwtSecret, pat, c.GetHeader("Authorization"))
		if err != nil {
			c.Header("WWW-Authenticate", "Bearer")
			c.Data(http.StatusUnauthorized, "application/json", mcp.UnauthorizedResponse())
			c.Abort()
			return
		}
		c.Set(auth.UserIDContextKey, id)
		if scopes != nil {
			c.Set(auth.PATScopesContextKey, scopes)
		}
		c.Set(auth.AuthKindContextKey, kind)
		c.Next()
	}
}

// mcpIdentity 从 gin 上下文还原 MCP 调用方身份。
func mcpIdentity(c *gin.Context) mcp.Identity {
	identity := mcp.Identity{UserID: userID(c)}
	if kind, ok := c.Get(auth.AuthKindContextKey); ok {
		identity.IsPAT = kind == auth.AuthKindPAT
	}
	if v, ok := c.Get(auth.PATScopesContextKey); ok {
		if scopes, valid := v.([]string); valid {
			identity.Scopes = scopes
		}
	}
	return identity
}

// mcpPost POST /mcp：单次 JSON-RPC 2.0 请求-响应（Accept 允许
// application/json 与 text/event-stream，本实现恒以 application/json 应答；
// notification 回 202 无 body）。
func (h *Handler) mcpPost(c *gin.Context) {
	if h.mcpServer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "mcp server is not configured"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, mcpMaxBodyBytes))
	if err != nil {
		c.Data(http.StatusOK, "application/json", mcp.NewError(jsonNull, mcp.CodeParseError, "parse error", nil).Encode())
		return
	}
	identity := mcpIdentity(c)
	response := h.mcpServer.Handle(c.Request.Context(), body, identity)
	if response == nil {
		c.Status(http.StatusAccepted)
		return
	}
	c.Data(http.StatusOK, "application/json", response)
}

// jsonNull 为 JSON-RPC 应答的 null id 常量。
var jsonNull = []byte("null")
