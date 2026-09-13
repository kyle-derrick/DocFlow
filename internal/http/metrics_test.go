package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newMetricsTestRouter(t *testing.T, enabled bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.SetMetricsEnabled(enabled)
	r := gin.New()
	h.Register(r, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	return r
}

// /metrics 挂根路由（不在 /api/v1 下）：默认启用；全局中间件以路由模板为
// route 标签（含 /health、/metrics 自身），404 归 unknown。
func TestMetricsEndpointMounted(t *testing.T) {
	r := newMetricsTestRouter(t, true)

	// 业务探针请求先于抓取发生，其计数应出现在 /metrics 输出中。
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	// 未注册路径 -> 404，route 归一 unknown。
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/nope status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"docflow_http_requests_total",
		`route="/health"`,
		`route="unknown"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 输出缺少 %q", want)
		}
	}
}

// METRICS_ENABLED=false（SetMetricsEnabled(false)）：/metrics 不注册（404），
// 其余路由不受影响。
func TestMetricsEndpointDisabled(t *testing.T) {
	r := newMetricsTestRouter(t, false)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404 when disabled", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
}
