package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
)

// openWithStore 抽象「默认打开方式」偏好的存取（生产实现 *auth.UserStore，
// NewHandler 装配；接口化便于单测注入内存实现）。
type openWithStore interface {
	ListOpenWith(userID uuid.UUID) ([]auth.OpenWithPreference, error)
	SetOpenWith(userID uuid.UUID, ext, opener string) (auth.OpenWithPreference, error)
	DeleteOpenWith(userID uuid.UUID, ext string) error
}

var _ openWithStore = (*auth.UserStore)(nil)

// listOpenWith GET /api/v1/me/open-with：当前用户的全部打开方式偏好
// （按 ext 排序；无记录返回空列表）。
func (h *Handler) listOpenWith(c *gin.Context) {
	if h.openWith == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "open-with preferences are not configured"})
		return
	}
	rows, err := h.openWith.ListOpenWith(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list open-with preferences"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"open_with": rows})
}

// updateOpenWith PUT /api/v1/me/open-with {ext, opener}：upsert 一条偏好。
// ext 规范化（小写、去点、白名单字符 [a-z0-9]、1..16 位）；opener 枚举校验
// （office/drawio/excalidraw/text/markdown/code/web/default）；非法 400。
func (h *Handler) updateOpenWith(c *gin.Context) {
	if h.openWith == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "open-with preferences are not configured"})
		return
	}
	var req struct {
		Ext    string `json:"ext"`
		Opener string `json:"opener"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	ext, err := auth.NormalizeOpenWithExt(req.Ext)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := auth.ValidateOpenWithOpener(req.Opener); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved, err := h.openWith.SetOpenWith(userID(c), ext, req.Opener)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save open-with preference"})
		return
	}
	c.JSON(http.StatusOK, saved)
}

// deleteOpenWith DELETE /api/v1/me/open-with?ext=：删除一条偏好（幂等）。
func (h *Handler) deleteOpenWith(c *gin.Context) {
	if h.openWith == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "open-with preferences are not configured"})
		return
	}
	ext, err := auth.NormalizeOpenWithExt(c.Query("ext"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.openWith.DeleteOpenWith(userID(c), ext); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to delete open-with preference"})
		return
	}
	c.Status(http.StatusNoContent)
}
