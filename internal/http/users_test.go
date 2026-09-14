package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeUserDirectory 是 userDirectory 的内存实现，记录 Lookup 调用参数供断言。
type fakeUserDirectory struct {
	lookupQ     string
	lookupLimit int
	results     []auth.User
	lookupErr   error
	names       map[uuid.UUID]string
	// status 为 Status 查询的返回值（默认返回 not found 错误）。
	status    string
	statusErr error
	// identifier 命中的用户与错误（FindActiveByIdentifier 用）。
	identifierUser auth.User
	identifierErr  error
	// 管理端 / 档案 / 锁定计数记录。
	adminList    []auth.User
	adminTotal   int64
	updatedUsers map[uuid.UUID]auth.AdminUserUpdate
	failures     map[uuid.UUID]int
	cleared      map[uuid.UUID]bool
	profiles     map[uuid.UUID]auth.ProfileUpdate
}

func (f *fakeUserDirectory) FindActiveByIdentifier(identifier string) (auth.User, error) {
	if f.identifierErr != nil {
		return auth.User{}, f.identifierErr
	}
	if f.identifierUser.ID == uuid.Nil {
		return auth.User{}, errors.New("user not found")
	}
	return f.identifierUser, nil
}

// adminUpdateOf 返回某用户最近一次管理端更新（未更新过时 ok=false）。
func (f *fakeUserDirectory) adminUpdateOf(id uuid.UUID) (auth.AdminUserUpdate, bool) {
	if f.updatedUsers == nil {
		return auth.AdminUserUpdate{}, false
	}
	u, ok := f.updatedUsers[id]
	return u, ok
}

// failureCountOf 返回某用户累计记录的登录失败次数。
func (f *fakeUserDirectory) failureCountOf(id uuid.UUID) int { return f.failures[id] }

func (f *fakeUserDirectory) RecordLoginFailure(id uuid.UUID, maxRetries int, lockFor time.Duration) error {
	if f.failures == nil {
		f.failures = make(map[uuid.UUID]int)
	}
	f.failures[id]++
	return nil
}

func (f *fakeUserDirectory) ClearLoginFailures(id uuid.UUID) error {
	if f.cleared == nil {
		f.cleared = make(map[uuid.UUID]bool)
	}
	f.cleared[id] = true
	return nil
}

func (f *fakeUserDirectory) GetByID(id uuid.UUID) (auth.User, error) {
	if f.identifierUser.ID == id {
		return f.identifierUser, nil
	}
	return auth.User{}, auth.ErrUserNotFound
}

func (f *fakeUserDirectory) UpdateProfile(id uuid.UUID, update auth.ProfileUpdate) error {
	if f.profiles == nil {
		f.profiles = make(map[uuid.UUID]auth.ProfileUpdate)
	}
	f.profiles[id] = update
	return nil
}

func (f *fakeUserDirectory) AdminListUsers(q string, limit, offset int) ([]auth.User, int64, error) {
	if offset > 0 && offset < len(f.adminList) {
		return f.adminList[offset:], f.adminTotal, nil
	}
	if offset >= len(f.adminList) {
		return nil, f.adminTotal, nil
	}
	return f.adminList, f.adminTotal, nil
}

func (f *fakeUserDirectory) AdminUpdateUser(id uuid.UUID, update auth.AdminUserUpdate) error {
	if f.updatedUsers == nil {
		f.updatedUsers = make(map[uuid.UUID]auth.AdminUserUpdate)
	}
	if _, exists := f.updatedUsers[id]; !exists && f.identifierUser.ID != id && f.adminTotal == 0 {
		return auth.ErrUserNotFound
	}
	f.updatedUsers[id] = update
	return nil
}

func (f *fakeUserDirectory) Lookup(q string, limit int) ([]auth.User, error) {
	f.lookupQ, f.lookupLimit = q, limit
	return f.results, f.lookupErr
}
func (f *fakeUserDirectory) Username(id uuid.UUID) (string, error) {
	if n, ok := f.names[id]; ok {
		return n, nil
	}
	return "", errors.New("user not found")
}
func (f *fakeUserDirectory) Status(uuid.UUID) (string, error) {
	if f.statusErr != nil {
		return "", f.statusErr
	}
	if f.status != "" {
		return f.status, nil
	}
	return "", errors.New("user not found")
}

// fakeSessionStore 是 auth.SessionStore 的内存实现（含旧 hash 重放检测，
// 语义与 internal/auth 的 GormSessionStore/fakeStore 一致），供 refresh
// 处理器测试断言撤销行为。
type fakeSessionStore struct {
	sessions map[string]auth.Session
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: make(map[string]auth.Session)}
}

func (f *fakeSessionStore) Create(session auth.Session, info auth.SessionInfo) error {
	f.sessions[session.RefreshTokenHash] = session
	return nil
}

// ListActive / RevokeByID 与 GormSessionStore 语义一致（会话管理端点测试使用）。
func (f *fakeSessionStore) ListActive(userID uuid.UUID, now time.Time) ([]auth.SessionView, error) {
	seen := make(map[uuid.UUID]bool)
	var out []auth.SessionView
	for _, session := range f.sessions {
		if session.UserID == userID && session.RevokedAt == nil && session.ExpiresAt.After(now) && !seen[session.ID] {
			seen[session.ID] = true
			out = append(out, auth.SessionView{ID: session.ID, CreatedAt: session.CreatedAt, LastActiveAt: session.LastActiveAt, ExpiresAt: session.ExpiresAt, IP: session.IP, UserAgent: session.UserAgent})
		}
	}
	return out, nil
}

func (f *fakeSessionStore) RevokeByID(owner, id uuid.UUID, now time.Time) (bool, error) {
	for hash, session := range f.sessions {
		if session.ID == id && session.UserID == owner && session.RevokedAt == nil {
			session.RevokedAt = &now
			f.sessions[hash] = session
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeSessionStore) writeBack(session auth.Session) {
	for hash, existing := range f.sessions {
		if existing.ID == session.ID {
			f.sessions[hash] = session
		}
	}
	f.sessions[session.RefreshTokenHash] = session
}

func (f *fakeSessionStore) Rotate(oldHash, newHash string, now, expiresAt time.Time) (auth.Session, error) {
	session, ok := f.sessions[oldHash]
	if !ok || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return auth.Session{}, auth.ErrInvalidRefreshToken
	}
	if session.RefreshTokenHash != oldHash {
		session.RevokedAt = &now
		f.writeBack(session)
		return auth.Session{}, auth.ErrInvalidRefreshToken
	}
	session.RefreshTokenHash, session.LastActiveAt, session.ExpiresAt = newHash, now, expiresAt
	f.writeBack(session)
	return session, nil
}

func (f *fakeSessionStore) Revoke(hash string, now time.Time) error {
	session, ok := f.sessions[hash]
	if !ok || session.RevokedAt != nil {
		return auth.ErrInvalidRefreshToken
	}
	session.RevokedAt = &now
	f.writeBack(session)
	return nil
}

// RevokeAllForUser 与 GormSessionStore 语义一致：撤销 user 的全部未撤销
// 会话，exceptHash 非空时保留对应会话。
func (f *fakeSessionStore) RevokeAllForUser(userID uuid.UUID, exceptHash string, now time.Time) error {
	for hash, session := range f.sessions {
		if session.UserID == userID && session.RevokedAt == nil && hash != exceptHash {
			session.RevokedAt = &now
			f.writeBack(session)
		}
	}
	return nil
}

// revokedSession 返回任意已被撤销的 session（用于断言撤销确实发生）。
func (f *fakeSessionStore) revokedSession() (auth.Session, bool) {
	for _, session := range f.sessions {
		if session.RevokedAt != nil {
			return session, true
		}
	}
	return auth.Session{}, false
}

func lookupContext(query string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/users/lookup?q="+url.QueryEscape(query), nil)
	return c, w
}

// 空 q（缺失或纯空白）返回 400。
func TestLookupUsersEmptyQueryRejected(t *testing.T) {
	h := &Handler{users: &fakeUserDirectory{}}
	for _, q := range []string{"", "   "} {
		c, w := lookupContext(q)
		h.lookupUsers(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("q=%q status = %d, want 400", q, w.Code)
		}
	}
}

// 响应只含 id 与 username（绝不返回 email），查询原样透传给目录层。
func TestLookupUsersReturnsIDAndUsernameOnly(t *testing.T) {
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: "x"}
	bob := auth.User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", PasswordHash: "y"}
	fake := &fakeUserDirectory{results: []auth.User{alice, bob}}
	h := &Handler{users: fake}

	c, w := lookupContext("ali")
	h.lookupUsers(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"username":"alice"`) || !strings.Contains(body, `"username":"bob"`) {
		t.Fatalf("body = %s, want usernames alice/bob", body)
	}
	for _, forbidden := range []string{"email", "alice@example.com", "password", "status"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("body %q must not contain %q (enumeration/privacy)", body, forbidden)
		}
	}
	if fake.lookupQ != "ali" {
		t.Fatalf("lookup q = %q, want ali", fake.lookupQ)
	}
	if fake.lookupLimit != 10 {
		t.Fatalf("lookup limit = %d, want 10", fake.lookupLimit)
	}
}

// 无匹配返回空数组（非 null），目录层错误返回 500。
func TestLookupUsersEmptyAndError(t *testing.T) {
	h := &Handler{users: &fakeUserDirectory{results: nil}}
	c, w := lookupContext("zzz")
	h.lookupUsers(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty result body = %q, want []", w.Body.String())
	}

	h = &Handler{users: &fakeUserDirectory{lookupErr: errors.New("db down")}}
	c, w = lookupContext("a")
	h.lookupUsers(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("error status = %d, want 500", w.Code)
	}
}

// q 含 @（邮箱查询）时原样透传给目录层（精确匹配策略在 store 层实现），
// HTTP 层不做任何预分支，避免泄漏匹配语义差异。
func TestLookupUsersEmailQueryPassedThrough(t *testing.T) {
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com"}
	fake := &fakeUserDirectory{results: []auth.User{alice}}
	h := &Handler{users: fake}
	c, w := lookupContext("alice@example.com")
	h.lookupUsers(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if fake.lookupQ != "alice@example.com" {
		t.Fatalf("lookup q = %q, want unchanged email query", fake.lookupQ)
	}
}

// 用户不存在（或非 active）：走 dummy bcrypt 等耗比较路径，仍统一返回 401。
func TestLoginUnknownUserReturnsUniform401(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = &fakeUserDirectory{} // FindActiveByEmail 恒返回错误
	router := gin.New()
	router.POST("/api/v1/auth/login", h.login)
	body := `{"email":"nobody@example.com","password":"Whatever12345"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid credentials") {
		t.Fatalf("body = %s, want invalid credentials", w.Body.String())
	}
}

func refreshTestHandler(t *testing.T, users userDirectory) (*Handler, *fakeSessionStore, string) {
	t.Helper()
	store := newFakeSessionStore()
	service := auth.NewService(store, "http-refresh-test-secret-0123456789ab", time.Minute, time.Hour)
	token, err := service.NewSession(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = users
	return h, store, token
}

func postRefresh(h *Handler, token string) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST("/api/v1/auth/refresh", h.refresh)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: token})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// active 用户 refresh 成功：返回 access_token 并轮换 refresh cookie。
func TestRefreshActiveUserSucceeds(t *testing.T) {
	h, _, token := refreshTestHandler(t, &fakeUserDirectory{status: auth.StatusActive})
	w := postRefresh(h, token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("body = %s, want access_token", w.Body.String())
	}
	var rotated bool
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("response must set rotated refresh_token cookie")
	}
}

// 非 active（disabled/查询失败）：401 且撤销刚轮换出的 session；自动锁定
// （C9，locked_until 未到期派生 status=locked）：423 ACCOUNT_LOCKED 且同样撤销。
func TestRefreshNonActiveUserSessionRevoked(t *testing.T) {
	for _, tc := range []struct {
		name       string
		users      *fakeUserDirectory
		wantStatus int
	}{
		{name: "disabled", users: &fakeUserDirectory{status: auth.StatusDisabled}, wantStatus: http.StatusUnauthorized},
		{name: "locked", users: &fakeUserDirectory{status: auth.StatusLocked}, wantStatus: http.StatusLocked},
		{name: "lookup-error", users: &fakeUserDirectory{statusErr: errors.New("db down")}, wantStatus: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, token := refreshTestHandler(t, tc.users)
			w := postRefresh(h, token)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusLocked && !strings.Contains(w.Body.String(), `"code":"ACCOUNT_LOCKED"`) {
				t.Fatalf("locked body = %s, want ACCOUNT_LOCKED code", w.Body.String())
			}
			session, ok := store.revokedSession()
			if !ok {
				t.Fatal("session must be revoked when user is not active")
			}
			if session.RevokedAt == nil {
				t.Fatal("revoked_at must be set")
			}
		})
	}
}

// refresh/logout 共享独立按 IP 轻限流（60/min）：超出后 429（带 Retry-After）。
// 同源校验（C10）：请求带与 Host 一致的 Origin（httptest 默认 Host=example.com）。
func TestRefreshLogoutRateLimitedPerIP(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	router := gin.New()
	h.Register(router, "rate-limit-test-secret-0123456789abcdef", 1_000_000, 1_000_000, 1_000_000)
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "203.0.113.9:1111"
		req.Header.Set("Origin", "http://example.com")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < authOpsRateLimitPerMin; i++ {
		if w := post("/api/v1/auth/refresh"); w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: code = %d, want 401 (no cookie)", i+1, w.Code)
		}
	}
	w := post("/api/v1/auth/refresh")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("request %d: code = %d, want 429", authOpsRateLimitPerMin+1, w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must include Retry-After")
	}
	// logout 与 refresh 共用同一限流实例（同 IP 计数合并）。
	if w := post("/api/v1/auth/logout"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("logout after exhausted bucket: code = %d, want 429", w.Code)
	}
	// 其他 IP 不受影响。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.RemoteAddr = "203.0.113.10:1111"
	req.Header.Set("Origin", "http://example.com")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("different IP logout: code = %d, want 204", w2.Code)
	}
}
