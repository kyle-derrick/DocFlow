package http

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/officetemplate"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type officeTemplateRequest struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

func (h *Handler) createOfficeTemplate(c *gin.Context) {
	var req officeTemplateRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	data, defaultName, err := officetemplate.Build(kind)
	if errors.Is(err, officetemplate.ErrInvalidKind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to build template"})
		return
	}
	ext, _ := officetemplate.Extension(kind)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultName
	} else if !strings.EqualFold(filepath.Ext(name), ext) {
		name += ext
	}

	owner := userID(c)
	parent := uuid.Nil
	if req.ParentID == "" {
		rootID, rootErr := h.defaultSpaceRootID(owner)
		if rootErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to ensure default space root"})
			return
		}
		parent = rootID
	} else {
		var ok bool
		parent, ok = parseID(c, req.ParentID)
		if !ok {
			return
		}
	}

	session, err := h.uploads.UploadBytes(owner, parent, name, data)
	if err != nil {
		uploadStartError(c, err, false)
		return
	}
	f, err := h.files.Get(owner, session.FileID)
	if err != nil {
		if errors.Is(err, files.ErrForbidden) {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load created file"})
		return
	}
	c.JSON(http.StatusCreated, fileJSON(f))
}
