package http

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/team"
)

type teamRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Handler) updateTeam(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req teamRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if teamError(c, h.teams.Update(userID(c), id, req.Name, req.Description)) {
		return
	}
	t, err := h.teams.Get(id)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusOK, teamJSON(t))
}
func (h *Handler) deleteTeam(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if teamError(c, h.teams.Delete(userID(c), id)) {
		return
	}
	// 审计：team.delete（仅 owner 可达此处；失败不阻塞删除结果）。
	h.recordTeamAudit(c, audit.ActionTeamDelete, id, audit.StatusSuccess, "")
	c.Status(http.StatusNoContent)
}

// leaveTeam POST /api/v1/teams/:id/leave：非 owner 成员主动退出团队
//（owner 须先转让所有权或解散，403）。
func (h *Handler) leaveTeam(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	if teamError(c, h.teams.Leave(userID(c), id)) {
		return
	}
	h.recordTeamAudit(c, audit.ActionTeamLeave, id, audit.StatusSuccess, "")
	c.Status(http.StatusNoContent)
}

// recordTeamAudit 团队生命周期审计（解散/退出/转让）：失败静默（审计
// 不可用不阻断业务结果）。
func (h *Handler) recordTeamAudit(c *gin.Context, action string, teamID uuid.UUID, status, metadata string) {
	if h.audit == nil {
		return
	}
	actor := userID(c)
	_ = h.audit.Record(audit.Entry{
		UserID:       &actor,
		Action:       action,
		ResourceType: audit.ResourceTeam,
		ResourceID:   teamID.String(),
		Status:       status,
		Metadata:     metadata,
	})
}

// teamJSON 序列化团队安全字段（不暴露 owner_id 之外的内部信息）。
func teamJSON(t team.Team) gin.H {
	return gin.H{"id": t.ID, "name": t.Name, "description": t.Description, "owner_id": t.OwnerID, "created_at": t.CreatedAt}
}

// teamError 统一映射团队服务错误：非成员/非管理者的资源一律 404 或 403，不泄露存在性细节。
func teamError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, team.ErrInvalidName), errors.Is(err, team.ErrInvalidRole):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, team.ErrNameConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "team name already exists"})
	case errors.Is(err, team.ErrMemberExists):
		c.JSON(http.StatusConflict, gin.H{"error": "user is already a member"})
	case errors.Is(err, team.ErrOwnerMember):
		c.JSON(http.StatusForbidden, gin.H{"error": "team owner membership cannot be removed"})
	case errors.Is(err, team.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed to access this team"})
	case errors.Is(err, team.ErrNotFound), errors.Is(err, files.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "team operation failed"})
	}
	return true
}

// createTeam POST /api/v1/teams {name, description}：创建团队，
// 创建者自动成为 owner 成员并生成团队根目录（事务内）。
func (h *Handler) createTeam(c *gin.Context) {
	var req teamRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	owner := userID(c)
	t, root, err := h.teams.CreateTeam(owner, req.Name, req.Description)
	if teamError(c, err) {
		return
	}
	out := teamJSON(t)
	out["root_folder_id"] = root.ID
	c.JSON(http.StatusCreated, out)
}

// listTeams GET /api/v1/teams：当前用户所属（成员或 owner）的团队。
// v1.7 起每条附带 my_role（五级内置角色）/ member_count / storage_used
//（团队页卡片数据；单 SQL 聚合，无 N+1）。
func (h *Handler) listTeams(c *gin.Context) {
	infos, err := h.teams.ListTeamsDetailed(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list teams"})
		return
	}
	items := make([]gin.H, 0, len(infos))
	for _, info := range infos {
		item := teamJSON(info.Team)
		item["my_role"] = info.MyRole
		item["member_count"] = info.MemberCount
		item["storage_used"] = info.StorageUsed
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"teams": items})
}

type teamMemberRequest struct {
	UserID string `json:"user_id"`
	// Role 五级内置角色（admin/member_share/member/guest；owner 经转让产生）。
	Role string `json:"role"`
}

// memberJSON 序列化成员安全字段：role（owner/admin/member_share/member/guest）
// 与 username/nickname/email（成员列表 JOIN users 补齐，展示替代 UUID；
// 仅非空时返回，兼容 MemoryStore 等无用户数据的实现）。joined_at 即
// team_members.created_at（加入时间，v1.7.1 显式字段；created_at 兼容保留）。
func memberJSON(m team.Member) gin.H {
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

// addTeamMember POST /api/v1/teams/:id/members {user_id, role}：
// 团队 owner/admin 可添加成员；角色为五级内置（admin 仅 owner 可授予）。
func (h *Handler) addTeamMember(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req teamMemberRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	memberUserID, ok := parseID(c, req.UserID)
	if !ok {
		return
	}
	m, err := h.teams.AddMember(userID(c), teamID, memberUserID, req.Role)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, memberJSON(m))
}

// updateTeamMember PATCH /api/v1/teams/:id/members/:uid {role}：
// owner/admin 可改成员角色（五级内置下拉）；owner 成员不可改（403）。
func (h *Handler) updateTeamMember(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	memberUserID, ok := parseID(c, c.Param("uid"))
	if !ok {
		return
	}
	var req teamMemberRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	m, err := h.teams.UpdateMemberRole(userID(c), teamID, memberUserID, req.Role)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusOK, memberJSON(m))
}

// removeTeamMember DELETE /api/v1/teams/:id/members/:uid：owner/admin 可移除
// 成员；owner 成员不可移除。
func (h *Handler) removeTeamMember(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	memberUserID, ok := parseID(c, c.Param("uid"))
	if !ok {
		return
	}
	if teamError(c, h.teams.RemoveMember(userID(c), teamID, memberUserID)) {
		return
	}
	c.Status(http.StatusNoContent)
}

// listTeamMembers GET /api/v1/teams/:id/members：团队成员列表（须为团队成员）。
func (h *Handler) listTeamMembers(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	members, err := h.teams.ListMembers(userID(c), teamID)
	if teamError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(members))
	for _, m := range members {
		items = append(items, memberJSON(m))
	}
	c.JSON(http.StatusOK, gin.H{"members": items})
}

type transferRequest struct {
	UserID string `json:"user_id"`
}

// transferTeamOwnership POST /api/v1/teams/:id/transfer-ownership {user_id}：
// 仅 owner 可把团队所有权转让给既有成员（新 owner 角色置 owner、原 owner
// 降为 admin，事务内完成）。
func (h *Handler) transferTeamOwnership(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
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
	if teamError(c, h.teams.TransferOwnership(userID(c), teamID, newOwner)) {
		return
	}
	t, err := h.teams.Get(teamID)
	if teamError(c, err) {
		return
	}
	// 审计：team.owner_transfer（metadata 记录新 owner；仅 owner 可达）。
	h.recordTeamAudit(c, audit.ActionTeamOwnerTransfer, teamID, audit.StatusSuccess,
		`{"new_owner":"`+newOwner.String()+`"}`)
	c.JSON(http.StatusOK, teamJSON(t))
}

// teamFolderLimit 解析团队空间列举的 limit 查询参数（默认 100，上限 1000）。
func teamFolderLimit(c *gin.Context) (int, bool) {
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

// requireTeamRead 校验 actor 对团队空间的读权限（五级内置角色任意成员）。
// 无读权限按 404 处理（不泄露团队存在性）。返回 true 表示校验通过
//（false 时已写响应）。
func (h *Handler) requireTeamRead(c *gin.Context, teamID, actor uuid.UUID) bool {
	if _, err := h.teams.Get(teamID); err != nil {
		teamError(c, err)
		return false
	}
	ok, err := h.teams.CanRead(actor, teamID)
	if err != nil {
		teamError(c, err)
		return false
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return false
	}
	return true
}

// listTeamFiles GET /api/v1/teams/:id/files?parent_id=：列出团队根目录或子目录，成员可读；
// 非成员 404（不泄露团队存在性）。parent_id 缺省时返回团队根目录内容。
// tag_id/starred 过滤（目录范围内）与 sort/order 排序可选，语义同个人 /files。
func (h *Handler) listTeamFiles(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	if !h.requireTeamRead(c, teamID, actor) {
		return
	}
	limit, ok := teamFolderLimit(c)
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
		// 注意用 = 而非 :=：:= 会在 if 块内重新声明 parent（遮蔽外层零值），
		// 导致下方 ListTeam 拿到 uuid.Nil（子目录列表恒空）且响应 parent_id
		// 为全零 UUID（前端回填后上传报 file not found，v1.1.1 修复）。
		var err error
		parent, err = h.files.Get(actor, parentID)
		if err != nil {
			// 团队目录不以 actor 为 owner，改按团队作用域查询。
			parent, err = h.files.GetTeamFolder(teamID, parentID)
			if err != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
				return
			}
		}
		if parent.TeamID == nil || *parent.TeamID != teamID || parent.Type != "folder" {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	} else {
		var err error
		parent, err = h.files.TeamRoot(teamID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	}
	out, err := h.files.ListTeam(teamID, parent.ID, limit, files.TeamListFilter{TagID: tagID, Starred: starred, SortOptions: sortOpt})
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

type teamFolderRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

// createTeamFolder POST /api/v1/teams/:id/folders {name, parent_id?}：
// 在团队根目录（缺省）或指定目录下建目录；写权限由 CreateFolderIn 内的
// CanWrite 判定（owner/admin/member_share/member，guest 403）。
func (h *Handler) createTeamFolder(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req teamFolderRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	if !h.requireTeamRead(c, teamID, actor) {
		return
	}
	var parent files.File
	if req.ParentID == "" {
		var err error
		parent, err = h.files.TeamRoot(teamID)
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
		parent, err = h.files.GetTeamFolder(teamID, parentID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	}
	f, err := h.files.CreateFolderIn(actor, parent.ID, req.Name)
	if h.fileError(c, err) {
		return
	}
	if f.TeamID == nil || *f.TeamID != teamID {
		// 防御：parent 必属于该团队，此处不应发生。
		c.JSON(http.StatusInternalServerError, gin.H{"error": "folder scope mismatch"})
		return
	}
	setETag(c, f)
	c.JSON(http.StatusCreated, fileJSON(f))
}

// ---- 团队邀请（v1.7.1 成员管理完善；team_invites，migration 039） ----

type teamInviteRequest struct {
	Email string `json:"email"`
	// Role 五级内置可授予角色（admin/member_share/member/guest）。
	Role string `json:"role"`
}

// inviteJSON 序列化邀请（不含 token_hash），附派生状态。
func inviteJSON(v team.Invite) gin.H {
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
	case errors.Is(err, team.ErrInvalidEmail), errors.Is(err, team.ErrInvalidRole):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, team.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "invitation not found"})
	case errors.Is(err, team.ErrInviteGone):
		c.JSON(http.StatusGone, gin.H{"error": "invitation is no longer available"})
	case errors.Is(err, team.ErrEmailMismatch):
		c.JSON(http.StatusForbidden, gin.H{"error": "invitation email does not match your account"})
	case errors.Is(err, team.ErrForbidden), errors.Is(err, team.ErrMemberExists):
		teamError(c, err)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invitation operation failed"})
	}
	return true
}

// createTeamInvite POST /api/v1/teams/:id/invites {email, role}：创建邮箱
// 邀请（owner/admin）。明文 token 仅本次响应可见一次（join_url 指向前端
// 路由 /teams/join/<token>）；幂等命中既有邀请返回 200（无链接）。
func (h *Handler) createTeamInvite(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req teamInviteRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	actor := userID(c)
	inv, token, err := h.teams.CreateInvite(actor, teamID, req.Email, req.Role)
	if inviteError(c, err) {
		return
	}
	out := inviteJSON(inv)
	status := http.StatusCreated
	if token != "" {
		out["join_url"] = "/teams/join/" + token
	} else {
		// 幂等命中既有邀请：明文 token 已不可恢复。
		status = http.StatusOK
	}
	h.recordTeamAudit(c, audit.ActionInviteCreate, teamID, audit.StatusSuccess,
		`{"email":"`+inv.Email+`","role":"`+inv.Role+`"}`)
	c.JSON(status, out)
}

// listTeamInvites GET /api/v1/teams/:id/invites：团队邀请列表（owner/admin），
// 每条附派生状态 pending/accepted/expired。
func (h *Handler) listTeamInvites(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	list, err := h.teams.ListInvites(userID(c), teamID)
	if inviteError(c, err) {
		return
	}
	items := make([]gin.H, 0, len(list))
	for _, v := range list {
		items = append(items, inviteJSON(v))
	}
	c.JSON(http.StatusOK, gin.H{"invites": items})
}

// revokeTeamInvite DELETE /api/v1/teams/:id/invites/:iid：撤销邀请（删行，
// token 立即失效；owner/admin）。
func (h *Handler) revokeTeamInvite(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	inviteID, ok := parseID(c, c.Param("iid"))
	if !ok {
		return
	}
	actor := userID(c)
	if inviteError(c, h.teams.RevokeInvite(actor, teamID, inviteID)) {
		return
	}
	h.recordTeamAudit(c, audit.ActionInviteRevoke, teamID, audit.StatusSuccess, "")
	c.Status(http.StatusNoContent)
}

// acceptTeamInvite POST /api/v1/team-invites/join/:token：登录用户凭一次性
// token 接受团队邀请（邮箱须匹配）。成功返回团队信息与 already_member
// 标记（已在团队时邀请被消费，不视为错误）。
func (h *Handler) acceptTeamInvite(c *gin.Context) {
	user := userID(c)
	u, err := h.users.GetByID(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load user"})
		return
	}
	t, inv, already, err := h.teams.AcceptInvite(user, c.Param("token"), u.Email)
	if inviteError(c, err) {
		return
	}
	h.recordTeamAudit(c, audit.ActionInviteCreate, t.ID, audit.StatusSuccess,
		`{"accepted":true,"email":"`+inv.Email+`"}`)
	c.JSON(http.StatusOK, gin.H{"team": teamJSON(t), "already_member": already, "role": inv.Role})
}
