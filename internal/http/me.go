package http

import (
	"errors"
	"net/http"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 本文件实现个人档案与配额查询端点（C3/C21a，设计 3.2.1/6.3.1/6.12.4）：
//   GET   /api/v1/me  当前用户档案 + 存储用量/配额（本人与管理端统计口径独立）
//   PATCH /api/v1/me  更新档案（nickname/department/position/phone/bio/
//                      language/timezone；language ∈ zh-CN|en-US）

// storageUsage 抽象个人空间存储占用查询（生产实现为 *files.Store.UsedStorage，
// 软删文件计入；接口化便于单测注入内存实现）。
type storageUsage interface {
	UsedStorage(owner uuid.UUID) (int64, error)
}

var _ storageUsage = (*files.Store)(nil)

// profileRequest 为 PATCH /me 请求体：指针字段区分「未提供」与「清空」
// （空串清空文本字段；language/timezone 空串视为未提供）。
type profileRequest struct {
	Nickname   *string `json:"nickname"`
	Department *string `json:"department"`
	Position   *string `json:"position"`
	Phone      *string `json:"phone"`
	Bio        *string `json:"bio"`
	Language   *string `json:"language"`
	Timezone   *string `json:"timezone"`
}

// meView 序列化 /me 响应：不含 password_hash；storage.used 与配额校验同口径
// （软删文件计入已用）；档案字段可空文本为 null。
func meView(u auth.User, used int64) gin.H {
	return gin.H{
		"id": u.ID, "username": u.Username, "email": u.Email,
		"role": u.Role, "status": u.Status,
		"storage": gin.H{"used": used, "quota": u.StorageQuota},
		"profile": gin.H{
			"nickname":   u.Nickname,
			"department": u.Department,
			"position":   u.Position,
			"phone":      u.Phone,
			"bio":        u.Bio,
			"language":   u.Language,
			"timezone":   u.Timezone,
		},
		"created_at": u.CreatedAt,
	}
}

// me GET /api/v1/me（认证）：当前用户完整档案与存储用量/配额。
func (h *Handler) me(c *gin.Context) {
	id := userID(c)
	user, err := h.users.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	used, err := h.usage.UsedStorage(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to collect storage usage"})
		return
	}
	c.JSON(http.StatusOK, meView(user, used))
}

// updateMe PATCH /api/v1/me（认证）：更新档案；校验失败 400，成功返回更新后
// 的完整 /me 视图。并发更新为最后写入胜出（字段级覆盖，无版本控制）。
func (h *Handler) updateMe(c *gin.Context) {
	var request profileRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	update := auth.ProfileUpdate{
		Nickname:   request.Nickname,
		Department: request.Department,
		Position:   request.Position,
		Phone:      request.Phone,
		Bio:        request.Bio,
		Language:   request.Language,
		Timezone:   request.Timezone,
	}
	if err := update.Validate(); err != nil {
		switch {
		case errors.Is(err, auth.ErrProfileTooLong), errors.Is(err, auth.ErrInvalidLanguage), errors.Is(err, auth.ErrInvalidTimezone):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		}
		return
	}
	id := userID(c)
	if err := h.users.UpdateProfile(id, update); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update profile"})
		return
	}
	user, err := h.users.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load profile"})
		return
	}
	used, err := h.usage.UsedStorage(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to collect storage usage"})
		return
	}
	c.JSON(http.StatusOK, meView(user, used))
}
