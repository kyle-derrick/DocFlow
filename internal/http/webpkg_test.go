package http

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/docflow/docflow/internal/webpkg"
)

// webpkgFakeSource 同时实现 share.FileSource 与 webpkg.FileSource，
// 供公开分享预览与内容端点做完整 httptest 链路验证。
type webpkgFakeSource struct {
	owner   uuid.UUID
	file    files.File
	version files.FileVersion
	blob    files.ObjectBlob
	views   int64
}

func (f *webpkgFakeSource) Get(owner, id uuid.UUID) (files.File, error) {
	if owner != f.owner || id != f.file.ID || f.file.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return f.file, nil
}

func (f *webpkgFakeSource) GetFileByID(id uuid.UUID) (files.File, error) {
	if id != f.file.ID || f.file.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return f.file, nil
}

func (f *webpkgFakeSource) CurrentVersion(owner, id uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.Get(owner, id); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	return f.version, f.blob, nil
}

func (f *webpkgFakeSource) IncrementDownloadCount(owner, id uuid.UUID) error { return nil }

func (f *webpkgFakeSource) IncrementViewCount(owner, id uuid.UUID) error {
	f.views++
	return nil
}

// webpkgSourceSHA 为内容/预览链路 fake blob 的固定 sha256
// （Resolve 校验包 source_blob_sha256 与当前版本一致，空串会被拒绝）。
const webpkgSourceSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// buildWebpkgZip 构造含 index.html 与 svg 资源的合法网页包。
func buildWebpkgZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string]string{
		"index.html":       "<!doctype html><html><body>PKG</body></html>",
		"assets/app.js":    "console.log(1)",
		"assets/bg.svg":    "<svg xmlns='http://www.w3.org/2000/svg'/>",
		"assets/notes.txt": "hi",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newWebpkgContentRouter 构建内容端点链路：本地存储 + 内存 repo + webpkg
// 服务（seed 已解包 ready）；返回路由与 public_id。
func newWebpkgContentRouter(t *testing.T, rateLimit int) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	zipBytes := buildWebpkgZip(t)
	if err := storage.Put("objects/pkg", bytes.NewReader(zipBytes)); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	source := &webpkgFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "site.zip", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/pkg", Size: int64(len(zipBytes)), MimeType: "application/zip", Status: files.BlobStatusAvailable, SHA256: webpkgSourceSHA},
	}
	svc := webpkg.NewService(webpkg.NewMemoryRepo(), source, storage, webpkg.DefaultLimits())
	if _, err := svc.ExtractForFile(source.file.ID); err != nil {
		t.Fatalf("seed extract: %v", err)
	}
	pid, ok := svc.ReadyPackage(source.file.ID)
	if !ok {
		t.Fatal("seed package not ready")
	}
	handler := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", 0)
	handler.SetWebpkg(svc, rateLimit)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	return router, pid
}

func TestWebpkgContentHeaders(t *testing.T) {
	router, pid := newWebpkgContentRouter(t, 1000)

	tests := []struct {
		path        string
		contentType string
	}{
		{"/" + pid + "/index.html", "text/html; charset=utf-8"},
		{"/" + pid + "/assets/app.js", "text/javascript; charset=utf-8"},
		{"/" + pid + "/assets/bg.svg", "image/svg+xml"},
		{"/" + pid + "/assets/notes.txt", "text/plain; charset=utf-8"},
	}
	for _, test := range tests {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/content"+test.path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d (body %s)", test.path, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Security-Policy"); got != webpkgCSP {
			t.Fatalf("%s CSP = %q, want %q", test.path, got, webpkgCSP)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s nosniff = %q", test.path, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "private, max-age=300" {
			t.Fatalf("%s Cache-Control = %q", test.path, got)
		}
		if got := w.Header().Get("Content-Type"); got != test.contentType {
			t.Fatalf("%s Content-Type = %q, want %q", test.path, got, test.contentType)
		}
	}
	// svg 处置选择：内联（无 Content-Disposition 头）——CSP 中的 sandbox
	// 指令使直接导航进入唯一化 origin 沙箱，导航型 XSS 已被覆盖。
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/content/"+pid+"/assets/bg.svg", nil))
	if d := w.Header().Get("Content-Disposition"); d != "" {
		t.Fatalf("svg Content-Disposition = %q, want empty (inline)", d)
	}
}

func TestWebpkgContent404NoDetail(t *testing.T) {
	router, pid := newWebpkgContentRouter(t, 1000)
	paths := []string{
		"/content/" + pid + "/missing.html",      // 不存在条目
		"/content/" + pid + "/../secret.txt",     // 穿越（原始 ..）
		"/content/" + pid + "/%2e%2e/secret.txt", // 穿越（编码 ..）
		"/content/" + pid + "/assets/app.exe",    // 非白名单扩展名
		"/content/" + pid + "/.webpkg-manifest",  // 内部清单
		"/content/UNKNOWNPID/index.html",         // 未知 public_id
		"/content/" + pid,                        // 缺 filepath 段
	}
	for _, path := range paths {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound && w.Code != http.StatusMovedPermanently {
			t.Fatalf("%s status = %d, want 404/301 (body %s)", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "manifest") {
			t.Fatalf("%s leaked detail: %s", path, w.Body.String())
		}
	}
}

func TestWebpkgContentRateLimit(t *testing.T) {
	router, pid := newWebpkgContentRouter(t, 2)
	url := "/content/" + pid + "/index.html"
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i+1, w.Code)
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third request status = %d, want 429", w.Code)
	}
}

// newWebpkgShareRouter 构建公开分享预览链路（webpkg 服务注入但按需解包）。
func newWebpkgShareRouter(t *testing.T, extract bool) (*gin.Engine, *webpkgFakeSource, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	zipBytes := buildWebpkgZip(t)
	if err := storage.Put("objects/pkg", bytes.NewReader(zipBytes)); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	source := &webpkgFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "site.zip", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/pkg", Size: int64(len(zipBytes)), MimeType: "application/zip", Status: files.BlobStatusAvailable, SHA256: webpkgSourceSHA},
	}
	svc := webpkg.NewService(webpkg.NewMemoryRepo(), source, storage, webpkg.DefaultLimits())
	if extract {
		if _, err := svc.ExtractForFile(source.file.ID); err != nil {
			t.Fatal(err)
		}
	}
	shares := share.NewService(share.NewMemoryStore(), source)
	_, token, err := shares.Create(owner, source.file.ID, share.PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, nil, nil, shares, nil, nil, storage, false, "", 0)
	handler.SetWebpkg(svc, 1000)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	return router, source, token
}

// TestPublicSharePreviewWebpkgLinkage：公开分享预览对 zip+ready 包返回
// {kind:webpkg,url} JSON、递增 view_count，且 URL 在内容端点立即可用。
func TestPublicSharePreviewWebpkgLinkage(t *testing.T) {
	router, source, token := newWebpkgShareRouter(t, true)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/"+token+"/preview", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Kind string `json:"kind"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kind != "webpkg" || !strings.HasPrefix(body.URL, "/content/") || !strings.HasSuffix(body.URL, "/index.html") {
		t.Fatalf("body = %+v", body)
	}
	if source.views != 1 {
		t.Fatalf("view_count = %d, want 1", source.views)
	}
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, body.URL, nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("content status = %d", w2.Code)
	}
	if got := w2.Header().Get("Content-Security-Policy"); got != webpkgCSP {
		t.Fatalf("CSP = %q", got)
	}
}

// TestPublicSharePreviewZipWithoutPackage415：zip 但无 ready 包 → 原路径 415。
func TestPublicSharePreviewZipWithoutPackage415(t *testing.T) {
	router, source, token := newWebpkgShareRouter(t, false)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/"+token+"/preview", nil))
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body %s)", w.Code, w.Body.String())
	}
	if source.views != 0 {
		t.Fatalf("view_count = %d, want 0", source.views)
	}
}

func TestWebpkgRoutesRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := webpkg.NewService(webpkg.NewMemoryRepo(), nil, storage, webpkg.DefaultLimits())
	handler := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", 0)
	handler.SetWebpkg(svc, 120)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	found := map[string]bool{}
	for _, r := range router.Routes() {
		found[r.Method+" "+r.Path] = true
	}
	for _, route := range []string{"GET /content/:pid/*filepath", "POST /api/v1/files/:id/webpkg/extract"} {
		if !found[route] {
			t.Errorf("route %s not registered", route)
		}
	}
	// 未注入（SetWebpkg 未调用）时不注册网页包路由。
	handler2 := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", 0)
	router2 := gin.New()
	handler2.Register(router2, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	for _, r := range router2.Routes() {
		if strings.HasPrefix(r.Path, "/content") || strings.Contains(r.Path, "webpkg") {
			t.Errorf("webpkg route %s registered without service", r.Path)
		}
	}
}
