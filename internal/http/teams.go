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
}

// addTeamMember POST /api/v1/teams/:id/members {user_id, role}：仅团队 owner 可添加成员。
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
	c.JSON(http.StatusCreated, gin.H{"user_id": m.UserID, "role": m.Role, "created_at": m.CreatedAt})
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
		items = append(items, gin.H{"user_id": m.UserID, "role": m.Role, "created_at": m.CreatedAt})
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

// listTeamFiles GET /api/v1/teams/:id/files?parent_id=：列出团队根目录或子目录，成员可读；
// 非成员 404（不泄露团队存在性）。parent_id 缺省时返回团队根目录内容。
func (h *Handler) listTeamFiles(c *gin.Context) {
	teamID, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	actor := userID(c)
	if _, err := h.teams.Get(teamID); err != nil {
		teamError(c, err)
		return
	}
	role, err := h.teams.Role(teamID, actor)
	if err != nil {
		teamError(c, err)
		return
	}
	if role == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}
	limit, ok := teamFolderLimit(c)
	if !ok {
		return
	}
	var parent files.File
	if raw := c.Query("parent_id"); raw != "" {
		parentID, ok := parseID(c, raw)
		if !ok {
			return
		}
		parent, err = h.files.Get(actor, parentID)
		if err != nil {
			// 团队目录不以 actor 为 owner，改按团队作用域查询。
			var ferr error
			parent, ferr = h.files.GetTeamFolder(teamID, parentID)
			if ferr != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
				return
			}
		}
		if parent.TeamID == nil || *parent.TeamID != teamID || parent.Type != "folder" {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	} else {
		parent, err = h.files.TeamRoot(teamID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
	}
	out, err := h.files.ListTeam(teamID, parent.ID, limit)
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
// 在团队根目录（缺省）或指定目录下建目录，editor 及以上角色可写（viewer 403）。
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
	if _, err := h.teams.Get(teamID); err != nil {
		teamError(c, err)
		return
	}
	role, err := h.teams.Role(teamID, actor)
	if err != nil {
		teamError(c, err)
		return
	}
	if role == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}
	var parent files.File
	if req.ParentID == "" {
		parent, err = h.files.TeamRoot(teamID)
	} else {
		var parentID uuid.UUID
		parentID, ok = parseID(c, req.ParentID)
		if !ok {
			return
		}
		parent, err = h.files.GetTeamFolder(teamID, parentID)
	}
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
		return
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
