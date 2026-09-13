package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// resetDocflowMetrics 清空全部 docflow_* 系列，保证文本断言可精确到值
// （包内测试串行执行，直接操作未导出 collector）。
func resetDocflowMetrics() {
	httpRequestsTotal.Reset()
	httpRequestDuration.Reset()
	uploadSessionsTotal.Reset()
	uploadProcessingDuration.Reset()
	scanResultsTotal.Reset()
	onlyofficeCallbacksTotal.Reset()
}

// scrape 经 promhttp 抓取 /metrics 文本（与生产端点同链路）。
func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestNormalizeRoute(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"参数路由模板保留", "/api/v1/files/:id", "/api/v1/files/:id"},
		{"静态路由", "/api/v1/files", "/api/v1/files"},
		{"嵌套参数路由", "/api/v1/shares/:id/files/:fid/download", "/api/v1/shares/:id/files/:fid/download"},
		{"未匹配路由归一 unknown", "", RouteUnknown},
		{"剥离 query", "/api/v1/files?parent_id=x&limit=10", "/api/v1/files"},
		{"仅 query 归一 unknown", "?a=1", RouteUnknown},
	}
	for _, tc := range cases {
		if got := NormalizeRoute(tc.in); got != tc.want {
			t.Errorf("%s: NormalizeRoute(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// 中间件按路由模板计数：静态/参数路由使用 gin 模板（动态 ID 不入标签），
// 404 归一 unknown，query 不影响 route 标签。
func TestGinMiddlewareCountsByRouteTemplate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetDocflowMetrics()
	r := gin.New()
	r.Use(GinMiddleware())
	r.GET("/api/v1/files/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/v1/uploads", func(c *gin.Context) { c.Status(http.StatusCreated) })

	steps := []struct {
		method, target string
		wantStatus     int
	}{
		{http.MethodGet, "/api/v1/files/0192f0a0-1111-4222-8333-444455556666?x=1", http.StatusOK},
		{http.MethodPost, "/api/v1/uploads", http.StatusCreated},
		{http.MethodGet, "/definitely/not/registered", http.StatusNotFound},
	}
	for _, s := range steps {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(s.method, s.target, nil))
		if rec.Code != s.wantStatus {
			t.Fatalf("%s %s: status = %d, want %d", s.method, s.target, rec.Code, s.wantStatus)
		}
	}

	body := scrape(t)
	wantLines := []string{
		`docflow_http_requests_total{method="GET",route="/api/v1/files/:id",status="200"} 1`,
		`docflow_http_requests_total{method="POST",route="/api/v1/uploads",status="201"} 1`,
		`docflow_http_requests_total{method="GET",route="unknown",status="404"} 1`,
		`docflow_http_request_duration_seconds_count{method="GET",route="/api/v1/files/:id"} 1`,
		`docflow_http_request_duration_seconds_count{method="GET",route="unknown"} 1`,
	}
	for _, line := range wantLines {
		if !strings.Contains(body, line) {
			t.Errorf("/metrics 输出缺少行 %q", line)
		}
	}
}

// 进程默认指标由 client_golang 自带（go_* / process_*）。
func TestHandlerExposesRuntimeMetrics(t *testing.T) {
	body := scrape(t)
	for _, prefix := range []string{"go_goroutines", "process_"} {
		if !strings.Contains(body, prefix) {
			t.Errorf("/metrics 输出缺少 %s 指标", prefix)
		}
	}
}

// 业务埋点 API：upload / scan / onlyoffice 计数均出现在抓取文本中。
func TestBusinessCountersExported(t *testing.T) {
	resetDocflowMetrics()
	IncUploadSession(UploadSessionCreated)
	IncUploadSession(UploadSessionFailed)
	ObserveUploadProcessing(UploadStageVerify, 0.125)
	IncScanResult(ScanResultClean)
	IncOnlyOfficeCallback(CallbackStatusSave, CallbackResultOK)

	body := scrape(t)
	wantLines := []string{
		`docflow_upload_sessions_total{status="created"} 1`,
		`docflow_upload_sessions_total{status="failed"} 1`,
		`docflow_upload_processing_duration_seconds_count{stage="verify"} 1`,
		`docflow_scan_results_total{result="clean"} 1`,
		`docflow_onlyoffice_callbacks_total{result="0",status="2"} 1`,
	}
	for _, line := range wantLines {
		if !strings.Contains(body, line) {
			t.Errorf("/metrics 输出缺少行 %q", line)
		}
	}
}
