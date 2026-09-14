package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/team"
)

// teamsTestEnv 构造挂内存团队服务的路由（聚焦业务语义，模式同 me_test.go）：
// actor 注入 user_id 上下文绕过 Bearer；注册成员/角色相关端点。
func teamsTestEnv(t *testing.T, actor uuid.UUID) (*gin.Engine, *team.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := team.NewService(team.NewMemoryStore())
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.teams = svc
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handler(c)
		}
	}
	router.POST("/api/v1/teams", withUser(h.createTeam))
	router.GET("/api/v1/teams/:id/members", withUser(h.listTeamMembers))
	router.POST("/api/v1/teams/:id/members", withUser(h.addTeamMember))
	router.PATCH("/api/v1/teams/:id/members/:uid", withUser(h.updateTeamMember))
	router.GET("/api/v1/teams/:id/roles", withUser(h.listRoles))
	router.POST("/api/v1/teams/:id/roles", withUser(h.createRole))
	router.DELETE("/api/v1/teams/:id/roles/:role_id", withUser(h.deleteRole))
	return router, svc
}

func callTeamsJSON(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestTeamMemberRoleEndpoints 覆盖成员自定义角色接线（设计 6.5 最小落地）：
// POST members 支持 role_id、PATCH members/:uid 改角色（系统↔自定义）、
// 列表返回 role/role_id/role_name、删除被引用角色 409、角色列表 member_count。
func TestTeamMemberRoleEndpoints(t *testing.T) {
	owner, member := uuid.New(), uuid.New()
	router, _ := teamsTestEnv(t, owner)

	// 建团队（创建者自动成为 owner 成员）。
	w := callTeamsJSON(router, http.MethodPost, "/api/v1/teams", `{"name":"契约团队"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create team: status = %d (body: %s)", w.Code, w.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/teams/" + created.ID

	// 建自定义角色（read/write，deny delete）。
	w = callTeamsJSON(router, http.MethodPost, base+"/roles", `{"name":"贡献者","permissions":{"read":true,"write":true,"deny":["delete"]}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create role: status = %d (body: %s)", w.Code, w.Body.String())
	}
	var role struct {
		ID          string `json:"id"`
		MemberCount int64  `json:"member_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &role); err != nil {
		t.Fatal(err)
	}

	// POST members 带 role_id：绑定自定义角色。
	addBody := `{"user_id":"` + member.String() + `","role_id":"` + role.ID + `"}`
	w = callTeamsJSON(router, http.MethodPost, base+"/members", addBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("add member with role_id: status = %d (body: %s)", w.Code, w.Body.String())
	}
	var added struct {
		Role     string `json:"role"`
		RoleID   string `json:"role_id"`
		RoleName string `json:"role_name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Role != "custom" || added.RoleID != role.ID || added.RoleName != "贡献者" {
		t.Fatalf("added member = %+v, want custom role with name", added)
	}

	// 成员列表含 role_id/role_name。
	w = callTeamsJSON(router, http.MethodGet, base+"/members", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list members: status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"role_name":"贡献者"`) {
		t.Fatalf("member list body missing role_name: %s", w.Body.String())
	}

	// PATCH members/:uid：自定义角色 → viewer。
	w = callTeamsJSON(router, http.MethodPatch, base+"/members/"+member.String(), `{"role":"viewer"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update member role: status = %d (body: %s)", w.Code, w.Body.String())
	}
	var updated struct {
		Role   string `json:"role"`
		RoleID any    `json:"role_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Role != "viewer" || updated.RoleID != nil {
		t.Fatalf("updated member = %+v, want viewer with role_id cleared", updated)
	}

	// PATCH 回自定义角色 → 再删除引用检查。
	w = callTeamsJSON(router, http.MethodPatch, base+"/members/"+member.String(), `{"role_id":"`+role.ID+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update member to custom role: status = %d (body: %s)", w.Code, w.Body.String())
	}
	w = callTeamsJSON(router, http.MethodDelete, base+"/roles/"+role.ID, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("delete in-use role: status = %d, want 409 (body: %s)", w.Code, w.Body.String())
	}

	// 角色列表 member_count 反映引用数。
	w = callTeamsJSON(router, http.MethodGet, base+"/roles", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list roles: status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"member_count":1`) {
		t.Fatalf("roles body missing member_count=1: %s", w.Body.String())
	}
}
