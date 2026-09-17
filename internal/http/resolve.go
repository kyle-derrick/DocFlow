package http

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/contenturl"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
)

// rawCSP 为 /raw/* 原始内容端点的 CSP：sandbox allow-scripts——文档进入
// 唯一化 origin 的沙箱，不允许 same-origin / top-navigation / popups / forms，
// 脚本既不能触碰主站源也带不上主站 Cookie（该路径在 /api/v1 之外）。
const rawCSP = "sandbox allow-scripts"

// rawRateLimitPerMin 为 /raw/* 内容端点的独立按 IP 轻限流（目录浏览会拉取
// 多个子资源，取比 /content 更宽的值）。
const rawRateLimitPerMin = 240

// rawContentTypes 为 /raw/* 的扩展名白名单与推断的 Content-Type；
// 未知扩展 415（不提供 octet-stream，避免浏览器嗅探面）。
var rawContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".avif":  "image/avif",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".pdf":   "application/pdf",
	".mp4":   "video/mp4",
	".webm":  "video/webm",
	".mp3":   "audio/mpeg",
}

// rawContentType 按文件名扩展返回白名单 Content-Type；无扩展或不在白名单
// 返回 ok=false（415）。
func rawContentType(name string) (string, bool) {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 || dot == len(name)-1 {
		return "", false
	}
	ct, ok := rawContentTypes[strings.ToLower(name[dot:])]
	return ct, ok
}

// resolveAPI 是路径解析与原始内容读取的最小依赖（*files.Store 满足；
// 接口化便于单测注入内存实现，模式同 versionReader）。
type resolveAPI interface {
	ResolveReadablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (files.File, []files.File, error)
	ResolveWritablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (files.File, []files.File, error)
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	FindChildByName(parent uuid.UUID, name string) (files.File, error)
}

var _ resolveAPI = (*files.Store)(nil)

// SetContentSigner 注入 /raw/* 短期授权签发器（幂等）；未注入时 resolve
// 端点 503、raw 端点 404（生产恒注入）。
func (h *Handler) SetContentSigner(s *contenturl.Signer) {
	if s != nil {
		h.contentSigner = s
	}
}

// SetContentPublicBaseURL 注入受控原始内容的对外基地址（CONTENT_PUBLIC_BASE_URL，
// 可选）：resolve API 以 origin_content=1 请求时用于拼接绝对 raw_url；
// 未配置（空）时保持相对路径。
func (h *Handler) SetContentPublicBaseURL(base string) {
	h.contentBaseURL = strings.TrimSuffix(strings.TrimSpace(base), "/")
}

// rawBase 返回 raw_url 的前缀：origin_content 模式且配置了基地址时为
// 绝对前缀，否则相对路径。
func (h *Handler) rawBase(originContent bool) string {
	if originContent && h.contentBaseURL != "" {
		return h.contentBaseURL
	}
	return ""
}

// canonicalPathOf 由解析链（chain[0]=根目录）构造 canonical 相对路径。
func canonicalPathOf(chain []files.File) string {
	if len(chain) < 2 {
		return ""
	}
	names := make([]string, 0, len(chain)-1)
	for _, f := range chain[1:] {
		names = append(names, f.Name)
	}
	return strings.Join(names, "/")
}

// escapePathSegments 对 canonical 路径逐段 percent-encoding（保留可读中文，
// 转义空格/#/? 等对 URL 有意义的字符）。
func escapePathSegments(path string) string {
	if path == "" {
		return ""
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// resolvePath GET /api/v1/resolve/:nsType/:nsScope/*path?mode=view|edit&origin_content=1：
// 登录鉴权；解析命名空间内路径（mode=edit 额外校验写权限），返回 file_id/
// name/type/mime/canonical_path 与 view_url（/view/...，前端路由）、edit_url
// （/edit/...）、raw_url（/raw/auth/<grant>/...，10 分钟短期授权，目录以
// 尾斜杠结尾供 index.html 解析）与 grant_expires_at。origin_content=1 且
// 配置 CONTENT_PUBLIC_BASE_URL 时 raw_url 为绝对地址。
// 前端查看器直接使用 raw_url（不提供 /raw/view 登录态端点：raw 域无 Cookie
// 依赖，授权一律经本端点换取 grant）。
func (h *Handler) resolvePath(c *gin.Context) {
	if h.contentSigner == nil || h.resolver == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "path resolve is not configured"})
		return
	}
	nsType := c.Param("nsType")
	scope, ok := parseID(c, c.Param("nsScope"))
	if !ok {
		return
	}
	rel := strings.Trim(c.Param("path"), "/")
	actor := userID(c)
	var f files.File
	var chain []files.File
	var err error
	if c.Query("mode") == "edit" {
		f, chain, err = h.resolver.ResolveWritablePath(actor, nsType, scope, rel)
	} else {
		f, chain, err = h.resolver.ResolveReadablePath(actor, nsType, scope, rel)
	}
	if resolvePathError(c, err) {
		return
	}
	canonical := canonicalPathOf(chain)
	escaped := escapePathSegments(canonical)
	// 文件：附当前版本 MIME（无版本/不可用则为 null）。
	var mimeType any
	if f.Type == "file" {
		if _, blob, verr := h.resolver.CurrentVersion(actor, f.ID); verr == nil {
			mimeType = blob.MimeType
		}
	}
	grant, err := h.contentSigner.Sign(contenturl.Claims{
		Purpose: contenturl.PurposeRaw,
		UserID:  actor.String(),
		NSType:  nsType,
		NSScope: scope.String(),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to sign raw url"})
		return
	}
	suffix := ""
	if f.Type == "folder" {
		suffix = "/"
	}
	origin := c.Query("origin_content") == "1"
	rawPath := fmt.Sprintf("%s/raw/auth/%s/%s/%s/%s%s", h.rawBase(origin), grant, nsType, scope, escaped, suffix)
	c.JSON(http.StatusOK, gin.H{
		"file_id":          f.ID,
		"name":             f.Name,
		"type":             f.Type,
		"mime_type":        mimeType,
		"canonical_path":   canonical,
		"view_url":         fmt.Sprintf("/view/%s/%s/%s%s", nsType, scope, escaped, suffix),
		"edit_url":         fmt.Sprintf("/edit/%s/%s/%s%s", nsType, scope, escaped, suffix),
		"raw_url":          rawPath,
		"grant_expires_at": h.contentSigner.ExpiresAt().Format(time.RFC3339),
	})
}

// resolvePathError 映射解析 API 的错误（登录 API 语义，可区分 400/403/404）。
func resolvePathError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, files.ErrInvalidNamespace), errors.Is(err, files.ErrInvalidName), errors.Is(err, files.ErrFolderDepth):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, files.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "no permission for this operation"})
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrInvalidTarget):
		c.JSON(http.StatusNotFound, gin.H{"error": "path not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to resolve path"})
	}
	return true
}

// rawNotFound 为 raw 内容域的统一 404（不泄露任何细节）。
func rawNotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
}

// serveRawAuth GET/HEAD /raw/auth/:grant/:nsType/:nsScope/*path：
// 验证 grant（purpose=raw、ns/scope 与 URL 一致、未过期）→ 以 grant 主体
// 每请求实时解析 + 读授权（authorizeFileAccess 复用）→ 目录无尾斜杠 308
// 加斜杠、目录尾斜杠解析 index.html → 文件当前版本 blob 须 available →
// 扩展名白名单 / Range / 安全头输出。HEAD 无 body。
func (h *Handler) serveRawAuth(c *gin.Context) {
	if h.contentSigner == nil || h.resolver == nil {
		rawNotFound(c)
		return
	}
	claims, err := h.contentSigner.VerifyPurpose(c.Param("grant"), contenturl.PurposeRaw)
	if err != nil {
		rawNotFound(c)
		return
	}
	nsType := c.Param("nsType")
	scopeRaw := c.Param("nsScope")
	if claims.NSType != nsType || claims.NSScope != scopeRaw {
		rawNotFound(c)
		return
	}
	actor, err := uuid.Parse(claims.UserID)
	if err != nil {
		rawNotFound(c)
		return
	}
	scope, err := uuid.Parse(scopeRaw)
	if err != nil {
		rawNotFound(c)
		return
	}
	rawPathParam := c.Param("path")
	f, _, err := h.resolver.ResolveReadablePath(actor, nsType, scope, strings.Trim(rawPathParam, "/"))
	if rawContentError(c, err) {
		return
	}
	if f.Type == "folder" {
		if !strings.HasSuffix(rawPathParam, "/") {
			h.rawRedirectTrailingSlash(c)
			return
		}
		index, ierr := h.resolver.FindChildByName(f.ID, "index.html")
		if ierr != nil {
			rawNotFound(c)
			return
		}
		f = index
	}
	if f.Type != "file" {
		rawNotFound(c)
		return
	}
	_, blob, err := h.resolver.CurrentVersion(actor, f.ID)
	if err != nil {
		rawNotFound(c)
		return
	}
	h.serveRawBlob(c, f.Name, blob)
}

// rawRedirectTrailingSlash 输出 308（保留查询串）：目录路径无尾斜杠时补齐，
// 供 index.html 相对资源解析。
func (h *Handler) rawRedirectTrailingSlash(c *gin.Context) {
	target := c.Request.URL.EscapedPath() + "/"
	if c.Request.URL.RawQuery != "" {
		target += "?" + c.Request.URL.RawQuery
	}
	c.Redirect(http.StatusPermanentRedirect, target)
}

// rawContentError 映射 raw 内容域的解析错误：非法段/超深/命名空间问题按
// 404 处理（不泄露细节），越权 403，未找到 404，其余 500。
func rawContentError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, files.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not found"})
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrInvalidTarget),
		errors.Is(err, files.ErrInvalidName), errors.Is(err, files.ErrInvalidNamespace),
		errors.Is(err, files.ErrFolderDepth):
		rawNotFound(c)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read content"})
	}
	return true
}

// serveRawBlob 输出受控原始内容（/raw/auth 与 /raw/share 共用）：
// 扩展名白名单（415）、blob 须 available（404）、单区间 Range（206/416）、
// CSP sandbox allow-scripts + nosniff + private,no-store + no-referrer、
// Content-Disposition inline（RFC 5987）。
func (h *Handler) serveRawBlob(c *gin.Context, name string, blob files.ObjectBlob) {
	contentType, allowed := rawContentType(name)
	if !allowed {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "unsupported content type"})
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		rawNotFound(c)
		return
	}
	fileRange, hasRange, err := parseRange(c.GetHeader("Range"), blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	start, length := int64(0), blob.Size
	if hasRange {
		start, length = fileRange.Start, fileRange.Length()
	}
	reader, err := upload.ReadSection(h.storage, blob.StorageKey, start, length)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	c.Header("Content-Security-Policy", rawCSP)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Disposition", inlineDisposition(name))
	c.Header("Accept-Ranges", "bytes")
	status, contentLength := http.StatusOK, blob.Size
	if hasRange {
		status, contentLength = http.StatusPartialContent, fileRange.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", fileRange.Start, fileRange.End, blob.Size))
	}
	c.DataFromReader(status, contentLength, contentType, reader, nil)
}
