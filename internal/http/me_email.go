package http

import (
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 本文件实现账号安全「换绑邮箱」（v2.4 整改项 14）的两段式确认流：
//
//	POST /api/v1/me/email/change-request  {password, new_email}
//	  验证当前密码 → 校验新邮箱（格式/与当前不同/未被占用）→ 生成 6 位数字
//	  验证码（10 分钟有效，email_change_codes 落哈希）→ 向新邮箱投递验证码。
//	POST /api/v1/me/email/change-confirm  {code}
//	  原子消费验证码 → 更新 users.email（并发撞唯一约束 409）。
//
// 邮箱归属验证即由「验证码投递到新邮箱」完成；密码验证防止盗号者接管。
// 会话不撤销（登录态基于 user_id），旧邮箱不再可用于登录。

// emailChangeRequestRequest 为 change-request 请求体。
type emailChangeRequestRequest struct {
	Password string `json:"password"`
	NewEmail string `json:"new_email"`
}

// validEmailFormat 做最小格式校验（非空、单个 @、两侧非空、无空白/控制字符）；
// 深度校验（域名可达性等）不在范围内。
func validEmailFormat(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.IndexByte(email[at+1:], '@') >= 0 {
		return false
	}
	for _, r := range email {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// requestEmailChange POST /api/v1/me/email/change-request（认证）。
func (h *Handler) requestEmailChange(c *gin.Context) {
	if h.emailChangeCodes == nil || h.emailChangeAccount == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email change is not configured"})
		return
	}
	var req emailChangeRequestRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	id := userID(c)
	user, err := h.emailChangeAccount.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if h.auth.VerifyPassword(user.PasswordHash, req.Password) != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid credentials"})
		return
	}
	email := auth.NormalizeEmail(req.NewEmail)
	if !validEmailFormat(email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid email address"})
		return
	}
	if email == auth.NormalizeEmail(user.Email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "new email must differ from the current one"})
		return
	}
	exists, err := h.emailChangeAccount.EmailExists(email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to verify email"})
		return
	}
	if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "email already in use"})
		return
	}
	code, err := auth.GenerateEmailChangeCode()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to generate code"})
		return
	}
	now := time.Now().UTC()
	if err := h.emailChangeCodes.Create(auth.EmailChangeCode{
		ID:        uuid.New(),
		UserID:    id,
		NewEmail:  email,
		CodeHash:  auth.HashEmailChangeCode(id, code),
		ExpiresAt: now.Add(auth.EmailChangeTokenTTL),
		CreatedAt: now,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to store code"})
		return
	}
	// 发送失败不回滚验证码行（可重试请求生成新码）；返回 500 提示重试。
	if err := h.mailer.SendEmailChangeCode(email, code, user.Username); err != nil {
		log.Printf("[mail] send email change code to %s: %v", email, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to send verification email"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &id, Action: audit.ActionUserEmailChange, ResourceType: audit.ResourceUser, ResourceID: id.String(), Status: audit.StatusSuccess, Metadata: `{"stage":"email-change-request"}`})
	c.JSON(http.StatusOK, gin.H{"status": "accepted", "expires_in": int(auth.EmailChangeTokenTTL.Seconds())})
}

// emailChangeConfirmRequest 为 change-confirm 请求体。
type emailChangeConfirmRequest struct {
	Code string `json:"code"`
}

// confirmEmailChange POST /api/v1/me/email/change-confirm（认证）。
func (h *Handler) confirmEmailChange(c *gin.Context) {
	if h.emailChangeCodes == nil || h.emailChangeAccount == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "email change is not configured"})
		return
	}
	var req emailChangeConfirmRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing code"})
		return
	}
	id := userID(c)
	newEmail, ok, err := h.emailChangeCodes.ConsumeForUser(id, auth.HashEmailChangeCode(id, code), time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to verify code"})
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or expired code"})
		return
	}
	// 确认时复查占用（请求与确认之间可能被他人注册）。
	if exists, eerr := h.emailChangeAccount.EmailExists(newEmail); eerr == nil && exists {
		c.JSON(http.StatusConflict, gin.H{"error": "email already in use"})
		return
	}
	if err := h.emailChangeAccount.UpdateEmail(id, newEmail); err != nil {
		if err == auth.ErrUserExists {
			c.JSON(http.StatusConflict, gin.H{"error": "email already in use"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to update email"})
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &id, Action: audit.ActionUserEmailChange, ResourceType: audit.ResourceUser, ResourceID: id.String(), Status: audit.StatusSuccess, Metadata: `{"stage":"email-change-confirm"}`})
	c.JSON(http.StatusOK, gin.H{"email": newEmail})
}
