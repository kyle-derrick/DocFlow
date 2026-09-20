package http

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/space"
)

type spaceRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) updateSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req spaceRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if spaceError(c, h.spaces.Update(userID(c), id, req.Name, req.Description)) {
		return
	}
	sp, err := h.spaces.Get(id)
	if spaceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, spaceJSON(sp))
}

// spacePatchRequest PATCH /spaces/:id：name/description 至少其一；
// quota_bytes 为 *int64（nil 不改；0=不限）。
type spacePatchRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	QuotaBytes  *int64  `json:"quota_bytes"`
}

// patchSpace PATCH /api/v1/spaces/:id：name/desc/quota（owner/admin；
// quota 不可超 space.max_quota，系统 admin 越权不限）。
func (h *Handler) patchSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req spacePatchRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	if req.Name != nil || req.Description != nil {
		name := ""
		if req.Name != nil {
			name = *req.Name
		}
		desc := ""
		if req.Description != nil {
			desc = *req.Description
		}
		// 未提供的字段保持现值：回读当前值后整体提交。
		current, err := h.spaces.Get(id)
		if spaceError(c, err) {
			return
		}
		if req.Name == nil {
			name = current.Name
		}
		if req.Description == nil {
			desc = current.Description
		}
		if spaceError(c, h.spaces.Update(actor, id, name, desc)) {
			return
		}
	}
	if req.QuotaBytes != nil {
		isSysAdmin := false
		if h.roles != nil {
			if role, rerr := h.roles.Role(actor); rerr == nil && role == auth.RoleAdmin {
				isSysAdmin = true
			}
		}
		if spaceError(c, h.spaces.UpdateQuota(actor, id, *req.QuotaBytes, isSysAdmin)) {
			return
		}
	}
	sp, err := h.spaces.Get(id)
	if spaceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, spaceJSON(sp))
}

func (h *Handler) deleteSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if spaceError(c, h.spaces.Delete(userID(c), id)) {
		return
	}
	// 审计：space.delete（仅 owner 可达此处；失败不阻塞删除结果）。
	h.recordSpaceAudit(c, audit.ActionSpaceDelete, id, audit.StatusSuccess, "")
	c.Status(http.StatusNoContent)
}

// leaveSpace POST /api/v1/spaces/:id/leave：非 owner 直接成员主动退出空间
// （owner 须先转让所有权或解散，403）。
func (h *Handler) leaveSpace(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if spaceError(c, h.spaces.Leave(userID(c), id)) {
		return
	}
	h.recordSpaceAudit(c, audit.ActionSpaceLeave, id, audit.StatusSuccess, "")
	c.Status(http.StatusNoContent)
}

// recordSpaceAudit 空间生命周期审计（解散/退出/转让/组变更）：失败静默
// （审计不可用不阻断业务结果）。
func (h *Handler) recordSpaceAudit(c *gin.Context, action string, spaceID uuid.UUID, status, metadata string) {
	if h.audit == nil {
		return
	}
	actor := userID(c)
	_ = h.audit.Record(audit.Entry{
		UserID:       &actor,
		Action:       action,
		ResourceType: audit.ResourceSpace,
		ResourceID:   spaceID.String(),
		Status:       status,
		Metadata:     metadata,
	})
}

// spaceJSON 序列化空间安全字段。
func spaceJSON(sp space.Space) gin.H {
	return gin.H{"id": sp.ID, "name": sp.Name, "description": sp.Description, "quota_bytes": sp.QuotaBytes, "owner_id": sp.OwnerID, "is_default": sp.IsDefault, "created_at": sp.CreatedAt}
}

// spaceError 统一映射空间服务错误：非成员/非管理者的资源一律 404 或 403，
// 不泄露存在性细节。
func spaceError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, space.ErrInvalidName), errors.Is(err, space.ErrInvalidRole),
		errors.Is(err, space.ErrInvalidQuota), errors.Is(err, space.ErrInvalidGroupID):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, space.ErrQuotaTooLarge):
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota exceeds the system maximum", "code": "QUOTA_TOO_LARGE"})
	case errors.Is(err, space.ErrTooManySpaces):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "space limit reached for this user", "code": "SPACE_LIMIT_REACHED"})
	case errors.Is(err, space.ErrDefaultSpace):
		c.JSON(http.StatusBadRequest, gin.H{"error": "default space cannot be deleted", "code": "DEFAULT_SPACE"})
	case errors.Is(err, space.ErrMemberExists), errors.Is(err, space.ErrGroupExists):
		c.JSON(http.StatusConflict, gin.H{"error": "user or group is already a member"})
	case errors.Is(err, space.ErrGroupNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
	case errors.Is(err, space.ErrOwnerMember):
		c.JSON(http.StatusForbidden, gin.H{"error": "space owner membership cannot be removed"})
	case errors.Is(err, space.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to access this space"})
	case errors.Is(err, space.ErrNotDissolved):
		c.JSON(http.StatusConflict, gin.H{"error": "space is not dissolved; dissolve it first", "code": "NOT_DISSOLVED"})
	case errors.Is(err, space.ErrNotFound), errors.Is(err, files.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "space not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "space operation failed"})
	}
	return true
}

// createSpace POST /api/v1/spaces {name, description}：创建空间（配额 =
// space.default_quota；校验 space.max_per_user），创建者自动成为 owner
// 成员并生成空间根目录（事务内）。
func (h *Handler) createSpace(c *gin.Context) {
	var req spaceRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	owner := userID(c)
	sp, root, err := h.spaces.CreateSpace(owner, req.Name, req.Description)
	if spaceError(c, err) {
		return
	}
	out := spaceJSON(sp)
	out["root_folder_id"] = root.ID
	c.JSON(http.StatusCreated, out)
}

// listSpaces GET /api/v1/spaces：当前用户可见的空间（直接成员或用户组）。
// 每条附带 my_role（五级内置，取最高）/ member_count / storage_used /
// quota_bytes（空间页卡片数据；单 SQL 聚合，无 N+1）。
func (h *Handler) listSpaces(c *gin.Context) {
	infos, err := h.spaces.ListSpacesDetailed(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list spaces"})
		return
	}
	items := make([]gin.H, 0, len(infos))
	for _, info := range infos {
		item := spaceJSON(info.Space)
		item["my_role"] = info.MyRole
		item["member_count"] = info.MemberCount
		item["storage_used"] = info.StorageUsed
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"spaces": items})
}

type spaceMemberRequest struct {
	UserID string `json:"user_id"`
	// Role 五级内置角色（admin/member_share/member/guest；owner 经转让产生）。
	Role string `json:"role"`
}

// memberJSON 序列化成员安全字段：role（owner/admin/member_share/member/guest）
// 与 username/nickname/email（成员列表 JOIN users 补齐，展示替代 UUID）。
func memberJSON(m space.Member) gin.H {
	out := gin.H{"user_id": m.UserID, "role": m.Role, "created_at": m.CreatedAt, "joined_at": m.CreatedAt}
	if m.Username != "" {
		out["username"] = m.Username
	}
	if m.Nickname != "" {
		out["nickname"] = m.Nickname
	}
	if m.Email != "" {
		out["email"] = m.Email
	}
	return out
}

// addSpaceMember POST /api/v1/spaces/:id/members {user_id, role}：
// 空间 owner/admin 可添加成员；角色为五级内置（admin 仅 owner 可授予）。
func (h *Handler) addSpaceMember(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req spaceMemberRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	memberUserID, ok := parseID(c, req.UserID)
	if !ok {
		return
	}
	m, err := h.spaces.AddMember(userID(c), spaceID, memberUserID, req.Role)
	if spaceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, memberJSON(m))
}

// updateSpaceMember PATCH /api/v1/spaces/:id/members/:uid {role}：
// owner/admin 可改成员角色（五级内置下拉）；owner 成员不可改（403）。
func (h *Handler) updateSpaceMember(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	memberUserID, ok := parseID(c, c.Param("uid"))
	if !ok {
		return
	}
	var req spaceMemberRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	m, err := h.spaces.UpdateMemberRole(userID(c), spaceID, memberUserID, req.Role)
	if spaceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, memberJSON(m))
}

// removeSpaceMember DELETE /api/v1/spaces/:id/members/:uid：owner/admin 可移除
// 成员；owner 成员不可移除。
func (h *Handler) removeSpaceMember(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	memberUserID, ok := parseID(c, c.Param("uid"))
	if !ok {
		return
	}
	if spaceError(c, h.spaces.RemoveMember(userID(c), spaceID, memberUserID)) {
		return
	}
	c.Status(http.StatusNoContent)
}

// listSpaceMembers GET /api/v1/spaces/:id/members：空间直接成员列表（须为
// 空间成员，直接或经用户组）+ group_users（经用户组加入的用户条目，供
// 成员列表合并展示与转让 owner 候选；无组授权时为空数组）。
func (h *Handler) listSpaceMembers(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	members, err := h.spaces.ListMembers(userID(c), spaceID)
	if spaceError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(members))
	for _, m := range members {
		items = append(items, memberJSON(m))
	}
	groupUsers := []gin.H{}
	if entries, err := h.spaces.ListGroupUsers(userID(c), spaceID); err == nil {
		for _, gu := range entries {
			entry := gin.H{"user_id": gu.UserID, "group_id": gu.GroupID, "group_role": gu.GroupRole, "group_name": gu.GroupName}
			if gu.Username != "" {
				entry["username"] = gu.Username
			}
			if gu.Nickname != "" {
				entry["nickname"] = gu.Nickname
			}
			if gu.Email != "" {
				entry["email"] = gu.Email
			}
			groupUsers = append(groupUsers, entry)
		}
	}
	c.JSON(http.StatusOK, gin.H{"members": items, "group_users": groupUsers})
}

// ---- 空间用户组成员（space_group_members，migration 040） ----

type spaceGroupRequest struct {
	GroupID string `json:"group_id"`
	// Role 组内全体用户在空间的角色（admin/member_share/member/guest）。
	Role string `json:"role"`
}

// spaceGroupJSON 序列化空间用户组条目（group_id/group_name/role/member_count）。
func spaceGroupJSON(g space.GroupMember) gin.H {
	out := gin.H{"group_id": g.GroupID, "role": g.Role, "created_at": g.CreatedAt}
	if g.GroupName != "" {
		out["group_name"] = g.GroupName
	}
	if g.MemberCount > 0 {
		out["member_count"] = g.MemberCount
	}
	return out
}

// listSpaceGroups GET /api/v1/spaces/:id/groups：空间的用户组授权列表
// （actor 须为空间成员）。
func (h *Handler) listSpaceGroups(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	groups, err := h.spaces.ListGroups(userID(c), spaceID)
	if spaceError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(groups))
	for _, g := range groups {
		items = append(items, spaceGroupJSON(g))
	}
	c.JSON(http.StatusOK, gin.H{"groups": items})
}

// addSpaceGroup POST /api/v1/spaces/:id/groups {group_id, role}：owner/admin
// 把用户组加入空间（组内全体按角色参与权限判定；admin 角色仅 owner 可授予）。
func (h *Handler) addSpaceGroup(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req spaceGroupRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	groupID, ok := parseID(c, req.GroupID)
	if !ok {
		return
	}
	g, err := h.spaces.AddGroup(userID(c), spaceID, groupID, req.Role)
	if spaceError(c, err) {
		return
	}
	h.recordSpaceAudit(c, audit.ActionSpaceGroupAdd, spaceID, audit.StatusSuccess,
		`{"group_id":"`+groupID.String()+`","role":"`+g.Role+`"}`)
	c.JSON(http.StatusCreated, spaceGroupJSON(g))
}

// updateSpaceGroup PATCH /api/v1/spaces/:id/groups/:gid {role}：owner/admin
// 修改用户组角色（边界同 addSpaceGroup）。
func (h *Handler) updateSpaceGroup(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	groupID, ok := parseID(c, c.Param("gid"))
	if !ok {
		return
	}
	var req spaceGroupRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	g, err := h.spaces.UpdateGroupRole(userID(c), spaceID, groupID, req.Role)
	if spaceError(c, err) {
		return
	}
	h.recordSpaceAudit(c, audit.ActionSpaceGroupUpdate, spaceID, audit.StatusSuccess,
		`{"group_id":"`+groupID.String()+`","role":"`+g.Role+`"}`)
	c.JSON(http.StatusOK, spaceGroupJSON(g))
}

// removeSpaceGroup DELETE /api/v1/spaces/:id/groups/:gid：owner/admin 移除
// 用户组授权。
func (h *Handler) removeSpaceGroup(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	groupID, ok := parseID(c, c.Param("gid"))
	if !ok {
		return
	}
	if spaceError(c, h.spaces.RemoveGroup(userID(c), spaceID, groupID)) {
		return
	}
	h.recordSpaceAudit(c, audit.ActionSpaceGroupRemove, spaceID, audit.StatusSuccess,
		`{"group_id":"`+groupID.String()+`"}`)
	c.Status(http.StatusNoContent)
}

type transferRequest struct {
	UserID string `json:"user_id"`
}

// transferSpaceOwnership POST /api/v1/spaces/:id/transfer-ownership {user_id}：
// 仅 owner 可把空间所有权转让给既有直接成员（新 owner 角色置 owner、
// 原 owner 降为 admin，事务内完成）。
func (h *Handler) transferSpaceOwnership(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req transferRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	newOwner, ok := parseID(c, req.UserID)
	if !ok {
		return
	}
	if spaceError(c, h.spaces.TransferOwnership(userID(c), spaceID, newOwner)) {
		return
	}
	sp, err := h.spaces.Get(spaceID)
	if spaceError(c, err) {
		return
	}
	// 审计：space.owner_transfer（metadata 记录新 owner；仅 owner 可达）。
	h.recordSpaceAudit(c, audit.ActionSpaceOwnerTransfer, spaceID, audit.StatusSuccess,
		`{"new_owner":"`+newOwner.String()+`"}`)
	c.JSON(http.StatusOK, spaceJSON(sp))
}

// spaceFolderLimit 解析空间列举的 limit 查询参数（默认 100，上限 1000）。
func spaceFolderLimit(c *gin.Context) (int, bool) {
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return 0, false
		}
		if n < 1000 {
			limit = n
		} else {
			limit = 1000
		}
	}
	return limit, true
}

// requireSpaceRead 校验 actor 对空间的读权限（五级内置角色任意成员）。
// 无读权限按 404 处理（不泄露空间存在性）。返回 true 表示校验通过
// （false 时已写响应）。
func (h *Handler) requireSpaceRead(c *gin.Context, spaceID, actor uuid.UUID) bool {
	if _, err := h.spaces.Get(spaceID); err != nil {
		spaceError(c, err)
		return false
	}
	ok, err := h.spaces.CanRead(actor, spaceID)
	if err != nil {
		spaceError(c, err)
		return false
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "space not found"})
		return false
	}
	return true
}

// listSpaceFiles GET /api/v1/spaces/:id/files?parent_id=：列出空间根目录或
// 子目录，成员可读；非成员 404（不泄露空间存在性）。parent_id 缺省时返回
// 空间根目录内容。tag_id/starred 过滤（目录范围内）与 sort/order 排序可选。
func (h *Handler) listSpaceFiles(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	if !h.requireSpaceRead(c, spaceID, actor) {
		return
	}
	limit, ok := spaceFolderLimit(c)
	if !ok {
		return
	}
	tagID, ok := h.parseTagFilter(c)
	if !ok {
		return
	}
	starred, ok := parseStarredFilter(c)
	if !ok {
		return
	}
	sortOpt, ok := parseSortQuery(c)
	if !ok {
		return
	}
	var parent files.File
	if raw := c.Query("parent_id"); raw != "" {
		parentID, ok := parseID(c, raw)
		if !ok {
			return
		}
		var err error
		parent, err = h.files.GetSpaceFolder(spaceID, parentID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	} else {
		var err error
		parent, err = h.files.SpaceRoot(spaceID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	}
	out, err := h.files.ListSpace(spaceID, parent.ID, limit, files.SpaceListFilter{TagID: tagID, Starred: starred, SortOptions: sortOpt})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list files"})
		return
	}
	items := make([]gin.H, 0, len(out))
	for _, f := range out {
		items = append(items, fileJSON(f))
	}
	c.JSON(http.StatusOK, gin.H{"files": items, "parent_id": parent.ID})
}

type spaceFolderRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

// createSpaceFolder POST /api/v1/spaces/:id/folders {name, parent_id?}：
// 在空间根目录（缺省）或指定目录下建目录；写权限由 CreateFolderIn 内的
// CanWrite 判定（owner/admin/member_share/member，guest 403）。
func (h *Handler) createSpaceFolder(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req spaceFolderRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	if !h.requireSpaceRead(c, spaceID, actor) {
		return
	}
	var parent files.File
	if req.ParentID == "" {
		var err error
		parent, err = h.files.SpaceRoot(spaceID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	} else {
		var parentID uuid.UUID
		parentID, ok = parseID(c, req.ParentID)
		if !ok {
			return
		}
		var err error
		parent, err = h.files.GetSpaceFolder(spaceID, parentID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	}
	f, err := h.files.CreateFolderIn(actor, parent.ID, req.Name)
	if h.fileError(c, err) {
		return
	}
	if f.SpaceID != spaceID {
		// 防御：parent 必属于该空间，此处不应发生。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "folder scope mismatch"})
		return
	}
	setETag(c, f)
	c.JSON(http.StatusCreated, fileJSON(f))
}

// ---- 空间邀请（space_invites，migration 040） ----

type spaceInviteRequest struct {
	Email string `json:"email"`
	// Role 五级内置可授予角色（admin/member_share/member/guest）。
	Role string `json:"role"`
}

// inviteJSON 序列化邀请（不含 token_hash），附派生状态。
func inviteJSON(v space.Invite) gin.H {
	return gin.H{
		"id":          v.ID,
		"email":       v.Email,
		"role":        v.Role,
		"invited_by":  v.InvitedBy,
		"status":      v.Status(time.Now().UTC()),
		"expires_at":  v.ExpiresAt,
		"accepted_at": v.AcceptedAt,
		"created_at":  v.CreatedAt,
	}
}

// inviteError 统一映射邀请服务错误。
func inviteError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, space.ErrInvalidEmail), errors.Is(err, space.ErrInvalidRole):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, space.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
	case errors.Is(err, space.ErrInviteGone):
		c.JSON(http.StatusGone, gin.H{"error": "invitation is no longer available"})
	case errors.Is(err, space.ErrEmailMismatch):
		c.JSON(http.StatusForbidden, gin.H{"error": "invitation email does not match your account"})
	case errors.Is(err, space.ErrForbidden), errors.Is(err, space.ErrMemberExists):
		spaceError(c, err)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invitation operation failed"})
	}
	return true
}

// createSpaceInvite POST /api/v1/spaces/:id/invites {email, role}：创建邮箱
// 邀请（owner/admin）。明文 token 仅本次响应可见一次（join_url 指向前端
// 路由 /spaces/join/<token>）；幂等命中既有邀请返回 200（无链接）。
func (h *Handler) createSpaceInvite(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req spaceInviteRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	inv, token, err := h.spaces.CreateInvite(actor, spaceID, req.Email, req.Role)
	if inviteError(c, err) {
		return
	}
	out := inviteJSON(inv)
	status := http.StatusCreated
	if token != "" {
		out["join_url"] = "/spaces/join/" + token
	} else {
		// 幂等命中既有邀请：明文 token 已不可恢复。
		status = http.StatusOK
	}
	h.recordSpaceAudit(c, audit.ActionInviteCreate, spaceID, audit.StatusSuccess,
		`{"email":"`+inv.Email+`","role":"`+inv.Role+`"}`)
	c.JSON(status, out)
}

// listSpaceInvites GET /api/v1/spaces/:id/invites：空间邀请列表（owner/admin），
// 每条附派生状态 pending/accepted/expired。
func (h *Handler) listSpaceInvites(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	list, err := h.spaces.ListInvites(userID(c), spaceID)
	if inviteError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, v := range list {
		items = append(items, inviteJSON(v))
	}
	c.JSON(http.StatusOK, gin.H{"invites": items})
}

// revokeSpaceInvite DELETE /api/v1/spaces/:id/invites/:iid：撤销邀请（删行，
// token 立即失效；owner/admin）。
func (h *Handler) revokeSpaceInvite(c *gin.Context) {
	spaceID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	inviteID, ok := parseID(c, c.Param("iid"))
	if !ok {
		return
	}
	actor := userID(c)
	if inviteError(c, h.spaces.RevokeInvite(actor, spaceID, inviteID)) {
		return
	}
	h.recordSpaceAudit(c, audit.ActionInviteRevoke, spaceID, audit.StatusSuccess, "")
	c.Status(http.StatusNoContent)
}

// acceptSpaceInvite POST /api/v1/space-invites/join/:token：登录用户凭一次性
// token 接受空间邀请（邮箱须匹配）。成功返回空间信息与 already_member
// 标记（已在空间时邀请被消费，不视为错误）。
func (h *Handler) acceptSpaceInvite(c *gin.Context) {
	user := userID(c)
	u, err := h.users.GetByID(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load user"})
		return
	}
	sp, inv, already, err := h.spaces.AcceptInvite(user, c.Param("token"), u.Email)
	if inviteError(c, err) {
		return
	}
	h.recordSpaceAudit(c, audit.ActionInviteCreate, sp.ID, audit.StatusSuccess,
		`{"accepted":true,"email":"`+inv.Email+`"}`)
	c.JSON(http.StatusOK, gin.H{"space": spaceJSON(sp), "already_member": already, "role": inv.Role})
}
