package http

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

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
	c.Status(http.StatusNoContent)
}

type roleRequest struct {
	Name        string         `json:"name"`
	Permissions map[string]any `json:"permissions"`
}

func (h *Handler) listRoles(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	roles, err := h.teams.Roles(userID(c), id)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": roles})
}
func (h *Handler) createRole(c *gin.Context) {
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	var req roleRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	r, err := h.teams.CreateRole(userID(c), id, req.Name, req.Permissions)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, r)
}
func (h *Handler) updateRole(c *gin.Context) {
	tid, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	rid, ok := parseID(c, c.Param("role_id"))
	if !ok {
		return
	}
	var req roleRequest
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	if teamError(c, h.teams.UpdateRole(userID(c), tid, rid, req.Name, req.Permissions)) {
		return
	}
	c.Status(http.StatusNoContent)
}
func (h *Handler) deleteRole(c *gin.Context) {
	tid, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	rid, ok := parseID(c, c.Param("role_id"))
	if !ok {
		return
	}
	if teamError(c, h.teams.DeleteRole(userID(c), tid, rid)) {
		return
	}
	c.Status(http.StatusNoContent)
}

// teamJSON 序列化团队安全字段（不暴露 owner_id 之外的内部信息）。
func teamJSON(t team.Team) gin.H {
	return gin.H{"id": t.ID, "name": t.Name, "description": t.Description, "owner_id": t.OwnerID, "created_at": t.CreatedAt}
}

// teamError 统一映射团队服务错误：非成员/非 owner 的资源一律 404 或 403，不泄露存在性细节。
func teamError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, team.ErrInvalidName), errors.Is(err, team.ErrInvalidRole), errors.Is(err, team.ErrInvalidPermission):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, team.ErrNameConflict):
		c.JSON(http.StatusConflict, gin.H{"error": "team name already exists"})
	case errors.Is(err, team.ErrMemberExists):
		c.JSON(http.StatusConflict, gin.H{"error": "user is already a member"})
	case errors.Is(err, team.ErrRoleInUse):
		// 删除角色前须先改派引用该角色的成员（migration 029 另有 ON DELETE RESTRICT 兜底）。
		c.JSON(http.StatusConflict, gin.H{"error": "role is assigned to team members"})
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
func (h *Handler) listTeams(c *gin.Context) {
	teams, err := h.teams.ListTeams(userID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to list teams"})
		return
	}
	items := make([]gin.H, 0, len(teams))
	for _, t := range teams {
		items = append(items, teamJSON(t))
	}
	c.JSON(http.StatusOK, gin.H{"teams": items})
}

type teamMemberRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	// RoleID 自定义角色 ID（可选）：非空时成员绑定该自定义角色（role 归一为 custom）。
	RoleID string `json:"role_id"`
}

// parseMemberRole 解析成员角色入参：返回 (role, roleID)；role_id 非法 UUID 时 400。
func parseMemberRole(c *gin.Context, req teamMemberRequest) (string, *uuid.UUID, bool) {
	if req.RoleID == "" {
		return req.Role, nil, true
	}
	roleID, ok := parseID(c, req.RoleID)
	if !ok {
		return "", nil, false
	}
	return req.Role, &roleID, true
}

// memberJSON 序列化成员安全字段：role（owner/editor/viewer/custom）、
// role_id/role_name（自定义角色时返回，供前端展示与改派）、
// username/nickname（成员列表 JOIN users 补齐，展示用户名替代 UUID；
// 仅非空时返回，兼容 MemoryStore 等无用户数据的实现）。
func memberJSON(m team.Member) gin.H {
	out := gin.H{"user_id": m.UserID, "role": m.Role, "created_at": m.CreatedAt}
	if m.RoleID != nil {
		out["role_id"] = m.RoleID
	}
	if m.RoleName != "" {
		out["role_name"] = m.RoleName
	}
	if m.Username != "" {
		out["username"] = m.Username
	}
	if m.Nickname != "" {
		out["nickname"] = m.Nickname
	}
	return out
}

// addTeamMember POST /api/v1/teams/:id/members {user_id, role?, role_id?}：
// 仅团队 owner 可添加成员；角色为系统角色（editor/viewer）或自定义角色（role_id）。
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
	role, roleID, ok := parseMemberRole(c, req)
	if !ok {
		return
	}
	m, err := h.teams.AddMember(userID(c), teamID, memberUserID, role, roleID)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, memberJSON(m))
}

// updateTeamMember PATCH /api/v1/teams/:id/members/:uid {role?, role_id?}：
// 仅团队 owner 可改成员角色（系统角色或自定义角色）；owner 成员不可改（403）。
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
	role, roleID, ok := parseMemberRole(c, req)
	if !ok {
		return
	}
	m, err := h.teams.UpdateMemberRole(userID(c), teamID, memberUserID, role, roleID)
	if teamError(c, err) {
		return
	}
	c.JSON(http.StatusOK, memberJSON(m))
}

// removeTeamMember DELETE /api/v1/teams/:id/members/:uid：仅团队 owner 可移除成员；owner 成员不可移除。
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

// requireTeamRead 校验 actor 对团队空间的读权限（系统角色任意成员；自定义角色
// 按 read 勾选且未被 deny；角色行缺失 fail closed）。无读权限按 404 处理
// （不泄露团队存在性）。返回 true 表示校验通过（false 时已写响应）。
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
// CanWrite 判定（owner/editor/含 write 权限的自定义角色，viewer 403）。
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
