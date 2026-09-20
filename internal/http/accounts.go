package http

import (
	"errors"
	"log"
	"net/http"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/invite"
	"github.com/gin-gonic/gin"
)

// 本文件实现邀请制注册与密码管理的公开/认证端点：
//   POST /api/v1/auth/register        凭一次性邀请 token 注册（响应同 login）
//   POST /api/v1/auth/forgot-password 请求重置邮件（恒 202，防邮箱枚举）
//   POST /api/v1/auth/reset-password  凭一次性重置 token 重置密码
//   POST /api/v1/auth/change-password 认证后改密（撤销其他会话并轮换当前会话）

type registerRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// register POST /api/v1/auth/register {token,username,password}：邀请接受。
// 注册成功即视为登录：签发 access token 并创建新 refresh 会话（同 login 响应）。
// 统一空间模型：注册成功后自动创建默认空间「{username}的空间」（幂等；
// 创建失败仅记日志不阻断登录——下次登录相关入口可经 EnsureDefaultSpace 补齐）。
func (h *Handler) register(c *gin.Context) {
	if h.invites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invitation service is not configured"})
		return
	}
	var request registerRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	user, inv, err := h.invites.Accept(request.Token, request.Username, request.Password)
	if err != nil {
		switch {
		case errors.Is(err, invite.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
		case errors.Is(err, invite.ErrGone):
			c.JSON(http.StatusGone, gin.H{"error": "invitation is no longer available"})
		case errors.Is(err, auth.ErrUserExists):
			c.JSON(http.StatusConflict, gin.H{"error": "username or email already exists"})
		case errors.Is(err, auth.ErrInvalidUsername), errors.Is(err, auth.ErrPasswordTooShort), errors.Is(err, auth.ErrPasswordTooWeak):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to complete registration"})
		}
		return
	}
	if h.spaces != nil {
		if _, _, serr := h.spaces.EnsureDefaultSpace(user.ID, user.Username); serr != nil {
			log.Printf("[space] ensure default space for %s: %v", user.ID, serr)
		}
	}
	access, err := h.auth.AccessToken(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
		return
	}
	refresh, err := h.auth.NewSessionWithInfo(user.ID, sessionInfoFromRequest(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session creation failed"})
		return
	}
	h.setRefreshCookie(c, refresh)
	uid := user.ID
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionInviteAccept, ResourceType: audit.ResourceInvitation, ResourceID: inv.ID.String(), Metadata: `{"email":"` + sanitizeAuditToken(inv.Email) + `","username":"` + sanitizeAuditToken(user.Username) + `","role":"` + user.Role + `"}`})
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer"})
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// forgotPassword POST /api/v1/auth/forgot-password {email}：创建 30 分钟有效的
// 一次性重置令牌并发送邮件。无论邮箱是否存在一律 202（不泄露账号存在性）；
// 发送失败仅记日志（令牌仍有效，可重试请求）。
func (h *Handler) forgotPassword(c *gin.Context) {
	var request forgotPasswordRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	token, err := h.auth.RequestPasswordReset(request.Email)
	if errors.Is(err, auth.ErrUserNotFound) {
		// 防枚举：与存在用户路径同样返回 202。
		c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to process request"})
		return
	}
	email := auth.NormalizeEmail(request.Email)
	if err := h.mailer.SendPasswordReset(email, h.publicLink("/reset/"+token)); err != nil {
		log.Printf("[mail] send password reset to %s: %v", email, err)
	}
	h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionPasswordReset, ResourceType: audit.ResourceSession, Status: audit.StatusSuccess, Metadata: `{"stage":"request","email":"` + sanitizeAuditToken(email) + `"}`})
	c.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}

type resetPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// resetPassword POST /api/v1/auth/reset-password {token,password}：凭一次性
// 令牌重置密码并撤销该用户全部会话。令牌无效/已用/过期返回 400。
func (h *Handler) resetPassword(c *gin.Context) {
	var request resetPasswordRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	userID, err := h.auth.ResetPassword(request.Token, request.Password)
	switch {
	case err == nil:
		id := userID
		h.recordAudit(c, audit.Entry{UserID: &id, Action: audit.ActionPasswordReset, ResourceType: audit.ResourceUser, ResourceID: userID.String(), Metadata: `{"stage":"reset"}`})
		c.Status(http.StatusNoContent)
	case errors.Is(err, auth.ErrResetTokenInvalid):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired reset token"})
	case errors.Is(err, auth.ErrPasswordTooShort), errors.Is(err, auth.ErrPasswordTooWeak):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, auth.ErrNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "password reset is not configured"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to reset password"})
	}
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// changePassword POST /api/v1/auth/change-password {old_password,new_password}
// （认证）：校验旧密码与新密码强度后更新哈希，撤销该用户全部其他会话，并
// 轮换当前会话（响应头下发新 refresh cookie，本会话保持可用）。204。
func (h *Handler) changePassword(c *gin.Context) {
	var request changePasswordRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	id := userID(c)
	// 当前会话凭 refresh cookie 识别（改密撤销其他会话时保留之）。
	currentRefresh := ""
	if cookie, err := c.Request.Cookie("refresh_token"); err == nil {
		currentRefresh = cookie.Value
	}
	if err := h.auth.ChangePassword(id, currentRefresh, request.OldPassword, request.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid credentials"})
		case errors.Is(err, auth.ErrPasswordTooShort), errors.Is(err, auth.ErrPasswordTooWeak):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, auth.ErrNotConfigured), errors.Is(err, auth.ErrUserNotFound):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "password change is not configured"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to change password"})
		}
		return
	}
	// 轮换当前会话：成功后响应头下发新的 refresh cookie。
	// 轮换失败（cookie 缺失/已过期）不影响改密结果：本会话本就不可用。
	if currentRefresh != "" {
		if _, replacement, err := h.auth.RotateRefreshToken(currentRefresh); err == nil {
			h.setRefreshCookie(c, replacement)
		}
	}
	h.recordAudit(c, audit.Entry{UserID: &id, Action: audit.ActionPasswordReset, ResourceType: audit.ResourceUser, ResourceID: id.String(), Metadata: `{"stage":"change"}`})
	c.Status(http.StatusNoContent)
}
