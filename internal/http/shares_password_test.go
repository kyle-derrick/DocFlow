package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// newPasswordFlowEnv 构造完整公开分享链路（内存仓库 + 内存文件源 + 本地存储），
// 分享带密码与水印，owner 可经 handler 方法直查详情统计。
func newPasswordFlowEnv(t *testing.T) (*gin.Engine, *Handler, *share.Service, *share.MemoryStore, uuid.UUID, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	if err := storage.Put("objects/secret", strings.NewReader("SECRET-CONTENT")); err != nil {
		t.Fatalf("put object: %v", err)
	}
	owner := uuid.New()
	source := &previewFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "机密文档.txt", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/secret", Size: 14, MimeType: "text/plain", Status: files.BlobStatusAvailable},
	}
	repo := share.NewMemoryStore()
	svc := share.NewService(repo, source)
	customTemplate := "{email} {date} {name}"
	_, token, err := svc.CreatePublic(owner, source.file.ID, share.PermissionDownload, 0, nil,
		share.ShareOptions{Password: "s3cret-pass", WatermarkText: &customTemplate})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	h := NewHandler(nil, nil, nil, svc, nil, nil, storage, false, "", 0)
	h.SetAuditRecorder(audit.NopRecorder{})
	router := gin.New()
	h.Register(router, "0123456789abcdef0123456789abcdef", 1_000_000, 1_000_000, 1_000_000)
	return router, h, svc, repo, owner, token
}

func doPublic(router *gin.Engine, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.RemoteAddr = "203.0.113.7:9999"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// 密码保护全链路：无 cookie 401 PASSWORD_REQUIRED（且不消耗下载计数）→
// 错误密码 401 → 正确密码 {ok:true} + 会话 cookie（HttpOnly/Lax/1h/
// Path=/api/v1/public）→ 携带 cookie 的 info/preview/download 正常，
// 水印字段返回渲染结果，下载/预览成功写入访问事件（owner 详情统计可见）。
func TestPublicSharePasswordFlow(t *testing.T) {
	router, h, svc, repo, owner, token := newPasswordFlowEnv(t)
	infoPath := "/api/v1/public/shares/" + token
	verifyPath := infoPath + "/verify"

	// 无 cookie：元数据 401 + code=PASSWORD_REQUIRED。
	w := doPublic(router, http.MethodGet, infoPath, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("info without session: %d (body %s)", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "PASSWORD_REQUIRED" || body["error"] != "password_required" {
		t.Fatalf("401 body = %v", body)
	}
	// 无 cookie：下载同样 401，且不得消耗 download_count（先于消耗路径拦截）。
	w = doPublic(router, http.MethodGet, infoPath+"/download", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("download without session: %d", w.Code)
	}
	sh, err := svc.Resolve(token)
	if err != nil {
		t.Fatal(err)
	}
	if sh.Share.DownloadCount != 0 {
		t.Fatalf("download_count = %d after 401, want 0", sh.Share.DownloadCount)
	}
	// 无 cookie：预览同样 401。
	if w := doPublic(router, http.MethodGet, infoPath+"/preview", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("preview without session: %d", w.Code)
	}

	// 错误密码：401，不设 cookie。
	postVerify := func(password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, verifyPath, strings.NewReader(fmt.Sprintf(`{"password":%q}`, password)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.7:9999"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	w = postVerify("wrong-password")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d (body %s)", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatalf("wrong password must not set cookies, got %v", w.Result().Cookies())
	}

	// 正确密码：{ok:true} + 会话 cookie 属性矩阵。
	w = postVerify("s3cret-pass")
	if w.Code != http.StatusOK {
		t.Fatalf("correct password: %d (body %s)", w.Code, w.Body.String())
	}
	var ok map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &ok); err != nil || ok["ok"] != true {
		t.Fatalf("verify body = %s", w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("verify must set exactly one cookie, got %v", cookies)
	}
	cookie := cookies[0]
	wantName := "docflow_sa_" + token[:8]
	if cookie.Name != wantName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, wantName)
	}
	if cookie.Path != "/api/v1/public" || cookie.MaxAge != 3600 || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attributes = %+v (want Path=/api/v1/public MaxAge=3600 HttpOnly Lax)", cookie)
	}
	if cookie.Value == "" || cookie.Value == "s3cret-pass" {
		t.Fatalf("cookie value must be the opaque session value, got %q", cookie.Value)
	}

	// 携带 cookie：元数据 200，附水印渲染结果（IP 前缀脱敏 + 文件名 + DocFlow 侧展示）。
	w = doPublic(router, http.MethodGet, infoPath, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("info with session: %d (body %s)", w.Code, w.Body.String())
	}
	var info map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info["watermark_enabled"] != true {
		t.Fatalf("watermark_enabled = %v", info["watermark_enabled"])
	}
	if text, _ := info["watermark_text"].(string); !strings.Contains(text, "203.0.*") || !strings.Contains(text, "机密文档.txt") {
		t.Fatalf("watermark_text = %q, want rendered template with ip prefix and file name", text)
	}

	// 携带 cookie：预览 200（事件 action=preview）。
	if w := doPublic(router, http.MethodGet, infoPath+"/preview", cookie); w.Code != http.StatusOK {
		t.Fatalf("preview with session: %d", w.Code)
	}
	// 携带 cookie：下载 200（事件 action=download），计数消耗 1。
	w = doPublic(router, http.MethodGet, infoPath+"/download", cookie)
	if w.Code != http.StatusOK || w.Body.String() != "SECRET-CONTENT" {
		t.Fatalf("download with session: %d (body %s)", w.Code, w.Body.String())
	}
	stats, err := repo.ShareAccessStats(sh.Share.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAccess != 2 || stats.UniqueVisitors != 1 {
		t.Fatalf("stats = %+v, want total=2 unique=1", stats)
	}
	actions := map[string]bool{}
	for _, e := range stats.Recent {
		actions[e.Action] = true
		if e.IPPrefix != "203.0.*" {
			t.Fatalf("event ip_prefix = %q, want 203.0.*", e.IPPrefix)
		}
	}
	if !actions[share.ActionDownload] || !actions[share.ActionPreview] {
		t.Fatalf("event actions = %v, want download+preview", actions)
	}

	// owner 详情端点（handler 直调）：stats 含上述事件与脱敏字段。
	c, w2 := authContext(owner, "/api/v1/shares/"+sh.Share.ID.String())
	c.Params = gin.Params{{Key: "id", Value: sh.Share.ID.String()}}
	h.getShare(c)
	if w2.Code != http.StatusOK {
		t.Fatalf("getShare: %d (body %s)", w2.Code, w2.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["has_password"] != true {
		t.Fatalf("detail has_password = %v", detail["has_password"])
	}
	statsField, _ := detail["stats"].(map[string]any)
	if statsField == nil || statsField["total_access"].(float64) != 2 || statsField["unique_visitors"].(float64) != 1 {
		t.Fatalf("detail stats = %v", detail["stats"])
	}
	recent, _ := statsField["recent"].([]any)
	if len(recent) != 2 {
		t.Fatalf("detail recent = %v", recent)
	}
	first, _ := recent[0].(map[string]any)
	if _, hasHash := first["ip_hash"]; hasHash {
		t.Fatal("recent record must not expose ip_hash")
	}
	if first["ip_prefix"] != "203.0.*" {
		t.Fatalf("recent ip_prefix = %v", first["ip_prefix"])
	}
}

// 无密码分享不受影响：无 cookie 直接 200，verify 返回 400。
func TestPublicShareWithoutPasswordUnaffected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Put("objects/plain", strings.NewReader("PLAIN")); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	source := &previewFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "plain.txt", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/plain", Size: 5, MimeType: "text/plain", Status: files.BlobStatusAvailable},
	}
	svc := share.NewService(share.NewMemoryStore(), source)
	_, token, err := svc.Create(owner, source.file.ID, share.PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(nil, nil, nil, svc, nil, nil, storage, false, "", 0)
	router := gin.New()
	h.Register(router, "0123456789abcdef0123456789abcdef", 1_000_000, 1_000_000, 1_000_000)

	if w := doPublic(router, http.MethodGet, "/api/v1/public/shares/"+token, nil); w.Code != http.StatusOK {
		t.Fatalf("passwordless info: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/public/shares/"+token+"/verify", strings.NewReader(`{"password":"whatever"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:9999"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("passwordless verify: %d, want 400", w.Code)
	}
}

// verify 端点限流：同 IP+token 每分钟 5 次，第 6 次 429（带 Retry-After）；
// 不同 token 独立计数。
func TestShareVerifyRateLimitedPerIPAndToken(t *testing.T) {
	router, _, _, _, _, token := newPasswordFlowEnv(t)
	path := "/api/v1/public/shares/" + token + "/verify"
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"password":"guess"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.4:1234"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d must not be limited yet", i+1)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"password":"guess"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.4:1234"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must include Retry-After")
	}
	// 正确密码也会被限流挡住（先到限流中间件）。
	req2 := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"password":"s3cret-pass"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.RemoteAddr = "198.51.100.4:1234"
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password after limit: %d, want 429", w2.Code)
	}
	// 不同 token（未知 token）：进入业务层（404）而非限流。
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/public/shares/other-token/verify", strings.NewReader(`{"password":"guess"}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.RemoteAddr = "198.51.100.4:1234"
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code == http.StatusTooManyRequests {
		t.Fatal("different token must not share the rate bucket")
	}
}

// PATCH 端点（handler 直调）：改水印与有效期，返回更新后的脱敏记录；
// 非 owner 404；非法值 400。
func TestUpdateShareHandler(t *testing.T) {
	router, h, svc, _, owner, token := newPasswordFlowEnv(t)
	_ = router
	sh, err := svc.Resolve(token)
	if err != nil {
		t.Fatal(err)
	}
	c, w := authContext(owner, "/api/v1/shares/"+sh.Share.ID.String())
	c.Params = gin.Params{{Key: "id", Value: sh.Share.ID.String()}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/shares/"+sh.Share.ID.String(), strings.NewReader(`{"watermark_enabled":false,"expires_in":7200,"max_downloads":5}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.updateShare(c)
	if w.Code != http.StatusOK {
		t.Fatalf("updateShare: %d (body %s)", w.Code, w.Body.String())
	}
	var updated map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated["watermark_enabled"] != false || updated["max_downloads"].(float64) != 5 || updated["expires_at"] == nil {
		t.Fatalf("updated share = %v", updated)
	}
	if updated["has_password"] != true {
		t.Fatalf("has_password must persist: %v", updated["has_password"])
	}

	// 非 owner → 404。
	stranger := uuid.New()
	c2, w2 := authContext(stranger, "/api/v1/shares/"+sh.Share.ID.String())
	c2.Params = gin.Params{{Key: "id", Value: sh.Share.ID.String()}}
	c2.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/shares/"+sh.Share.ID.String(), strings.NewReader(`{"max_downloads":9}`))
	c2.Request.Header.Set("Content-Type", "application/json")
	h.updateShare(c2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("non-owner update: %d, want 404", w2.Code)
	}

	// 非法值 → 400。
	c3, w3 := authContext(owner, "/api/v1/shares/"+sh.Share.ID.String())
	c3.Params = gin.Params{{Key: "id", Value: sh.Share.ID.String()}}
	c3.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/shares/"+sh.Share.ID.String(), strings.NewReader(`{"max_downloads":-1}`))
	c3.Request.Header.Set("Content-Type", "application/json")
	h.updateShare(c3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("invalid update: %d, want 400", w3.Code)
	}
}
