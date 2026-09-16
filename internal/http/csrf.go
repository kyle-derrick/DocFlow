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
		// 删除旧版本曾下发的 Path=/api/v1 cookie。若不删除，浏览器请求 refresh
		// 时会同时发送两个同名 cookie，并把更长路径的旧值排在前面。
		http.SetCookie(c.Writer, &http.Cookie{Name: "docflow_csrf", Value: "", Path: "/api/v1", Domain: h.cookieDomain, MaxAge: -1, HttpOnly: false, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode})
		http.SetCookie(c.Writer, &http.Cookie{Name: "docflow_csrf", Value: token, Path: "/", Domain: h.cookieDomain, MaxAge: int(h.refreshTokenTTL.Seconds()), HttpOnly: false, Secure: h.cookieSecure, SameSite: http.SameSiteLaxMode})
	}
}

func clearCSRFCookies(c *gin.Context, domain string, secure bool) {
	for _, path := range []string{"/", "/api/v1"} {
		http.SetCookie(c.Writer, &http.Cookie{Name: "docflow_csrf", Value: "", Path: path, Domain: domain, MaxAge: -1, HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode})
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
			token := c.GetHeader("X-CSRF-Token")
			matched := false
			for _, cookie := range c.Request.Cookies() {
				if cookie.Name == "docflow_csrf" && token != "" && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(token)) == 1 {
					matched = true
					break
				}
			}
			if !matched {
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
