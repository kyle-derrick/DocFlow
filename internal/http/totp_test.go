package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// hFakeTOTPStore（http 包副本）是 auth.TOTPStore 的内存实现。
type hFakeTOTPStore struct {
	records map[uuid.UUID]auth.UserTOTP
}

func newHFakeTOTPStore() *hFakeTOTPStore {
	return &hFakeTOTPStore{records: make(map[uuid.UUID]auth.UserTOTP)}
}

func (f *hFakeTOTPStore) Get(userID uuid.UUID) (auth.UserTOTP, bool, error) {
	record, ok := f.records[userID]
	return record, ok, nil
}

func (f *hFakeTOTPStore) CreateOrUpdate(record auth.UserTOTP) error {
	f.records[record.UserID] = record
	return nil
}

func (f *hFakeTOTPStore) Enable(userID uuid.UUID, now time.Time) error {
	record, ok := f.records[userID]
	if !ok {
		return auth.ErrTOTPSetupRequired
	}
	record.Enabled = true
	record.ConfirmedAt = &now
	f.records[userID] = record
	return nil
}

func (f *hFakeTOTPStore) Delete(userID uuid.UUID) error {
	delete(f.records, userID)
	return nil
}

func (f *hFakeTOTPStore) ReplaceRecoveryCodes(userID uuid.UUID, hashes []string) error {
	record, ok := f.records[userID]
	if !ok {
		return auth.ErrTOTPSetupRequired
	}
	raw, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	record.RecoveryCodes = string(raw)
	f.records[userID] = record
	return nil
}

func (f *hFakeTOTPStore) ConsumeRecoveryCode(userID uuid.UUID, codeHash string, now time.Time) (bool, error) {
	record, ok := f.records[userID]
	if !ok {
		return false, nil
	}
	var hashes []string
	if err := json.Unmarshal([]byte(record.RecoveryCodes), &hashes); err != nil {
		return false, err
	}
	kept := make([]string, 0, len(hashes))
	found := false
	for _, h := range hashes {
		if !found && h == codeHash {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if !found {
		return false, nil
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return false, err
	}
	record.RecoveryCodes = string(raw)
	f.records[userID] = record
	return true, nil
}

// remainingRecoveryCodes 读取库中剩余恢复码哈希数（断言用）。
func (f *hFakeTOTPStore) remainingRecoveryCodes(t *testing.T, userID uuid.UUID) int {
	t.Helper()
	var hashes []string
	if err := json.Unmarshal([]byte(f.records[userID].RecoveryCodes), &hashes); err != nil {
		t.Fatalf("unmarshal recovery codes: %v", err)
	}
	return len(hashes)
}

// totpTestEnv 构造挂好 TOTP 依赖（凭据源 + TOTP store + 审计）的测试环境。
type totpTestEnv struct {
	h        *Handler
	totp     *hFakeTOTPStore
	sessions *fakeSessionStore
	account  *fakeAccount
	recorder *memAuditRecorder
	user     auth.User
	password string
	router   *gin.Engine
	codeNow  func() string
}

func newTotpTestEnv(t *testing.T) *totpTestEnv {
	t.Helper()
	store := newFakeSessionStore()
	totpStore := newHFakeTOTPStore()
	account := newFakeAccount()
	service := auth.NewService(store, "http-totp-test-secret-0123456789", time.Minute, time.Hour)
	service.SetCredentials(account)
	service.SetTOTPStore(totpStore)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = account
	rec := &memAuditRecorder{}
	h.SetAuditRecorder(rec)

	password := "Sup3rSecretPass1"
	user := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive}
	account.seed(user, password)

	env := &totpTestEnv{h: h, totp: totpStore, sessions: store, account: account, recorder: rec, user: user, password: password}
	secretOf := func() string {
		record, ok := totpStore.records[user.ID]
		if !ok {
			t.Fatal("totp record missing")
		}
		return record.Secret
	}
	env.codeNow = func() string {
		code, err := totp.GenerateCodeCustom(secretOf(), time.Now().UTC(), totp.ValidateOpts{
			Period: 30, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	// 公开登录端点（login + login/totp）与认证端点（以注入 user_id 绕过
	// Bearer，聚焦业务语义）分别注册。
	gin.SetMode(gin.TestMode)
	env.router = gin.New()
	env.router.POST("/api/v1/auth/login", h.login)
	env.router.POST("/api/v1/auth/login/totp", h.loginTOTP)
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, user.ID)
			handler(c)
		}
	}
	env.router.GET("/api/v1/auth/totp", withUser(h.totpStatus))
	env.router.POST("/api/v1/auth/totp/setup", withUser(h.totpSetup))
	env.router.POST("/api/v1/auth/totp/confirm", withUser(h.totpConfirm))
	env.router.DELETE("/api/v1/auth/totp", withUser(h.totpDisable))
	return env
}

// callJSON 以给定路由表执行请求（GET/POST/DELETE 统一处理）。
func callJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// enableTOTP 走完整 setup→confirm 链路启用并返回恢复码（HTTP 层调用）。
func (env *totpTestEnv) enableTOTP(t *testing.T) []string {
	t.Helper()
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/totp/setup", `{}`); w.Code != http.StatusOK {
		t.Fatalf("setup status = %d (body: %s)", w.Code, w.Body.String())
	}
	w := callJSON(env.router, http.MethodPost, "/api/v1/auth/totp/confirm", `{"code":"`+env.codeNow()+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm status = %d (body: %s)", w.Code, w.Body.String())
	}
	var response struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.RecoveryCodes
}

// loginBody 构造 /auth/login 请求体。
func loginBody(email, password string) string {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return string(b)
}

// totpLoginBody 构造 /auth/login/totp 请求体（code 与 recoveryCode 二选一）。
func totpLoginBody(email, password, code, recoveryCode string) string {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password, "code": code, "recovery_code": recoveryCode})
	return string(b)
}

// 防绕过（核心断言）：enabled 用户密码正确时 /login 仍 401 TOTP_REQUIRED，
// 不发放任何 token（body 无 access_token、无 refresh cookie、无会话创建）。
func TestLoginTOTPRequiredBlocksToken(t *testing.T) {
	env := newTotpTestEnv(t)
	env.enableTOTP(t)

	w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login", loginBody(env.user.Email, env.password))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code":"TOTP_REQUIRED"`) || !strings.Contains(body, `"error":"totp_required"`) {
		t.Fatalf("body = %s, want TOTP_REQUIRED marker", body)
	}
	if strings.Contains(body, "access_token") {
		t.Fatal("login must not issue any token for totp-enabled user")
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			t.Fatal("login must not set refresh cookie for totp-enabled user")
		}
	}
	if len(env.sessions.sessions) != 0 {
		t.Fatal("login must not create a session for totp-enabled user")
	}
	// 审计：不记 login_success（未完成登录）。
	if e := env.recorder.find(audit.ActionLoginSuccess); e != nil {
		t.Fatalf("login_success audit must not fire before second factor: %+v", e)
	}

	// 未启用用户（密码正确）：正常发 token。
	plain := auth.User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", Status: auth.StatusActive}
	env.account.seed(plain, "AnotherSecret123")
	w = callJSON(env.router, http.MethodPost, "/api/v1/auth/login", loginBody(plain.Email, "AnotherSecret123"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("plain login status = %d (body: %s), want 200 with token", w.Code, w.Body.String())
	}
}

// 第二段登录矩阵：密码+码通过（发 token、建会话、审计 login_success）；
// 密码错误 → invalid credentials；码错误 → invalid totp code；
// code 与 recovery_code 均缺 → 400。
func TestLoginTOTPSecondFactor(t *testing.T) {
	env := newTotpTestEnv(t)
	env.enableTOTP(t)

	// 密码 + 有效码：200 + access_token + refresh cookie + 会话。
	w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", totpLoginBody(env.user.Email, env.password, env.codeNow(), ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("body = %s, want access_token", w.Body.String())
	}
	var refreshSet bool
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == "refresh_token" {
			refreshSet = true
		}
	}
	if !refreshSet {
		t.Fatal("response must set refresh_token cookie")
	}
	if len(env.sessions.sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(env.sessions.sessions))
	}
	if e := env.recorder.find(audit.ActionLoginSuccess); e == nil || e.UserID == nil || *e.UserID != env.user.ID {
		t.Fatalf("login_success audit missing or wrong user: %+v", e)
	}

	// 密码错误：401 invalid credentials（与 login 一致）。
	bad := totpLoginBody(env.user.Email, "WrongPassword123", env.codeNow(), "")
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", bad); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid credentials") {
		t.Fatalf("wrong password: status = %d body = %s, want 401 invalid credentials", w.Code, w.Body.String())
	}
	// 码错误：401 invalid totp code。
	badCode := totpLoginBody(env.user.Email, env.password, "000000", "")
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", badCode); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid totp code") {
		t.Fatalf("wrong code: status = %d body = %s, want 401 invalid totp code", w.Code, w.Body.String())
	}
	// code 与 recovery_code 均缺：400。
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", totpLoginBody(env.user.Email, env.password, "", "")); w.Code != http.StatusBadRequest {
		t.Fatalf("missing factors: status = %d, want 400", w.Code)
	}
}

// 恢复码登录：命中即消耗（一次性）——首次 200，重放 401；库中哈希数递减。
func TestLoginTOTPRecoveryCodeConsumed(t *testing.T) {
	env := newTotpTestEnv(t)
	codes := env.enableTOTP(t)

	body := func(code string) string {
		return totpLoginBody(env.user.Email, env.password, "", code)
	}
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", body(codes[0])); w.Code != http.StatusOK {
		t.Fatalf("recovery login status = %d (body: %s)", w.Code, w.Body.String())
	}
	if got := env.totp.remainingRecoveryCodes(t, env.user.ID); got != len(codes)-1 {
		t.Fatalf("remaining codes = %d, want %d", got, len(codes)-1)
	}
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", body(codes[0])); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed recovery code status = %d, want 401", w.Code)
	}
}

// TOTP 管理端点：setup 返回 secret+otpauth_url；confirm 返回 10 个恢复码；
// 状态端点反映 enabled/confirmed_at；禁用需凭据（错 403 / 对 204）且写审计。
func TestTOTPManagementEndpoints(t *testing.T) {
	env := newTotpTestEnv(t)

	// 初始状态：未启用。
	w := callJSON(env.router, http.MethodGet, "/api/v1/auth/totp", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("initial status = %d (body: %s), want enabled=false", w.Code, w.Body.String())
	}

	// setup：secret 与 otpauth_url。
	w = callJSON(env.router, http.MethodPost, "/api/v1/auth/totp/setup", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("setup status = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"secret"`) || !strings.Contains(w.Body.String(), `"otpauth_url"`) {
		t.Fatalf("setup body = %s, want secret + otpauth_url", w.Body.String())
	}

	// confirm 错误码 400；正确码 200 且返回 10 个恢复码（一次性明文）。
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/totp/confirm", `{"code":"000000"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("confirm wrong code status = %d, want 400", w.Code)
	}
	codes := env.enableTOTP(t)
	if len(codes) != 10 {
		t.Fatalf("recovery codes = %d, want 10", len(codes))
	}
	if e := env.recorder.find(audit.ActionTOTPEnable); e == nil || e.UserID == nil || *e.UserID != env.user.ID {
		t.Fatalf("totp.enable audit missing: %+v", e)
	}

	// 已启用：setup 409；confirm 409。
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/totp/setup", `{}`); w.Code != http.StatusConflict {
		t.Fatalf("setup while enabled status = %d, want 409", w.Code)
	}
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/totp/confirm", `{"code":"000000"}`); w.Code != http.StatusConflict {
		t.Fatalf("confirm while enabled status = %d, want 409", w.Code)
	}

	// 状态：enabled=true 且带 confirmed_at。
	if w := callJSON(env.router, http.MethodGet, "/api/v1/auth/totp", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), "confirmed_at") {
		t.Fatalf("enabled status = %d (body: %s)", w.Code, w.Body.String())
	}

	// 禁用：均空 400；错密码 403；正确密码 204 + 审计 + 状态回落。
	if w := callJSON(env.router, http.MethodDelete, "/api/v1/auth/totp", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty credentials status = %d, want 400", w.Code)
	}
	if w := callJSON(env.router, http.MethodDelete, "/api/v1/auth/totp", `{"password":"WrongPassword123"}`); w.Code != http.StatusForbidden {
		t.Fatalf("wrong password status = %d, want 403", w.Code)
	}
	if w := callJSON(env.router, http.MethodDelete, "/api/v1/auth/totp", `{"password":"`+env.password+`"}`); w.Code != http.StatusNoContent {
		t.Fatalf("disable status = %d (body: %s), want 204", w.Code, w.Body.String())
	}
	if e := env.recorder.find(audit.ActionTOTPDisable); e == nil || e.UserID == nil || *e.UserID != env.user.ID {
		t.Fatalf("totp.disable audit missing: %+v", e)
	}
	if w := callJSON(env.router, http.MethodGet, "/api/v1/auth/totp", ""); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("status after disable = %s, want enabled=false", w.Body.String())
	}
	// 再禁用：404。
	if w := callJSON(env.router, http.MethodDelete, "/api/v1/auth/totp", `{"password":"`+env.password+`"}`); w.Code != http.StatusNotFound {
		t.Fatalf("double disable status = %d, want 404", w.Code)
	}
	// 禁用后 login 不再要求第二因子。
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login", loginBody(env.user.Email, env.password)); w.Code != http.StatusOK {
		t.Fatalf("login after disable status = %d (body: %s), want 200", w.Code, w.Body.String())
	}
	// 禁用后走 login/totp：未启用 → 统一 401 invalid credentials（不泄露状态）。
	if w := callJSON(env.router, http.MethodPost, "/api/v1/auth/login/totp", totpLoginBody(env.user.Email, env.password, "123456", "")); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "invalid credentials") {
		t.Fatalf("login/totp after disable status = %d body = %s, want 401 invalid credentials", w.Code, w.Body.String())
	}
}

// 未注入 TOTPStore：管理端点 503（fail closed，不 panic）；login 不受阻。
func TestTOTPEndpointsNotConfigured(t *testing.T) {
	store := newFakeSessionStore()
	service := auth.NewService(store, "http-totp-test-secret-0123456789", time.Minute, time.Hour)
	account := newFakeAccount()
	user := auth.User{ID: uuid.New(), Username: "carol", Email: "carol@example.com", Status: auth.StatusActive}
	account.seed(user, "SecretPass12345")
	service.SetCredentials(account)
	h := NewHandler(service, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.users = account

	gin.SetMode(gin.TestMode)
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, user.ID)
			handler(c)
		}
	}
	router.GET("/api/v1/auth/totp", withUser(h.totpStatus))
	router.POST("/api/v1/auth/totp/setup", withUser(h.totpSetup))
	router.POST("/api/v1/auth/totp/confirm", withUser(h.totpConfirm))
	router.DELETE("/api/v1/auth/totp", withUser(h.totpDisable))
	router.POST("/api/v1/auth/login", h.login)
	router.POST("/api/v1/auth/login/totp", h.loginTOTP)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/auth/totp", ""},
		{http.MethodPost, "/api/v1/auth/totp/setup", `{}`},
		{http.MethodPost, "/api/v1/auth/totp/confirm", `{"code":"123456"}`},
		{http.MethodDelete, "/api/v1/auth/totp", `{"password":"SecretPass12345"}`},
	} {
		if w := callJSON(router, tc.method, tc.path, tc.body); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s status = %d, want 503", tc.method, tc.path, w.Code)
		}
	}
	// login/totp：store 未配置（功能整体关闭）fail closed → 500，不发 token。
	body := totpLoginBody(user.Email, "SecretPass12345", "123456", "")
	if w := callJSON(router, http.MethodPost, "/api/v1/auth/login/totp", body); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "access_token") {
		t.Fatalf("login/totp unconfigured status = %d body = %s, want 500 without token", w.Code, w.Body.String())
	}
	// login：不受影响（未配置 store 恒 enabled=false）。
	if w := callJSON(router, http.MethodPost, "/api/v1/auth/login", loginBody(user.Email, "SecretPass12345")); w.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", w.Code)
	}
}
