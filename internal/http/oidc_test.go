package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/oidc"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// oidcDirectoryFake 实现 oidc.Directory（HTTP 端点测试用内存目录）。
type oidcDirectoryFake struct {
	users     map[uuid.UUID]auth.User
	emails    map[string]uuid.UUID
	usernames map[string]bool
}

func newOIDCDirectoryFake() *oidcDirectoryFake {
	return &oidcDirectoryFake{users: make(map[uuid.UUID]auth.User), emails: make(map[string]uuid.UUID), usernames: make(map[string]bool)}
}

func (d *oidcDirectoryFake) seed(u auth.User) {
	d.users[u.ID] = u
	d.emails[u.Email] = u.ID
	d.usernames[u.Username] = true
}

func (d *oidcDirectoryFake) FindActiveByEmail(email string) (auth.User, error) {
	id, ok := d.emails[auth.NormalizeEmail(email)]
	if !ok || d.users[id].Status != auth.StatusActive {
		return auth.User{}, auth.ErrUserNotFound
	}
	return d.users[id], nil
}

func (d *oidcDirectoryFake) GetByID(id uuid.UUID) (auth.User, error) {
	u, ok := d.users[id]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return u, nil
}

func (d *oidcDirectoryFake) UsernameExists(username string) (bool, error) {
	return d.usernames[username], nil
}

func (d *oidcDirectoryFake) CreateUser(u auth.User) error {
	if d.usernames[u.Username] {
		return auth.ErrUserExists
	}
	d.users[u.ID] = u
	d.emails[u.Email] = u.ID
	d.usernames[u.Username] = true
	return nil
}

// oidcLinksFake 实现 oidc.LinkStore。
type oidcLinksFake struct{ links map[string]oidc.Link }

func (f *oidcLinksFake) Upsert(link oidc.Link) error {
	if f.links == nil {
		f.links = make(map[string]oidc.Link)
	}
	f.links[link.Sub] = link
	return nil
}

func (f *oidcLinksFake) FindBySub(sub string) (oidc.Link, bool, error) {
	link, ok := f.links[sub]
	return link, ok, nil
}

// newOIDCTestIdP 起最小 IdP：token + userinfo（v2 契约同 internal/oidc 测试）。
func newOIDCTestIdP(t *testing.T, sub, email, preferredUsername string) (oidc.Provider, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "token_type": "Bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": sub, "email": email, "preferred_username": preferredUsername})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return oidc.Provider{
		Issuer:                server.URL,
		AuthorizationEndpoint: server.URL + "/authorize",
		TokenEndpoint:         server.URL + "/token",
		UserinfoEndpoint:      server.URL + "/userinfo",
	}, server
}

// oidcTestEnv 构造挂好 OIDC 服务与 auth 会话服务的 Handler 及路由。
type oidcTestEnv struct {
	h         *Handler
	service   *auth.Service
	directory *oidcDirectoryFake
	router    *gin.Engine
}

func newOIDCTestEnv(t *testing.T, sub, email, preferredUsername string, autoProvision bool) *oidcTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	provider, _ := newOIDCTestIdP(t, sub, email, preferredUsername)
	directory := newOIDCDirectoryFake()
	service := auth.NewService(newFakeSessionStore(), "http-oidc-test-secret-0123456789", time.Minute, time.Hour)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.SetOIDC(oidc.NewService(oidc.New(provider, "cid", "csecret", "http://backend:8080/api/v1/auth/oidc/callback"), &oidcLinksFake{}, directory, autoProvision))
	recorder := &memAuditRecorder{}
	h.SetAuditRecorder(recorder)
	router := gin.New()
	router.GET("/api/v1/auth/oidc/config", h.oidcConfig)
	router.GET("/api/v1/auth/oidc/login", h.oidcLogin)
	router.GET("/api/v1/auth/oidc/callback", h.oidcCallback)
	return &oidcTestEnv{h: h, service: service, directory: directory, router: router}
}

func TestOIDCConfigEndpointDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	router := gin.New()
	router.GET("/api/v1/auth/oidc/config", h.oidcConfig)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Enabled {
		t.Fatal("enabled must be false when oidc is not injected")
	}
}

// beginSSOLogin 走 /oidc/login，返回 state 与 state cookie（模拟浏览器）。
func beginSSOLogin(t *testing.T, env *oidcTestEnv) (state, stateCookie string) {
	t.Helper()
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", w.Code)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state = location.Query().Get("state")
	if state == "" {
		t.Fatal("authorize URL must carry state")
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcStateCookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != state || !cookie.HttpOnly {
		t.Fatalf("oidc_state cookie = %+v, want HttpOnly double-check of %q", cookie, state)
	}
	return state, cookie.Value
}

func TestOIDCLoginCallbackFullFlow(t *testing.T) {
	existing := auth.User{ID: uuid.New(), Username: "existing", Email: "existing@example.com", Status: auth.StatusActive, Role: auth.RoleUser}
	env := newOIDCTestEnv(t, "sub-http-1", "existing@example.com", "existing", true)
	env.directory.seed(existing)

	state, cookie := beginSSOLogin(t, env)
	callback := "/api/v1/auth/oidc/callback?state=" + url.QueryEscape(state) + "&code=good-code"
	request := httptest.NewRequest(http.MethodGet, callback, nil)
	request.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookie})
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, request)

	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body %s", w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, "/sso#access_token=") {
		t.Fatalf("location = %q, want /sso#access_token=...", location)
	}
	// access_token 为绑定既有用户的 JWT。
	token := strings.TrimPrefix(location, "/sso#access_token=")
	parsed, _, err := jwt.NewParser().ParseUnverified(token, &jwt.RegisteredClaims{})
	if err != nil {
		t.Fatalf("fragment token parse: %v", err)
	}
	if parsed.Claims.(*jwt.RegisteredClaims).Subject != existing.ID.String() {
		t.Fatal("access token subject must be the matched user")
	}
	// refresh cookie 已下发（Path 限定 refresh 端点）。
	var refresh *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			refresh = c
		}
	}
	if refresh == nil || !refresh.HttpOnly || refresh.Path != "/api/v1/auth/refresh" {
		t.Fatalf("refresh cookie = %+v", refresh)
	}
}

func TestOIDCCallbackStateMismatch(t *testing.T) {
	env := newOIDCTestEnv(t, "sub-m", "m@example.com", "m", true)
	state, _ := beginSSOLogin(t, env)
	// 不带 state cookie：双验证失败。
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=c", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on cookie mismatch", w.Code)
	}
	// cookie 与参数不一致。
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?state=other&code=c", nil)
	request.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on state mismatch", w.Code)
	}
}

func TestOIDCCallbackStateSingleUse(t *testing.T) {
	env := newOIDCTestEnv(t, "sub-r", "r@example.com", "r", true)
	state, cookie := beginSSOLogin(t, env)
	doCallback := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=c", nil)
		request.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookie})
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, request)
		return w
	}
	if first := doCallback(); first.Code != http.StatusFound {
		t.Fatalf("first callback status = %d", first.Code)
	}
	if replay := doCallback(); replay.Code != http.StatusUnauthorized {
		t.Fatalf("replayed state status = %d, want 401 (burn after use)", replay.Code)
	}
}

func TestOIDCCallbackNotProvisioned(t *testing.T) {
	env := newOIDCTestEnv(t, "sub-np", "stranger@example.com", "stranger", false)
	state, cookie := beginSSOLogin(t, env)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=c", nil)
	request.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookie})
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SSO_NOT_PROVISIONED" {
		t.Fatalf("code = %q, want SSO_NOT_PROVISIONED", body["code"])
	}
}
