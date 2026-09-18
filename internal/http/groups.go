package http

import (
	"errors"
	"net/http"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/group"
	"github.com/gin-gonic/gin"
)

// 本文件实现管理端用户组管理（migration 035，仅 admin 组路由）：
//   GET    /api/v1/admin/groups                 组列表（含成员数聚合）
//   POST   /api/v1/admin/groups                 创建组 {name, description}
//   PATCH  /api/v1/admin/groups/:id             改名/改描述 {name?, description?}
//   DELETE /api/v1/admin/groups/:id             删除组（级联清 group_members，不删用户）
//   GET    /api/v1/admin/groups/:id/members     组成员列表
//   POST   /api/v1/admin/groups/:id/members     添加成员 {user_id}
//   DELETE /api/v1/admin/groups/:id/members/:uid 移除成员
//
// 组为纯组织维度（不挂文件空间、不参与文件权限判定），与团队（协作空间）
// 互补；管理权限由 /admin 路由组的 RequireRole(admin) 保证。全部变更写审计。

type groupRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// requireGroups 校验组服务已注入（生产恒注入，NewHandler 默认 nil 时端点 503）。
func (h *Handler) requireGroups(c *gin.Context) bool {
	if h.groups == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "group service is not configured"})
		return false
	}
	return true
}

// groupJSON 序列化组安全字段（含 member_count 聚合与更新时间）。
func groupJSON(g group.Group) gin.H {
	return gin.H{"id": g.ID, "name": g.Name, "description": g.Description, "member_count": g.MemberCount, "created_at": g.CreatedAt, "updated_at": g.UpdatedAt}
}

// groupMemberJSON 序列化组成员安全字段（username/nickname 仅非空时返回，
// 兼容 MemoryStore 等无用户数据的实现）。
func groupMemberJSON(m group.Member) gin.H {
	out := gin.H{"user_id": m.UserID, "joined_at": m.JoinedAt}
	if m.Username != "" {
		out["username"] = m.Username
	}
	if m.Nickname != "" {
		out["nickname"] = m.Nickname
	}
	return out
}

// groupError 统一映射组服务错误（不含 auth.ErrUserNotFound，由成员端点单独处理）。
func groupError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, group.ErrInvalidName):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, group.ErrNameConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "group name already exists"})
	case errors.Is(err, group.ErrMemberExists):
		c.JSON(http.StatusConflict, gin.H{"error": "user is already a group member"})
	case errors.Is(err, group.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "group operation failed"})
	}
	return true
}

// adminListGroups GET /api/v1/admin/groups：全部用户组（含成员数）。
func (h *Handler) adminListGroups(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	groups, err := h.groups.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list groups"})
		return
	}
	items := make([]gin.H, 0, len(groups))
	for _, g := range groups {
		items = append(items, groupJSON(g))
	}
	c.JSON(http.StatusOK, gin.H{"groups": items})
}

// adminCreateGroup POST /api/v1/admin/groups {name, description}：创建组；
// 成功 201 并写 group.create 审计（ResourceID 为组 ID）。
func (h *Handler) adminCreateGroup(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	var req groupRequest
	if c.ShouldBindJSON(&req) != nil || req.Name == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	description := ""
	if req.Description != nil {
		description = *req.Description
	}
	actor := userID(c)
	g, err := h.groups.Create(*req.Name, description, actor)
	if groupError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionGroupCreate, ResourceType: audit.ResourceGroup, ResourceID: g.ID.String()})
	c.JSON(http.StatusCreated, groupJSON(g))
}

// adminUpdateGroup PATCH /api/v1/admin/groups/:id {name?, description?}：
// 指针语义更新（均缺省为无操作）；成功 200 返回更新后的组，写 group.update 审计。
func (h *Handler) adminUpdateGroup(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req groupRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	g, err := h.groups.Update(id, req.Name, req.Description)
	if groupError(c, err) {
		return
	}
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionGroupUpdate, ResourceType: audit.ResourceGroup, ResourceID: id.String()})
	c.JSON(http.StatusOK, groupJSON(g))
}

// adminDeleteGroup DELETE /api/v1/admin/groups/:id：删除组（group_members
// 级联清空，组不删用户）；成功 204，写 group.delete 审计。
func (h *Handler) adminDeleteGroup(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if groupError(c, h.groups.Delete(id)) {
		return
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionGroupDelete, ResourceType: audit.ResourceGroup, ResourceID: id.String()})
	c.Status(http.StatusNoContent)
}

// adminListGroupMembers GET /api/v1/admin/groups/:id/members：组成员列表
// （JOIN users 补齐 username/nickname 展示列）。
func (h *Handler) adminListGroupMembers(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	members, err := h.groups.ListMembers(id)
	if groupError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(members))
	for _, m := range members {
		items = append(items, groupMemberJSON(m))
	}
	c.JSON(http.StatusOK, gin.H{"members": items})
}

type groupMemberRequest struct {
	UserID string `json:"user_id"`
}

// adminAddGroupMember POST /api/v1/admin/groups/:id/members {user_id}：
// 添加成员（用户不存在 404）；成功 201，写 group.member.add 审计
// （metadata 记录成员用户 ID）。
func (h *Handler) adminAddGroupMember(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req groupMemberRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	memberUserID, ok := parseID(c, req.UserID)
	if !ok {
		return
	}
	// 用户存在性校验（组服务只管组维度；FK 兜底之外的显式 404 语义）。
	if _, err := h.users.GetByID(memberUserID); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load user"})
		return
	}
	m, err := h.groups.AddMember(id, memberUserID)
	if groupError(c, err) {
		return
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionGroupMemberAdd, ResourceType: audit.ResourceGroup, ResourceID: id.String(), Metadata: `{"user_id":"` + memberUserID.String() + `"}`})
	c.JSON(http.StatusCreated, groupMemberJSON(m))
}

// adminRemoveGroupMember DELETE /api/v1/admin/groups/:id/members/:uid：
// 移除成员；成功 204，写 group.member.remove 审计（metadata 记录成员用户 ID）。
func (h *Handler) adminRemoveGroupMember(c *gin.Context) {
	if !h.requireGroups(c) {
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	memberUserID, ok := parseID(c, c.Param("uid"))
	if !ok {
		return
	}
	if groupError(c, h.groups.RemoveMember(id, memberUserID)) {
		return
	}
	actor := userID(c)
	h.recordAudit(c, audit.Entry{UserID: &actor, Action: audit.ActionGroupMemberRemove, ResourceType: audit.ResourceGroup, ResourceID: id.String(), Metadata: `{"user_id":"` + memberUserID.String() + `"}`})
	c.Status(http.StatusNoContent)
}
