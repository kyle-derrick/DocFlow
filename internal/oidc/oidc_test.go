package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// sha256Sum 返回原始 SHA-256 摘要（ChallengeS256 的前置步骤）。
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func TestPKCEVerifierAndChallenge(t *testing.T) {
	verifier, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	// RFC 7636 §4.1：43-128 字符，unreserved 字母表（base64url）。
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("verifier length = %d, want 43-128", len(verifier))
	}
	if _, err := base64.RawURLEncoding.DecodeString(verifier); err != nil {
		t.Fatalf("verifier not base64url: %v", err)
	}
	// S256：challenge = BASE64URL(SHA256(verifier))，去填充。
	challenge := ChallengeS256(verifier)
	expected := base64.RawURLEncoding.EncodeToString(sha256Sum(verifier))
	if challenge != expected {
		t.Fatalf("challenge = %q, want %q", challenge, expected)
	}
	if strings.Contains(challenge, "=") {
		t.Fatal("challenge must be unpadded base64url")
	}
	// 不同 verifier 产生不同 challenge。
	other, _ := NewVerifier()
	if ChallengeS256(other) == challenge {
		t.Fatal("distinct verifiers must yield distinct challenges")
	}
}

func TestStateStoreIssueAndConsume(t *testing.T) {
	store := NewStateStore()
	state, verifier, err := store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if state == "" || verifier == "" {
		t.Fatal("state/verifier must be non-empty")
	}
	// 一次性：首次消费返回登记的 verifier，再次消费失败。
	got, ok := store.Consume(state)
	if !ok || got != verifier {
		t.Fatalf("consume = (%q, %v), want (%q, true)", got, ok, verifier)
	}
	if _, ok := store.Consume(state); ok {
		t.Fatal("state must be single-use (burn after consume)")
	}
	if _, ok := store.Consume("unknown-state"); ok {
		t.Fatal("unknown state must not consume")
	}
	if _, ok := store.Consume(""); ok {
		t.Fatal("empty state must not consume")
	}
}

func TestStateStoreTTLExpiry(t *testing.T) {
	store := NewStateStore()
	now := time.Now()
	store.now = func() time.Time { return now }
	state, _, err := store.Issue()
	if err != nil {
		t.Fatal(err)
	}
	// 推进时钟越过 TTL（10 分钟 + 1 秒）：state 过期，消费失败。
	store.now = func() time.Time { return now.Add(StateTTL + time.Second) }
	if _, ok := store.Consume(state); ok {
		t.Fatal("expired state must not consume")
	}
}

// fakeDirectory 是 Directory 的内存实现（email 匹配/开户矩阵测试）。
type fakeDirectory struct {
	mu        sync.Mutex
	users     map[uuid.UUID]auth.User
	emails    map[string]uuid.UUID
	usernames map[string]bool
}

func newFakeDirectory(users ...auth.User) *fakeDirectory {
	d := &fakeDirectory{users: make(map[uuid.UUID]auth.User), emails: make(map[string]uuid.UUID), usernames: make(map[string]bool)}
	for _, u := range users {
		d.users[u.ID] = u
		d.emails[u.Email] = u.ID
		d.usernames[u.Username] = true
	}
	return d
}

func (d *fakeDirectory) FindActiveByEmail(email string) (auth.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, ok := d.emails[auth.NormalizeEmail(email)]
	if !ok || d.users[id].Status != auth.StatusActive {
		return auth.User{}, errors.New("not found")
	}
	return d.users[id], nil
}

func (d *fakeDirectory) GetByID(id uuid.UUID) (auth.User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.users[id]
	if !ok {
		return auth.User{}, errors.New("not found")
	}
	return u, nil
}

func (d *fakeDirectory) UsernameExists(username string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.usernames[username], nil
}

func (d *fakeDirectory) CreateUser(u auth.User) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.usernames[u.Username] || d.emails[u.Email] != uuid.Nil {
		return auth.ErrUserExists
	}
	d.users[u.ID] = u
	d.emails[u.Email] = u.ID
	d.usernames[u.Username] = true
	return nil
}

// fakeLinkStore 是 LinkStore 的内存实现。
type fakeLinkStore struct {
	mu    sync.Mutex
	links map[string]Link
}

func newFakeLinkStore() *fakeLinkStore { return &fakeLinkStore{links: make(map[string]Link)} }

func (f *fakeLinkStore) Upsert(link Link) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links[link.Sub] = link
	return nil
}

func (f *fakeLinkStore) FindBySub(sub string) (Link, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	link, ok := f.links[sub]
	return link, ok, nil
}

// fakeIdP 起一个最小 OIDC IdP：token endpoint（校验 code_verifier 与
// client 凭据）+ userinfo endpoint（Bearer access_token）。
type fakeIdP struct {
	server       *httptest.Server
	tokenCalls   int
	lastVerifier string
	lastCode     string
}

func newFakeIdP(t *testing.T, sub, email, preferredUsername string) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idp.tokenCalls++
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		idp.lastVerifier = r.PostFormValue("code_verifier")
		idp.lastCode = r.PostFormValue("code")
		if r.PostFormValue("client_id") == "" || r.PostFormValue("client_secret") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "fake-access-token", "token_type": "Bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": sub, "email": email, "preferred_username": preferredUsername})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeIdP) provider() Provider {
	return Provider{
		Issuer:                idp.server.URL,
		AuthorizationEndpoint: idp.server.URL + "/authorize",
		TokenEndpoint:         idp.server.URL + "/token",
		UserinfoEndpoint:      idp.server.URL + "/userinfo",
	}
}

// loginWithCode 经 BeginLogin → CompleteLogin 走完整授权码回调（不校验
// 浏览器跳转，聚焦服务端逻辑），返回映射到的用户 ID。
func loginWithCode(t *testing.T, svc *Service, code string) (uuid.UUID, error) {
	t.Helper()
	state, _, err := svc.BeginLogin()
	if err != nil {
		return uuid.Nil, err
	}
	return svc.CompleteLogin(context.Background(), state, code)
}

func TestServiceEmailMatchLinksSub(t *testing.T) {
	idp := newFakeIdP(t, "sub-1", "Alice@Example.COM ", "alice")
	existing := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive, Role: auth.RoleUser}
	dir := newFakeDirectory(existing)
	links := newFakeLinkStore()
	svc := NewService(New(idp.provider(), "cid", "csecret", "http://backend/callback"), links, dir, true)

	uid, err := loginWithCode(t, svc, "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	if uid != existing.ID {
		t.Fatalf("mapped user = %s, want existing %s", uid, existing.ID)
	}
	// email 归一小写后匹配；成功后写 sub 关联。
	link, found, _ := links.FindBySub("sub-1")
	if !found || link.UserID != existing.ID {
		t.Fatalf("link = %+v found=%v, want user %s", link, found, existing.ID)
	}
	// 二次登录：email 在 IdP 侧已变更，仍凭 sub 关联命中同一用户。
	idp2 := newFakeIdP(t, "sub-1", "new-email@example.com", "alice")
	svc2 := NewService(New(idp2.provider(), "cid", "csecret", "http://backend/callback"), links, dir, true)
	uid2, err := loginWithCode(t, svc2, "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	if uid2 != existing.ID {
		t.Fatalf("sub-linked login = %s, want %s", uid2, existing.ID)
	}
}

func TestServiceAutoProvision(t *testing.T) {
	idp := newFakeIdP(t, "sub-new", "newbie@example.com", "Newbie_01")
	dir := newFakeDirectory(auth.User{ID: uuid.New(), Username: "newbie_01", Email: "taken@example.com", Status: auth.StatusActive})
	svc := NewService(New(idp.provider(), "cid", "csecret", "http://backend/callback"), newFakeLinkStore(), dir, true)

	uid, err := loginWithCode(t, svc, "auth-code")
	if err != nil {
		t.Fatal(err)
	}
	user, err := dir.GetByID(uid)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != auth.StatusActive || user.Role != auth.RoleUser {
		t.Fatalf("provisioned user status=%s role=%s, want active/user", user.Status, user.Role)
	}
	if user.Email != "newbie@example.com" {
		t.Fatalf("provisioned email = %q, want normalized lowercase", user.Email)
	}
	// preferred_username 已被占用：去重加后缀。
	if user.Username != "newbie_01-1" {
		t.Fatalf("provisioned username = %q, want deduped newbie_01-1", user.Username)
	}
	// 随机密码不可用于常见弱密码登录（bcrypt 哈希与任意常见口令不匹配）。
	if auth.ValidatePasswordStrength("password123") == nil && svc == nil {
		t.Fatal("unreachable")
	}
	if err := auth.NewService(nil, "", 0, 0).VerifyPassword(user.PasswordHash, "password123"); err == nil {
		t.Fatal("provisioned password hash must not match common passwords")
	}
}

func TestServiceProvisionDisabled(t *testing.T) {
	idp := newFakeIdP(t, "sub-x", "stranger@example.com", "stranger")
	svc := NewService(New(idp.provider(), "cid", "csecret", "http://backend/callback"), newFakeLinkStore(), newFakeDirectory(), false)
	if _, err := loginWithCode(t, svc, "auth-code"); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("err = %v, want ErrNotProvisioned", err)
	}
}

func TestServiceDisabledUserRejected(t *testing.T) {
	// email 命中 disabled 用户：FindActiveByEmail 不匹配（非 active），
	// 自动开户又与唯一约束冲突——但目录实现里 email 已占用，CreateUser
	// 返回 ErrUserExists 前应先经用户名去重；此矩阵按「disabled 用户不可
	// SSO」期望失败（任何错误均可，绝不能成功登录）。
	idp := newFakeIdP(t, "sub-d", "disabled@example.com", "disabled-user")
	disabled := auth.User{ID: uuid.New(), Username: "disabled-user", Email: "disabled@example.com", Status: auth.StatusDisabled}
	dir := newFakeDirectory(disabled)
	svc := NewService(New(idp.provider(), "cid", "csecret", "http://backend/callback"), newFakeLinkStore(), dir, true)
	if _, err := loginWithCode(t, svc, "auth-code"); err == nil {
		t.Fatal("disabled user must not login via sso")
	}

	// sub 已关联但账号被禁用：明确 ErrUserDisabled。
	links := newFakeLinkStore()
	_ = links.Upsert(Link{Sub: "sub-e", UserID: disabled.ID, Issuer: idp.server.URL})
	idp2 := newFakeIdP(t, "sub-e", "anywhere@example.com", "x")
	svc2 := NewService(New(idp2.provider(), "cid", "csecret", "http://backend/callback"), links, dir, true)
	if _, err := loginWithCode(t, svc2, "auth-code"); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("err = %v, want ErrUserDisabled", err)
	}
}

func TestServiceStateInvalid(t *testing.T) {
	idp := newFakeIdP(t, "sub-s", "s@example.com", "s")
	svc := NewService(New(idp.provider(), "cid", "csecret", "http://backend/callback"), newFakeLinkStore(), newFakeDirectory(), true)
	if _, err := svc.CompleteLogin(context.Background(), "bogus-state", "code"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

func TestClientAuthorizeURLParams(t *testing.T) {
	client := New(Provider{Issuer: "http://idp", AuthorizationEndpoint: "http://idp/authorize", TokenEndpoint: "http://idp/token"}, "cid", "csecret", "http://backend/callback")
	raw := client.AuthorizeURL("st4te", "ch4llenge")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "cid" {
		t.Fatalf("authorize params = %v", q)
	}
	if q.Get("redirect_uri") != "http://backend/callback" || q.Get("state") != "st4te" {
		t.Fatalf("authorize params = %v", q)
	}
	if q.Get("code_challenge") != "ch4llenge" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize params = %v", q)
	}
	if !strings.Contains(q.Get("scope"), "openid") || !strings.Contains(q.Get("scope"), "email") {
		t.Fatalf("scope = %q, want openid+email", q.Get("scope"))
	}
}

func TestClientPKCEVerifierReachesTokenEndpoint(t *testing.T) {
	idp := newFakeIdP(t, "sub-p", "p@example.com", "p")
	svc := NewService(New(idp.provider(), "cid", "csecret", "http://backend/callback"), newFakeLinkStore(), newFakeDirectory(), true)
	state, authorizeURL, err := svc.BeginLogin()
	if err != nil {
		t.Fatal(err)
	}
	// 授权地址携带 challenge = S256(服务端登记的 verifier)；token 端点
	// 收到的 verifier 与之配对（fakeIdP 记录 lastVerifier）。
	parsed, _ := url.Parse(authorizeURL)
	challenge := parsed.Query().Get("code_challenge")
	if _, err := svc.CompleteLogin(context.Background(), state, "code-123"); err != nil {
		t.Fatal(err)
	}
	if idp.lastCode != "code-123" || idp.lastVerifier == "" {
		t.Fatalf("token endpoint got code=%q verifier=%q", idp.lastCode, idp.lastVerifier)
	}
	if ChallengeS256(idp.lastVerifier) != challenge {
		t.Fatal("code_verifier received by token endpoint must match code_challenge from authorize URL")
	}
}

func TestParseIDTokenClaimsFallback(t *testing.T) {
	// userinfo 不可用（无 endpoint）时回退解析 ID token claims（未验签：
	// token 源于后端直连 token endpoint）。
	claims := jwt.MapClaims{"sub": "sub-jwt", "email": "JWT@Example.COM", "preferred_username": "jwt-user"}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte("idp-secret"))
	if err != nil {
		t.Fatal(err)
	}
	client := New(Provider{Issuer: "http://idp", AuthorizationEndpoint: "http://idp/a", TokenEndpoint: "http://idp/t"}, "cid", "csecret", "http://backend/callback")
	got, err := client.ResolveClaims(context.Background(), TokenResponse{IDToken: signed})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sub != "sub-jwt" || got.Email != "jwt@example.com" || got.PreferredUsername != "jwt-user" {
		t.Fatalf("claims = %+v", got)
	}
}

func TestResolveClaimsNoSource(t *testing.T) {
	client := New(Provider{Issuer: "http://idp", AuthorizationEndpoint: "http://idp/a", TokenEndpoint: "http://idp/t"}, "cid", "csecret", "http://backend/callback")
	if _, err := client.ResolveClaims(context.Background(), TokenResponse{AccessToken: "x"}); err == nil {
		t.Fatal("expect error when neither userinfo nor id_token is usable")
	}
}

func TestDeriveUsername(t *testing.T) {
	tests := []struct {
		preferred, email, want string
	}{
		{"Alice_01", "alice@example.com", "alice_01"},
		{"ab", "alongerlocal@example.com", "alongerlocal"}, // preferred 太短回退 email 前缀
		{"非法字符!", "u@x.io", "user"},                        // 两者皆非法 → 回退 user
		{"", "Capital.Local@Example.COM", "capitallocal"},  // 大写归一（'.' 不在用户名字母表，剔除）
	}
	for _, test := range tests {
		got := deriveUsername(test.preferred, test.email)
		if err := auth.ValidateUsername(got); err != nil {
			t.Fatalf("deriveUsername(%q,%q) = %q invalid: %v", test.preferred, test.email, got, err)
		}
		if got != test.want {
			t.Errorf("deriveUsername(%q,%q) = %q, want %q", test.preferred, test.email, got, test.want)
		}
	}
}
