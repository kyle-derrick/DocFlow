package http

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type shareRequest struct {
	FileID       string   `json:"file_id"`
	Permission   string   `json:"permission"`
	Visibility   string   `json:"visibility"`    // public|private，缺省 public
	UserIDs      []string `json:"user_ids"`      // 私有分享：显式授权用户
	TeamIDs      []string `json:"team_ids"`      // 私有分享：授权团队
	ExpiresIn    *int64   `json:"expires_in"`    // 秒；缺省或 0 表示永久
	MaxDownloads *int     `json:"max_downloads"` // 缺省表示不限
}

// parseIDList 解析 UUID 列表（去重顺序保留）。
func parseIDList(c *gin.Context, values []string) ([]uuid.UUID, bool) {
	seen := make(map[uuid.UUID]struct{}, len(values))
	out := make([]uuid.UUID, 0, len(values))
	for _, v := range values {
		id, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id in list"})
			return nil, false
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, true
}

// createShare POST /api/v1/shares 为当前用户文件创建分享。
// visibility=public（默认）返回公开 token（仅本次响应可见一次，之后只能通过 token_hash 匹配）；
// visibility=private 创建私有分享（不生成 token，授权给 user_ids/team_ids）。
func (h *Handler) createShare(c *gin.Context) {
	var req shareRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	fileID, ok := parseID(c, req.FileID)
	if !ok {
		return
	}
	owner := userID(c)
	permission := req.Permission
	if permission == "" {
		permission = share.PermissionView
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = share.VisibilityPublic
	}
	if visibility != share.VisibilityPublic && visibility != share.VisibilityPrivate {
		c.JSON(http.StatusBadRequest, gin.H{"error": share.ErrInvalidVisibility.Error()})
		return
	}
	var expiresIn time.Duration
	if req.ExpiresIn != nil {
		expiresIn = time.Duration(*req.ExpiresIn) * time.Second
	}
	var out gin.H
	if visibility == share.VisibilityPrivate {
		userIDs, ok := parseIDList(c, req.UserIDs)
		if !ok {
			return
		}
		teamIDs, ok := parseIDList(c, req.TeamIDs)
		if !ok {
			return
		}
		created, err := h.shares.CreatePrivate(owner, fileID, permission, expiresIn, req.MaxDownloads, userIDs, teamIDs)
		if err != nil {
			h.shareCreateError(c, err)
			return
		}
		h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionShareCreate, ResourceType: audit.ResourceShare, ResourceID: created.ID.String(), Metadata: `{"file_id":"` + created.FileID.String() + `","permission":"` + created.Permission + `","visibility":"private","users":` + strconv.Itoa(len(userIDs)) + `,"teams":` + strconv.Itoa(len(teamIDs)) + `}`})
		out = shareJSON(created)
		// 私有分享无 token / share_url。
	} else {
		if len(req.UserIDs) > 0 || len(req.TeamIDs) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user_ids/team_ids are only allowed for private shares"})
			return
		}
		created, token, err := h.shares.Create(owner, fileID, permission, expiresIn, req.MaxDownloads)
		if err != nil {
			h.shareCreateError(c, err)
			return
		}
		h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionShareCreate, ResourceType: audit.ResourceShare, ResourceID: created.ID.String(), Metadata: `{"file_id":"` + created.FileID.String() + `","permission":"` + created.Permission + `","max_downloads":` + nullableIntJSON(created.MaxDownloads) + `}`})
		out = shareJSON(created)
		out["token"] = token
		out["share_url"] = "/api/v1/public/shares/" + token
	}
	c.JSON(http.StatusCreated, out)
}

func nullableIntJSON(v *int) string {
	if v == nil {
		return "null"
	}
	return strconv.Itoa(*v)
}

func (h *Handler) shareCreateError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, share.ErrInvalidPermission), errors.Is(err, share.ErrInvalidExpiry), errors.Is(err, share.ErrInvalidMaxDownloads):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, share.ErrFileNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
	case errors.Is(err, share.ErrFileNotShareable):
		c.JSON(http.StatusConflict, gin.H{"error": "file cannot be shared"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create share"})
	}
}

// listShares GET /api/v1/shares 列出当前用户的分享（created_at 倒序）。
// 每条附 file_name（文件已删除时为空）与 visibility（public|private）。
func (h *Handler) listShares(c *gin.Context) {
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < 1000 {
			limit = n
		} else {
			limit = 1000
		}
	}
	out, err := h.shares.ListWithFileNames(userID(c), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list shares"})
		return
	}
	items := make([]gin.H, 0, len(out))
	for _, it := range out {
		item := shareJSON(it.Share)
		item["visibility"] = it.Share.Visibility
		item["file_name"] = it.FileName
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"shares": items})
}

// listSharedWithMe GET /api/v1/shares/shared-with-me：分享给当前用户的有效私有分享列表
// （share_users 显式授权或 share_teams 团队成员；公开分享与已撤销/过期/达上限的不含）。
// 每条附文件元数据（name/size/mime_type）与分享者用户名（owner_username，不含 email）。
func (h *Handler) listSharedWithMe(c *gin.Context) {
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < 1000 {
			limit = n
		} else {
			limit = 1000
		}
	}
	items, err := h.shares.SharedWithMe(userID(c), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list shared with me"})
		return
	}
	out := make([]gin.H, 0, len(items))
	for _, it := range items {
		out = append(out, gin.H{
			"id":             it.Share.ID,
			"file_id":        it.Share.FileID,
			"name":           it.FileName,
			"size":           it.FileSize,
			"mime_type":      it.FileMime,
			"permission":     it.Share.Permission,
			"owner_username": it.OwnerName,
			"expires_at":     it.Share.ExpiresAt,
			"created_at":     it.Share.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"shares": out})
}

// revokeShare DELETE /api/v1/shares/:id 撤销分享（幂等）。
func (h *Handler) revokeShare(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	owner := userID(c)
	revoked, err := h.shares.Revoke(owner, id)
	if err != nil {
		if errors.Is(err, share.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to revoke share"})
		}
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionShareRevoke, ResourceType: audit.ResourceShare, ResourceID: revoked.ID.String(), Metadata: `{"file_id":"` + revoked.FileID.String() + `"}`})
	c.Status(http.StatusNoContent)
}

func shareJSON(s share.Share) gin.H {
	return gin.H{"id": s.ID, "file_id": s.FileID, "permission": s.Permission, "expires_at": s.ExpiresAt, "max_downloads": s.MaxDownloads, "download_count": s.DownloadCount, "revoked_at": s.RevokedAt, "created_at": s.CreatedAt}
}

// publicShareError 映射公开接口错误：不存在 404、失效 410、权限不足/不可用 403，
// 其余一律 500 且不泄露内部细节。
func publicShareError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, share.ErrNotFound), errors.Is(err, share.ErrFileNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "share not found"})
	case errors.Is(err, share.ErrGone), errors.Is(err, share.ErrDownloadLimit):
		c.JSON(http.StatusGone, gin.H{"error": "share is no longer available"})
	case errors.Is(err, share.ErrDownloadForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "download not allowed"})
	case errors.Is(err, share.ErrFileNotAvailable):
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available for download"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to process share"})
	}
	return true
}

// publicShareInfo GET /api/v1/public/shares/:token 公开文件元数据（无认证、不设 cookie）。
// 仅暴露安全字段：名称、大小、mime、版本状态等；不含 owner、内部 ID 与存储路径。
func (h *Handler) publicShareInfo(c *gin.Context) {
	r, err := h.shares.Resolve(c.Param("token"))
	if publicShareError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":           r.File.Name,
		"size":           r.Blob.Size,
		"mime_type":      r.Blob.MimeType,
		"version":        r.Version.Version,
		"version_status": r.Blob.Status,
		"permission":     r.Share.Permission,
		"expires_at":     r.Share.ExpiresAt,
		"max_downloads":  r.Share.MaxDownloads,
		"download_count": r.Share.DownloadCount,
	})
}

// publicShareDownload GET /api/v1/public/shares/:token/download 公开流式下载。
// 校验权限与 ObjectBlob 可用性，成功时已原子递增分享与文件下载计数，并写入审计。
// 存储读取失败（500）时经 DecrementDownload 补偿回退已消耗的分享/文件计数。
// 同样支持 Range 与 Content-Disposition。
func (h *Handler) publicShareDownload(c *gin.Context) {
	r, err := h.shares.ResolveForDownload(c.Param("token"))
	if publicShareError(c, err) {
		return
	}
	fileRange, hasRange, err := parseRange(c.GetHeader("Range"), r.Blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", r.Blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	// 区间读取统一走 upload.ReadSection（S3 原生 Range / LocalStorage Seek；
	// 无 Range 时整读 [0, size)），不再依赖 io.Seeker 断言。
	start, length := int64(0), r.Blob.Size
	if hasRange {
		start, length = fileRange.Start, fileRange.Length()
	}
	reader, err := upload.ReadSection(h.storage, r.Blob.StorageKey, start, length)
	if err != nil {
		_ = h.shares.DecrementDownload(r)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionPublicDownload, ResourceType: audit.ResourceShare, ResourceID: r.Share.ID.String(), Metadata: `{"file_id":"` + r.File.ID.String() + `","size":` + strconv.FormatInt(r.Blob.Size, 10) + `}`})
	status := http.StatusOK
	contentLength := r.Blob.Size
	if hasRange {
		status = http.StatusPartialContent
		contentLength = fileRange.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", fileRange.Start, fileRange.End, r.Blob.Size))
	}
	c.Header("Content-Disposition", contentDisposition(r.File.Name))
	c.Header("Accept-Ranges", "bytes")
	c.DataFromReader(status, contentLength, r.Blob.MimeType, reader, nil)
}

// parseShareFileParams 解析 /api/v1/shares/:id/files/:fid 路径参数。
func (h *Handler) parseShareFileParams(c *gin.Context) (shareID, fileID uuid.UUID, ok bool) {
	shareID, ok = parseID(c, c.Param("id"))
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	fileID, ok = parseID(c, c.Param("fid"))
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return shareID, fileID, true
}

// privateShareError 映射私有分享访问错误：未授权 403、不存在 404、失效 410，
// 其余一律 500 且不泄露内部细节（含分享/文件存在性）。
func privateShareError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, share.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
	default:
		// 复用公开接口映射：ErrNotFound→404、ErrGone/ErrDownloadLimit→410、
		// ErrDownloadForbidden/ErrFileNotAvailable→403、其余 500。
		publicShareError(c, err)
	}
	return true
}

// shareFileInfo GET /api/v1/shares/:id/files/:fid：私有分享文件元数据（登录用户）。
// owner 直接可见；其他用户须通过 CanAccess（显式授权或团队成员）。
func (h *Handler) shareFileInfo(c *gin.Context) {
	shareID, fileID, ok := h.parseShareFileParams(c)
	if !ok {
		return
	}
	r, err := h.shares.ResolveForUser(shareID, fileID, userID(c))
	if privateShareError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":           r.File.Name,
		"size":           r.Blob.Size,
		"mime_type":      r.Blob.MimeType,
		"version":        r.Version.Version,
		"version_status": r.Blob.Status,
		"permission":     r.Share.Permission,
		"expires_at":     r.Share.ExpiresAt,
		"max_downloads":  r.Share.MaxDownloads,
		"download_count": r.Share.DownloadCount,
	})
}

// shareFileDownload GET /api/v1/shares/:id/files/:fid/download：私有分享流式下载（登录用户）。
// 校验授权与下载权限，成功时原子递增分享与文件下载计数，支持 Range。
// 存储读取失败（500）时经 DecrementDownload 补偿回退已消耗的分享/文件计数。
func (h *Handler) shareFileDownload(c *gin.Context) {
	shareID, fileID, ok := h.parseShareFileParams(c)
	if !ok {
		return
	}
	user := userID(c)
	r, err := h.shares.ResolveForUserForDownload(shareID, fileID, user)
	if privateShareError(c, err) {
		return
	}
	fileRange, hasRange, err := parseRange(c.GetHeader("Range"), r.Blob.Size)
	if err != nil {
		c.Header("Content-Range", fmt.Sprintf("bytes */%d", r.Blob.Size))
		c.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"error": "invalid range"})
		return
	}
	// 同公开下载：区间读取统一走 upload.ReadSection。
	start, length := int64(0), r.Blob.Size
	if hasRange {
		start, length = fileRange.Start, fileRange.Length()
	}
	reader, err := upload.ReadSection(h.storage, r.Blob.StorageKey, start, length)
	if err != nil {
		_ = h.shares.DecrementDownload(r)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	h.recordAudit(c, audit.Entry{UserID: &user, Action: audit.ActionPublicDownload, ResourceType: audit.ResourceShare, ResourceID: r.Share.ID.String(), Metadata: `{"file_id":"` + r.File.ID.String() + `","size":` + strconv.FormatInt(r.Blob.Size, 10) + `,"access":"private"}`})
	status := http.StatusOK
	contentLength := r.Blob.Size
	if hasRange {
		status = http.StatusPartialContent
		contentLength = fileRange.Length()
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", fileRange.Start, fileRange.End, r.Blob.Size))
	}
	c.Header("Content-Disposition", contentDisposition(r.File.Name))
	c.Header("Accept-Ranges", "bytes")
	c.DataFromReader(status, contentLength, r.Blob.MimeType, reader, nil)
}

// shareFilePreview GET /api/v1/shares/:id/files/:fid/preview：私有分享内联预览（登录用户）。
// view 与 download 权限均可预览，不消耗分享 download_count。
func (h *Handler) shareFilePreview(c *gin.Context) {
	shareID, fileID, ok := h.parseShareFileParams(c)
	if !ok {
		return
	}
	r, err := h.shares.ResolveForUserForPreview(shareID, fileID, userID(c))
	if privateShareError(c, err) {
		return
	}
	h.servePreviewBlob(c, r.File.Name, r.Blob, func() error {
		return h.shares.IncrementPreviewView(r)
	})
}
