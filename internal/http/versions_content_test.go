package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeVersionReader 是 versionContentReader 的内存实现：按 (user,file,version)
// 返回预置结果（错误场景注入）。
type fakeVersionReader struct {
	calls    int
	lastUser uuid.UUID
	result   map[string]struct {
		user uuid.UUID
		f    files.File
		v    files.FileVersion
		b    files.ObjectBlob
		err  error
	}
}

func newFakeVersionReader() *fakeVersionReader {
	return &fakeVersionReader{result: make(map[string]struct {
		user uuid.UUID
		f    files.File
		v    files.FileVersion
		b    files.ObjectBlob
		err  error
	})}
}

func (f *fakeVersionReader) stub(user uuid.UUID, f2 files.File, v files.FileVersion, b files.ObjectBlob, err error) {
	f.result[v.ID.String()] = struct {
		user uuid.UUID
		f    files.File
		v    files.FileVersion
		b    files.ObjectBlob
		err  error
	}{user, f2, v, b, err}
}

func (f *fakeVersionReader) ReadVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, files.ObjectBlob, error) {
	f.calls++
	f.lastUser = user
	stub, ok := f.result[versionID.String()]
	if !ok {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	if stub.user != user {
		// 非 owner 请求他人文件：与生产 Get 一致按 ErrNotFound 处理
		//（个人文件不泄露存在性）。
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	return stub.f, stub.v, stub.b, stub.err
}

// callVersionContent 以 uid 身份调用版本内容端点（绕过中间件，聚焦权限语义）。
func callVersionContent(h *Handler, uid uuid.UUID, fileID, versionID string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/files/:id/versions/:versionId/content", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, uid)
		h.fileVersionContent(c)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileID+"/versions/"+versionID+"/content", nil))
	return w
}

func newVersionContentHandler(reader *fakeVersionReader) *Handler {
	h := NewHandler(nil, nil, nil, nil, nil, nil, newMemStorage(), false, "", time.Hour)
	h.versionReader = reader
	return h
}

func TestFileVersionContentOK(t *testing.T) {
	owner := uuid.New()
	fileID, versionID := uuid.New(), uuid.New()
	storage := newMemStorage()
	if err := storage.Put("objects/v1", strings.NewReader("hello\nworld\n")); err != nil {
		t.Fatal(err)
	}
	reader := newFakeVersionReader()
	reader.stub(owner, files.File{ID: fileID, Name: "notes.txt", OwnerID: owner, Type: "file"}, files.FileVersion{ID: versionID, FileID: fileID, Version: 2}, files.ObjectBlob{StorageKey: "objects/v1", Size: 12, MimeType: "text/plain", Status: files.BlobStatusAvailable}, nil)
	h := newVersionContentHandler(reader)
	h.storage = storage

	w := callVersionContent(h, owner, fileID.String(), versionID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "hello\nworld\n" {
		t.Fatalf("body = %q", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `inline; filename="notes.txt"` {
		t.Fatalf("content-disposition = %q", cd)
	}
	if nosniff := w.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("nosniff = %q", nosniff)
	}
	if reader.lastUser != owner {
		t.Fatal("read must be authorized as the requesting user")
	}
}

func TestFileVersionContentNotFound(t *testing.T) {
	owner := uuid.New()
	h := newVersionContentHandler(newFakeVersionReader())
	// 版本未知。
	w := callVersionContent(h, owner, uuid.New().String(), uuid.New().String())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown version", w.Code)
	}
	// 非属主访问他人文件（生产语义：个人文件非 owner 一律 ErrNotFound）。
	fileID, versionID := uuid.New(), uuid.New()
	reader := newFakeVersionReader()
	reader.stub(owner, files.File{ID: fileID, OwnerID: owner}, files.FileVersion{ID: versionID, FileID: fileID}, files.ObjectBlob{Status: files.BlobStatusAvailable}, nil)
	h2 := newVersionContentHandler(reader)
	w = callVersionContent(h2, uuid.New(), fileID.String(), versionID.String())
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for other's personal file", w.Code)
	}
}

func TestFileVersionContentForbidden(t *testing.T) {
	owner := uuid.New()
	fileID, versionID := uuid.New(), uuid.New()
	// 团队非成员：ErrForbidden → 403。
	reader := newFakeVersionReader()
	reader.stub(owner, files.File{ID: fileID, OwnerID: owner}, files.FileVersion{ID: versionID, FileID: fileID}, files.ObjectBlob{Status: files.BlobStatusAvailable}, files.ErrForbidden)
	w := callVersionContent(newVersionContentHandler(reader), owner, fileID.String(), versionID.String())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for team non-member", w.Code)
	}
	// blob 非 available（如 quarantined）：403 且附 status。
	reader2 := newFakeVersionReader()
	reader2.stub(owner, files.File{ID: fileID, OwnerID: owner}, files.FileVersion{ID: versionID, FileID: fileID}, files.ObjectBlob{Status: files.BlobStatusQuarantined, MimeType: "text/plain"}, nil)
	w = callVersionContent(newVersionContentHandler(reader2), owner, fileID.String(), versionID.String())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for quarantined blob", w.Code)
	}
	if !contains(w.Body.String(), "quarantined") {
		t.Fatalf("body = %q, want status detail", w.Body.String())
	}
}

func TestFileVersionContentInvalidIDs(t *testing.T) {
	h := newVersionContentHandler(newFakeVersionReader())
	if w := callVersionContent(h, uuid.New(), "not-a-uuid", uuid.New().String()); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid file id", w.Code)
	}
	if w := callVersionContent(h, uuid.New(), uuid.New().String(), "not-a-uuid"); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid version id", w.Code)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
