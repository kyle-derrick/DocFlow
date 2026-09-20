package http

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 公开分享接口的限流独立按 IP 计数，不要求认证、不设置 cookie。
func TestPublicLimiterPerIPWithoutAuth(t *testing.T) {
	l := NewRateLimiter(2)
	router := gin.New()
	router.GET("/api/v1/public/shares/:token", publicLimiter(l), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})
	get := func(ip string) *httptest.ResponseRecorder {
		// 不带 Authorization 头与 cookie。
		req := httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/tok", nil)
		req.RemoteAddr = ip + ":4242"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := get("198.51.100.1"); w.Code != http.StatusOK {
		t.Fatalf("first request: %d", w.Code)
	}
	if w := get("198.51.100.1"); w.Code != http.StatusOK {
		t.Fatalf("second request: %d", w.Code)
	}
	w := get("198.51.100.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: want 429, got %d", w.Code)
	}
	if _, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil {
		t.Fatalf("429 must include numeric Retry-After, got %q", w.Header().Get("Retry-After"))
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("public endpoints must not set cookies, got %v", cookies)
	}
	// 不同 IP 独立计数。
	if w := get("198.51.100.2"); w.Code != http.StatusOK {
		t.Fatalf("different IP: %d", w.Code)
	}
}

// 公开接口错误映射：不存在 404、失效 410、权限不足/不可用 403、内部错误不泄露细节。
func TestPublicShareErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"share not found", share.ErrNotFound, http.StatusNotFound},
		{"file not found", share.ErrFileNotFound, http.StatusNotFound},
		{"gone", share.ErrGone, http.StatusGone},
		{"download limit exceeded", share.ErrDownloadLimit, http.StatusGone},
		{"download forbidden", share.ErrDownloadForbidden, http.StatusForbidden},
		{"blob not available", share.ErrFileNotAvailable, http.StatusForbidden},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/tok", nil)
		if !publicShareError(c, tc.err) {
			t.Fatalf("%s: expected error to be handled", tc.name)
		}
		if w.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d", tc.name, w.Code, tc.want)
		}
		if w.Body.Len() == 0 {
			t.Fatalf("%s: response body must not be empty", tc.name)
		}
	}
	// nil 错误不写响应。
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if publicShareError(c, nil) {
		t.Fatal("nil error must not be handled")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status written for nil error: %d", w.Code)
	}
}

func TestNullableIntJSON(t *testing.T) {
	if got := nullableIntJSON(nil); got != "null" {
		t.Fatalf("nullableIntJSON(nil) = %q, want null", got)
	}
	five := 5
	if got := nullableIntJSON(&five); got != "5" {
		t.Fatalf("nullableIntJSON(5) = %q, want 5", got)
	}
}

// 路由注册冒烟测试：认证分享路由与公开路由不得与现有 /api/v1 树冲突（gin 会 panic）。
func TestRegisterShareRoutes(t *testing.T) {
	handler := NewHandler(nil, nil, nil, share.NewService(share.NewMemoryStore(), nil), nil, nil, nil, false, "", 0)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 120, 10, 60)
	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/shares"},
		{http.MethodGet, "/api/v1/shares"},
		{http.MethodGet, "/api/v1/shares/shared-with-me"},
		{http.MethodDelete, "/api/v1/shares/:id"},
		{http.MethodGet, "/api/v1/shares/:id/files/:fid"},
		{http.MethodGet, "/api/v1/shares/:id/files/:fid/download"},
		{http.MethodGet, "/api/v1/shares/:id/files/:fid/preview"},
		{http.MethodGet, "/api/v1/users/lookup"},
		{http.MethodPost, "/api/v1/spaces"},
		{http.MethodGet, "/api/v1/spaces"},
		{http.MethodPost, "/api/v1/spaces/:id/members"},
		{http.MethodDelete, "/api/v1/spaces/:id/members/:uid"},
		{http.MethodGet, "/api/v1/spaces/:id/members"},
		{http.MethodGet, "/api/v1/spaces/:id/files"},
		{http.MethodPost, "/api/v1/spaces/:id/folders"},
		{http.MethodGet, "/api/v1/public/shares/:token"},
		{http.MethodGet, "/api/v1/public/shares/:token/download"},
	} {
		found := false
		for _, r := range router.Routes() {
			if r.Method == route.method && r.Path == route.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("route %s %s not registered", route.method, route.path)
		}
	}
	// 公开路由未挂认证中间件：无 Authorization 头也应进入处理链（此处返回业务错误而非 401）。
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/whatever", nil)
	req.RemoteAddr = "192.0.2.9:5555"
	router.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("public share route must not require authentication")
	}
}

// ---------- 列表增强与「与我共享」（内存分享仓库 + 内存文件源） ----------

// newShareListEnv 构造带单文件的分享服务（file 元数据经 previewFakeSource 提供）。
func newShareListEnv(t *testing.T) (*share.Service, *previewFakeSource, uuid.UUID, uuid.UUID) {
	t.Helper()
	owner := uuid.New()
	source := &previewFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "report.txt", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "k", Size: 10, MimeType: "text/plain", Status: files.BlobStatusAvailable},
	}
	svc := share.NewService(share.NewMemoryStore(), source)
	return svc, source, owner, source.file.ID
}

func authContext(user uuid.UUID, path string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, path, nil)
	c.Set(auth.UserIDContextKey, user)
	return c, w
}

// GET /shares：每条附 file_name 与 visibility，保留现有字段。
func TestListSharesIncludesFileNameAndVisibility(t *testing.T) {
	svc, _, owner, fileID := newShareListEnv(t)
	if _, _, err := svc.Create(owner, fileID, share.PermissionView, 0, nil); err != nil {
		t.Fatal(err)
	}
	private, err := svc.CreatePrivate(owner, fileID, share.PermissionDownload, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{shares: svc}
	c, w := authContext(owner, "/api/v1/shares")
	h.listShares(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	var payload struct {
		Shares []map[string]any `json:"shares"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Shares) != 2 {
		t.Fatalf("shares = %d, want 2", len(payload.Shares))
	}
	byVisibility := map[string]map[string]any{}
	for _, item := range payload.Shares {
		byVisibility[item["visibility"].(string)] = item
	}
	if _, ok := byVisibility["public"]; !ok {
		t.Fatalf("missing public share: %v", payload.Shares)
	}
	priv, ok := byVisibility["private"]
	if !ok {
		t.Fatalf("missing private share: %v", payload.Shares)
	}
	if priv["file_name"] != "report.txt" {
		t.Fatalf("file_name = %v, want report.txt", priv["file_name"])
	}
	if priv["id"] != private.ID.String() {
		t.Fatalf("id = %v, want %v", priv["id"], private.ID)
	}
	// 现有字段保留。
	for _, key := range []string{"file_id", "permission", "expires_at", "max_downloads", "download_count", "revoked_at", "created_at"} {
		if _, ok := priv[key]; !ok {
			t.Errorf("share item missing existing field %q: %v", key, priv)
		}
	}
}

// GET /shares/shared-with-me：显式授权用户可见，附文件元数据与分享者用户名；
// 陌生人空列表；公开分享不算。
func TestListSharedWithMeHandler(t *testing.T) {
	svc, _, owner, fileID := newShareListEnv(t)
	grantee, stranger := uuid.New(), uuid.New()
	if _, _, err := svc.Create(owner, fileID, share.PermissionView, 0, nil); err != nil {
		t.Fatal(err)
	}
	sh, err := svc.CreatePrivate(owner, fileID, share.PermissionDownload, 0, nil, []uuid.UUID{grantee}, nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.SetUserDirectory(&fakeUserDirectory{names: map[uuid.UUID]string{owner: "alice"}})
	h := &Handler{shares: svc}

	c, w := authContext(grantee, "/api/v1/shares/shared-with-me")
	h.listSharedWithMe(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	var payload struct {
		Shares []map[string]any `json:"shares"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Shares) != 1 {
		t.Fatalf("shares = %d, want 1 (public excluded): %s", len(payload.Shares), w.Body.String())
	}
	item := payload.Shares[0]
	if item["id"] != sh.ID.String() {
		t.Fatalf("id = %v, want %v", item["id"], sh.ID)
	}
	if item["file_id"] != fileID.String() {
		t.Fatalf("file_id = %v", item["file_id"])
	}
	if item["name"] != "report.txt" {
		t.Fatalf("name = %v, want report.txt", item["name"])
	}
	if item["mime_type"] != "text/plain" {
		t.Fatalf("mime_type = %v", item["mime_type"])
	}
	if item["permission"] != share.PermissionDownload {
		t.Fatalf("permission = %v", item["permission"])
	}
	if item["owner_username"] != "alice" {
		t.Fatalf("owner_username = %v, want alice", item["owner_username"])
	}
	if _, exposed := item["email"]; exposed {
		t.Fatal("shared-with-me must not expose email")
	}
	if item["size"].(float64) != 10 {
		t.Fatalf("size = %v, want 10", item["size"])
	}

	// 陌生人视角：空列表；owner 自身创建的分享不算「分享给我」。
	for _, who := range []uuid.UUID{stranger, owner} {
		c, w := authContext(who, "/api/v1/shares/shared-with-me")
		h.listSharedWithMe(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var empty struct {
			Shares []map[string]any `json:"shares"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
			t.Fatal(err)
		}
		if len(empty.Shares) != 0 {
			t.Fatalf("user %v sees %d items, want 0", who, len(empty.Shares))
		}
	}
}

// readFailStorage 的 Read 一律失败（补偿路径测试用）。
type readFailStorage struct{}

func (readFailStorage) Put(string, io.Reader) error             { return nil }
func (readFailStorage) Append(string, io.Reader) (int64, error) { return 0, nil }
func (readFailStorage) Read(string) (io.ReadCloser, error) {
	return nil, errors.New("storage down")
}
func (readFailStorage) Delete(string) error { return nil }

// 下载内容读取失败（500）时补偿回退已消耗的分享 download_count：
// 公开下载与私有分享下载（登录用户）两条路径均覆盖。
func TestShareDownloadReadFailureCompensatesCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	owner := uuid.New()
	source := &previewFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "f.txt", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "k", Size: 4, MimeType: "text/plain", Status: files.BlobStatusAvailable},
	}
	repo := share.NewMemoryStore()
	svc := share.NewService(repo, source)
	h := &Handler{shares: svc, storage: readFailStorage{}, audit: audit.NopRecorder{}}

	// 公开下载：ResolveForDownload 已递增计数 → Read 失败 500 → 补偿回 0。
	pub, token, err := svc.Create(owner, source.file.ID, share.PermissionDownload, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/"+token+"/download", nil)
	c.Params = gin.Params{{Key: "token", Value: token}}
	h.publicShareDownload(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("public download read failure: status = %d, body = %s", w.Code, w.Body.String())
	}
	stored, err := repo.Get(pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DownloadCount != 0 {
		t.Fatalf("public share download_count after compensation = %d, want 0", stored.DownloadCount)
	}

	// 私有分享下载（登录用户）：同样补偿。
	grantee := uuid.New()
	private, err := svc.CreatePrivate(owner, source.file.ID, share.PermissionDownload, 0, nil, []uuid.UUID{grantee}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/shares/"+private.ID.String()+"/files/"+source.file.ID.String()+"/download", nil)
	c.Params = gin.Params{{Key: "id", Value: private.ID.String()}, {Key: "fid", Value: source.file.ID.String()}}
	c.Set(auth.UserIDContextKey, grantee)
	h.shareFileDownload(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("private download read failure: status = %d, body = %s", w.Code, w.Body.String())
	}
	stored, err = repo.Get(private.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DownloadCount != 0 {
		t.Fatalf("private share download_count after compensation = %d, want 0", stored.DownloadCount)
	}
}
