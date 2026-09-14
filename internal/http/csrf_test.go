package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// csrfTestRouter 构造挂载 applyCSRF 中间件的测试路由（POST /op）。
func csrfTestRouter(strict bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/op", applyCSRF(strict), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	return r
}

// csrfPost 发起带可选 Origin/Referer 头的 POST（Host 取 httptest 默认 example.com）。
func csrfPost(r *gin.Engine, origin, referer string) int {
	req := httptest.NewRequest(http.MethodPost, "/op", nil)
	if origin != "\x00unset" {
		req.Header.Set("Origin", origin)
	}
	if referer != "\x00unset" {
		req.Header.Set("Referer", referer)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// CSRF 矩阵（C10，设计 6.1.5）：
//   - 严格模式（默认）：无 Origin/Referer → 403；
//   - 非严格（CSRF_STRICT=false）：无两头 → 放行（curl 等非浏览器客户端）；
//   - 异源（Origin host ≠ Host）→ 一律 403（严格与否均拒绝）；
//   - 同源（Origin host = Host，大小写不敏感）→ 放行；
//   - Origin: null（sandbox iframe）→ 不可解析为 host → 403。
func TestApplyCSRFMatrix(t *testing.T) {
	strict := csrfTestRouter(true)
	relaxed := csrfTestRouter(false)
	const unset = "\x00unset"

	cases := []struct {
		name        string
		origin      string
		referer     string
		strictWant  int
		relaxedWant int
	}{
		{"no-headers", unset, unset, http.StatusForbidden, http.StatusNoContent},
		{"same-origin", "http://example.com", unset, http.StatusNoContent, http.StatusNoContent},
		{"same-origin-upper-host", "http://EXAMPLE.com", unset, http.StatusNoContent, http.StatusNoContent},
		{"cross-origin", "https://evil.example.net", unset, http.StatusForbidden, http.StatusForbidden},
		{"origin-null", "null", unset, http.StatusForbidden, http.StatusForbidden},
		{"same-origin-port-mismatch", "http://example.com:8080", unset, http.StatusForbidden, http.StatusForbidden},
		{"referer-same-origin", unset, "http://example.com/some/page", http.StatusNoContent, http.StatusNoContent},
		{"referer-cross-origin", unset, "https://evil.example.net/x", http.StatusForbidden, http.StatusForbidden},
		{"origin-ok-referer-evil", "http://example.com", "https://evil.example.net/x", http.StatusForbidden, http.StatusForbidden},
		{"origin-evil-referer-ok", "https://evil.example.net", "http://example.com/x", http.StatusForbidden, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := csrfPost(strict, tc.origin, tc.referer); got != tc.strictWant {
				t.Fatalf("strict: code = %d, want %d", got, tc.strictWant)
			}
			if got := csrfPost(relaxed, tc.origin, tc.referer); got != tc.relaxedWant {
				t.Fatalf("relaxed: code = %d, want %d", got, tc.relaxedWant)
			}
		})
	}
}

// refresh/logout 全链路（Register 装配）：严格模式下无 Origin 的 refresh 被
// CSRF 中间件拒绝（403 CSRF_REJECTED）；带同源 Origin 后进入业务（401 无
// cookie）。SetCSRFStrict(false) 后无两头请求进入业务路径。
func TestCSRFWiredOnRefreshLogout(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Minute)
	router := gin.New()
	h.Register(router, "csrf-wired-test-secret-0123456789ab", 1_000_000, 1_000_000, 1_000_000)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("strict no-origin refresh: code = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "CSRF_REJECTED") {
		t.Fatalf("body = %s, want CSRF_REJECTED", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.Header.Set("Origin", "http://example.com")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("same-origin refresh (no cookie): code = %d, want 401", w.Code)
	}

	h.SetCSRFStrict(false)
	router2 := gin.New()
	h.Register(router2, "csrf-wired-test-secret-0123456789ab", 1_000_000, 1_000_000, 1_000_000)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	w = httptest.NewRecorder()
	router2.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("relaxed no-origin logout: code = %d, want 204", w.Code)
	}
}
