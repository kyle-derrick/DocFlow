package http

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/group"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// groupsTestEnv 构造用户组管理测试环境：内存组服务 + fakeAccount 用户目录
// （成员存在性校验），actor（admin）经 user_id 上下文注入。
type groupsTestEnv struct {
	h        *Handler
	svc      *group.Service
	account  *fakeAccount
	recorder *memAuditRecorder
	router   *gin.Engine
	actor    auth.User
	member   auth.User
}

func newGroupsTestEnv(t *testing.T) *groupsTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := auth.NewService(newFakeSessionStore(), "groups-test-secret-0123456789", time.Minute, time.Hour)
	account := newFakeAccount()
	service.SetCredentials(account)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = account
	svc := group.NewService(group.NewMemoryStore())
	h.SetGroups(svc)
	rec := &memAuditRecorder{}
	h.SetAuditRecorder(rec)
	actor := auth.User{ID: uuid.New(), Username: "admin", Email: "admin@example.com", Status: auth.StatusActive, Role: auth.RoleAdmin, StorageQuota: auth.DefaultStorageQuota}
	member := auth.User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", Status: auth.StatusActive, Role: auth.RoleUser, StorageQuota: auth.DefaultStorageQuota}
	account.seed(actor, "AdminPassword123")
	account.seed(member, "BobPassword123456")
	env := &groupsTestEnv{h: h, svc: svc, account: account, recorder: rec, router: gin.New(), actor: actor, member: member}
	withActor := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor.ID)
			handler(c)
		}
	}
	env.router.GET("/api/v1/admin/groups", withActor(h.adminListGroups))
	env.router.POST("/api/v1/admin/groups", withActor(h.adminCreateGroup))
	env.router.PATCH("/api/v1/admin/groups/:id", withActor(h.adminUpdateGroup))
	env.router.DELETE("/api/v1/admin/groups/:id", withActor(h.adminDeleteGroup))
	env.router.GET("/api/v1/admin/groups/:id/members", withActor(h.adminListGroupMembers))
	env.router.POST("/api/v1/admin/groups/:id/members", withActor(h.adminAddGroupMember))
	env.router.DELETE("/api/v1/admin/groups/:id/members/:uid", withActor(h.adminRemoveGroupMember))
	env.router.GET("/api/v1/admin/users", withActor(h.adminListUsers))
	return env
}

// 组 CRUD 矩阵：非法名/重名 400/409；改名与描述生效；删除后 404；写审计。
func TestAdminGroupsCRUD(t *testing.T) {
	env := newGroupsTestEnv(t)

	// 非法请求体（缺 name）与非法名。
	for _, body := range []string{`{"description":"d"}`, `{"name":"  "}`, `{"name":"` + string([]byte{0x01}) + `"}`} {
		w := callJSON(env.router, http.MethodPost, "/api/v1/admin/groups", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400 (resp: %s)", body, w.Code, w.Body.String())
		}
	}

	w := callJSON(env.router, http.MethodPost, "/api/v1/admin/groups", `{"name":"研发组","description":"研发人员"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201 (resp: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"name":"研发组"`) {
		t.Fatalf("create body = %s", w.Body.String())
	}
	if entry := env.recorder.find(audit.ActionGroupCreate); entry == nil || entry.ResourceType != audit.ResourceGroup {
		t.Fatalf("group.create audit missing: %+v", entry)
	}

	// 重名 409。
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/groups", `{"name":"研发组"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate: status = %d, want 409", w.Code)
	}

	// 列表（含 member_count）。
	w = callJSON(env.router, http.MethodGet, "/api/v1/admin/groups", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"member_count":0`) {
		t.Fatalf("list: status = %d body = %s", w.Code, w.Body.String())
	}
	createdID := env.mustGroupID(t, "研发组")

	// PATCH：改名 + 改描述；均缺省为无操作。
	w = callJSON(env.router, http.MethodPatch, "/api/v1/admin/groups/"+createdID, `{"name":"平台组","description":"新描述"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"平台组"`) {
		t.Fatalf("update: status = %d body = %s", w.Code, w.Body.String())
	}
	w = callJSON(env.router, http.MethodPatch, "/api/v1/admin/groups/"+createdID, `{}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"平台组"`) {
		t.Fatalf("no-op update: status = %d body = %s", w.Code, w.Body.String())
	}

	// 不存在的组：404。
	w = callJSON(env.router, http.MethodPatch, "/api/v1/admin/groups/"+uuid.New().String(), `{"name":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown group update: status = %d, want 404", w.Code)
	}

	// 删除：204 + 审计；再删 404。
	w = callJSON(env.router, http.MethodDelete, "/api/v1/admin/groups/"+createdID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204", w.Code)
	}
	if entry := env.recorder.find(audit.ActionGroupDelete); entry == nil {
		t.Fatal("group.delete audit missing")
	}
	w = callJSON(env.router, http.MethodDelete, "/api/v1/admin/groups/"+createdID, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete twice: status = %d, want 404", w.Code)
	}
}

// 组成员管理：添加（用户不存在 404 / 重复 409）、列表、移除（404）、审计。
func TestAdminGroupMembers(t *testing.T) {
	env := newGroupsTestEnv(t)
	w := callJSON(env.router, http.MethodPost, "/api/v1/admin/groups", `{"name":"研发组"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d (resp: %s)", w.Code, w.Body.String())
	}
	id := env.mustGroupID(t, "研发组")

	// 非法 UUID / 用户不存在。
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/groups/"+id+"/members", `{"user_id":"not-a-uuid"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid: status = %d, want 400", w.Code)
	}
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/groups/"+id+"/members", `{"user_id":"`+uuid.New().String()+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown user: status = %d, want 404", w.Code)
	}

	// 添加成功（MemoryStore 无 users 表，username 展示列缺省——生产 GormStore
	// 经 JOIN users 回填，见 GormStore.ListMembers）。
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/groups/"+id+"/members", `{"user_id":"`+env.member.ID.String()+`"}`)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"user_id":"`+env.member.ID.String()+`"`) {
		t.Fatalf("add member: status = %d body = %s", w.Code, w.Body.String())
	}
	if entry := env.recorder.find(audit.ActionGroupMemberAdd); entry == nil || !strings.Contains(entry.Metadata, env.member.ID.String()) {
		t.Fatalf("group.member.add audit missing or wrong metadata: %+v", entry)
	}
	// 重复添加 409。
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/groups/"+id+"/members", `{"user_id":"`+env.member.ID.String()+`"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate member: status = %d, want 409", w.Code)
	}

	// 成员列表。
	w = callJSON(env.router, http.MethodGet, "/api/v1/admin/groups/"+id+"/members", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user_id":"`+env.member.ID.String()+`"`) {
		t.Fatalf("list members: status = %d body = %s", w.Code, w.Body.String())
	}

	// 移除成员；重复移除 404；写审计。
	w = callJSON(env.router, http.MethodDelete, "/api/v1/admin/groups/"+id+"/members/"+env.member.ID.String(), "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("remove member: status = %d, want 204", w.Code)
	}
	if entry := env.recorder.find(audit.ActionGroupMemberRemove); entry == nil {
		t.Fatal("group.member.remove audit missing")
	}
	w = callJSON(env.router, http.MethodDelete, "/api/v1/admin/groups/"+id+"/members/"+env.member.ID.String(), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("remove twice: status = %d, want 404", w.Code)
	}
}

// 用户列表 group_names 聚合：组成员关系按页回填到 GET /admin/users。
func TestAdminListUsersGroupNames(t *testing.T) {
	env := newGroupsTestEnv(t)
	for _, name := range []string{"g1", "g2"} {
		if _, err := env.svc.Create(name, "", env.actor.ID); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := env.svc.List()
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups = %+v, err = %v", groups, err)
	}
	for _, g := range groups {
		if _, err := env.svc.AddMember(g.ID, env.member.ID); err != nil {
			t.Fatal(err)
		}
	}
	env.account.adminList = []auth.User{env.actor, env.member}
	env.account.adminTotal = 2
	w := callJSON(env.router, http.MethodGet, "/api/v1/admin/users", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list users: status = %d (resp: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"group_names":["g1","g2"]`) {
		t.Fatalf("body = %s, want g1/g2 aggregated for bob", body)
	}
}

// mustGroupID 按名称取组 ID（测试辅助）。
func (e *groupsTestEnv) mustGroupID(t *testing.T, name string) string {
	t.Helper()
	groups, err := e.svc.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == name {
			return g.ID.String()
		}
	}
	t.Fatalf("group %q not found", name)
	return ""
}
