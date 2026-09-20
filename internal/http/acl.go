package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/acl"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/space"
)

// aclService 抽象路径级 ACL 管理能力（SetACL 注入；生产实现 *acl.Service，
// 契约测试注入内存实现）。未注入时端点返回 503。
type aclService interface {
	// List 返回目录行与当前条目；目录不存在 acl.ErrNotFound、
	// 非空间作用域 acl.ErrNotSpaceFolder。
	List(folderID uuid.UUID) (files.File, []acl.Entry, error)
	// Replace 整体替换条目（校验 + 事务内删旧插新），返回目录行。
	Replace(folderID uuid.UUID, entries []acl.EntryInput, actor uuid.UUID) (files.File, error)
}

// SetACL 注入路径级 ACL 管理服务（幂等）；GET/PUT /api/v1/folders/:id/acl。
func (h *Handler) SetACL(svc aclService) {
	if svc != nil {
		h.acl = svc
	}
}

// canManageFolderACL 判定 actor 能否管理目录的 ACL：文件夹所在空间的
// owner/admin（空间级管理权限，五级内置角色）或系统 admin（其余成员 403）。
// 空间不存在按 false 处理（调用方前置校验）。
func (h *Handler) canManageFolderACL(actor, spaceID uuid.UUID) bool {
	if h.spaces == nil {
		return false
	}
	if _, err := h.spaces.Get(spaceID); err != nil {
		return false
	}
	if ok, err := h.spaces.CanAdmin(actor, spaceID); err == nil && ok {
		return true
	}
	if h.roles != nil {
		if role, err := h.roles.Role(actor); err == nil && role == auth.RoleAdmin {
			return true
		}
	}
	return false
}

// aclError 统一映射 ACL 服务错误：不存在 404（不泄露存在性）、非空间
// 作用域目录 400、条目非法/重复 400。
func aclError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, acl.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
	case errors.Is(err, acl.ErrNotSpaceFolder):
		c.JSON(http.StatusBadRequest, gin.H{"error": "acl is only available for space folders", "code": "NOT_SPACE_FOLDER"})
	case errors.Is(err, acl.ErrInvalidEntry), errors.Is(err, acl.ErrDuplicateEntry):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "acl operation failed"})
	}
	return true
}

// aclEntryView 序列化条目（附 subject 名称解析，供前端展示）。
func (h *Handler) aclEntryView(e acl.Entry, folderSpace space.Space) gin.H {
	name := ""
	switch {
	case e.SubjectType == acl.SubjectUser && h.users != nil:
		if n, err := h.users.Username(e.SubjectID); err == nil {
			name = n
		}
	case e.SubjectType == acl.SubjectSpace:
		if e.SubjectID == folderSpace.ID {
			name = folderSpace.Name
		} else if h.spaces != nil {
			if sp, err := h.spaces.Get(e.SubjectID); err == nil {
				name = sp.Name
			}
		}
	}
	return gin.H{
		"subject_type": e.SubjectType, "subject_id": e.SubjectID, "subject_name": name,
		"effect": e.Effect, "permissions": e.Permissions, "created_at": e.CreatedAt,
	}
}

// getFolderACL GET /api/v1/folders/:id/acl：返回目录的路径级 ACL 条目
// （含 subject 名称解析）。仅文件夹所在空间 owner/admin 或系统 admin 可读；
// 非授权 403。
func (h *Handler) getFolderACL(c *gin.Context) {
	if h.acl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "acl service unavailable"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	f, entries, err := h.acl.List(id)
	if aclError(c, err) {
		return
	}
	sp, err := h.spaces.Get(f.SpaceID)
	if spaceError(c, err) {
		return
	}
	actor := userID(c)
	if !h.canManageFolderACL(actor, sp.ID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to manage this folder acl"})
		return
	}
	items := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		items = append(items, h.aclEntryView(e, sp))
	}
	c.JSON(http.StatusOK, gin.H{"folder_id": f.ID, "entries": items})
}

type aclEntryRequest struct {
	SubjectType string   `json:"subject_type"`
	SubjectID   string   `json:"subject_id"`
	Effect      string   `json:"effect"`
	Permissions []string `json:"permissions"`
}

type aclPutRequest struct {
	Entries []aclEntryRequest `json:"entries"`
}

// replaceFolderACL PUT /api/v1/folders/:id/acl：整体替换目录条目数组
// （空数组即清空）。仅文件夹所在空间 owner/admin 或系统 admin。
// 审计 acl.update。
func (h *Handler) replaceFolderACL(c *gin.Context) {
	if h.acl == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "acl service unavailable"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req aclPutRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	// 先读目录校验空间作用域与管理权（Replace 内部会再校验一遍作用域）。
	f, _, err := h.acl.List(id)
	if aclError(c, err) {
		return
	}
	sp, err := h.spaces.Get(f.SpaceID)
	if spaceError(c, err) {
		return
	}
	actor := userID(c)
	if !h.canManageFolderACL(actor, sp.ID) {
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to manage this folder acl"})
		return
	}
	inputs := make([]acl.EntryInput, 0, len(req.Entries))
	for _, e := range req.Entries {
		subjectID, ok := parseID(c, e.SubjectID)
		if !ok {
			return
		}
		inputs = append(inputs, acl.EntryInput{
			SubjectType: e.SubjectType, SubjectID: subjectID,
			Effect: e.Effect, Permissions: e.Permissions,
		})
	}
	if _, err := h.acl.Replace(id, inputs, actor); aclError(c, err) {
		return
	}
	uid := actor
	h.recordAudit(c, audit.Entry{UserID: &uid, Action: "acl.update", ResourceType: audit.ResourceFolder, ResourceID: id.String(), Metadata: `{"entries":` + strconv.Itoa(len(inputs)) + `}`})
	// 回读新条目返回（与 GET 同构，前端可直接刷新视图）。
	_, entries, err := h.acl.List(id)
	if aclError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(entries))
	for _, e := range entries {
		items = append(items, h.aclEntryView(e, sp))
	}
	c.JSON(http.StatusOK, gin.H{"folder_id": id, "entries": items})
}
