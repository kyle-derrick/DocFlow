package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/search"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// callSearch 以 uid 身份调用 GET /api/v1/search（绕过认证中间件）。
func callSearch(h *Handler, path string, uid uuid.UUID) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/v1/search", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, uid)
		h.searchFiles(c)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// newSearchTestHandler 注入内存检索实现并预置一条可命中数据。
func newSearchTestHandler(uid uuid.UUID) *Handler {
	repo := search.NewMemoryRepo()
	fileID := uuid.New()
	repo.PutDoc(search.Doc{FileID: fileID, OwnerID: uid, Name: "notes", Content: "quarterly report"})
	repo.PutFile(fileID, search.MemoryFile{Name: "notes.md", Type: "file", UpdatedAt: time.Now()})
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.SetSearch(search.NewStore(repo))
	return h
}

// q 非空：200 且返回 results（名称命中带 snippet=名称）；q 缺失/空白 400；
// 未注入检索服务 503；limit 非法 400；starred/tag_id 非法 400。
func TestSearchEndpoint(t *testing.T) {
	uid := uuid.New()
	h := newSearchTestHandler(uid)

	w := callSearch(h, "/api/v1/search?q=notes", uid)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `"name":"notes.md"`) || !strings.Contains(body, `"snippet":"notes.md"`) {
		t.Fatalf("unexpected body: %s", body)
	}

	for _, path := range []string{"/api/v1/search", "/api/v1/search?q=", "/api/v1/search?q=%20%20"} {
		if w := callSearch(h, path, uid); w.Code != http.StatusBadRequest {
			t.Fatalf("q missing/blank %q: status = %d, want 400", path, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/search?q=x&limit=0", "/api/v1/search?q=x&limit=abc"} {
		if w := callSearch(h, path, uid); w.Code != http.StatusBadRequest {
			t.Fatalf("bad limit %q: status = %d, want 400", path, w.Code)
		}
	}
	if w := callSearch(h, "/api/v1/search?q=x&starred=maybe", uid); w.Code != http.StatusBadRequest {
		t.Fatalf("bad starred: status = %d, want 400", w.Code)
	}
	if w := callSearch(h, "/api/v1/search?q=x&tag_id=not-a-uuid", uid); w.Code != http.StatusBadRequest {
		t.Fatalf("bad tag_id: status = %d, want 400", w.Code)
	}

	// 未注入检索服务：503。
	bare := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	if w := callSearch(bare, "/api/v1/search?q=x", uid); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: status = %d, want 503", w.Code)
	}
}
