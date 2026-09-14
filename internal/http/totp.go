package http

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
)

// 本文件实现两步验证（TOTP）端点（v2 设计）：
//   POST   /api/v1/auth/totp          登录第二段（公开，与 /login 同级限流）
//   GET    /api/v1/auth/totp          状态（认证）
//   POST   /api/v1/auth/totp/setup    开始设置（认证）→ {secret, otpauth_url}
//   POST   /api/v1/auth/totp/confirm  确认启用（认证）→ {recovery_codes[]}
//   DELETE /api/v1/auth/totp          禁用（认证）{password, code?} 二选一
//
// 防绕过：/auth/login 对 enabled 用户一律 401 TOTP_REQUIRED、不发任何
// token（见 handler.go login）；第二段重新验证密码后才校验码并发放会话。

type loginTOTPRequest struct {
	// Identifier 为登录标识（C21a）：email 或 username，优先于 Email。
	Identifier string `json:"identifier"`
	// Email 为旧字段（兼容保留）：identifier 缺省时回退使用。
	Email        string `json:"email"`
	Password     string `json:"password"`
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

// loginIdentifier 解析登录标识：identifier 优先，缺省回退旧 email 字段。
func (r loginTOTPRequest) loginIdentifier() string {
	if id := strings.TrimSpace(r.Identifier); id != "" {
		return id
	}
	return strings.TrimSpace(r.Email)
}

// loginTOTP POST /api/v1/auth/login/totp {identifier,password,code|recovery_code}：
// 密码 + TOTP 码/恢复码双因子通过后发放 token（响应同 login；恢复码命中即
// 消耗，一次性）。密码错误与 login 同响应（401 invalid credentials，防枚举，
// 且同样计入 C9 失败锁定）；码错误 401 invalid totp code（不计失败——限流
// 已约束爆破，避免攻击者以错误码恶意锁定他人账号）。
func (h *Handler) loginTOTP(c *gin.Context) {
	var request loginTOTPRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if strings.TrimSpace(request.Code) == "" && strings.TrimSpace(request.RecoveryCode) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code or recovery_code is required"})
		return
	}
	identifier := request.loginIdentifier()
	reject := func() {
		h.recordAudit(c, audit.Entry{UserID: nil, Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"identifier":"` + sanitizeAuditToken(identifier) + `","stage":"totp"}`})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
	}
	user, err := h.users.FindActiveByIdentifier(identifier)
	if err != nil {
		_ = compareDummyPassword(request.Password)
		reject()
		return
	}
	// 锁定检查（C9）：锁定期间一律 423，不计新失败。
	if auth.IsLocked(user.LockedUntil, time.Now()) {
		lockedResponse(c)
		return
	}
	if h.auth.VerifyPassword(user.PasswordHash, request.Password) != nil {
		h.recordLoginFailure(user.ID)
		reject()
		return
	}
	if err := h.auth.VerifyTOTP(user.ID, request.Code, request.RecoveryCode); err != nil {
		switch {
		case errors.Is(err, auth.ErrTOTPNotEnabled):
			// 用户未启用（或已禁用）：按凭据失败统一响应，不泄露启用状态。
			reject()
		case errors.Is(err, auth.ErrTOTPInvalidCode):
			h.recordAudit(c, audit.Entry{UserID: &user.ID, Action: audit.ActionLoginFailure, ResourceType: audit.ResourceSession, Status: audit.StatusFailure, Metadata: `{"identifier":"` + sanitizeAuditToken(identifier) + `","stage":"totp"}`})
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid totp code"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to verify second factor"})
		}
		return
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
	// 成功登录清零失败计数并解除锁定（C9）。
	_ = h.users.ClearLoginFailures(user.ID)
	h.setRefreshCookie(c, refresh)
	h.recordAudit(c, audit.Entry{UserID: &user.ID, Action: audit.ActionLoginSuccess, ResourceType: audit.ResourceSession, ResourceID: user.ID.String(), Status: audit.StatusSuccess, Metadata: `{"stage":"totp"}`})
	c.JSON(http.StatusOK, gin.H{"access_token": access, "token_type": "Bearer"})
}

// totpStatus GET /api/v1/auth/totp（认证）：{enabled, confirmed_at}。
// 未 setup 时 enabled=false、confirmed_at=null。
func (h *Handler) totpStatus(c *gin.Context) {
	status, err := h.auth.TOTPStatus(userID(c))
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"enabled": status.Enabled, "confirmed_at": status.ConfirmedAt})
	case errors.Is(err, auth.ErrTOTPNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "totp is not configured"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load totp status"})
	}
}

// totpSetup POST /api/v1/auth/totp/setup（认证）：生成新 secret（作废未完成
// 的旧 setup）并落库 enabled=false；返回 secret 与 otpauth URL（认证器手动
// 添加用——本版本不渲染二维码，见设计限制）。
func (h *Handler) totpSetup(c *gin.Context) {
	secret, otpauthURL, err := h.auth.BeginTOTPSetup(userID(c))
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"secret": secret, "otpauth_url": otpauthURL})
	case errors.Is(err, auth.ErrTOTPNotConfigured), errors.Is(err, auth.ErrNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "totp is not configured"})
	case errors.Is(err, auth.ErrTOTPAlreadyEnabled):
		c.JSON(http.StatusConflict, gin.H{"error": "totp is already enabled"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to start totp setup"})
	}
}

type confirmTOTPRequest struct {
	Code string `json:"code"`
}

// totpConfirm POST /api/v1/auth/totp/confirm {code}（认证）：校验 6 位码
// （±1 窗口）后启用并生成 10 个恢复码；明文（xxxx-xxxx）仅本响应返回一次，
// 库中只存 SHA-256 哈希。写 totp.enable 审计。
func (h *Handler) totpConfirm(c *gin.Context) {
	var request confirmTOTPRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	uid := userID(c)
	codes, err := h.auth.ConfirmTOTPSetup(uid, request.Code)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrTOTPInvalidCode):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid totp code"})
		return
	case errors.Is(err, auth.ErrTOTPSetupRequired):
		c.JSON(http.StatusBadRequest, gin.H{"error": "totp setup has not been started"})
		return
	case errors.Is(err, auth.ErrTOTPAlreadyEnabled):
		c.JSON(http.StatusConflict, gin.H{"error": "totp is already enabled"})
		return
	case errors.Is(err, auth.ErrTOTPNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "totp is not configured"})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to confirm totp setup"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionTOTPEnable, ResourceType: audit.ResourceUser, ResourceID: uid.String()})
	c.JSON(http.StatusOK, gin.H{"recovery_codes": codes})
}

type disableTOTPRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// totpDisable DELETE /api/v1/auth/totp {password, code?}（认证）：密码或
// 当前有效 TOTP 码二选一验证后禁用（删行）；恢复码不可用于禁用。204。
// 写 totp.disable 审计。
func (h *Handler) totpDisable(c *gin.Context) {
	var request disableTOTPRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if request.Password == "" && strings.TrimSpace(request.Code) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password or code is required"})
		return
	}
	uid := userID(c)
	switch err := h.auth.DisableTOTP(uid, request.Password, request.Code); {
	case err == nil:
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid credentials"})
		return
	case errors.Is(err, auth.ErrTOTPNotEnabled):
		c.JSON(http.StatusNotFound, gin.H{"error": "totp is not enabled"})
		return
	case errors.Is(err, auth.ErrTOTPNotConfigured), errors.Is(err, auth.ErrNotConfigured):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "totp is not configured"})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to disable totp"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: audit.ActionTOTPDisable, ResourceType: audit.ResourceUser, ResourceID: uid.String()})
	c.Status(http.StatusNoContent)
}
