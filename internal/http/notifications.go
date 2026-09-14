package http

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/docflow/docflow/internal/notify"
	"github.com/gin-gonic/gin"
)

// 本文件实现站内通知与通知偏好端点（本人维度）：
//   GET  /api/v1/notifications?unread_only&limit&cursor  通知列表（分页+未读数）
//   POST /api/v1/notifications/:id/read                  标记单条已读
//   POST /api/v1/notifications/read-all                  全部已读
//   GET  /api/v1/notification-preferences                各事件类型开关（无记录=默认）
//   PUT  /api/v1/notification-preferences/:type          更新单个开关

// SetNotifications 注入通知服务（幂等）；未注入时通知端点 503
// （生产恒注入；契约测试只注册路由不触发依赖）。
func (h *Handler) SetNotifications(svc *notify.Service) {
	if svc != nil {
		h.notifications = svc
	}
}

// requireNotifications 通知服务未注入时 503。
func (h *Handler) requireNotifications(c *gin.Context) bool {
	if h.notifications == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "notification service not configured"})
		return false
	}
	return true
}

func notificationJSON(n notify.Notification) gin.H {
	return gin.H{
		"id":          n.ID,
		"type":        n.Type,
		"title":       n.Title,
		"body":        n.Body,
		"resource_id": n.ResourceID,
		"is_read":     n.IsRead,
		"created_at":  n.CreatedAt,
		"read_at":     n.ReadAt,
	}
}

// listNotifications GET /api/v1/notifications：当前用户通知列表
// （created_at 倒序，created_at 游标分页），响应恒附 unread_count（铃铛徽标）。
func (h *Handler) listNotifications(c *gin.Context) {
	if !h.requireNotifications(c) {
		return
	}
	unreadOnly := false
	if raw := c.Query("unread_only"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unread_only"})
			return
		}
		unreadOnly = v
	}
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < 100 {
			limit = n
		} else {
			limit = 100
		}
	}
	cursor := c.Query("cursor")
	user := userID(c)
	items, err := h.notifications.List(user, unreadOnly, limit, cursor)
	if err != nil {
		if errors.Is(err, notify.ErrInvalidCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cursor"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list notifications"})
		return
	}
	unread, err := h.notifications.CountUnread(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to count unread notifications"})
		return
	}
	next := ""
	if len(items) == limit {
		next = items[len(items)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	out := make([]gin.H, 0, len(items))
	for _, n := range items {
		out = append(out, notificationJSON(n))
	}
	c.JSON(http.StatusOK, gin.H{"items": out, "next_cursor": next, "unread_count": unread})
}

// markNotificationRead POST /api/v1/notifications/:id/read：标记自己的通知为
// 已读（幂等）。通知不存在或不属于当前用户一律 404（不泄露存在性）。
func (h *Handler) markNotificationRead(c *gin.Context) {
	if !h.requireNotifications(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	found, err := h.notifications.MarkRead(userID(c), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to mark notification"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "notification not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// markAllNotificationsRead POST /api/v1/notifications/read-all：当前用户全部
// 未读通知标记已读。
func (h *Handler) markAllNotificationsRead(c *gin.Context) {
	if !h.requireNotifications(c) {
		return
	}
	if _, err := h.notifications.MarkAllRead(userID(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to mark notifications"})
		return
	}
	c.Status(http.StatusNoContent)
}

// listNotificationPreferences GET /api/v1/notification-preferences：返回全部
// 事件类型的生效开关（无偏好记录 = 默认开启）。
func (h *Handler) listNotificationPreferences(c *gin.Context) {
	if !h.requireNotifications(c) {
		return
	}
	user := userID(c)
	out := make([]gin.H, 0, len(notify.EventTypes))
	for _, t := range notify.EventTypes {
		enabled, err := h.notifications.PreferenceEnabled(user, t)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list preferences"})
			return
		}
		out = append(out, gin.H{"event_type": t, "enabled": enabled})
	}
	c.JSON(http.StatusOK, gin.H{"preferences": out})
}

type preferenceRequest struct {
	Enabled *bool `json:"enabled"`
}

// updateNotificationPreference PUT /api/v1/notification-preferences/:type
// {enabled}：upsert 当前用户对事件类型的开关；未知事件类型 400。
func (h *Handler) updateNotificationPreference(c *gin.Context) {
	if !h.requireNotifications(c) {
		return
	}
	eventType := c.Param("type")
	var request preferenceRequest
	if c.ShouldBindJSON(&request) != nil || request.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	user := userID(c)
	if err := h.notifications.SetPreference(user, eventType, *request.Enabled); err != nil {
		if errors.Is(err, notify.ErrUnknownEventType) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown notification event type"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update preference"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"event_type": eventType, "enabled": *request.Enabled})
}
