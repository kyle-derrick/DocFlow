// Package metrics 提供 DocFlow 的 Prometheus 指标（设计文档 13.3 / 16.6）：
// HTTP 请求计数与耗时、上传会话与 Complete 各阶段耗时、病毒扫描结果、
// ONLYOFFICE 回调计数，以及 GET /metrics 抓取端点（promhttp，默认 registry
// 自带 go_* 与 process_* 进程指标）。
//
// 高基数约束：所有 route 标签一律使用 gin 路由模板（c.FullPath()，如
// /api/v1/files/:id），未匹配任何路由的请求（404）归一为 "unknown"；
// 禁止把文件 ID、分享 token 等动态 path 片段作为标签值。
package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// 指标标签取值约定（导出常量供埋点方使用，避免拼写漂移）。
const (
	// RouteUnknown 为未匹配路由模板的请求（404）的 route 标签值。
	RouteUnknown = "unknown"

	// 上传会话创建结果（docflow_upload_sessions_total 的 status）。
	UploadSessionCreated = "created"
	UploadSessionFailed  = "failed"

	// Complete 阶段（docflow_upload_processing_duration_seconds 的 stage）。
	UploadStageVerify = "verify"
	UploadStageScan   = "scan"

	// 扫描结果（docflow_scan_results_total 的 result）。
	ScanResultClean    = "clean"
	ScanResultInfected = "infected"
	ScanResultError    = "error"

	// ONLYOFFICE 回调状态归一（docflow_onlyoffice_callbacks_total 的 status）：
	// 2=保存、4=清理，其余（1/3/6/7 与校验失败）归为 other。
	CallbackStatusSave    = "2"
	CallbackStatusCleanup = "4"
	CallbackStatusOther   = "other"

	// ONLYOFFICE 回调处理结果：0=成功（{"error":0}），1=失败（{"error":1}）。
	CallbackResultOK    = "0"
	CallbackResultError = "1"

	// 后台任务队列驱动（docflow_queue_enqueued_total 的 driver）。
	QueueDriverInProcess = "inprocess"
	QueueDriverRedis     = "redis"

	// 队列任务处理结果（docflow_queue_processed_total 的 status）。
	QueueStatusSuccess = "success"
	QueueStatusFailed  = "failed"
)

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "docflow_http_requests_total",
		Help: "HTTP 请求总数。route 为 gin 路由模板（如 /api/v1/files/:id），未匹配路由（404）为 unknown。",
	}, []string{"method", "route", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "docflow_http_request_duration_seconds",
		Help:    "HTTP 请求耗时分布（秒）。route 为 gin 路由模板。",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})

	uploadSessionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "docflow_upload_sessions_total",
		Help: "上传会话创建计数（自定义上传 API 与 tus 创建扩展共用）。",
	}, []string{"status"})

	uploadProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "docflow_upload_processing_duration_seconds",
		Help:    "上传 Complete 各阶段处理耗时（秒）。",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"stage"})

	scanResultsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "docflow_scan_results_total",
		Help: "病毒扫描结果计数（clean=放行，infected=检出恶意内容，error=扫描器错误）。",
	}, []string{"result"})

	onlyofficeCallbacksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "docflow_onlyoffice_callbacks_total",
		Help: "ONLYOFFICE DocumentServer 回调计数。status 归一为 2（保存）/4（清理）/other；result 0=成功、1=失败。",
	}, []string{"status", "result"})

	queueEnqueuedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "docflow_queue_enqueued_total",
		Help: "后台任务队列入队计数。type 为任务类型（task:complete-upload / task:extract-webpkg），driver 为 inprocess / redis。",
	}, []string{"type", "driver"})

	queueProcessedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "docflow_queue_processed_total",
		Help: "后台任务队列处理结果计数。type 为任务类型，status 为 success / failed。",
	}, []string{"type", "status"})
)

// Handler 返回 /metrics 端点的 http.Handler（promhttp 默认 registry，
// 含 client_golang 自带的 go_* 与 process_* 指标）。
func Handler() http.Handler { return promhttp.Handler() }

// GinMiddleware 返回全局 HTTP 指标中间件：按路由模板计数与计时，
// 必须挂载在任何路由注册之前（gin 全局中间件对 404 同样生效，
// 此时 route 归一为 unknown）。请求 query 不参与标签（FullPath 本身
// 为路由模板，不含 query）。
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := NormalizeRoute(c.FullPath())
		status := strconv.Itoa(c.Writer.Status())
		httpRequestsTotal.WithLabelValues(c.Request.Method, route, status).Inc()
		httpRequestDuration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
	}
}

// NormalizeRoute 归一化 route 标签：gin 路由模板（c.FullPath()）不含 query，
// 这里防御性剥离 "?" 之后的内容；空模板（未匹配任何路由，404）归一为
// "unknown"，避免动态 path 造成高基数。
func NormalizeRoute(fullPath string) string {
	if i := strings.IndexByte(fullPath, '?'); i >= 0 {
		fullPath = fullPath[:i]
	}
	if fullPath == "" {
		return RouteUnknown
	}
	return fullPath
}

// IncUploadSession 记录一次上传会话创建（status 取 UploadSessionCreated /
// UploadSessionFailed）。
func IncUploadSession(status string) { uploadSessionsTotal.WithLabelValues(status).Inc() }

// ObserveUploadProcessing 记录 Complete 阶段耗时（stage 取 UploadStageVerify /
// UploadStageScan）。
func ObserveUploadProcessing(stage string, seconds float64) {
	uploadProcessingDuration.WithLabelValues(stage).Observe(seconds)
}

// IncScanResult 记录一次扫描结果（result 取 ScanResultClean / ScanResultInfected /
// ScanResultError）。
func IncScanResult(result string) { scanResultsTotal.WithLabelValues(result).Inc() }

// IncOnlyOfficeCallback 记录一次 DocumentServer 回调（status 取 CallbackStatus*，
// result 取 CallbackResult*）。
func IncOnlyOfficeCallback(status, result string) {
	onlyofficeCallbacksTotal.WithLabelValues(status, result).Inc()
}

// IncQueueEnqueued 记录一次后台任务入队（taskType 取 TaskType 常量
// （internal/tasks），driver 取 QueueDriverInProcess / QueueDriverRedis）。
func IncQueueEnqueued(taskType, driver string) {
	queueEnqueuedTotal.WithLabelValues(taskType, driver).Inc()
}

// IncQueueProcessed 记录一次后台任务处理结果（taskType 同上，
// status 取 QueueStatusSuccess / QueueStatusFailed）。
func IncQueueProcessed(taskType, status string) {
	queueProcessedTotal.WithLabelValues(taskType, status).Inc()
}
