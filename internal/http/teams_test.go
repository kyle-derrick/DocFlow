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
// actor 注入 user_id 上下文绕过 Bearer；注册成员管理端点（v1.7 五级内置
// 角色，自定义角色 CRUD 已随 migration 037 移除）。
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
	router.GET("/api/v1/teams", withUser(h.listTeams))
	router.GET("/api/v1/teams/:id/members", withUser(h.listTeamMembers))
	router.POST("/api/v1/teams/:id/members", withUser(h.addTeamMember))
	router.PATCH("/api/v1/teams/:id/members/:uid", withUser(h.updateTeamMember))
	router.DELETE("/api/v1/teams/:id/members/:uid", withUser(h.removeTeamMember))
	router.POST("/api/v1/teams/:id/leave", withUser(h.leaveTeam))
	router.POST("/api/v1/teams/:id/transfer-ownership", withUser(h.transferTeamOwnership))
	router.DELETE("/api/v1/teams/:id", withUser(h.deleteTeam))
	return router, svc
}

func callTeamsJSON(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestTeamMemberBuiltinRoleEndpoints 覆盖五级内置角色接线（v1.7）：
// POST members 按内置角色添加、PATCH 改派、GET 列表、GET /teams 附带
// my_role/member_count/storage_used、转让所有权、成员退出。
func TestTeamMemberBuiltinRoleEndpoints(t *testing.T) {
	owner, member := uuid.New(), uuid.New()
	router, _ := teamsTestEnv(t, owner)

	// 建团队（创建者自动成为 owner 成员）。
	w := callTeamsJSON(router, http.MethodPost, "/api/v1/teams", `{"name":"契约团队","description":"v1.7"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create team: status = %d (body: %s)", w.Code, w.Body.String())
	}
	var created struct {
		ID           string `json:"id"`
		RootFolderID string `json:"root_folder_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.RootFolderID == "" {
		t.Fatal("create team response missing root_folder_id")
	}
	base := "/api/v1/teams/" + created.ID

	// POST members：guest（只读）。
	w = callTeamsJSON(router, http.MethodPost, base+"/members", `{"user_id":"`+member.String()+`","role":"guest"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add member: status = %d (body: %s)", w.Code, w.Body.String())
	}
	var added struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Role != "guest" {
		t.Fatalf("added member role = %q, want guest", added.Role)
	}

	// 旧角色（editor/viewer/custom）与 owner 均拒绝。
	for _, role := range []string{"editor", "viewer", "custom", "owner"} {
		w = callTeamsJSON(router, http.MethodPost, base+"/members", `{"user_id":"`+uuid.New().String()+`","role":"`+role+`"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("add member with legacy role %q: status = %d, want 400", role, w.Code)
		}
	}

	// PATCH members/:uid：guest → member_share。
	w = callTeamsJSON(router, http.MethodPatch, base+"/members/"+member.String(), `{"role":"member_share"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update member role: status = %d (body: %s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Role != "member_share" {
		t.Fatalf("updated role = %q, want member_share", added.Role)
	}

	// 成员列表返回五级角色。
	w = callTeamsJSON(router, http.MethodGet, base+"/members", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list members: status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"role":"member_share"`) || !strings.Contains(w.Body.String(), `"role":"owner"`) {
		t.Fatalf("member list body = %s", w.Body.String())
	}

	// GET /teams 附带 my_role/member_count/storage_used（团队页卡片）。
	w = callTeamsJSON(router, http.MethodGet, "/api/v1/teams", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list teams: status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"my_role":"owner"`, `"member_count":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("teams body missing %s: %s", want, body)
		}
	}

	// 转让所有权（仅 owner）：member 成为 owner，原 owner 降 admin。
	w = callTeamsJSON(router, http.MethodPost, base+"/transfer-ownership", `{"user_id":"`+member.String()+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("transfer ownership: status = %d (body: %s)", w.Code, w.Body.String())
	}
	w = callTeamsJSON(router, http.MethodGet, base+"/members", "")
	if !strings.Contains(w.Body.String(), `"role":"owner"`) || strings.Count(w.Body.String(), `"role":"admin"`) != 1 {
		t.Fatalf("members after transfer = %s", w.Body.String())
	}

	// 成员退出（非 owner）：owner（原成员）之外的旧 owner 退出。
	w = callTeamsJSON(router, http.MethodPost, base+"/leave", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("leave team: status = %d (body: %s)", w.Code, w.Body.String())
	}
}
