package http

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// lockoutTestEnv 构造 login/login-totp 公开端点的测试环境（fakeAccount 目录
// + 会话存储），供 C9 锁定与 C21a identifier 登录测试使用。
type lockoutTestEnv struct {
	h       *Handler
	account *fakeAccount
	router  *gin.Engine
}

func newLockoutTestEnv(t *testing.T) *lockoutTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newFakeSessionStore()
	service := auth.NewService(store, "lockout-test-secret-0123456789ab", time.Minute, time.Hour)
	account := newFakeAccount()
	service.SetCredentials(account)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = account
	h.audit = audit.NopRecorder{}
	router := gin.New()
	router.POST("/api/v1/auth/login", h.login)
	router.POST("/api/v1/auth/login/totp", h.loginTOTP)
	return &lockoutTestEnv{h: h, account: account, router: router}
}

// lockUntil 把 fake 目录中的用户置为锁定（future）/解锁（past）状态。
func (e *lockoutTestEnv) lockUntil(id uuid.UUID, until time.Time) {
	user := e.account.accounts[id]
	user.LockedUntil = &until
	e.account.accounts[id] = user
}

// C21a：identifier 支持 username 与 email 登录；旧 email 字段兼容。
func TestLoginIdentifierUsernameAndEmail(t *testing.T) {
	env := newLockoutTestEnv(t)
	password := "Sup3rSecretPass1"
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	env.account.seed(alice, password)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"identifier-username", `{"identifier":"alice","password":"` + password + `"}`},
		{"identifier-email", `{"identifier":"alice@example.com","password":"` + password + `"}`},
		{"legacy-email-field", `{"email":"alice@example.com","password":"` + password + `"}`},
		{"identifier-priority", `{"identifier":"alice","email":"bob@example.com","password":"` + password + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "access_token") {
				t.Fatalf("body = %s, want access_token", w.Body.String())
			}
		})
	}
	// 成功登录清零失败计数（C9）。
	if !env.account.cleared[alice.ID] {
		t.Fatal("successful login must clear failure counter")
	}
}

// C9：密码错误计入失败；锁定期间（locked_until 未到期）登录一律
// 423 ACCOUNT_LOCKED 且不计新失败；成功登录清零。
func TestLoginLockoutMatrix(t *testing.T) {
	env := newLockoutTestEnv(t)
	password := "Sup3rSecretPass1"
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	env.account.seed(alice, password)

	// 密码错误 → 401 且计数 +1（两次）。
	for i := 0; i < 2; i++ {
		w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login", `{"identifier":"alice","password":"WrongPass123"}`)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, w.Code)
		}
	}
	if got := env.account.failureCountOf(alice.ID); got != 2 {
		t.Fatalf("failure count = %d, want 2", got)
	}

	// 锁定中：正确密码也 423，且不计新失败（无 token、无清零）。
	env.lockUntil(alice.ID, time.Now().Add(10*time.Minute))
	w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login", `{"identifier":"alice","password":"`+password+`"}`)
	if w.Code != http.StatusLocked {
		t.Fatalf("locked login: status = %d, want 423 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"ACCOUNT_LOCKED"`) {
		t.Fatalf("body = %s, want ACCOUNT_LOCKED", w.Body.String())
	}
	if got := env.account.failureCountOf(alice.ID); got != 2 {
		t.Fatalf("locked login must not count new failures: got %d, want 2", got)
	}
	if strings.Contains(w.Body.String(), "access_token") {
		t.Fatal("locked login must not issue token")
	}
	if env.account.cleared[alice.ID] {
		t.Fatal("locked login must not clear failure counter")
	}

	// 锁定到期（locked_until 已过）：正常登录成功并清零。
	env.lockUntil(alice.ID, time.Now().Add(-time.Minute))
	w = callJSON(env.router, http.MethodPost, "/api/v1/auth/login", `{"identifier":"alice","password":"`+password+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("unlocked login: status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !env.account.cleared[alice.ID] {
		t.Fatal("successful login must clear failure counter")
	}
}

// C9（TOTP 二段）：密码重验失败同样计数；锁定期间 423（不计新失败）。
func TestLoginTOTPLockoutAndFailureCounting(t *testing.T) {
	env := newLockoutTestEnv(t)
	password := "Sup3rSecretPass1"
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	env.account.seed(alice, password)

	// 密码错误：401 invalid credentials 且计数 +1。
	w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", `{"identifier":"alice","password":"WrongPass123","code":"123456"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := env.account.failureCountOf(alice.ID); got != 1 {
		t.Fatalf("failure count = %d, want 1", got)
	}

	// 锁定期间：423 且不再计数（密码正确也拒绝）。
	env.lockUntil(alice.ID, time.Now().Add(10*time.Minute))
	w = callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", `{"identifier":"alice","password":"`+password+`","code":"123456"}`)
	if w.Code != http.StatusLocked {
		t.Fatalf("locked totp login: status = %d, want 423", w.Code)
	}
	if got := env.account.failureCountOf(alice.ID); got != 1 {
		t.Fatalf("locked totp login must not count new failures: got %d, want 1", got)
	}
}

// 防爆破参数运行时化：每次登录失败热读取锁定策略——settings 的
// security.login_max_retries / login_lock_minutes 优先（改键值即时生效，
// 无需重启），键未入库或 settings 未装配回落 env 基线
// （SetLoginLockout / LOGIN_MAX_RETRIES / LOGIN_LOCK_MINUTES）。
func TestLoginLockoutPolicyRuntimeSettings(t *testing.T) {
	env := newLockoutTestEnv(t)
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	env.account.seed(alice, "Sup3rSecretPass1")

	// settings 未装配：回落 env 注入值。
	env.h.SetLoginLockout(7, 42*time.Minute)
	callJSON(env.router, http.MethodPost, "/api/v1/auth/login", `{"identifier":"alice","password":"WrongPass123"}`)
	if env.account.lastLockoutRetries != 7 || env.account.lastLockoutFor != 42*time.Minute {
		t.Fatalf("settings 未装配应回落 env: retries=%d lockFor=%v, want 7/42m", env.account.lastLockoutRetries, env.account.lastLockoutFor)
	}

	// settings 装配但键未入库：同样回落 env。
	env.h.SetSettingsService(&fakeSettingsService{intKeys: map[string]int{}})
	callJSON(env.router, http.MethodPost, "/api/v1/auth/login", `{"identifier":"alice","password":"WrongPass123"}`)
	if env.account.lastLockoutRetries != 7 || env.account.lastLockoutFor != 42*time.Minute {
		t.Fatalf("键未入库应回落 env: retries=%d lockFor=%v, want 7/42m", env.account.lastLockoutRetries, env.account.lastLockoutFor)
	}

	// settings 配置后即时生效（下一次失败即用新阈值/时长）。
	env.h.SetSettingsService(&fakeSettingsService{intKeys: map[string]int{
		settings.KeyLoginMaxRetries:  2,
		settings.KeyLoginLockMinutes: 30,
	}})
	callJSON(env.router, http.MethodPost, "/api/v1/auth/login", `{"identifier":"alice","password":"WrongPass123"}`)
	if env.account.lastLockoutRetries != 2 || env.account.lastLockoutFor != 30*time.Minute {
		t.Fatalf("settings 应优先: retries=%d lockFor=%v, want 2/30m", env.account.lastLockoutRetries, env.account.lastLockoutFor)
	}
}
