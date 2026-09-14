package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// memAuditRecorder 记录审计条目（token.create / token.revoke 断言用）。
type memAuditRecorder struct {
	entries []audit.Entry
}

func (m *memAuditRecorder) Record(e audit.Entry) error {
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAuditRecorder) find(action string) *audit.Entry {
	for i := range m.entries {
		if m.entries[i].Action == action {
			return &m.entries[i]
		}
	}
	return nil
}

// fakeTokenStore（http 包副本）是 auth.TokenStore 的内存实现。
type hFakeTokenStore struct {
	tokens  []auth.APIToken
	touched []uuid.UUID
}

func (f *hFakeTokenStore) Create(t auth.APIToken) error {
	f.tokens = append(f.tokens, t)
	return nil
}

func (f *hFakeTokenStore) List(owner uuid.UUID) ([]auth.APIToken, error) {
	out := make([]auth.APIToken, 0)
	for i := len(f.tokens) - 1; i >= 0; i-- {
		if f.tokens[i].UserID == owner && f.tokens[i].RevokedAt == nil {
			out = append(out, f.tokens[i])
		}
	}
	return out, nil
}

func (f *hFakeTokenStore) Revoke(owner, id uuid.UUID, now time.Time) (bool, error) {
	for i := range f.tokens {
		t := &f.tokens[i]
		if t.ID == id && t.UserID == owner && t.RevokedAt == nil {
			t.RevokedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (f *hFakeTokenStore) Update(owner, id uuid.UUID, name *string, scopes *[]string) (auth.APIToken, error) {
	for i := range f.tokens {
		if f.tokens[i].ID == id && f.tokens[i].UserID == owner && f.tokens[i].RevokedAt == nil {
			if name != nil {
				f.tokens[i].Name = *name
			}
			if scopes != nil {
				f.tokens[i].Scopes = *scopes
			}
			return f.tokens[i], nil
		}
	}
	return auth.APIToken{}, nil
}

func (f *hFakeTokenStore) FindActiveByPrefix(prefix string, now time.Time) (auth.PATLookup, bool, error) {
	for i := range f.tokens {
		t := &f.tokens[i]
		if t.Prefix == prefix && t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(now)) {
			return auth.PATLookup{ID: t.ID, UserID: t.UserID, TokenHash: t.TokenHash}, true, nil
		}
	}
	return auth.PATLookup{}, false, nil
}

func (f *hFakeTokenStore) TouchLastUsed(id uuid.UUID) {
	f.touched = append(f.touched, id)
}

func (f *hFakeTokenStore) DeleteExpired(now time.Time) (int64, error) { return 0, nil }

// sessionsTestEnv 构造挂好会话/PAT 依赖的 Handler 与底层内存存储。
type sessionsTestEnv struct {
	h          *Handler
	sessions   *fakeSessionStore
	tokens     *hFakeTokenStore
	recorder   *memAuditRecorder
	service    *auth.Service
	routerPath string
}

func newSessionsTestEnv(t *testing.T) *sessionsTestEnv {
	t.Helper()
	store := newFakeSessionStore()
	tokenStore := &hFakeTokenStore{}
	service := auth.NewService(store, "http-sessions-test-secret-0123456789", time.Minute, time.Hour)
	service.SetTokenStore(tokenStore)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	rec := &memAuditRecorder{}
	h.SetAuditRecorder(rec)
	return &sessionsTestEnv{h: h, sessions: store, tokens: tokenStore, recorder: rec, service: service}
}

// callSessions 以 uid 的身份调用会话/PAT 处理器（绕过中间件，聚焦属主语义）。
func callSessions(h *Handler, method, path, body string, uid uuid.UUID) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, uid)
			handler(c)
		}
	}
	router.GET("/api/v1/auth/sessions", withUser(h.listSessions))
	router.DELETE("/api/v1/auth/sessions", withUser(h.revokeAllSessions))
	router.DELETE("/api/v1/auth/sessions/:id", withUser(h.revokeSession))
	router.POST("/api/v1/tokens", withUser(h.createToken))
	router.GET("/api/v1/tokens", withUser(h.listTokens))
	router.DELETE("/api/v1/tokens/:id", withUser(h.revokeToken))
	w := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	router.ServeHTTP(w, req)
	return w
}

func mustSession(t *testing.T, env *sessionsTestEnv, uid uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	token, err := env.service.NewSession(uid)
	if err != nil {
		t.Fatal(err)
	}
	return env.sessions.sessions[auth.HashRefreshToken(token)].ID, token
}

// 列表只含自己的活跃会话（他人/已撤销/已过期的排除），且不泄露 token 材料。
func TestListSessionsScopedToOwnerActive(t *testing.T) {
	env := newSessionsTestEnv(t)
	uid := uuid.New()
	other := uuid.New()
	mine, _ := mustSession(t, env, uid)
	mustSession(t, env, other)
	revokedID, revokedToken := mustSession(t, env, uid)
	if err := env.service.RevokeRefreshToken(revokedToken); err != nil {
		t.Fatal(err)
	}

	w := callSessions(env.h, http.MethodGet, "/api/v1/auth/sessions", "", uid)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, mine.String()) {
		t.Fatalf("body must contain own active session %s: %s", mine, body)
	}
	if strings.Contains(body, revokedID.String()) {
		t.Fatal("revoked session must be excluded")
	}
	// 无从断言 other 的具体 id（随机），改为计数：恰好 1 条。
	if got := strings.Count(body, `"id":`); got != 1 {
		t.Fatalf("session count = %d, want 1 (only own active): %s", got, body)
	}
	if strings.Contains(body, "refresh") || strings.Contains(body, "hash") {
		t.Fatalf("body must not contain token material: %s", body)
	}
}

// 撤销单个会话：属主 204 且真实撤销；非属主与不存在一律 404；非法 id 400。
func TestRevokeSessionOwnership(t *testing.T) {
	env := newSessionsTestEnv(t)
	uid := uuid.New()
	intruder := uuid.New()
	mine, _ := mustSession(t, env, uid)

	if w := callSessions(env.h, http.MethodDelete, "/api/v1/auth/sessions/"+mine.String(), "", intruder); w.Code != http.StatusNotFound {
		t.Fatalf("non-owner status = %d, want 404", w.Code)
	}
	// 非属主撤销未生效。
	if s := env.sessions.sessions; s != nil {
		for _, session := range env.sessions.sessions {
			if session.ID == mine && session.RevokedAt != nil {
				t.Fatal("non-owner revoke must not take effect")
			}
		}
	}
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/auth/sessions/"+mine.String(), "", uid); w.Code != http.StatusNoContent {
		t.Fatalf("owner status = %d, want 204", w.Code)
	}
	for _, session := range env.sessions.sessions {
		if session.ID == mine && session.RevokedAt == nil {
			t.Fatal("session must be revoked")
		}
	}
	// 重复撤销（已撤销）→ 404；未知 id → 404；非法 id → 400。
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/auth/sessions/"+mine.String(), "", uid); w.Code != http.StatusNotFound {
		t.Fatalf("double revoke status = %d, want 404", w.Code)
	}
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/auth/sessions/"+uuid.New().String(), "", uid); w.Code != http.StatusNotFound {
		t.Fatalf("unknown session status = %d, want 404", w.Code)
	}
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/auth/sessions/not-a-uuid", "", uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", w.Code)
	}
}

// 撤销全部：自己的会话（含「当前」）全部撤销，他人不动；响应 204 且清除
// refresh cookie（实现取舍：不依赖可伪造的 X-Session-Hint）。
func TestRevokeAllSessionsIncludesCurrent(t *testing.T) {
	env := newSessionsTestEnv(t)
	uid := uuid.New()
	other := uuid.New()
	mustSession(t, env, uid)
	mustSession(t, env, uid)
	otherID, _ := mustSession(t, env, other)

	w := callSessions(env.h, http.MethodDelete, "/api/v1/auth/sessions", "", uid)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	for _, session := range env.sessions.sessions {
		if session.UserID == uid && session.RevokedAt == nil {
			t.Fatal("all own sessions must be revoked (including current)")
		}
		if session.UserID == other && session.RevokedAt != nil {
			t.Fatal("other users' sessions must be untouched")
		}
	}
	var cleared bool
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "refresh_token" && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("response must clear refresh_token cookie")
	}
	_ = otherID
}

// PAT 创建：201 含一次性明文（dfpat_ 前缀、49 字符），库中只存哈希，
// 写 token.create 审计；名称/有效期非法 400。
func TestCreateToken(t *testing.T) {
	env := newSessionsTestEnv(t)
	uid := uuid.New()
	w := callSessions(env.h, http.MethodPost, "/api/v1/tokens", `{"name":"ci 脚本","expires_in_days":30}`, uid)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"token":"dfpat_`) {
		t.Fatalf("body must carry one-time plaintext: %s", body)
	}
	var plaintext string
	if idx := strings.Index(body, `"token":"`); idx >= 0 {
		rest := body[idx+len(`"token":"`):]
		if end := strings.Index(rest, `"`); end >= 0 {
			plaintext = rest[:end]
		}
	}
	if len(plaintext) != len(auth.PATPrefix)+43 {
		t.Fatalf("plaintext length = %d, want 49", len(plaintext))
	}
	if len(env.tokens.tokens) != 1 {
		t.Fatalf("stored tokens = %d, want 1", len(env.tokens.tokens))
	}
	stored := env.tokens.tokens[0]
	if stored.TokenHash == plaintext || len(stored.TokenHash) != 64 {
		t.Fatal("store must hold sha256 hex hash, not plaintext")
	}
	if stored.Prefix != plaintext[:len(stored.Prefix)] {
		t.Fatal("stored prefix must match plaintext head")
	}
	if stored.ExpiresAt == nil {
		t.Fatal("30-day token must expire")
	}
	if e := env.recorder.find(audit.ActionTokenCreate); e == nil || e.ResourceID != stored.ID.String() {
		t.Fatalf("token.create audit missing or wrong id: %+v", e)
	}
	// 校验失败：空名 / 超上限有效期。
	for _, bad := range []string{`{"name":"  "}`, `{"name":"x","expires_in_days":-1}`} {
		if w := callSessions(env.h, http.MethodPost, "/api/v1/tokens", bad, uid); w.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want 400", bad, w.Code)
		}
	}
	// 畸形 JSON → 400。
	if w := callSessions(env.h, http.MethodPost, "/api/v1/tokens", `not-json`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want 400", w.Code)
	}
}

// PAT 列表：不含明文（prefix 展示用），含 last_used_at；撤销后不再列出，
// 写 token.revoke 审计；非属主与未知 id 404。
func TestTokenListAndRevoke(t *testing.T) {
	env := newSessionsTestEnv(t)
	uid := uuid.New()
	other := uuid.New()
	w := callSessions(env.h, http.MethodPost, "/api/v1/tokens", `{"name":"a"}`, uid)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d", w.Code)
	}
	if w := callSessions(env.h, http.MethodPost, "/api/v1/tokens", `{"name":"b","expires_in_days":7}`, uid); w.Code != http.StatusCreated {
		t.Fatalf("create b status = %d", w.Code)
	}
	otherToken, _, err := env.service.NewPersonalAccessToken(other, "c", 0)
	if err != nil {
		t.Fatal(err)
	}

	// 列表：只有自己的，不含明文 token 字段。
	w = callSessions(env.h, http.MethodGet, "/api/v1/tokens", "", uid)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `"token":"dfpat_`) {
		t.Fatalf("list must not carry plaintext: %s", body)
	}
	if !strings.Contains(body, `"last_used_at":null`) {
		t.Fatalf("list must include last_used_at: %s", body)
	}
	if strings.Contains(body, otherToken.ID.String()) {
		t.Fatal("list must not include other users' tokens")
	}

	// 非属主撤销 404；属主撤销 204 + 审计；已撤销/未知 404。
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/tokens/"+env.tokens.tokens[0].ID.String(), "", other); w.Code != http.StatusNotFound {
		t.Fatalf("non-owner revoke status = %d, want 404", w.Code)
	}
	first := env.tokens.tokens[0].ID
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/tokens/"+first.String(), "", uid); w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204", w.Code)
	}
	if e := env.recorder.find(audit.ActionTokenRevoke); e == nil || e.ResourceID != first.String() {
		t.Fatalf("token.revoke audit missing: %+v", e)
	}
	if w := callSessions(env.h, http.MethodDelete, "/api/v1/tokens/"+first.String(), "", uid); w.Code != http.StatusNotFound {
		t.Fatalf("double revoke status = %d, want 404", w.Code)
	}
	// 撤销后列表只剩 b。
	w = callSessions(env.h, http.MethodGet, "/api/v1/tokens", "", uid)
	if got := strings.Count(w.Body.String(), `"id":`); got != 1 {
		t.Fatalf("token count after revoke = %d, want 1: %s", got, w.Body.String())
	}
}

// 未注入 TokenStore 时 PAT 端点 503（fail closed，不 panic）。
func TestTokenEndpointsNotConfigured(t *testing.T) {
	h := NewHandler(auth.NewService(newFakeSessionStore(), "http-sessions-test-secret-0123456789", time.Minute, time.Hour), nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	uid := uuid.New()
	if w := callSessions(h, http.MethodPost, "/api/v1/tokens", `{"name":"x"}`, uid); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("create status = %d, want 503", w.Code)
	}
	if w := callSessions(h, http.MethodGet, "/api/v1/tokens", "", uid); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("list status = %d, want 503", w.Code)
	}
}
