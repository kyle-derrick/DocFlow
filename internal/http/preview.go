package http

import (
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
)

// 预览白名单常量：仅允许可安全内联展示的媒体类型。
const (
	previewTextPlain = "text/plain"
	previewAppJSON   = "application/json"
	previewAppPDF    = "application/pdf"
	previewSVGPrefix = "image/svg"
)

// previewResponse 判定 blob 的 MIME 类型是否允许内联预览（纯函数，便于矩阵单测）。
// 返回 ok=false 表示类型不在白名单（应答 415 preview not supported）；
// contentType 为规范化后的响应 Content-Type：类型与参数大小写归一，text/* 强制 charset=utf-8。
// 安全规则：
//   - image/svg* 前缀（含标准 image/svg+xml 及历史变体）一律拒绝（SVG 可携带脚本，防 XSS）；
//   - 其余 image/*、application/pdf、text/plain、application/json 允许；
//   - text/html、JS、可执行文件、压缩包等一律拒绝。
func previewResponse(rawMime string) (contentType string, ok bool) {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(rawMime))
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case strings.HasPrefix(mediaType, previewSVGPrefix):
		return "", false
	case strings.HasPrefix(mediaType, "image/"):
	case mediaType == previewAppPDF:
	case mediaType == previewAppJSON:
	case mediaType == previewTextPlain:
		// text/* 强制 charset=utf-8，丢弃原始 charset 参数。
		return previewTextPlain + "; charset=utf-8", true
	default:
		return "", false
	}
	if formatted := mime.FormatMediaType(mediaType, params); formatted != "" {
		return formatted, true
	}
	return mediaType, true
}

// servePreviewBlob 输出内联预览响应，认证与公开端点共用：
// 类型白名单（415）、blob 可用性校验（403）、Range（206/416）、
// Content-Disposition: inline（RFC 5987）、X-Content-Type-Options: nosniff。
// incrementView 在响应体写出前调用一次（可为 nil，表示计数已由调用方完成）；
// 调用失败返回 500，不计入成功预览。
func (h *Handler) servePreviewBlob(c *gin.Context, name string, blob files.ObjectBlob, incrementView func() error) {
	contentType, allowed := previewResponse(blob.MimeType)
	if !allowed {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "preview not supported"})
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available for preview", "status": blob.Status})
		return
	}
	fileRange, hasRange, err := parseRange(c.GetHeader("Range"), blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	// 区间读取统一走 upload.ReadSection：S3 原生 GetObject Range（不依赖
	// io.Seeker 断言），LocalStorage Open+Seek；无 Range/多区间（忽略）时
	// 整读 [0, size)。
	start, length := int64(0), blob.Size
	if hasRange {
		start, length = fileRange.Start, fileRange.Length()
	}
	body, err := upload.ReadSection(h.storage, blob.StorageKey, start, length)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer body.Close()
	if incrementView != nil {
		if err := incrementView(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to record preview"})
			return
		}
	}
	status := http.StatusOK
	contentLength := blob.Size
	if hasRange {
		status = http.StatusPartialContent
		contentLength = fileRange.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", fileRange.Start, fileRange.End, blob.Size))
	}
	c.Header("Content-Disposition", inlineDisposition(name))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Accept-Ranges", "bytes")
	c.DataFromReader(status, contentLength, contentType, body, nil)
}

// previewFile GET /api/v1/files/:id/preview：认证内联预览（owner 隔离）。
// 与 download 的差异：Content-Disposition 为 inline、类型白名单、nosniff、递增 view_count。
// 网页包联动：zip 候选文件且 web_packages 存在 ready 包时改返 200 JSON
// {kind:"webpkg", url:"/content/<pid>/index.html"}（替代 415）。
func (h *Handler) previewFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	owner := userID(c)
	_, blob, err := h.files.CurrentVersion(owner, id)
	if err != nil {
		h.fileError(c, err)
		return
	}
	f, err := h.files.Get(owner, id)
	if err != nil {
		h.fileError(c, err)
		return
	}
	if h.tryWebpkgPreview(c, f, blob, func() error {
		return h.files.IncrementViewCount(owner, id)
	}) {
		return
	}
	h.servePreviewBlob(c, f.Name, blob, func() error {
		return h.files.IncrementViewCount(owner, id)
	})
}

// publicSharePreview GET /api/v1/public/shares/:token/preview：公开内联预览。
// view 与 download 权限均可预览（ResolveForPreview：校验有效期与对象可用性，
// 不消耗分享 download_count）；类型白名单通过后原子递增 files.view_count，
// 安全头与认证端点一致。密码保护分享须先经 /verify 取得会话 cookie
// （未通过时 401 PASSWORD_REQUIRED）。网页包联动同认证预览（ready 包改返
// webpkg JSON）。预览成功（view_count 递增）时写入访问事件（action=preview）。
func (h *Handler) publicSharePreview(c *gin.Context) {
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
	record := func() error {
		if err := h.shares.IncrementPreviewView(r); err != nil {
			return err
		}
		h.recordPublicAccessEvent(c, r, share.ActionPreview)
		return nil
	}
	if h.tryWebpkgPreview(c, r.File, r.Blob, record) {
		return
	}
	h.servePreviewBlob(c, r.File.Name, r.Blob, record)
}
