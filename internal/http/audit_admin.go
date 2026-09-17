package http

import (
	"github.com/docflow/docflow/internal/audit"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"io"
	"strconv"
	"time"
)

type auditQuerySource interface {
	Query(audit.Query) (audit.Result, error)
	Export(io.Writer, audit.Query) error
}

func (h *Handler) SetAuditQuerySource(s auditQuerySource) { h.auditQuery = s }
func parseAuditQuery(c *gin.Context) (audit.Query, error) {
	q := audit.Query{Action: c.Query("action"), Status: c.Query("status"), ResourceType: c.Query("resource_type"), ResourceID: c.Query("resource_id")}
	if raw := c.Query("user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return q, err
		}
		q.UserID = &id
	}
	for raw, dest := range map[string]**time.Time{"from": &q.From, "to": &q.To} {
		if value := c.Query(raw); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return q, err
			}
			*dest = &parsed
		}
	}
	return q, nil
}

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
	q, err := parseAuditQuery(c)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid audit filter"})
		return
	}
	q.Limit = limit
	q.Cursor, _ = strconv.ParseInt(c.Query("cursor"), 10, 64)
	r, e := h.auditQuery.Query(q)
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
	q, err := parseAuditQuery(c)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid audit filter"})
		return
	}
	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", "attachment; filename=\"audit-logs.csv\"")
	if e := h.auditQuery.Export(c.Writer, q); e != nil {
		return
	}
}
