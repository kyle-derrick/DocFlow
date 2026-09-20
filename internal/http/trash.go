package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
)

// listTrash GET /api/v1/trash?space= 列出空间软删除文件（统一空间模型；
// space 缺省=用户默认空间；limit 简单分页）。
func (h *Handler) listTrash(c *gin.Context) {
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
	user := userID(c)
	spaceID, ok := h.resolveListSpace(c, user)
	if !ok {
		return
	}
	out, err := h.files.ListTrashSpace(user, spaceID, limit)
	if err != nil {
		if errors.Is(err, files.ErrForbidden) {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		if errors.Is(err, files.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "space not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list trash"})
		return
	}
	result := make([]gin.H, 0, len(out))
	for _, f := range out {
		item := fileJSON(f)
		item["deleted_at"] = f.DeletedAt
		result = append(result, item)
	}
	c.JSON(http.StatusOK, gin.H{"files": result})
}

// restoreFile POST /api/v1/files/:id/restore 恢复软删除文件。
// 原父目录被删除或名称冲突时返回 409 并带错误码，不静默改名。
func (h *Handler) restoreFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	f, err := h.files.Restore(userID(c), id)
	if err != nil {
		switch {
		case errors.Is(err, files.ErrParentDeleted):
			c.JSON(http.StatusConflict, gin.H{"error": "parent folder is deleted", "code": "PARENT_DELETED"})
		case errors.Is(err, files.ErrConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "name conflict", "code": "NAME_CONFLICT"})
		case errors.Is(err, files.ErrNotDeleted):
			c.JSON(http.StatusConflict, gin.H{"error": "file is not deleted", "code": "NOT_DELETED"})
		case errors.Is(err, files.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		case errors.Is(err, files.ErrRoot):
			c.JSON(http.StatusForbidden, gin.H{"error": "root folder cannot be changed"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "restore failed"})
		}
		return
	}
	c.JSON(http.StatusOK, fileJSON(f))
}

// purgeFile DELETE /api/v1/trash/:id 彻底删除（硬删除）软删除文件。
// 仅软删除文件可彻底删除；根目录不可删除。
func (h *Handler) purgeFile(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	owner := userID(c)
	purged, deleting, err := h.files.Purge(owner, id)
	if err != nil {
		switch {
		case errors.Is(err, files.ErrNotDeleted):
			c.JSON(http.StatusConflict, gin.H{"error": "file is not deleted", "code": "NOT_DELETED"})
		case errors.Is(err, files.ErrRoot):
			c.JSON(http.StatusForbidden, gin.H{"error": "root folder cannot be deleted"})
		case errors.Is(err, files.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "purge failed"})
		}
		return
	}
	// 事务已提交：删除引用计数归零的物理对象（失败不回滚数据库硬删除，blob 记录保留待清理任务重试）。
	_ = h.files.PurgeBlobs(deleting, h.storage.Delete)
	h.recordAudit(c, audit.Entry{UserID: &owner, Action: audit.ActionPurge, ResourceType: audit.ResourceFile, ResourceID: id.String(), Status: audit.StatusSuccess, Metadata: `{"purged":` + strconv.Itoa(len(purged)) + `}`})
	c.Status(http.StatusNoContent)
}
