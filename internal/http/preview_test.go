package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ---------- 纯函数矩阵 ----------

func TestPreviewResponseAllowMatrix(t *testing.T) {
	tests := []struct {
		mime   string
		wantCT string
	}{
		{"image/png", "image/png"},
		{"image/jpeg", "image/jpeg"},
		{"image/gif", "image/gif"},
		{"image/webp", "image/webp"},
		{"image/png; name=x.png", "image/png; name=x.png"},
		{"application/pdf", "application/pdf"},
		{"text/plain", "text/plain; charset=utf-8"},
		{"text/plain; charset=utf-8", "text/plain; charset=utf-8"},
		{"text/plain; charset=gbk", "text/plain; charset=utf-8"}, // 强制 utf-8
		{"application/json", "application/json"},
		{"application/json; charset=utf-8", "application/json; charset=utf-8"},
		{"IMAGE/PNG", "image/png"}, // 类型大小写归一
		{"Text/Plain", "text/plain; charset=utf-8"},
	}
	for _, test := range tests {
		ct, ok := previewResponse(test.mime)
		if !ok {
			t.Errorf("previewResponse(%q) unexpectedly rejected", test.mime)
			continue
		}
		if ct != test.wantCT {
			t.Errorf("previewResponse(%q) = %q, want %q", test.mime, ct, test.wantCT)
		}
	}
}

func TestPreviewResponseRejectMatrix(t *testing.T) {
	rejected := []string{
		"image/svg+xml",                // SVG 一律拒绝（XSS）
		"image/svg+xml; charset=utf-8", // 带参数同样拒绝
		"IMAGE/SVG+XML",                // 大小写绕过无效
		"text/html",                    // HTML 拒绝
		"text/html; charset=utf-8",
		"text/javascript", // JS 拒绝
		"application/javascript",
		"application/x-msdownload", // 可执行文件
		"application/vnd.microsoft.portable-executable",
		"application/zip", // 压缩包
		"application/x-zip-compressed",
		"application/x-tar",
		"application/octet-stream",
		"video/mp4",
		"audio/mpeg",
		"application/x-sh",
		"text/csv",
		"text/xml",
		"application/xhtml+xml",
		"",           // 空 MIME
		"not a mime", // 非法 MIME
		"text/",      // 缺 subtype
		"image/",
		"image/svg", // SVG 历史非标准变体，同样按 image/svg* 前缀拒绝
		"image/svg-xml",
	}
	for _, mime := range rejected {
		if _, ok := previewResponse(mime); ok {
			t.Errorf("previewResponse(%q) unexpectedly allowed", mime)
		}
	}
}

func TestInlineDisposition(t *testing.T) {
	tests := []struct {
		filename string
		want     string
	}{
		{"photo.png", `inline; filename="photo.png"`},
		{"my photo.png", `inline; filename="my photo.png"`},
		{"图片.png", `inline; filename="__.png"; filename*=UTF-8''%E5%9B%BE%E7%89%87.png`},
	}
	for _, test := range tests {
		if got := inlineDisposition(test.filename); got != test.want {
			t.Errorf("inlineDisposition(%q) = %q, want %q", test.filename, got, test.want)
		}
	}
	for _, name := range []string{"正常.png", `quote".png`, "nl\nx"} {
		value := inlineDisposition(name)
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] == 0x7f {
				t.Fatalf("inlineDisposition(%q) contains control byte", name)
			}
		}
	}
}

// ---------- servePreviewBlob（不依赖 DB：Handler 仅注入存储） ----------

func newPreviewHandler(t *testing.T) (*Handler, *upload.LocalStorage) {
	t.Helper()
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	return &Handler{storage: storage}, storage
}

func putPreviewObject(t *testing.T, storage *upload.LocalStorage, key, content string) {
	t.Helper()
	if err := storage.Put(key, strings.NewReader(content)); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func previewRequest(rangeHeader string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/preview", nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	c.Request = req
	return c, w
}

func TestServePreviewUnsupportedType415(t *testing.T) {
	h, storage := newPreviewHandler(t)
	putPreviewObject(t, storage, "objects/logo.svg", "<svg onload=alert(1)>")
	blob := files.ObjectBlob{StorageKey: "objects/logo.svg", Size: 22, MimeType: "image/svg+xml", Status: files.BlobStatusAvailable}

	c, w := previewRequest("")
	views := 0
	h.servePreviewBlob(c, "logo.svg", blob, func() error { views++; return nil })
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg preview status = %d, want 415", w.Code)
	}
	if !strings.Contains(w.Body.String(), "preview not supported") {
		t.Fatalf("415 body = %q, want message %q", w.Body.String(), "preview not supported")
	}
	if views != 0 {
		t.Fatal("unsupported type must not increment view_count")
	}

	// HTML 与可执行文件同样 415。
	for _, mime := range []string{"text/html", "application/x-msdownload", "application/zip"} {
		c, w := previewRequest("")
		h.servePreviewBlob(c, "f", files.ObjectBlob{StorageKey: "objects/logo.svg", Size: 4, MimeType: mime, Status: files.BlobStatusAvailable}, nil)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("mime %s status = %d, want 415", mime, w.Code)
		}
	}
}

func TestServePreviewInlineHeadersAndContent(t *testing.T) {
	h, storage := newPreviewHandler(t)
	putPreviewObject(t, storage, "objects/图片 1.png", "PNGDATA")
	blob := files.ObjectBlob{StorageKey: "objects/图片 1.png", Size: 7, MimeType: "image/png", Status: files.BlobStatusAvailable}

	c, w := previewRequest("")
	views := 0
	h.servePreviewBlob(c, "图片 1.png", blob, func() error { views++; return nil })
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Disposition"); got != `inline; filename="__ 1.png"; filename*=UTF-8''%E5%9B%BE%E7%89%87%201.png` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if w.Body.String() != "PNGDATA" {
		t.Fatalf("body = %q", w.Body.String())
	}
	if views != 1 {
		t.Fatalf("view callback calls = %d, want 1", views)
	}

	// text/plain 强制 charset=utf-8。
	putPreviewObject(t, storage, "objects/a.txt", "hello")
	txtBlob := files.ObjectBlob{StorageKey: "objects/a.txt", Size: 5, MimeType: "text/plain; charset=gbk", Status: files.BlobStatusAvailable}
	c, w = previewRequest("")
	h.servePreviewBlob(c, "a.txt", txtBlob, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("txt status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("txt Content-Type = %q, want forced charset=utf-8", got)
	}
}

func TestServePreviewRange(t *testing.T) {
	h, storage := newPreviewHandler(t)
	putPreviewObject(t, storage, "objects/r.bin", "0123456789")
	blob := files.ObjectBlob{StorageKey: "objects/r.bin", Size: 10, MimeType: "application/pdf", Status: files.BlobStatusAvailable}

	c, w := previewRequest("bytes=2-5")
	h.servePreviewBlob(c, "r.pdf", blob, nil)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}
	if w.Body.String() != "2345" {
		t.Fatalf("range body = %q, want 2345", w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Fatalf("range Content-Disposition = %q, want inline", got)
	}

	c, w = previewRequest("bytes=99-")
	h.servePreviewBlob(c, "r.pdf", blob, nil)
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range status = %d, want 416", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes */10" {
		t.Fatalf("416 Content-Range = %q", got)
	}
}

func TestServePreviewBlobNotAvailable(t *testing.T) {
	h, _ := newPreviewHandler(t)
	blob := files.ObjectBlob{StorageKey: "objects/x.png", Size: 1, MimeType: "image/png", Status: files.BlobStatusQuarantined}
	c, w := previewRequest("")
	views := 0
	h.servePreviewBlob(c, "x.png", blob, func() error { views++; return nil })
	if w.Code != http.StatusForbidden {
		t.Fatalf("quarantined blob status = %d, want 403", w.Code)
	}
	if views != 0 {
		t.Fatal("unavailable blob must not increment view_count")
	}
}

// ---------- 公开分享预览端点（完整链路：内存分享仓库 + 内存文件源 + 本地存储） ----------
// 说明：认证端点 previewFile 依赖 *files.Store（具体类型，需 gorm/PostgreSQL），
// 现有 download 测试同样只测纯函数（无 DB 基建），故认证端点由共用的 servePreviewBlob
// 测试覆盖，公开端点因 share.FileSource 为接口可做完整 httptest 链路验证。

// previewFakeSource 是 share.FileSource 的最小内存实现。
type previewFakeSource struct {
	owner   uuid.UUID
	file    files.File
	version files.FileVersion
	blob    files.ObjectBlob
	views   int64
}

func (f *previewFakeSource) Get(owner, id uuid.UUID) (files.File, error) {
	if owner != f.owner || id != f.file.ID || f.file.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return f.file, nil
}

func (f *previewFakeSource) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.Get(owner, fileID); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	return f.version, f.blob, nil
}

func (f *previewFakeSource) IncrementDownloadCount(owner, fileID uuid.UUID) error {
	return nil
}

func (f *previewFakeSource) IncrementViewCount(owner, fileID uuid.UUID) error {
	f.views++
	return nil
}

func newPublicPreviewRouter(t *testing.T, mime string) (*gin.Engine, *previewFakeSource, string, string) {
	t.Helper()
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	content := "PREVIEW-CONTENT"
	if err := storage.Put("objects/pub", strings.NewReader(content)); err != nil {
		t.Fatalf("put object: %v", err)
	}
	owner := uuid.New()
	source := &previewFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "共享文件", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/pub", Size: int64(len(content)), MimeType: mime, Status: files.BlobStatusAvailable},
	}
	svc := share.NewService(share.NewMemoryStore(), source)
	_, token, err := svc.Create(owner, source.file.ID, share.PermissionView, 0, nil)
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	handler := NewHandler(nil, nil, nil, svc, nil, nil, storage, false, "", 0)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	return router, source, token, content
}

func TestPublicSharePreviewViewPermission(t *testing.T) {
	router, source, token, content := newPublicPreviewRouter(t, "image/png")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/"+token+"/preview", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("view-permission preview status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	disposition := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "inline;") {
		t.Fatalf("Content-Disposition = %q, want inline", disposition)
	}
	if !strings.Contains(disposition, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition = %q, want RFC 5987 filename*", disposition)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}
	if w.Body.String() != content {
		t.Fatalf("body = %q, want %q", w.Body.String(), content)
	}
	if source.views != 1 {
		t.Fatalf("view_count increments = %d, want 1", source.views)
	}
}

func TestPublicSharePreviewUnsupported415(t *testing.T) {
	router, source, token, _ := newPublicPreviewRouter(t, "image/svg+xml")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/"+token+"/preview", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("svg public preview status = %d, want 415", w.Code)
	}
	if !strings.Contains(w.Body.String(), "preview not supported") {
		t.Fatalf("415 body = %q", w.Body.String())
	}
	if source.views != 0 {
		t.Fatalf("svg public preview view increments = %d, want 0", source.views)
	}
}

func TestPublicSharePreviewUnknownToken404(t *testing.T) {
	router, _, _, _ := newPublicPreviewRouter(t, "image/png")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/no-such-token/preview", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown token status = %d, want 404", w.Code)
	}
}

func TestPublicSharePreviewExpiredGone410(t *testing.T) {
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	owner := uuid.New()
	source := &previewFakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: "f.png", OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "k", Size: 1, MimeType: "image/png", Status: files.BlobStatusAvailable},
	}
	token, err := share.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	repo := share.NewMemoryStore()
	repo.Put(share.Share{
		ID: uuid.New(), OwnerID: owner, FileID: source.file.ID,
		TokenHash: share.HashToken(token), Permission: share.PermissionView,
		ExpiresAt: &past, CreatedAt: past.Add(-time.Hour),
	})
	svc := share.NewService(repo, source)
	handler := NewHandler(nil, nil, nil, svc, nil, nil, storage, false, "", 0)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 1000, 1000, 1000)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/shares/"+token+"/preview", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusGone {
		t.Fatalf("expired share status = %d, want 410", w.Code)
	}
}

func TestPreviewRoutesRegistered(t *testing.T) {
	handler := NewHandler(nil, nil, nil, share.NewService(share.NewMemoryStore(), nil), nil, nil, nil, false, "", 0)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 120, 10, 60)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/files/:id/preview"},
		{http.MethodGet, "/api/v1/public/shares/:token/preview"},
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
}
