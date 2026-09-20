package http

import (
	"errors"
	"log"
	"net/http"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/oidc"
	"github.com/gin-gonic/gin"
)

// 本文件实现 OIDC 单点登录端点（公开，无 Bearer 认证）：
//   GET /api/v1/auth/oidc/config    恒注册；{enabled}（探测，登录页门控按钮）
//   GET /api/v1/auth/oidc/login     仅启用（SetOIDC 注入）时注册；302 跳 IdP
//   GET /api/v1/auth/oidc/callback  仅启用时注册；校验 state → 换令牌 →
//                                   映射用户 → 302 {PUBLIC_BASE_URL}/sso#access_token=<jwt>
//                                   并 Set-Cookie refresh_token
// 安全取舍：SSO 信任 IdP 认证强度，跳过本地 TOTP 二验（密码登录的 TOTP
// 拦截不变）；state 内存 + HttpOnly cookie 双验证、用后即焚，PKCE S256。

// oidcStateCookie 为登录 state 的 HttpOnly cookie 名（Path 限定 callback）。
const oidcStateCookie = "oidc_state"

// SetOIDC 注入 OIDC 单点登录服务（幂等）；非 nil 时 Register 挂载
// /api/v1/auth/oidc/login 与 callback 路由，config 探测端点返回
// enabled=true。未注入时不注册（默认 404）。
func (h *Handler) SetOIDC(svc *oidc.Service) {
	if svc != nil {
		h.oidc = svc
	}
}

// oidcConfig GET /api/v1/auth/oidc/config（公开）：{enabled}。
func (h *Handler) oidcConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"enabled": h.oidc != nil})
}

// oidcLogin GET /api/v1/auth/oidc/login：签发 state（内存登记 PKCE
// verifier）后 302 跳转 IdP 授权地址；state 同时写 HttpOnly cookie，
// callback 侧与 URL 参数双验证（防 CSRF/重放）。
func (h *Handler) oidcLogin(c *gin.Context) {
	state, authorizeURL, err := h.oidc.BeginLogin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to start sso login"})
		return
	}
	c.SetCookie(oidcStateCookie, state, int(oidc.StateTTL.Seconds()), "/api/v1/auth/oidc/callback", h.cookieDomain, h.cookieSecure, true)
	c.Redirect(http.StatusFound, authorizeURL)
}

// oidcCallback GET /api/v1/auth/oidc/callback?state=&code=：
//  1. state 双验证（cookie == 参数）并经服务端内存消费（用后即焚）；
//  2. 授权码换令牌、解析身份、映射/开户用户（见 oidc.Service）；
//  3. 成功：签发 DocFlow 会话（refresh cookie + access token 经 URL
//     fragment 跳转前端落地页 /sso——fragment 不发往服务器，不进日志与
//     Referer），302 {PUBLIC_BASE_URL 或相对}/sso#access_token=<jwt>。
func (h *Handler) oidcCallback(c *gin.Context) {
	stateParam := c.Query("state")
	code := c.Query("code")
	cookie, err := c.Request.Cookie(oidcStateCookie)
	// state cookie 立即失效（无论成败，一次性语义）。
	c.SetCookie(oidcStateCookie, "", -1, "/api/v1/auth/oidc/callback", h.cookieDomain, h.cookieSecure, true)
	if err != nil || cookie.Value == "" || cookie.Value != stateParam {
		h.recordAudit(c, audit.Entry{Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"stage":"oidc","reason":"state_mismatch"}`})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid sso state"})
		return
	}
	uid, err := h.oidc.CompleteLogin(c.Request.Context(), stateParam, code)
	if err != nil {
		switch {
		case errors.Is(err, oidc.ErrInvalidState), errors.Is(err, oidc.ErrExchangeFailed):
			h.recordAudit(c, audit.Entry{Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"stage":"oidc","reason":"state_or_exchange"}`})
			c.JSON(http.StatusUnauthorized, gin.H{"error": "sso login failed"})
		case errors.Is(err, oidc.ErrUserDisabled):
			h.recordAudit(c, audit.Entry{Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"stage":"oidc","reason":"user_disabled"}`})
			c.JSON(http.StatusForbidden, gin.H{"error": "account is disabled"})
		case errors.Is(err, oidc.ErrNotProvisioned):
			h.recordAudit(c, audit.Entry{Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"stage":"oidc","reason":"not_provisioned"}`})
			c.JSON(http.StatusForbidden, gin.H{"error": "no matching account; contact an administrator to be invited", "code": "SSO_NOT_PROVISIONED"})
		case errors.Is(err, oidc.ErrNoEmail):
			h.recordAudit(c, audit.Entry{Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"stage":"oidc","reason":"no_email"}`})
			c.JSON(http.StatusUnauthorized, gin.H{"error": "sso identity has no email"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "sso login failed"})
		}
		return
	}
	access, err := h.auth.AccessToken(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	// 统一空间模型：SSO 登录幂等补齐默认空间（自动开户或存量用户均可）。
	if h.spaces != nil {
		display := ""
		if u, uerr := h.users.GetByID(uid); uerr == nil {
			display = u.Username
		}
		if _, _, serr := h.spaces.EnsureDefaultSpace(uid, display); serr != nil {
			log.Printf("[space] ensure default space for %s: %v", uid, serr)
		}
	}
	refresh, err := h.auth.NewSessionWithInfo(uid, sessionInfoFromRequest(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	h.setRefreshCookie(c, refresh)
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionLoginSuccess, ResourceType: audit.ResourceSession, ResourceID: uid.String(), Status: audit.StatusSuccess, Metadata: `{"stage":"oidc"}`})
	c.Redirect(http.StatusFound, h.publicLink("/sso#access_token="+access))
}
