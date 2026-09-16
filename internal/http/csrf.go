package http

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// applyCSRF 为 cookie 认证端点（refresh/logout，设计 6.1.5/7.3，C10）做同源
// 校验，防跨站携带 refresh cookie 的 CSRF：
//   - Origin/Referer 至少其一存在，且 host 与请求 Host 一致（大小写不敏感）
//     → 放行；任一存在的头 host 不匹配（或不可解析，如 Origin: null）→ 403；
//   - 两个头均缺失：strict（默认，CSRF_STRICT）拒绝 403——现代浏览器对
//     POST 一律携带 Origin，同源 fetch 自然满足；非浏览器客户端（curl 等）
//     需配置 CSRF_STRICT=false 放行（异源头仍拒绝）。
//
// 搭配 refresh cookie 的 SameSite=Lax 双重防御；Bearer 认证端点不经此中间件。
func newCSRFToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (h *Handler) setCSRFCookie(c *gin.Context) {
	if token := newCSRFToken(); token != "" {
		// Path 必须为 "/"：SPA 页面路径（/、/files 等）下 document.cookie 读不到
		// 更深路径的 cookie，Path=/api/v1 会导致刷新页/新标签页的 refresh 请求
		// 缺 X-CSRF-Token 头而被 403（表现为"老是掉登录"）。该 cookie 本就设计
		// 为 JS 可读（双提交令牌），放开到根路径无额外暴露。
		http.SetCookie(c.Writer, &http.Cookie{Name: "docflow_csrf", Value: token, Path: "/", Domain: h.cookieDomain, MaxAge: int(h.refreshTokenTTL.Seconds()), HttpOnly: false, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode})
	}
}

func applyCSRF(strict bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		method := c.Request.Method
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			c.Next()
			return
		}
		if _, err := c.Request.Cookie("refresh_token"); err != nil && strings.HasPrefix(strings.ToLower(c.GetHeader("Authorization")), "bearer ") {
			c.Next()
			return
		}
		origin := c.GetHeader("Origin")
		referer := c.GetHeader("Referer")
		if origin == "" && referer == "" {
			if strict {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "missing origin or referer header", "code": "CSRF_REJECTED"})
				return
			}
			c.Next()
			return
		}
		if !originHeadersMatchHost(c.Request, origin, referer) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cross-origin request rejected", "code": "CSRF_REJECTED"})
			return
		}
		if _, err := c.Request.Cookie("refresh_token"); err == nil {
			cookie, cookieErr := c.Request.Cookie("docflow_csrf")
			token := c.GetHeader("X-CSRF-Token")
			if cookieErr != nil || token == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) != 1 {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "csrf token rejected", "code": "CSRF_REJECTED"})
				return
			}
		}
		c.Next()
	}
}

// originHeadersMatchHost 校验已携带的 Origin/Referer 头 host 与请求 Host
// 一致；请求 Host 为空（异常请求行）同样拒绝。
func originHeadersMatchHost(r *http.Request, origin, referer string) bool {
	if r.Host == "" {
		return false
	}
	for _, header := range []string{origin, referer} {
		if header == "" {
			continue
		}
		u, err := url.Parse(header)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}
