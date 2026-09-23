package http

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type documentComment struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	FileID     uuid.UUID  `gorm:"type:uuid" json:"file_id"`
	VersionID  uuid.UUID  `gorm:"type:uuid" json:"version_id"`
	ParentID   *uuid.UUID `gorm:"type:uuid" json:"parent_id"`
	AnchorFrom int        `json:"anchor_from"`
	AnchorTo   int        `json:"anchor_to"`
	Quote      string     `json:"quote"`
	Body       string     `json:"body"`
	AuthorID   uuid.UUID  `gorm:"type:uuid" json:"author_id"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (documentComment) TableName() string { return "document_comments" }

func (h *Handler) SetCommentsDB(db *gorm.DB) { h.commentsDB = db }

func (h *Handler) commentFile(c *gin.Context, id uuid.UUID, write bool) (files.File, bool) {
	if h.commentsDB == nil || h.files == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "comments unavailable"})
		return files.File{}, false
	}
	var f files.File
	var err error
	if write {
		f, err = h.files.ValidateReplaceTarget(userID(c), id)
	} else {
		f, err = h.files.Get(userID(c), id)
	}
	if h.fileError(c, err) {
		return files.File{}, false
	}
	if f.Type != "file" || !(strings.HasSuffix(strings.ToLower(f.Name), ".dfrt") || strings.HasSuffix(strings.ToLower(f.Name), ".dfdoc")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a rich text document"})
		return files.File{}, false
	}
	return f, true
}

func validCommentBody(body string) bool {
	return strings.TrimSpace(body) != "" && utf8.RuneCountInString(body) <= 4000
}
func validAnchor(from, to int, quote string) bool {
	return from >= 0 && to > from && to <= 1000000 && utf8.RuneCountInString(quote) <= 1000
}

func (h *Handler) listComments(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if _, ok = h.commentFile(c, id, false); !ok {
		return
	}
	var items []documentComment
	if err := h.commentsDB.Where("file_id = ?", id).Order("created_at, id").Find(&items).Error; err != nil {
		c.JSON(500, gin.H{"error": "unable to list comments"})
		return
	}
	if items == nil {
		items = []documentComment{}
	}
	c.JSON(200, gin.H{"comments": items})
}

type createCommentRequest struct {
	VersionID  string `json:"version_id"`
	AnchorFrom int    `json:"anchor_from"`
	AnchorTo   int    `json:"anchor_to"`
	Quote      string `json:"quote"`
	Body       string `json:"body"`
}

func (h *Handler) createComment(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	f, ok := h.commentFile(c, id, true)
	if !ok {
		return
	}
	var req createCommentRequest
	if c.ShouldBindJSON(&req) != nil || !validCommentBody(req.Body) || !validAnchor(req.AnchorFrom, req.AnchorTo, req.Quote) {
		c.JSON(400, gin.H{"error": "invalid comment"})
		return
	}
	versionID, err := uuid.Parse(req.VersionID)
	if err != nil || f.CurrentVersionID == nil || versionID != *f.CurrentVersionID {
		c.JSON(409, gin.H{"error": "version is not current"})
		return
	}
	item := documentComment{ID: uuid.New(), FileID: id, VersionID: versionID, AnchorFrom: req.AnchorFrom, AnchorTo: req.AnchorTo, Quote: req.Quote, Body: strings.TrimSpace(req.Body), AuthorID: userID(c), Status: "open"}
	if err := h.commentsDB.Create(&item).Error; err != nil {
		c.JSON(500, gin.H{"error": "unable to create comment"})
		return
	}
	c.JSON(201, item)
}

func (h *Handler) findComment(c *gin.Context, write bool) (documentComment, bool) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return documentComment{}, false
	}
	if h.commentsDB == nil {
		c.JSON(503, gin.H{"error": "comments unavailable"})
		return documentComment{}, false
	}
	var item documentComment
	if err := h.commentsDB.First(&item, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(404, gin.H{"error": "comment not found"})
		} else {
			c.JSON(500, gin.H{"error": "unable to read comment"})
		}
		return documentComment{}, false
	}
	if _, ok := h.commentFile(c, item.FileID, write); !ok {
		return documentComment{}, false
	}
	return item, true
}

func (h *Handler) replyComment(c *gin.Context) {
	parent, ok := h.findComment(c, true)
	if !ok {
		return
	}
	if parent.ParentID != nil {
		c.JSON(400, gin.H{"error": "cannot reply to a reply"})
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if c.ShouldBindJSON(&req) != nil || !validCommentBody(req.Body) {
		c.JSON(400, gin.H{"error": "invalid reply"})
		return
	}
	item := documentComment{ID: uuid.New(), FileID: parent.FileID, VersionID: parent.VersionID, ParentID: &parent.ID, AnchorFrom: parent.AnchorFrom, AnchorTo: parent.AnchorTo, Quote: "", Body: strings.TrimSpace(req.Body), AuthorID: userID(c), Status: "open"}
	if err := h.commentsDB.Create(&item).Error; err != nil {
		c.JSON(500, gin.H{"error": "unable to create reply"})
		return
	}
	c.JSON(201, item)
}

func (h *Handler) patchComment(c *gin.Context) {
	item, ok := h.findComment(c, true)
	if !ok {
		return
	}
	var req struct {
		Body   *string `json:"body"`
		Status *string `json:"status"`
	}
	if c.ShouldBindJSON(&req) != nil || (req.Body == nil && req.Status == nil) || (req.Body != nil && !validCommentBody(*req.Body)) || (req.Status != nil && (*req.Status != "open" && *req.Status != "resolved" || item.ParentID != nil)) {
		c.JSON(400, gin.H{"error": "invalid update"})
		return
	}
	if req.Body != nil && item.AuthorID != userID(c) {
		c.JSON(403, gin.H{"error": "only author may edit body"})
		return
	}
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if req.Body != nil {
		updates["body"] = strings.TrimSpace(*req.Body)
	}
	if req.Status != nil {
		updates["status"] = *req.Status
	}
	if err := h.commentsDB.Model(&documentComment{}).Where("id = ?", item.ID).Updates(updates).Error; err != nil {
		c.JSON(500, gin.H{"error": "unable to update comment"})
		return
	}
	if err := h.commentsDB.First(&item, "id = ?", item.ID).Error; err != nil {
		c.JSON(500, gin.H{"error": "unable to read comment"})
		return
	}
	c.JSON(200, item)
}

func (h *Handler) deleteComment(c *gin.Context) {
	item, ok := h.findComment(c, true)
	if !ok {
		return
	}
	if item.AuthorID != userID(c) {
		c.JSON(403, gin.H{"error": "only author may delete comment"})
		return
	}
	if err := h.commentsDB.Delete(&documentComment{}, "id = ?", item.ID).Error; err != nil {
		c.JSON(500, gin.H{"error": "unable to delete comment"})
		return
	}
	c.Status(204)
}
