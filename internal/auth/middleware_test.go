package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeRoleLookup 是 RoleLookup 的内存实现。
type fakeRoleLookup struct {
	roles map[uuid.UUID]string
	err   error
}

func (f *fakeRoleLookup) Role(id uuid.UUID) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	role, ok := f.roles[id]
	if !ok {
		return "", ErrUserNotFound
	}
	return role, nil
}

// requireRoleRequest 构造与生产一致的中间件链（RequireAccessToken → RequireRole(admin)）
// 并以 uid 的有效令牌发起 GET /admin。
func requireRoleRequest(secret string, lookup RoleLookup, token string) int {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/admin", RequireAccessToken(secret), RequireRole(RoleAdmin, lookup), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w.Code
}

func tokenFor(t *testing.T, secret string, uid uuid.UUID) string {
	t.Helper()
	svc := NewService(nil, secret, time.Minute, time.Hour)
	token, err := svc.AccessToken(uid)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// admin 角色放行，user 角色与用户不存在均 403。
func TestRequireRoleAllowsAdminAndRejectsOthers(t *testing.T) {
	const secret = "require-role-test-secret-0123456789ab"
	adminID, userID, ghostID := uuid.New(), uuid.New(), uuid.New()
	lookup := &fakeRoleLookup{roles: map[uuid.UUID]string{adminID: RoleAdmin, userID: RoleUser}}

	if code := requireRoleRequest(secret, lookup, tokenFor(t, secret, adminID)); code != http.StatusOK {
		t.Fatalf("admin status = %d, want 200", code)
	}
	if code := requireRoleRequest(secret, lookup, tokenFor(t, secret, userID)); code != http.StatusForbidden {
		t.Fatalf("user status = %d, want 403", code)
	}
	if code := requireRoleRequest(secret, lookup, tokenFor(t, secret, ghostID)); code != http.StatusForbidden {
		t.Fatalf("missing user status = %d, want 403", code)
	}
}

// 无令牌/无效令牌在 RequireAccessToken 即被 401，不会触达角色查询。
func TestRequireRoleRequiresAccessTokenFirst(t *testing.T) {
	const secret = "require-role-test-secret-0123456789ab"
	lookup := &fakeRoleLookup{}
	if code := requireRoleRequest(secret, lookup, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", code)
	}
	if code := requireRoleRequest(secret, lookup, "not-a-jwt"); code != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d, want 401", code)
	}
}

// 角色源故障返回 500（区别于权限不足的 403）；未注入角色源同样 500。
func TestRequireRoleLookupErrors(t *testing.T) {
	const secret = "require-role-test-secret-0123456789ab"
	uid := uuid.New()
	if code := requireRoleRequest(secret, &fakeRoleLookup{err: errors.New("db down")}, tokenFor(t, secret, uid)); code != http.StatusInternalServerError {
		t.Fatalf("lookup failure status = %d, want 500", code)
	}
	if code := requireRoleRequest(secret, nil, tokenFor(t, secret, uid)); code != http.StatusInternalServerError {
		t.Fatalf("nil lookup status = %d, want 500", code)
	}
}
