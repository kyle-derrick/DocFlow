package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	r.GET("/admin", RequireAccessToken(secret, nil), RequireRole(RoleAdmin, lookup), func(c *gin.Context) {
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

// bearerRequest 构造 RequireAccessToken（可注入 PAT 校验器）链并返回状态码
// 与 auth_kind（请求成功时）。
func bearerRequest(secret string, pat AccessTokenVerifier, token string) (int, string) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var kind string
	r.GET("/probe", RequireAccessToken(secret, pat), func(c *gin.Context) {
		value, _ := c.Get(AuthKindContextKey)
		kind, _ = value.(string)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w.Code, kind
}

// patBearerService 构造带内存 PAT 存储与给定状态用户的服务（PAT 路径依赖）。
func patBearerService(t *testing.T, userStatus string, uid uuid.UUID) (*Service, string) {
	t.Helper()
	hash, err := HashPassword("SomePassword123")
	if err != nil {
		t.Fatal(err)
	}
	user := User{ID: uid, Username: "alice", Email: "alice@example.com", PasswordHash: hash, Status: userStatus}
	service := NewService(newFakeStore(), "pat-bearer-test-secret-0123456789", time.Minute, time.Hour)
	service.SetCredentials(newFakeCredentials(user))
	service.SetTokenStore(newFakeTokenStore())
	token, plaintext, err := service.NewPersonalAccessToken(uid, "probe", 0)
	if err != nil {
		t.Fatal(err)
	}
	if token.UserID != uid {
		t.Fatalf("token owner = %v, want %v", token.UserID, uid)
	}
	return service, plaintext
}

// 双路径矩阵：JWT/PAT 均放行且 auth_kind 正确；过期/撤销/禁用/错误前缀/
// 哈希不匹配/畸形输入与未注入校验器一律 401。
func TestRequireAccessTokenDualPathMatrix(t *testing.T) {
	const secret = "pat-bearer-test-secret-0123456789"
	uid := uuid.New()

	// 基础：active 用户的 JWT 与 PAT。
	jwtToken := tokenFor(t, secret, uid)
	if code, kind := bearerRequest(secret, nil, jwtToken); code != http.StatusOK || kind != AuthKindJWT {
		t.Fatalf("jwt status=%d kind=%q, want 200 jwt", code, kind)
	}
	service, plaintext := patBearerService(t, StatusActive, uid)
	if code, kind := bearerRequest(secret, service, plaintext); code != http.StatusOK || kind != AuthKindPAT {
		t.Fatalf("pat status=%d kind=%q, want 200 pat", code, kind)
	}

	// 同前缀、明文被篡改（哈希不匹配）。
	tampered := plaintext[:len(plaintext)-2] + "zz"
	if tampered == plaintext {
		t.Fatal("tampered must differ")
	}
	if code, _ := bearerRequest(secret, service, tampered); code != http.StatusUnauthorized {
		t.Fatalf("tampered pat status = %d, want 401", code)
	}
	// 未知前缀 / 畸形（过短）/ 无 dfpat_ 前缀的垃圾串。
	if code, _ := bearerRequest(secret, service, PATPrefix+strings.Repeat("Q", 43)); code != http.StatusUnauthorized {
		t.Fatalf("unknown prefix status = %d, want 401", code)
	}
	if code, _ := bearerRequest(secret, service, PATPrefix+"short"); code != http.StatusUnauthorized {
		t.Fatalf("short pat status = %d, want 401", code)
	}
	if code, _ := bearerRequest(secret, service, "not-a-jwt-or-pat"); code != http.StatusUnauthorized {
		t.Fatalf("garbage status = %d, want 401", code)
	}

	// 撤销后拒绝。
	if ok, err := service.RevokePersonalAccessToken(uid, patTokenID(t, service, plaintext)); err != nil || !ok {
		t.Fatalf("revoke = %v %v", ok, err)
	}
	if code, _ := bearerRequest(secret, service, plaintext); code != http.StatusUnauthorized {
		t.Fatalf("revoked pat status = %d, want 401", code)
	}

	// 属主禁用：PAT 拒绝（JWT 仍有效——其短 TTL 语义不变）。
	disabledService, disabledToken := patBearerService(t, StatusDisabled, uid)
	if code, _ := bearerRequest(secret, disabledService, disabledToken); code != http.StatusUnauthorized {
		t.Fatalf("disabled owner pat status = %d, want 401", code)
	}
	if code, kind := bearerRequest(secret, disabledService, jwtToken); code != http.StatusOK || kind != AuthKindJWT {
		t.Fatalf("jwt under disabled probe status=%d kind=%q, want 200 jwt", code, kind)
	}

	// 未注入 PAT 校验器：dfpat_ 前缀一律 401（fail closed）。
	if code, _ := bearerRequest(secret, nil, plaintext); code != http.StatusUnauthorized {
		t.Fatalf("pat without verifier status = %d, want 401", code)
	}
}

// patTokenID 从内存存储按明文反查令牌行 ID（测试辅助）。
func patTokenID(t *testing.T, service *Service, plaintext string) uuid.UUID {
	t.Helper()
	store, _ := service.tokens.(*fakeTokenStore)
	if token, ok := store.byPlaintext(plaintext); ok {
		return token.ID
	}
	t.Fatal("token row not found by plaintext")
	return uuid.Nil
}
