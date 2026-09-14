package http

import (
	"errors"
	"net/http"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/tagging"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// tagError 统一映射标签服务错误到 HTTP 状态码；文件侧错误（读授权等）
// 回退 fileError 映射。
func (h *Handler) tagError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, tagging.ErrInvalidName):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tag name"})
	case errors.Is(err, tagging.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "tag not found"})
	case errors.Is(err, tagging.ErrConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "tag already exists"})
	default:
		if !h.fileError(c, err) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "tag operation failed"})
		}
	}
	return true
}

func tagJSON(t tagging.Tag) gin.H {
	return gin.H{"id": t.ID, "name": t.Name, "created_at": t.CreatedAt}
}

// requireTagging 标签服务未注入时 503（生产恒注入；契约测试注入内存实现）。
func (h *Handler) requireTagging(c *gin.Context) bool {
	if h.tags == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tagging service not configured"})
		return false
	}
	return true
}

// listTags GET /api/v1/tags：当前用户标签列表（按名称排序）。
func (h *Handler) listTags(c *gin.Context) {
	if !h.requireTagging(c) {
		return
	}
	tags, err := h.tags.ListTags(userID(c))
	if h.tagError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(tags))
	for _, t := range tags {
		items = append(items, tagJSON(t))
	}
	c.JSON(http.StatusOK, gin.H{"tags": items})
}

type createTagRequest struct {
	Name string `json:"name"`
}

// createTag POST /api/v1/tags {name}：创建标签（NFC/≤64 rune/无控制字符）。
func (h *Handler) createTag(c *gin.Context) {
	if !h.requireTagging(c) {
		return
	}
	var req createTagRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	t, err := h.tags.CreateTag(userID(c), req.Name)
	if h.tagError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, tagJSON(t))
}

// deleteTag DELETE /api/v1/tags/:id：删除自己的标签并解除全部关联（级联）。
func (h *Handler) deleteTag(c *gin.Context) {
	if !h.requireTagging(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if h.tagError(c, h.tags.DeleteTag(userID(c), id)) {
		return
	}
	c.Status(http.StatusNoContent)
}

// listFileTags GET /api/v1/files/:id/tags：文件上属于当前用户的标签。
func (h *Handler) listFileTags(c *gin.Context) {
	if !h.requireTagging(c) {
		return
	}
	fileID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	tags, err := h.tags.ListFileTags(userID(c), fileID)
	if h.tagError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(tags))
	for _, t := range tags {
		items = append(items, tagJSON(t))
	}
	c.JSON(http.StatusOK, gin.H{"tags": items})
}

type addFileTagRequest struct {
	TagID string `json:"tag_id"`
}

// addFileTag POST /api/v1/files/:id/tags {tag_id}：把标签打到文件上。
// 权限：标签须属于操作者；文件读权限即可（标签为用户维度组织视图）。
// 幂等：重复打同一标签仍返回 201。
func (h *Handler) addFileTag(c *gin.Context) {
	if !h.requireTagging(c) {
		return
	}
	fileID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req addFileTagRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	tagID, ok := parseID(c, req.TagID)
	if !ok {
		return
	}
	ft, err := h.tags.AddFileTag(userID(c), fileID, tagID)
	if h.tagError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, gin.H{"tag_id": ft.TagID, "file_id": ft.FileID})
}

// removeFileTag DELETE /api/v1/files/:id/tags/:tagId：解除关联（幂等）。
func (h *Handler) removeFileTag(c *gin.Context) {
	if !h.requireTagging(c) {
		return
	}
	fileID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	tagID, ok := parseID(c, c.Param("tagId"))
	if !ok {
		return
	}
	if h.tagError(c, h.tags.RemoveFileTag(userID(c), fileID, tagID)) {
		return
	}
	c.Status(http.StatusNoContent)
}

type starredRequest struct {
	Starred *bool `json:"starred"`
}

// setFileStarred PATCH /api/v1/files/:id/starred {starred}：切换收藏。
// 权限取舍：读权限即可（is_starred 为行级共享标记，团队文件全队共享星标）。
func (h *Handler) setFileStarred(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req starredRequest
	if c.ShouldBindJSON(&req) != nil || req.Starred == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	f, err := h.files.SetStarred(userID(c), id, *req.Starred)
	if h.fileError(c, err) {
		return
	}
	setETag(c, f)
	c.JSON(http.StatusOK, fileJSON(f))
}

// parseTagFilter 解析 GET /files 与 /teams/:id/files 的 tag_id 查询参数：
// 提供时校验标签归属（他人的标签按 404 处理，不泄露存在性）。
func (h *Handler) parseTagFilter(c *gin.Context) (*uuid.UUID, bool) {
	raw := c.Query("tag_id")
	if raw == "" {
		return nil, true
	}
	id, ok := parseID(c, raw)
	if !ok {
		return nil, false
	}
	if h.tags != nil {
		if _, err := h.tags.GetOwnedTag(userID(c), id); err != nil {
			if errors.Is(err, tagging.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "tag not found"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to query tag"})
			}
			return nil, false
		}
	}
	return &id, true
}

// parseStarredFilter 解析 starred 查询参数（"true"/"false"，其余 400）。
func parseStarredFilter(c *gin.Context) (*bool, bool) {
	raw := c.Query("starred")
	if raw == "" {
		return nil, true
	}
	var v bool
	switch raw {
	case "true":
		v = true
	case "false":
		v = false
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid starred filter"})
		return nil, false
	}
	return &v, true
}

// parseSortQuery 解析 sort/order 白名单（name|updated_at|size × asc|desc）。
func parseSortQuery(c *gin.Context) (files.SortOptions, bool) {
	sortOpt := files.SortOptions{Sort: c.Query("sort"), Order: c.Query("order")}
	if sortOpt.Sort == "" {
		sortOpt.Sort = "name"
	}
	if _, ok := files.SortClause(sortOpt.Sort, sortOpt.Order); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sort or order"})
		return files.SortOptions{}, false
	}
	return sortOpt, true
}
