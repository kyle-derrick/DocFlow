package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/invite"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeAccount 同时实现 userDirectory 与 auth.Credentials（内存版），
// 供注册/改密/重置链路的 HTTP 层测试使用。
type fakeAccount struct {
	fakeUserDirectory
	accounts map[uuid.UUID]auth.User
	hashes   map[uuid.UUID]string
	emails   map[string]uuid.UUID
	names    map[string]uuid.UUID
}

func newFakeAccount() *fakeAccount {
	return &fakeAccount{
		accounts: make(map[uuid.UUID]auth.User),
		hashes:   make(map[uuid.UUID]string),
		emails:   make(map[string]uuid.UUID),
		names:    make(map[string]uuid.UUID),
	}
}

func (f *fakeAccount) seed(u auth.User, password string) {
	if password != "" {
		hash, _ := auth.HashPassword(password)
		u.PasswordHash = hash
		f.hashes[u.ID] = u.PasswordHash
	}
	f.accounts[u.ID] = u
	f.emails[u.Email] = u.ID
	f.names[u.Username] = u.ID
}

// FindActiveByEmail 覆盖 fakeUserDirectory 默认实现。
func (f *fakeAccount) FindActiveByEmail(email string) (auth.User, error) {
	id, ok := f.emails[auth.NormalizeEmail(email)]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return f.accounts[id], nil
}

// CreateUser 实现邀请注册的用户创建（唯一冲突 → auth.ErrUserExists）。
func (f *fakeAccount) CreateUser(u auth.User) error {
	if _, dup := f.emails[u.Email]; dup {
		return auth.ErrUserExists
	}
	if _, dup := f.names[u.Username]; dup {
		return auth.ErrUserExists
	}
	f.accounts[u.ID] = u
	f.hashes[u.ID] = u.PasswordHash
	f.emails[u.Email] = u.ID
	f.names[u.Username] = u.ID
	return nil
}

func (f *fakeAccount) GetByID(id uuid.UUID) (auth.User, error) {
	u, ok := f.accounts[id]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return u, nil
}

func (f *fakeAccount) UpdatePasswordHash(id uuid.UUID, passwordHash string) error {
	f.hashes[id] = passwordHash
	return nil
}

// newAccountTestHandler 构造装配了邀请服务、凭据源与重置令牌存储的 Handler。
func newAccountTestHandler(t *testing.T) (*Handler, *fakeAccount, *fakeSessionStore, *invite.MemoryStore) {
	t.Helper()
	store := newFakeSessionStore()
	service := auth.NewService(store, "accounts-test-secret-0123456789abcdef", time.Minute, time.Hour)
	account := newFakeAccount()
	service.SetCredentials(account)
	service.SetPasswordResetStore(newResetTokenStore())
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = account
	h.audit = audit.NopRecorder{}
	repo := invite.NewMemoryStore()
	h.SetInvites(invite.NewService(repo, account), nil, "")
	return h, account, store, repo
}

// newResetTokenStore 构造内存版一次性重置令牌存储。
func newResetTokenStore() *resetTokenStore {
	return &resetTokenStore{tokens: make(map[string]auth.PasswordResetToken)}
}

type resetTokenStore struct {
	tokens map[string]auth.PasswordResetToken
}

func (s *resetTokenStore) Create(t auth.PasswordResetToken) error {
	s.tokens[t.TokenHash] = t
	return nil
}

func (s *resetTokenStore) Consume(tokenHash string, now time.Time) (uuid.UUID, bool, error) {
	t, ok := s.tokens[tokenHash]
	if !ok || t.UsedAt != nil || !now.Before(t.ExpiresAt) {
		return uuid.Nil, false, nil
	}
	t.UsedAt = &now
	s.tokens[tokenHash] = t
	return t.UserID, true, nil
}

func postJSON(handler gin.HandlerFunc, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST(path, handler)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// 注册：凭有效邀请 token 注册成功，返回 access_token 并下发 refresh cookie。
func TestRegisterWithInvitation(t *testing.T) {
	h, account, _, repo := newAccountTestHandler(t)
	admin := uuid.New()
	_, token, err := h.invites.Create(admin, "new@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"token":"` + token + `","username":"newbie","password":"StrongPass123"}`
	w := postJSON(h.register, "/api/v1/auth/register", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("body = %s, want access_token (login-compatible)", w.Body.String())
	}
	var hasCookie bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "refresh_token" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatal("register must set refresh_token cookie")
	}
	// 邀请被标记接受；用户为 active 且邮箱继承邀请。
	list, err := h.invites.List(10)
	if err != nil || len(list) != 1 || list[0].AcceptedAt == nil {
		t.Fatalf("invitation must be accepted, list=%v err=%v", list, err)
	}
	user, err := account.FindActiveByEmail("new@example.com")
	if err != nil || user.Username != "newbie" || user.Status != auth.StatusActive {
		t.Fatalf("created user = %+v err=%v", user, err)
	}
	_ = repo

	// 同一 token 二次注册：410。
	w = postJSON(h.register, "/api/v1/auth/register", body)
	if w.Code != http.StatusGone {
		t.Fatalf("reuse status = %d, want 410", w.Code)
	}
}

// 注册错误映射：未知 token 404、弱密码 400、用户名冲突 409。
func TestRegisterErrors(t *testing.T) {
	h, account, _, _ := newAccountTestHandler(t)
	w := postJSON(h.register, "/api/v1/auth/register", `{"token":"unknown","username":"newbie","password":"StrongPass123"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown token status = %d, want 404", w.Code)
	}
	_, token, err := h.invites.Create(uuid.New(), "x@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	w = postJSON(h.register, "/api/v1/auth/register", `{"token":"`+token+`","username":"newbie","password":"weak"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("weak password status = %d, want 400", w.Code)
	}
	// 用户名被占用：409 且邀请保持待接受（可换名重试）。
	account.names["taken"] = uuid.New()
	w = postJSON(h.register, "/api/v1/auth/register", `{"token":"`+token+`","username":"taken","password":"StrongPass123"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", w.Code)
	}
	list, _ := h.invites.List(10)
	if len(list) != 1 || list[0].AcceptedAt != nil {
		t.Fatal("invitation must stay pending on username conflict")
	}
}

// 忘记密码：无论邮箱是否存在一律 202（防枚举）。
func TestForgotPasswordAlwaysAccepted(t *testing.T) {
	h, account, _, _ := newAccountTestHandler(t)
	user := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	account.seed(user, "OldPassword123")

	for _, email := range []string{"alice@example.com", "nobody@example.com", ""} {
		w := postJSON(h.forgotPassword, "/api/v1/auth/forgot-password", `{"email":"`+email+`"}`)
		if w.Code != http.StatusAccepted {
			t.Fatalf("email %q status = %d, want 202", email, w.Code)
		}
	}
}

// 重置密码：有效 token 204，无效/已用 token 400。
func TestResetPasswordEndpoint(t *testing.T) {
	h, account, store, _ := newAccountTestHandler(t)
	user := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	account.seed(user, "OldPassword123")
	if _, err := h.auth.NewSession(user.ID); err != nil {
		t.Fatal(err)
	}
	token, err := h.auth.RequestPasswordReset("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}

	w := postJSON(h.resetPassword, "/api/v1/auth/reset-password", `{"token":"bad","password":"NewPassword123"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad token status = %d, want 400", w.Code)
	}
	w = postJSON(h.resetPassword, "/api/v1/auth/reset-password", `{"token":"`+token+`","password":"NewPassword123"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", w.Code, w.Body.String())
	}
	// 全部会话撤销、哈希更新。
	for _, session := range store.sessions {
		if session.UserID == user.ID && session.RevokedAt == nil {
			t.Fatal("sessions must be revoked after reset")
		}
	}
	if err := h.auth.VerifyPassword(account.hashes[user.ID], "NewPassword123"); err != nil {
		t.Fatal("password hash must be updated")
	}
	// token 一次性：二次使用 400。
	w = postJSON(h.resetPassword, "/api/v1/auth/reset-password", `{"token":"`+token+`","password":"Another123456"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reuse status = %d, want 400", w.Code)
	}
}

// 改密：204、旧密码错误 403、其他会话撤销且当前会话轮换（新 cookie）。
func TestChangePasswordEndpoint(t *testing.T) {
	h, account, store, _ := newAccountTestHandler(t)
	user := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	account.seed(user, "OldPassword123")

	current, err := h.auth.NewSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.auth.NewSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.POST("/api/v1/auth/change-password", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, user.ID)
		h.changePassword(c)
	})
	doChange := func(body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/change-password", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w := doChange(`{"old_password":"Wrong12345678","new_password":"NewPassword123"}`, &http.Cookie{Name: "refresh_token", Value: current})
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong old password status = %d, want 403", w.Code)
	}

	w = doChange(`{"old_password":"OldPassword123","new_password":"NewPassword123"}`, &http.Cookie{Name: "refresh_token", Value: current})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", w.Code, w.Body.String())
	}
	if s := store.sessions[auth.HashRefreshToken(other)]; s.RevokedAt == nil {
		t.Fatal("other session must be revoked")
	}
	var rotated bool
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "refresh_token" && ck.Value != current {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("response must carry the rotated refresh cookie")
	}
	if err := h.auth.VerifyPassword(account.hashes[user.ID], "NewPassword123"); err != nil {
		t.Fatal("password hash must be updated")
	}
}

// 管理端邀请：创建 201 返回一次性 accept_url；幂等 200 不带链接；列表与撤销。
func TestAdminInvitationEndpoints(t *testing.T) {
	h, _, _, _ := newAccountTestHandler(t)

	router := gin.New()
	admin := uuid.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, admin)
			handler(c)
		}
	}
	router.POST("/api/v1/admin/invitations", withUser(h.createInvitation))
	router.GET("/api/v1/admin/invitations", withUser(h.listInvitations))
	router.DELETE("/api/v1/admin/invitations/:id", withUser(h.revokeInvitation))
	req := func(method, path, body string) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		r := httptest.NewRequest(method, path, reader)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}

	w := req(http.MethodPost, "/api/v1/admin/invitations", `{"email":"Invite@Example.com","role":"admin"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"accept_url":"/register/`) || strings.Contains(w.Body.String(), "token_hash") {
		t.Fatalf("create body must expose one-time accept_url only: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"email":"invite@example.com"`) {
		t.Fatalf("email must be normalized: %s", w.Body.String())
	}

	// 幂等：同邮箱再次创建 → 200，无新 accept_url。
	w = req(http.MethodPost, "/api/v1/admin/invitations", `{"email":"invite@example.com"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent create status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "accept_url") {
		t.Fatal("idempotent create must not mint a new token")
	}

	// 非法邮箱/角色 → 400。
	if w := req(http.MethodPost, "/api/v1/admin/invitations", `{"email":"bad"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad email status = %d, want 400", w.Code)
	}
	if w := req(http.MethodPost, "/api/v1/admin/invitations", `{"email":"a@b.com","role":"root"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad role status = %d, want 400", w.Code)
	}

	w = req(http.MethodGet, "/api/v1/admin/invitations", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"pending"`) {
		t.Fatalf("list status = %d body = %s", w.Code, w.Body.String())
	}
	invitationID := extractJSONStringField(w.Body.String(), "id")

	if w := req(http.MethodDelete, "/api/v1/admin/invitations/"+invitationID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", w.Code)
	}
	if w := req(http.MethodDelete, "/api/v1/admin/invitations/"+invitationID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("revoke again status = %d, want 404", w.Code)
	}
}

// extractJSONStringField 从 {"invitations":[{...}]} 形状响应提取首个 "id" 值。
func extractJSONStringField(body, field string) string {
	needle := `"` + field + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

// 注册/忘记/重置共享 10/min 独立限流：超出后 429；login 限流不受影响。
func TestAuthSensitiveEndpointsRateLimited(t *testing.T) {
	h, _, _, _ := newAccountTestHandler(t)
	router := gin.New()
	h.Register(router, "rate-limit-test-secret-0123456789abcdef", 1_000_000, 1_000_000, 1_000_000)
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.RemoteAddr = "203.0.113.77:1111"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < authSensitiveRateLimitPerMin; i++ {
		if w := post("/api/v1/auth/forgot-password"); w.Code != http.StatusBadRequest {
			t.Fatalf("request %d: code = %d, want 400 (empty body)", i+1, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/auth/forgot-password", "/api/v1/auth/register", "/api/v1/auth/reset-password"} {
		if w := post(path); w.Code != http.StatusTooManyRequests {
			t.Fatalf("%s after exhausted bucket: code = %d, want 429", path, w.Code)
		}
	}
	// 同 IP 的 login（独立限流器）不受影响；不同 IP 不受影响。
	if w := post("/api/v1/auth/login"); w.Code == http.StatusTooManyRequests {
		t.Fatal("login limiter must be independent from sensitive endpoints")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", nil)
	req.RemoteAddr = "203.0.113.78:1111"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("different IP code = %d, want 400", w.Code)
	}
}
