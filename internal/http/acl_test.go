package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/acl"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/team"
)

// aclRoleLookup 是 auth.RoleLookup 的内存实现（admin 判定用）。
type aclRoleLookup struct{ admins map[uuid.UUID]bool }

func (l aclRoleLookup) Role(id uuid.UUID) (string, error) {
	if l.admins[id] {
		return auth.RoleAdmin, nil
	}
	return auth.RoleUser, nil
}

// aclTestEnv 构造挂内存 ACL/团队服务的路由（模式同 teams_test.go）；
// 返回路由、ACL 服务、底层内存 repo（构造目录数据用）与团队服务。
func aclTestEnv(t *testing.T, actor uuid.UUID) (*gin.Engine, *acl.Service, *acl.MemoryRepo, *team.Service, *fakeUserDirectory) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	teamSvc := team.NewService(team.NewMemoryStore())
	repo := acl.NewMemoryRepo()
	svc := acl.NewService(repo)
	users := &fakeUserDirectory{names: map[uuid.UUID]string{}}
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.teams = teamSvc
	h.acl = svc
	h.users = users
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handler(c)
		}
	}
	router.GET("/api/v1/folders/:id/acl", withUser(h.getFolderACL))
	router.PUT("/api/v1/folders/:id/acl", withUser(h.replaceFolderACL))
	return router, svc, repo, teamSvc, users
}

func callACL(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestFolderACLEndpoints 覆盖 GET/PUT /folders/:id/acl 的权限与校验：
// owner 200（含 subject 名称解析）、成员 403、个人空间 400、条目校验 400、
// 整体替换生效（含清空）、目录不存在 404、未配置 503。
func TestFolderACLEndpoints(t *testing.T) {
	owner, member := uuid.New(), uuid.New()
	router, _, repo, teamSvc, users := aclTestEnv(t, owner)
	users.names[member] = "张三"

	tm, root, err := teamSvc.CreateTeam(owner, "研发团队", "")
	if err != nil {
		t.Fatal(err)
	}
	// ACL repo 独立于团队 store：登记团队根目录（团队作用域目录）。
	repo.PutFolder(root)
	// 个人空间目录（400 用例）。
	personal := files.File{ID: uuid.New(), Name: "personal", OwnerID: owner, Type: "folder", ScopeType: "personal"}
	repo.PutFolder(personal)

	// owner 查看：空条目。
	w := callACL(router, http.MethodGet, "/api/v1/folders/"+root.ID.String()+"/acl", "")
	if w.Code != http.StatusOK {
		t.Fatalf("owner get: status = %d (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Fatalf("empty entries body: %s", w.Body.String())
	}

	// 整体替换：team deny write/delete + user allow read；返回解析后的
	// subject_name（团队成员用户名 / 团队名）。
	body := `{"entries":[
		{"subject_type":"team","subject_id":"` + tm.ID.String() + `","effect":"deny","permissions":["write","delete"]},
		{"subject_type":"user","subject_id":"` + member.String() + `","effect":"allow","permissions":["read"]}
	]}`
	w = callACL(router, http.MethodPut, "/api/v1/folders/"+root.ID.String()+"/acl", body)
	if w.Code != http.StatusOK {
		t.Fatalf("owner put: status = %d (body %s)", w.Code, w.Body.String())
	}
	for _, want := range []string{`"subject_name":"张三"`, `"subject_name":"研发团队"`, `"effect":"deny"`, `"effect":"allow"`, `"permissions":["write","delete"]`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("put body missing %s: %s", want, w.Body.String())
		}
	}

	// 回读持久化；空数组整体清空。
	w = callACL(router, http.MethodGet, "/api/v1/folders/"+root.ID.String()+"/acl", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"subject_name":"张三"`) {
		t.Fatalf("get after put: status = %d body %s", w.Code, w.Body.String())
	}
	w = callACL(router, http.MethodPut, "/api/v1/folders/"+root.ID.String()+"/acl", `{"entries":[]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Fatalf("clear: status = %d body %s", w.Code, w.Body.String())
	}

	// 校验：个人空间 400；非法权限词 400；重复主体 400；非 UUID subject 400；
	// 不存在的目录 404。
	if w := callACL(router, http.MethodGet, "/api/v1/folders/"+personal.ID.String()+"/acl", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("personal get: status = %d, want 400", w.Code)
	}
	if w := callACL(router, http.MethodPut, "/api/v1/folders/"+root.ID.String()+"/acl",
		`{"entries":[{"subject_type":"team","subject_id":"`+tm.ID.String()+`","effect":"allow","permissions":["admin"]}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid permission: status = %d, want 400", w.Code)
	}
	dup := `{"entries":[
		{"subject_type":"user","subject_id":"` + member.String() + `","effect":"allow","permissions":["read"]},
		{"subject_type":"user","subject_id":"` + member.String() + `","effect":"deny","permissions":["write"]}
	]}`
	if w := callACL(router, http.MethodPut, "/api/v1/folders/"+root.ID.String()+"/acl", dup); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate subject: status = %d, want 400", w.Code)
	}
	if w := callACL(router, http.MethodPut, "/api/v1/folders/"+root.ID.String()+"/acl",
		`{"entries":[{"subject_type":"user","subject_id":"not-a-uuid","effect":"allow","permissions":["read"]}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad subject id: status = %d, want 400", w.Code)
	}
	if w := callACL(router, http.MethodGet, "/api/v1/folders/"+uuid.New().String()+"/acl", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing folder: status = %d, want 404", w.Code)
	}

	// 成员（非 owner、无 roles 源）：GET/PUT 均 403。
	memberRouter, _, memberRepo, memberTeamSvc, _ := aclTestEnv(t, member)
	_, memberRoot, err := memberTeamSvc.CreateTeam(owner, "运营团队", "")
	if err != nil {
		t.Fatal(err)
	}
	memberRepo.PutFolder(memberRoot)
	if w := callACL(memberRouter, http.MethodGet, "/api/v1/folders/"+memberRoot.ID.String()+"/acl", ""); w.Code != http.StatusForbidden {
		t.Fatalf("member get: status = %d, want 403", w.Code)
	}
	if w := callACL(memberRouter, http.MethodPut, "/api/v1/folders/"+memberRoot.ID.String()+"/acl", body); w.Code != http.StatusForbidden {
		t.Fatalf("member put: status = %d, want 403", w.Code)
	}

	// 未注入 ACL 服务：503。
	gin.SetMode(gin.TestMode)
	bare := gin.New()
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	bare.GET("/api/v1/folders/:id/acl", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, owner)
		h.getFolderACL(c)
	})
	if w := callACL(bare, http.MethodGet, "/api/v1/folders/"+root.ID.String()+"/acl", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: status = %d, want 503", w.Code)
	}
}

// TestFolderACLAdminAllowed 系统 admin（h.roles 注入）即使非团队成员也可管理。
func TestFolderACLAdminAllowed(t *testing.T) {
	owner, admin := uuid.New(), uuid.New()
	_, _, repo, teamSvc, _ := aclTestEnv(t, owner)
	_, root, err := teamSvc.CreateTeam(owner, "运营团队", "")
	if err != nil {
		t.Fatal(err)
	}
	repo.PutFolder(root)

	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	handler.teams = teamSvc
	handler.acl = acl.NewService(repo)
	handler.roles = aclRoleLookup{admins: map[uuid.UUID]bool{admin: true}}
	r := gin.New()
	r.GET("/api/v1/folders/:id/acl", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, admin)
		handler.getFolderACL(c)
	})
	if w := callACL(r, http.MethodGet, "/api/v1/folders/"+root.ID.String()+"/acl", ""); w.Code != http.StatusOK {
		t.Fatalf("admin get: status = %d (body %s)", w.Code, w.Body.String())
	}
}
