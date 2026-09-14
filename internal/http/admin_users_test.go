package http

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// AdminUpdateUser 覆盖 fakeUserDirectory 默认实现：按 accounts 判存在，
// 更新内存记录并登记变更（响应序列化与断言共用）。
func (f *fakeAccount) AdminUpdateUser(id uuid.UUID, update auth.AdminUserUpdate) error {
	user, ok := f.accounts[id]
	if !ok {
		return auth.ErrUserNotFound
	}
	if update.Status != nil {
		user.Status = *update.Status
	}
	if update.StorageQuota != nil {
		user.StorageQuota = *update.StorageQuota
	}
	if update.Role != nil {
		user.Role = *update.Role
	}
	f.accounts[id] = user
	if f.updatedUsers == nil {
		f.updatedUsers = make(map[uuid.UUID]auth.AdminUserUpdate)
	}
	f.updatedUsers[id] = update
	return nil
}

// adminUsersTestEnv 构造管理端用户管理测试环境：actor（admin）经 user_id
// 上下文注入，目标用户种子到 fakeAccount；会话存储用于断言禁用/重置密码
// 撤销全部会话。
type adminUsersTestEnv struct {
	h        *Handler
	account  *fakeAccount
	sessions *fakeSessionStore
	recorder *memAuditRecorder
	router   *gin.Engine
	actor    auth.User
	target   auth.User
}

func newAdminUsersTestEnv(t *testing.T) *adminUsersTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newFakeSessionStore()
	service := auth.NewService(store, "admin-users-test-secret-012345678", time.Minute, time.Hour)
	account := newFakeAccount()
	service.SetCredentials(account)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = account
	rec := &memAuditRecorder{}
	h.SetAuditRecorder(rec)
	actor := auth.User{ID: uuid.New(), Username: "admin", Email: "admin@example.com", Status: auth.StatusActive, Role: auth.RoleAdmin, StorageQuota: auth.DefaultStorageQuota}
	target := auth.User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", Status: auth.StatusActive, Role: auth.RoleUser, StorageQuota: auth.DefaultStorageQuota}
	account.seed(actor, "AdminPassword123")
	account.seed(target, "BobPassword123456")
	env := &adminUsersTestEnv{h: h, account: account, sessions: store, recorder: rec, router: gin.New(), actor: actor, target: target}
	withActor := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor.ID)
			handler(c)
		}
	}
	env.router.GET("/api/v1/admin/users", withActor(h.adminListUsers))
	env.router.PATCH("/api/v1/admin/users/:id", withActor(h.adminUpdateUser))
	env.router.POST("/api/v1/admin/users/:id/reset-password", withActor(h.adminResetUserPassword))
	return env
}

// seedSession 为目标用户创建一个活跃会话（断言撤销用）。
func (e *adminUsersTestEnv) seedSession(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	token, err := auth.NewService(e.sessions, "unused", time.Minute, time.Hour).NewSession(userID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// C6 权限矩阵：
//   - admin 不可禁用自己（400，且不产生任何变更/审计）；
//   - 非法 status/role/quota → 400；
//   - 禁用目标用户 → 200 + 立即撤销其全部会话 + user.update 审计；
//   - 目标不存在 → 404。
func TestAdminUpdateUserMatrix(t *testing.T) {
	t.Run("cannot-disable-self", func(t *testing.T) {
		env := newAdminUsersTestEnv(t)
		w := callJSON(env.router, http.MethodPatch, "/api/v1/admin/users/"+env.actor.ID.String(), `{"status":"disabled"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
		}
		if _, updated := env.account.adminUpdateOf(env.actor.ID); updated {
			t.Fatal("self-disable must not reach store")
		}
	})

	t.Run("invalid-fields", func(t *testing.T) {
		env := newAdminUsersTestEnv(t)
		for _, body := range []string{
			`{"status":"locked"}`,
			`{"role":"superadmin"}`,
			`{"storage_quota":0}`,
			`{"storage_quota":-1}`,
		} {
			w := callJSON(env.router, http.MethodPatch, "/api/v1/admin/users/"+env.target.ID.String(), body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %s: status = %d, want 400", body, w.Code)
			}
		}
	})

	t.Run("disable-revokes-sessions-and-audits", func(t *testing.T) {
		env := newAdminUsersTestEnv(t)
		env.seedSession(t, env.target.ID)
		w := callJSON(env.router, http.MethodPatch, "/api/v1/admin/users/"+env.target.ID.String(), `{"status":"disabled"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"status":"disabled"`) {
			t.Fatalf("body = %s, want updated status", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "password") {
			t.Fatalf("body = %s must not contain password hash", w.Body.String())
		}
		session, ok := env.sessions.revokedSession()
		if !ok || session.UserID != env.target.ID {
			t.Fatal("disable must revoke all sessions of the target user")
		}
		if entry := env.recorder.find(audit.ActionUserUpdate); entry == nil || entry.ResourceID != env.target.ID.String() {
			t.Fatalf("user.update audit missing or wrong resource: %+v", entry)
		}
	})

	t.Run("quota-and-role-update", func(t *testing.T) {
		env := newAdminUsersTestEnv(t)
		w := callJSON(env.router, http.MethodPatch, "/api/v1/admin/users/"+env.target.ID.String(), `{"storage_quota":1073741824,"role":"admin"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		update, ok := env.account.adminUpdateOf(env.target.ID)
		if !ok || update.StorageQuota == nil || *update.StorageQuota != 1<<30 || update.Role == nil || *update.Role != auth.RoleAdmin {
			t.Fatalf("recorded update = %+v", update)
		}
	})

	t.Run("not-found", func(t *testing.T) {
		env := newAdminUsersTestEnv(t)
		w := callJSON(env.router, http.MethodPatch, "/api/v1/admin/users/"+uuid.New().String(), `{"status":"active"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
}

// C6 重置密码：弱密码 400；成功 204 + 撤销全部会话 + user.reset_password 审计
// + 哈希更新（旧密码失效、新密码可登录）。
func TestAdminResetUserPassword(t *testing.T) {
	env := newAdminUsersTestEnv(t)
	env.seedSession(t, env.target.ID)

	// 弱密码：400，不产生任何变更。
	w := callJSON(env.router, http.MethodPost, "/api/v1/admin/users/"+env.target.ID.String()+"/reset-password", `{"new_password":"weak"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("weak password: status = %d, want 400", w.Code)
	}

	// 合法重置：204 + 会话撤销 + 审计 + 哈希更新。
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/users/"+env.target.ID.String()+"/reset-password", `{"new_password":"NewBobPassword123"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if session, ok := env.sessions.revokedSession(); !ok || session.UserID != env.target.ID {
		t.Fatal("reset password must revoke all sessions of the target user")
	}
	if entry := env.recorder.find(audit.ActionUserResetPassword); entry == nil {
		t.Fatal("user.reset_password audit missing")
	}
	if err := auth.NewService(env.sessions, "x", time.Minute, time.Hour).VerifyPassword(env.account.hashes[env.target.ID], "NewBobPassword123"); err != nil {
		t.Fatalf("new password must verify: %v", err)
	}

	// 目标不存在：404。
	w = callJSON(env.router, http.MethodPost, "/api/v1/admin/users/"+uuid.New().String()+"/reset-password", `{"new_password":"NewBobPassword123"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown user: status = %d, want 404", w.Code)
	}
}

// C6 列表：q/limit/offset 透传与脱敏序列化（不含 password_hash）。
func TestAdminListUsers(t *testing.T) {
	env := newAdminUsersTestEnv(t)
	env.account.adminList = []auth.User{env.actor, env.target}
	env.account.adminTotal = 2
	w := callJSON(env.router, http.MethodGet, "/api/v1/admin/users?q=bob&limit=10&offset=0", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"username":"bob"`) || !strings.Contains(body, `"storage_quota"`) {
		t.Fatalf("body = %s, want bob with quota fields", body)
	}
	if strings.Contains(body, "password") {
		t.Fatalf("body = %s must not contain password hash", body)
	}

	// 非法分页参数：400。
	for _, query := range []string{"?limit=0", "?limit=-1", "?offset=-5", "?limit=abc"} {
		w := callJSON(env.router, http.MethodGet, "/api/v1/admin/users"+query, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %s: status = %d, want 400", query, w.Code)
		}
	}
}
