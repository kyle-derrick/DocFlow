package http

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
)

// 本文件实现管理端用户管理（C6，设计 6.2.1/9.1.1，仅 admin 组）：
//   GET  /api/v1/admin/users                      用户列表（q 前缀检索 + 分页）
//   PATCH /api/v1/admin/users/:id                  禁用/启用/改配额/改角色
//   POST /api/v1/admin/users/:id/reset-password    重置密码（撤销全部会话）
//
// 设计 DELETE /users/:id 以软禁用（status=disabled）替代：删除用户会级联
// 失效其名下文件/分享/会话等引用行，破坏审计与数据完整性；禁用立即撤销全部
// 会话并阻止登录，达到同等的「账号不可用」效果（取舍见设计 6.2.1）。

// adminUserMaxQuota 为管理端可设置的单用户配额上限（1 PiB）。
const adminUserMaxQuota int64 = 1 << 50

// adminUserJSON 序列化管理端用户视图（脱敏：不含 password_hash；全字段
// 含 status/quota/locked）。
func adminUserJSON(u auth.User) gin.H {
	return gin.H{
		"id": u.ID, "username": u.Username, "email": u.Email,
		"role": u.Role, "status": u.Status,
		"storage_quota":      u.StorageQuota,
		"failed_login_count": u.FailedLoginCount,
		"locked_until":       u.LockedUntil,
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
		"updated_at": u.UpdatedAt,
	}
}

// adminListUsers GET /api/v1/admin/users?q=&limit=&offset=：username/email
// 前缀检索（ILIKE，通配符已转义）+ 分页（limit 默认 50 上限 200，offset>=0），
// 返回 {users, total, limit, offset}。
func (h *Handler) adminGetUser(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	user, err := h.users.GetByID(id)
	if errors.Is(err, auth.ErrUserNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load user"})
		return
	}
	c.JSON(http.StatusOK, adminUserJSON(user))
}

func (h *Handler) adminDeleteUser(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if id == userID(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot disable your own account"})
		return
	}
	if err := h.users.AdminUpdateUser(id, auth.AdminUserUpdate{Status: func() *string { v := auth.StatusDisabled; return &v }()}); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to disable user"})
		return
	}
	_ = h.auth.RevokeAllSessions(id)
	c.Status(http.StatusNoContent)
}

func (h *Handler) adminListUsers(c *gin.Context) {
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < 200 {
			limit = n
		} else {
			limit = 200
		}
	}
	offset := 0
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid offset"})
			return
		}
		offset = n
	}
	users, total, err := h.users.AdminListUsers(c.Query("q"), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list users"})
		return
	}
	items := make([]gin.H, 0, len(users))
	for _, u := range users {
		items = append(items, adminUserJSON(u))
	}
	c.JSON(http.StatusOK, gin.H{"users": items, "total": total, "limit": limit, "offset": offset})
}

type adminUpdateUserRequest struct {
	Status       *string `json:"status"`
	StorageQuota *int64  `json:"storage_quota"`
	Role         *string `json:"role"`
}

// adminUpdateUser PATCH /api/v1/admin/users/:id {status?, storage_quota?, role?}：
// status ∈ active|disabled（禁用立即撤销其全部会话；置 active 视为解锁）；
// storage_quota ∈ [1, 1PiB]；role ∈ user|admin。admin 不可禁用自己（400）。
// 成功写 user.update 审计（含变更字段）并返回更新后的用户视图。
func (h *Handler) adminUpdateUser(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var request adminUpdateUserRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if request.Status != nil && *request.Status != auth.StatusActive && *request.Status != auth.StatusDisabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be active or disabled"})
		return
	}
	if request.Role != nil && *request.Role != auth.RoleUser && *request.Role != auth.RoleAdmin {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be user or admin"})
		return
	}
	if request.StorageQuota != nil && (*request.StorageQuota < 1 || *request.StorageQuota > adminUserMaxQuota) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "storage_quota must be 1 byte to 1 PiB"})
		return
	}
	actor := userID(c)
	if request.Status != nil && *request.Status == auth.StatusDisabled && id == actor {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot disable your own account"})
		return
	}
	if err := h.users.AdminUpdateUser(id, auth.AdminUserUpdate{
		Status:       request.Status,
		StorageQuota: request.StorageQuota,
		Role:         request.Role,
	}); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update user"})
		return
	}
	// 禁用立即撤销其全部会话（设计 6.2.1）：已在册的 refresh token 全部失效。
	if request.Status != nil && *request.Status == auth.StatusDisabled {
		if err := h.auth.RevokeAllSessions(id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "user updated but session revocation failed"})
			return
		}
	}
	user, err := h.users.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load user"})
		return
	}
	// 审计：仅记录本次变更的字段（不回显未变更值）。
	var meta []string
	if request.Status != nil {
		meta = append(meta, `"status":"`+*request.Status+`"`)
	}
	if request.StorageQuota != nil {
		meta = append(meta, `"storage_quota":`+strconv.FormatInt(*request.StorageQuota, 10))
	}
	if request.Role != nil {
		meta = append(meta, `"role":"`+*request.Role+`"`)
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionUserUpdate, ResourceType: audit.ResourceUser, ResourceID: id.String(), Metadata: "{" + strings.Join(meta, ",") + "}"})
	c.JSON(http.StatusOK, adminUserJSON(user))
}

type adminResetPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// adminResetUserPassword POST /api/v1/admin/users/:id/reset-password
// {new_password}：强度校验（与自助改密同规则）后更新哈希并撤销该用户全部
// 会话（被重置者所有设备需以新密码重新登录）。成功 204，写 user.reset_password
// 审计（不记录密码任何形态）。
func (h *Handler) adminResetUserPassword(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var request adminResetPasswordRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if err := h.auth.AdminResetPassword(id, request.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrPasswordTooShort), errors.Is(err, auth.ErrPasswordTooWeak):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, auth.ErrUserNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		case errors.Is(err, auth.ErrNotConfigured):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "password reset is not configured"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to reset password"})
		}
		return
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionUserResetPassword, ResourceType: audit.ResourceUser, ResourceID: id.String()})
	c.Status(http.StatusNoContent)
}
