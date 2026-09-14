package http

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/invite"
	"github.com/gin-gonic/gin"
)

// 本文件实现管理端邀请管理（admin 组，RequireRole(admin) 之后）：
//   POST   /api/v1/admin/invitations     创建邀请（201，返回一次性 accept_url）
//   GET    /api/v1/admin/invitations     邀请列表（含派生状态）
//   DELETE /api/v1/admin/invitations/:id 撤销邀请（删行，token 立即失效）

type invitationRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"` // user|admin，缺省 user
}

// invitationJSON 序列化邀请（不含 token_hash），附派生状态。
func invitationJSON(v invite.Invitation) gin.H {
	return gin.H{
		"id":          v.ID,
		"email":       v.Email,
		"invited_by":  v.InvitedBy,
		"role":        v.Role,
		"status":      v.Status(time.Now().UTC()),
		"expires_at":  v.ExpiresAt,
		"accepted_at": v.AcceptedAt,
		"created_at":  v.CreatedAt,
	}
}

// createInvitation POST /api/v1/admin/invitations {email,role}：创建邀请。
// 成功返回 201 与一次性 accept_url（前端路由 /register/<token>，明文 token
// 仅本次响应可见，数据库只存哈希）；幂等命中既有邀请时返回 200（无新链接）。
// 邀请邮件 best-effort 发送（失败仅记日志，管理员仍可复制 accept_url）。
func (h *Handler) createInvitation(c *gin.Context) {
	if h.invites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invitation service is not configured"})
		return
	}
	var request invitationRequest
	if c.ShouldBindJSON(&request) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	inv, token, err := h.invites.Create(actor, request.Email, request.Role)
	if err != nil {
		switch {
		case errors.Is(err, invite.ErrInvalidEmail), errors.Is(err, invite.ErrInvalidRole):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to create invitation"})
		}
		return
	}
	out := invitationJSON(inv)
	status := http.StatusCreated
	if token != "" {
		// 明文 token 仅本次返回：accept_url 指向前端注册页 /register/<token>。
		out["accept_url"] = "/register/" + token
		inviter := ""
		if name, err := h.users.Username(actor); err == nil {
			inviter = name
		}
		if err := h.mailer.SendInvitation(inv.Email, h.publicLink("/register/"+token), inviter); err != nil {
			log.Printf("[mail] send invitation to %s: %v", inv.Email, err)
		}
	} else {
		// 幂等命中既有邀请：明文 token 已不可恢复，200 且不带链接。
		status = http.StatusOK
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionInviteCreate, ResourceType: audit.ResourceInvitation, ResourceID: inv.ID.String(), Metadata: `{"email":"` + sanitizeAuditToken(inv.Email) + `","role":"` + inv.Role + `","new":` + strconv.FormatBool(token != "") + `}`})
	c.JSON(status, out)
}

// listInvitations GET /api/v1/admin/invitations：邀请列表（created_at 倒序，
// 默认 100 条，limit 参数上限 1000），每条附派生状态 pending/accepted/expired。
func (h *Handler) listInvitations(c *gin.Context) {
	if h.invites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invitation service is not configured"})
		return
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if n < 1000 {
			limit = n
		} else {
			limit = 1000
		}
	}
	list, err := h.invites.List(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list invitations"})
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, v := range list {
		items = append(items, invitationJSON(v))
	}
	c.JSON(http.StatusOK, gin.H{"invitations": items})
}

// revokeInvitation DELETE /api/v1/admin/invitations/:id：撤销邀请（删行，
// token 立即不可解析）；不存在 404。已接受的邀请撤销无实际效果（token 已消费）。
func (h *Handler) revokeInvitation(c *gin.Context) {
	if h.invites == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "invitation service is not configured"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if err := h.invites.Revoke(id); err != nil {
		if errors.Is(err, invite.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to revoke invitation"})
		}
		return
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionInviteRevoke, ResourceType: audit.ResourceInvitation, ResourceID: id.String()})
	c.Status(http.StatusNoContent)
}
