package http

import (
	"errors"
	"net/http"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type snapshotCreateRequest struct {
	Name string `json:"name"`
}

func snapshotJSON(s files.DirectorySnapshot) gin.H {
	return gin.H{"id": s.ID, "root_id": s.RootID, "space_id": s.SpaceID, "name": s.Name, "creator_id": s.CreatorID, "created_at": s.CreatedAt, "entries": s.Entries}
}
func (h *Handler) createSnapshot(c *gin.Context) {
	root, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req snapshotCreateRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	snap, err := h.files.CreateSnapshot(actor, root, req.Name)
	if h.fileError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: "snapshot.create", ResourceType: audit.ResourceFolder, ResourceID: root.String()})
	c.JSON(http.StatusCreated, snapshotJSON(snap))
}
func (h *Handler) listSnapshots(c *gin.Context) {
	root, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	out, err := h.files.ListSnapshots(userID(c), root)
	if h.fileError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(out))
	for _, s := range out {
		items = append(items, snapshotJSON(s))
	}
	c.JSON(http.StatusOK, gin.H{"snapshots": items})
}
func (h *Handler) getSnapshot(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	s, err := h.files.GetSnapshot(userID(c), id)
	if errors.Is(err, files.ErrSnapshotNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "snapshot not found"})
		return
	}
	if h.fileError(c, err) {
		return
	}
	c.JSON(http.StatusOK, snapshotJSON(s))
}
func (h *Handler) diffSnapshot(c *gin.Context) {
	root, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	sid, err := uuid.Parse(c.Query("snapshot_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid snapshot_id"})
		return
	}
	s, err := h.files.GetSnapshot(userID(c), sid)
	if err != nil || s.RootID != root {
		c.JSON(http.StatusNotFound, gin.H{"error": "snapshot not found"})
		return
	}
	d, err := h.files.DiffSnapshot(userID(c), sid)
	if h.fileError(c, err) {
		return
	}
	c.JSON(http.StatusOK, d)
}
func (h *Handler) restoreSnapshot(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	result, err := h.files.RestoreSnapshotVersionsOnly(actor, id)
	if errors.Is(err, files.ErrSnapshotVersionMissing) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "SNAPSHOT_VERSION_MISSING"})
		return
	}
	if h.fileError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: "snapshot.restore", ResourceType: audit.ResourceFolder, ResourceID: id.String()})
	c.JSON(http.StatusOK, result)
}
