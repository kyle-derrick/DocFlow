package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
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
