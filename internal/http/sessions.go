package http

import (
	"errors"
	"net/http"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
)

// 本文件实现会话管理（多端登录）与个人访问令牌（PAT）端点：
//   GET    /api/v1/auth/sessions      当前用户全部活跃会话
//   DELETE /api/v1/auth/sessions/:id  撤销自己的指定会话（非属主 404）
//   DELETE /api/v1/auth/sessions      撤销全部会话（含当前）+ 清 Cookie
//   POST   /api/v1/tokens             创建 PAT（明文仅本次返回一次）
//   GET    /api/v1/tokens             PAT 列表（无明文）
//   DELETE /api/v1/tokens/:id         撤销 PAT
//
// 实现取舍：会话列表不标记「当前会话」——当前会话由 refresh cookie 识别，
// 而 cookie 材料（哈希）不参与任何响应；请求头 X-Session-Hint 可伪造，
// 不作为信任源。撤销全部时不区分当前会话（一律撤销，含发起请求的会话），
// 前端收到 204 后清除本地令牌并跳转登录页。

// listSessions GET /api/v1/auth/sessions：列出当前用户全部活跃会话
// （未撤销未过期，last_active_at 倒序）。
func (h *Handler) listSessions(c *gin.Context) {
	sessions, err := h.auth.ListSessions(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list sessions"})
		return
	}
	out := make([]gin.H, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, gin.H{
			"id":             s.ID,
			"created_at":     s.CreatedAt,
			"last_active_at": s.LastActiveAt,
			"expires_at":     s.ExpiresAt,
			"ip":             s.IP,
			"user_agent":     s.UserAgent,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": out})
}

// revokeSession DELETE /api/v1/auth/sessions/:id：撤销自己的指定会话。
// 会话不存在、非属主或已撤销一律 404（不泄露存在性）。
func (h *Handler) revokeSession(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	uid := userID(c)
	revoked, err := h.auth.RevokeSessionByID(uid, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to revoke session"})
		return
	}
	if !revoked {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// revokeAllSessions DELETE /api/v1/auth/sessions：撤销当前用户全部会话
// （含发起请求的当前会话——不依赖可伪造的请求头标记），并清除
// refresh_token Cookie；PAT 不受影响（无 session 语义）。
func (h *Handler) revokeAllSessions(c *gin.Context) {
	if err := h.auth.RevokeAllSessions(userID(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to revoke sessions"})
		return
	}
	c.SetCookie("refresh_token", "", -1, "/api/v1/auth/refresh", h.cookieDomain, h.cookieSecure, true)
	c.Status(http.StatusNoContent)
}

type createTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"`
}

func tokenJSON(t auth.APIToken) gin.H {
	return gin.H{
		"id":           t.ID,
		"name":         t.Name,
		"prefix":       t.Prefix,
		"last_used_at": t.LastUsedAt,
		"expires_at":   t.ExpiresAt,
		"revoked_at":   t.RevokedAt,
		"created_at":   t.CreatedAt,
	}
}

// createToken POST /api/v1/tokens {name, expires_in_days?}：创建 PAT。
// 一次性明文 token（dfpat_ + 43 字符）仅在 201 响应返回；expires_in_days
// 缺省或 0 表示永久。写 token.create 审计（PAT 认证本身不审计）。
func (h *Handler) createToken(c *gin.Context) {
	var request createTokenRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	uid := userID(c)
	token, plaintext, err := h.auth.NewPersonalAccessToken(uid, request.Name, request.ExpiresInDays)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal access tokens are not configured"})
		return
	case errors.Is(err, auth.ErrInvalidTokenName), errors.Is(err, auth.ErrInvalidTokenExpiry):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create token"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionTokenCreate, ResourceType: audit.ResourceToken, ResourceID: token.ID.String(), Metadata: `{"name":"` + sanitizeAuditToken(token.Name) + `","prefix":"` + sanitizeAuditToken(token.Prefix) + `"` + expiryAuditMetadata(token.ExpiresAt) + `}`})
	body := tokenJSON(token)
	body["token"] = plaintext
	c.JSON(http.StatusCreated, body)
}

// expiryAuditMetadata 生成 PAT 审计 metadata 的有效期片段（永久时省略）。
func expiryAuditMetadata(expiresAt *time.Time) string {
	if expiresAt == nil {
		return ""
	}
	return `,"expires_at":"` + expiresAt.UTC().Format(time.RFC3339) + `"`
}

// listTokens GET /api/v1/tokens：当前用户未撤销的 PAT 列表（不含明文；
// last_used_at 为最近一次认证时间，未使用过为 null）。
func (h *Handler) listTokens(c *gin.Context) {
	tokens, err := h.auth.ListPersonalAccessTokens(userID(c))
	if err != nil {
		if errors.Is(err, auth.ErrNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "personal access tokens are not configured"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list tokens"})
		return
	}
	out := make([]gin.H, 0, len(tokens))
	for i := range tokens {
		out = append(out, tokenJSON(tokens[i]))
	}
	c.JSON(http.StatusOK, gin.H{"tokens": out})
}

// revokeToken DELETE /api/v1/tokens/:id：撤销自己的 PAT（立即失效）。
// 不存在、非属主或已撤销一律 404。写 token.revoke 审计。
func (h *Handler) revokeToken(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	uid := userID(c)
	revoked, err := h.auth.RevokePersonalAccessToken(uid, id)
	if err != nil && !errors.Is(err, auth.ErrNotConfigured) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to revoke token"})
		return
	}
	if err != nil || !revoked {
		c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionTokenRevoke, ResourceType: audit.ResourceToken, ResourceID: id.String()})
	c.Status(http.StatusNoContent)
}
