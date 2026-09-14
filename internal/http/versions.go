package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// versionJSON 组装版本条目（含 blob status/size/sha256/mime）。
func versionJSON(v files.VersionDetail) gin.H {
	return gin.H{"id": v.ID, "version": v.Version, "size": v.Size, "sha256": v.SHA256, "mime_type": v.MimeType, "status": v.Status, "comment": v.Comment, "user_id": v.UserID, "created_at": v.CreatedAt}
}

// listFileVersions GET /api/v1/files/:id/versions 版本列表（按版本号倒序）。
// 读权限同文件元数据：个人文件 owner、团队文件任意在册成员。
func (h *Handler) listFileVersions(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	versions, err := h.files.ListVersions(userID(c), id)
	if h.fileError(c, err) {
		return
	}
	out := make([]gin.H, 0, len(versions))
	for _, v := range versions {
		out = append(out, versionJSON(v))
	}
	c.JSON(http.StatusOK, gin.H{"versions": out})
}

// versionContentReader 抽象版本内容读取（生产实现 *files.Store.ReadVersion；
// 接口化便于单测注入内存实现——files.Store 为 gorm 具体类型，见
// preview_test.go 同一取舍说明）。
type versionContentReader interface {
	ReadVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, files.ObjectBlob, error)
}

var _ versionContentReader = (*files.Store)(nil)

// fileVersionContent GET /api/v1/files/:id/versions/:versionId/content：
// 版本原始内容（文本版本对比用）。读权限同版本列表（个人 owner、团队
// 任意在册成员）；blob 须 available（quarantined 等回 403）。响应为
// blob.mime + inline disposition + nosniff，不支持 Range（整读）。
func (h *Handler) fileVersionContent(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	versionID, ok := parseID(c, c.Param("versionId"))
	if !ok {
		return
	}
	if h.versionReader == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "version content is not available"})
		return
	}
	f, _, blob, err := h.versionReader.ReadVersion(userID(c), id, versionID)
	if h.fileError(c, err) {
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusForbidden, gin.H{"error": "file is not available", "status": blob.Status})
		return
	}
	reader, err := upload.ReadSection(h.storage, blob.StorageKey, 0, blob.Size)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file"})
		return
	}
	defer reader.Close()
	c.Header("Content-Disposition", inlineDisposition(f.Name))
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, blob.Size, blob.MimeType, reader, nil)
}

func (h *Handler) deleteFileVersion(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	versionID, ok := parseID(c, c.Param("versionId"))
	if !ok {
		return
	}
	err := h.files.DeleteVersion(userID(c), id, versionID)
	if errors.Is(err, files.ErrCurrentVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "current version cannot be deleted"})
		return
	}
	if errors.Is(err, files.ErrNotFileVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "version does not belong to this file", "code": "NOT_FILE_VERSION"})
		return
	}
	if h.fileError(c, err) {
		return
	}
	c.Status(http.StatusNoContent)
}

// restoreFileVersion POST /api/v1/files/:id/versions/:versionId/restore
// 将 current_version 指向既有版本（回滚，版本内容不可变）。
// 权限：个人文件 owner、团队文件 CanWrite；仅允许指向该文件的版本。
func (h *Handler) restoreFileVersion(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	versionID, ok := parseID(c, c.Param("versionId"))
	if !ok {
		return
	}
	user := userID(c)
	f, version, err := h.files.SetCurrentVersion(user, id, versionID)
	if err != nil {
		switch {
		case errors.Is(err, files.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "file or version not found"})
		case errors.Is(err, files.ErrNotFileVersion):
			c.JSON(http.StatusConflict, gin.H{"error": "version does not belong to this file", "code": "NOT_FILE_VERSION"})
		default:
			h.fileError(c, err)
		}
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &user, Action: audit.ActionVersionRestore, ResourceType: audit.ResourceFile, ResourceID: id.String(), Status: audit.StatusSuccess, Metadata: `{"version_id":"` + versionID.String() + `","version":` + strconv.Itoa(version.Version) + `}`})
	out := fileJSON(f)
	out["current_version"] = gin.H{"id": version.ID, "version": version.Version, "size": version.Size, "sha256": version.ContentSHA256, "user_id": version.UserID, "created_at": version.CreatedAt}
	setETag(c, f)
	c.JSON(http.StatusOK, out)
}
