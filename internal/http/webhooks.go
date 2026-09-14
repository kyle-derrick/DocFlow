package http

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/webhook"
	"github.com/gin-gonic/gin"
)

// 本文件实现 Webhook 通知渠道端点（v1.1，本人维度）：
//   POST   /api/v1/webhooks      注册（一次性 secret 仅本次返回）
//   GET    /api/v1/webhooks      列表（含 last_status/failure_count/enabled）
//   PATCH  /api/v1/webhooks/:id  启用/停用
//   DELETE /api/v1/webhooks/:id  删除

// SetWebhooks 注入 webhook 服务（幂等）；未注入时 webhook 端点 503
// （生产恒注入；契约测试只注册路由不触发依赖）。
func (h *Handler) SetWebhooks(svc *webhook.Service) {
	if svc != nil {
		h.webhooks = svc
	}
}

// requireWebhooks webhook 服务未注入时 503。
func (h *Handler) requireWebhooks(c *gin.Context) bool {
	if h.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook service not configured"})
		return false
	}
	return true
}

// webhookJSON 渲染 webhook 行（不含 secret：明文仅创建响应返回一次）。
func webhookJSON(w webhook.Webhook) gin.H {
	return gin.H{
		"id":                w.ID,
		"url":               w.URL,
		"events":            ([]string)(w.Events),
		"enabled":           w.Enabled,
		"failure_count":     w.FailureCount,
		"last_status":       w.LastStatus,
		"last_delivered_at": w.LastDeliveredAt,
		"created_at":        w.CreatedAt,
		"updated_at":        w.UpdatedAt,
	}
}

type createWebhookRequest struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// webhookAuditMetadata 组装 webhook 审计 metadata（JSON 序列化防注入，
// URL 含引号/反斜杠时仍为合法 JSON）。
func webhookAuditMetadata(url string, events []string) string {
	raw, err := json.Marshal(map[string]any{"url": url, "events": events})
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// createWebhook POST /api/v1/webhooks {url, events[]}：注册 webhook。
// 一次性签名 secret（whsec_ + 43 字符）仅在 201 响应返回一次；URL 须为
// http(s) 且无 userinfo（localhost 允许），events 为白名单事件类型的
// 非空子集。同一用户重复注册同一 URL 返回 409。写 webhook.create 审计。
func (h *Handler) createWebhook(c *gin.Context) {
	if !h.requireWebhooks(c) {
		return
	}
	var request createWebhookRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	uid := userID(c)
	hook, secret, err := h.webhooks.Create(uid, request.URL, request.Events)
	switch {
	case err == nil:
	case errors.Is(err, webhook.ErrInvalidURL):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook url"})
		return
	case errors.Is(err, webhook.ErrInvalidEvents):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook events"})
		return
	case errors.Is(err, webhook.ErrDuplicateURL):
		c.JSON(http.StatusConflict, gin.H{"error": "webhook url already registered"})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create webhook"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionWebhookCreate, ResourceType: audit.ResourceWebhook, ResourceID: hook.ID.String(), Metadata: webhookAuditMetadata(hook.URL, hook.Events)})
	body := webhookJSON(hook)
	body["secret"] = secret
	c.JSON(http.StatusCreated, body)
}

// listWebhooks GET /api/v1/webhooks：当前用户的 webhook 列表（created_at
// 倒序，含已禁用与投递状态；不含 secret）。
func (h *Handler) listWebhooks(c *gin.Context) {
	if !h.requireWebhooks(c) {
		return
	}
	hooks, err := h.webhooks.List(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list webhooks"})
		return
	}
	out := make([]gin.H, 0, len(hooks))
	for _, w := range hooks {
		out = append(out, webhookJSON(w))
	}
	c.JSON(http.StatusOK, gin.H{"webhooks": out})
}

type updateWebhookRequest struct {
	Enabled *bool `json:"enabled"`
}

// updateWebhook PATCH /api/v1/webhooks/:id {enabled}：启停本人的 webhook。
// 不存在或不属于当前用户一律 404（不泄露存在性）。
func (h *Handler) updateWebhook(c *gin.Context) {
	if !h.requireWebhooks(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var request updateWebhookRequest
	if c.ShouldBindJSON(&request) != nil || request.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	hook, err := h.webhooks.Update(userID(c), id, *request.Enabled)
	if errors.Is(err, webhook.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update webhook"})
		return
	}
	c.JSON(http.StatusOK, webhookJSON(hook))
}

// deleteWebhook DELETE /api/v1/webhooks/:id：删除本人的 webhook（幂等语义
// 同撤销：不存在/非属主一律 404）。写 webhook.delete 审计。
func (h *Handler) deleteWebhook(c *gin.Context) {
	if !h.requireWebhooks(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	uid := userID(c)
	if err := h.webhooks.Delete(uid, id); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to delete webhook"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionWebhookDelete, ResourceType: audit.ResourceWebhook, ResourceID: id.String()})
	c.Status(http.StatusNoContent)
}
