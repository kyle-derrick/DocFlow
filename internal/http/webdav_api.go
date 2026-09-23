package http

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type webDAVTokenDTO struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (h *Handler) webdavTokensList(c *gin.Context) {
	if h.webdavTokens == nil {
		c.JSON(503, gin.H{"error": "webdav unavailable"})
		return
	}
	xs, e := h.webdavTokens.List(userID(c))
	if e != nil {
		c.JSON(500, gin.H{"error": "unable to list tokens"})
		return
	}
	out := make([]webDAVTokenDTO, 0, len(xs))
	for _, t := range xs {
		out = append(out, webDAVTokenDTO{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt})
	}
	c.JSON(200, out)
}
func (h *Handler) webdavTokenCreate(c *gin.Context) {
	if h.webdavTokens == nil {
		c.JSON(503, gin.H{"error": "webdav unavailable"})
		return
	}
	var in struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if c.ShouldBindJSON(&in) != nil || strings.TrimSpace(in.Name) == "" || in.ExpiresInDays < 0 || in.ExpiresInDays > 3650 {
		c.JSON(400, gin.H{"error": "invalid request"})
		return
	}
	t, raw, e := h.webdavTokens.Create(userID(c), in.Name, in.ExpiresInDays)
	if e != nil {
		c.JSON(400, gin.H{"error": e.Error()})
		return
	}
	c.JSON(201, gin.H{"id": t.ID, "name": t.Name, "token": raw, "expires_at": t.ExpiresAt, "created_at": t.CreatedAt})
}
func (h *Handler) webdavTokenRevoke(c *gin.Context) {
	id, e := uuid.Parse(c.Param("id"))
	if e != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	if h.webdavTokens == nil {
		c.JSON(404, gin.H{"error": "token not found"})
		return
	}
	if e = h.webdavTokens.Revoke(userID(c), id); e != nil {
		c.JSON(404, gin.H{"error": "token not found"})
		return
	}
	c.Status(204)
}
