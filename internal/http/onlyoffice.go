package http

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// onlyofficeRateLimitDefault 为 onlyoffice 公开组（download/callback）独立
// 按 IP 轻限流的默认值（每分钟次数；SetOnlyOffice 传入非正值时使用）。
const onlyofficeRateLimitDefault = 60

// onlyofficeConfig GET /api/v1/onlyoffice/config：前端探测集成可用性与
// DocumentServer 地址（加载 DocEditor 脚本、决定是否显示「编辑」入口）。
// 该端点恒注册（不随 ONLYOFFICE_ENABLED 开关 404）：禁用时返回
// {enabled:false, server_url:null}，不暴露内部 URL；启用时 server_url 返回
// 浏览器可达地址（ONLYOFFICE_PUBLIC_URL 优先，未配置回退 ONLYOFFICE_SERVER_URL
// ——后者为 docker 内网名时浏览器无法解析加载 api.js，生产应配置 PUBLIC_URL）。
func (h *Handler) onlyofficeConfig(c *gin.Context) {
	if h.onlyoffice == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "server_url": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "server_url": h.onlyoffice.PublicServerURL()})
}

// SetOnlyOffice 注入 ONLYOFFICE 集成服务；非 nil 时启用路由挂载（幂等）。
// rateLimitPerMinute 为公开组独立轻限流，<=0 时回退默认 60。
func (h *Handler) SetOnlyOffice(svc *onlyoffice.Service, rateLimitPerMinute int) {
	if svc != nil {
		h.onlyoffice = svc
		if rateLimitPerMinute > 0 {
			h.onlyofficeRateLimitPerMin = rateLimitPerMinute
		}
	}
}

// registerOnlyOfficeRoutes 挂载 ONLYOFFICE 路由（仅在集成启用时调用）：
//   - POST /api/v1/onlyoffice/session 挂认证组（Bearer + 通用限流）；
//   - GET  /api/v1/onlyoffice/download/:fileId 与 POST /api/v1/onlyoffice/callback
//     为 DocumentServer 回源链路（无用户会话）：挂公开组绕过 Bearer 与认证接口
//     限流，自带 JWT 校验与独立按 IP 轻限流。
func (h *Handler) registerOnlyOfficeRoutes(api *gin.RouterGroup, r *gin.Engine) {
	api.POST("/onlyoffice/session", h.createOnlyOfficeSession)
	limit := h.onlyofficeRateLimitPerMin
	if limit <= 0 {
		limit = onlyofficeRateLimitDefault
	}
	group := r.Group("/api/v1/onlyoffice", publicLimiter(NewRateLimiter(limit)))
	group.GET("/download/:fileId", h.onlyofficeDownload)
	group.GET("/download/:fileId/:filename", h.onlyofficeDownload)
	group.POST("/callback", h.onlyofficeCallback)
}

type onlyofficeSessionRequest struct {
	FileID string `json:"file_id"`
	// mode 会话模式：默认按写权限判定（edit/view）；显式 "view" 强制只读
	// （在线预览），其余值按默认处理。
	Mode string `json:"mode"`
	// Lang 编辑器界面语言（如 zh-CN/en-US，随前端界面语言传入）。
	Lang string `json:"lang"`
}

// createOnlyOfficeSession POST /api/v1/onlyoffice/session：校验读权限与当前
// 版本可用性后，返回可直接传给 DocsAPI.DocEditor 的编辑配置（含 5 分钟有效
// 的 JWT token 与签名下载 URL）。mode="view" 强制只读会话（预览用）。
func (h *Handler) createOnlyOfficeSession(c *gin.Context) {
	var req onlyofficeSessionRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(req.FileID))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	config, err := h.onlyoffice.NewSessionConfig(userID(c), id, onlyoffice.SessionOptions{
		View: strings.EqualFold(strings.TrimSpace(req.Mode), "view"),
		Lang: req.Lang,
	})
	if onlyofficeError(c, err) {
		return
	}
	c.JSON(http.StatusOK, config)
}

// publicShareOfficeConfig GET /api/v1/public/shares/:token/office?lang=：
// 公开分享的 OnlyOffice 只读查看会话（访客无需登录）。解析分享 token
// （有效期/撤销/密码会话同 publicSharePreview 口径，view 与 download 权限均
// 可预览、不消耗 download_count），成功时返回 {server_url, config}（config
// 为可直接传给 DocsAPI.DocEditor 的 JWT 签名配置，恒 view 模式；分享
// permission=download 时编辑器内开放下载/打印）。集成未启用时 404（前端
// 回退「不支持在线预览」分支）。预览成功计入 view_count 与访问事件
// （action=preview，与 publicSharePreview 一致）。
func (h *Handler) publicShareOfficeConfig(c *gin.Context) {
	if h.onlyoffice == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "onlyoffice integration disabled"})
		return
	}
	token := c.Param("token")
	pre, err := h.shares.Resolve(token)
	if publicShareError(c, err) {
		return
	}
	if !h.shareSessionAllowed(c, token, pre.Share) {
		return
	}
	r, err := h.shares.ResolveForPreview(token)
	if publicShareError(c, err) {
		return
	}
	if r.File.Type != "file" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "share target is not a file"})
		return
	}
	config, err := h.onlyoffice.NewShareViewConfig(r.File, r.Version, onlyoffice.SessionOptions{
		Lang:          c.Query("lang"),
		AllowDownload: pre.Share.Permission == share.PermissionDownload,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "onlyoffice operation failed"})
		return
	}
	if err := h.shares.IncrementPreviewView(r); err == nil {
		h.recordPublicAccessEvent(c, r, share.ActionPreview)
	}
	c.JSON(http.StatusOK, gin.H{"server_url": h.onlyoffice.PublicServerURL(), "config": config})
}

// onlyofficeDownload GET /api/v1/onlyoffice/download/:fileId?v=&token=：
// DocumentServer 回源下载（无 Bearer）。签名校验通过后流式返回该版本内容，
// Range 与 Content-Disposition: attachment 语义同个人下载（不计数——内部回源）。
func (h *Handler) onlyofficeDownload(c *gin.Context) {
	id, ok := parseID(c, c.Param("fileId"))
	if !ok {
		return
	}
	f, _, blob, err := h.onlyoffice.ResolveDownload(id, c.Query("v"), c.Query("token"))
	if onlyofficeError(c, err) {
		return
	}
	if filename := c.Param("filename"); filename != "" && norm.NFC.String(filename) != norm.NFC.String(f.Name) {
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		return
	}
	r, hasRange, err := parseRange(c.GetHeader("Range"), blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	// 区间读取统一走 upload.ReadSection（S3 原生 Range / LocalStorage Seek；
	// 无 Range 时整读 [0, size)），不再依赖 io.Seeker 断言。
	start, length := int64(0), blob.Size
	if hasRange {
		start, length = r.Start, r.Length()
	}
	reader, err := upload.ReadSection(h.storage, blob.StorageKey, start, length)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	status := http.StatusOK
	contentLength := blob.Size
	if hasRange {
		status = http.StatusPartialContent
		contentLength = r.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", r.Start, r.End, blob.Size))
	}
	c.Header("Content-Disposition", contentDisposition(f.Name))
	c.Header("Accept-Ranges", "bytes")
	contentType := blob.MimeType
	if contentType == "" || strings.EqualFold(contentType, "application/octet-stream") {
		if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Name))); detected != "" {
			contentType = detected
		}
	}
	c.DataFromReader(status, contentLength, contentType, reader, nil)
}

// onlyofficeCallback POST /api/v1/onlyoffice/callback：DocumentServer 保存回调
// （无 Bearer，服务内自校验 JWT）。始终 HTTP 200：成功 {"error":0}，失败 {"error":1}
// （ONLYOFFICE 协议约定，非 200 会触发 DocumentServer 重试）。
func (h *Handler) onlyofficeCallback(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"error": 1})
		return
	}
	if err := h.onlyoffice.HandleCallback(body, c.GetHeader("Authorization"), c.ClientIP(), c.GetHeader("User-Agent")); err != nil {
		c.JSON(http.StatusOK, gin.H{"error": 1})
		return
	}
	c.JSON(http.StatusOK, gin.H{"error": 0})
}

// onlyofficeError 统一映射集成错误；nil 时不写响应并返回 false。
func onlyofficeError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, onlyoffice.ErrInvalidToken):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid onlyoffice token"})
	case errors.Is(err, onlyoffice.ErrInvalidKey), errors.Is(err, files.ErrInvalidTarget):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, onlyoffice.ErrURLNotAllowed), errors.Is(err, files.ErrForbidden),
		errors.Is(err, files.ErrBlobUnavailable):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	case errors.Is(err, files.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
	case errors.Is(err, files.ErrNoVersion):
		c.JSON(http.StatusConflict, gin.H{"error": "file has no current version"})
	case errors.Is(err, onlyoffice.ErrDownloadTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "onlyoffice operation failed"})
	}
	return true
}
