package http

import (
	"github.com/docflow/docflow/internal/audit"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"io"
	"strconv"
)

type auditQuerySource interface {
	Query(audit.Query) (audit.Result, error)
	Export(io.Writer, audit.Query) error
}

func (h *Handler) SetAuditQuerySource(s auditQuerySource) { h.auditQuery = s }
func (h *Handler) adminAudit(c *gin.Context) {
	if h.auditQuery == nil {
		c.JSON(503, gin.H{"error": "audit query not configured"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit < 1 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	var uid *uuid.UUID
	if raw := c.Query("user_id"); raw != "" {
		id, e := uuid.Parse(raw)
		if e != nil {
			c.JSON(400, gin.H{"error": "invalid user_id"})
			return
		}
		uid = &id
	}
	cur, _ := strconv.ParseInt(c.Query("cursor"), 10, 64)
	r, e := h.auditQuery.Query(audit.Query{Limit: limit, Cursor: cur, Action: c.Query("action"), UserID: uid})
	if e != nil {
		c.JSON(500, gin.H{"error": "unable to query audit logs"})
		return
	}
	c.JSON(200, r)
}
func (h *Handler) adminAuditCSV(c *gin.Context) {
	if h.auditQuery == nil {
		c.JSON(503, gin.H{"error": "audit query not configured"})
		return
	}
	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", "attachment; filename=\"audit-logs.csv\"")
	if e := h.auditQuery.Export(c.Writer, audit.Query{Action: c.Query("action")}); e != nil {
		return
	}
}
