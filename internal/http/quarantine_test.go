package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeQuarantineService 是 quarantineService 的内存实现（动作矩阵测试）。
type fakeQuarantineService struct {
	items     []QuarantineItem
	rescanFn  func(string) (string, error)
	releaseFn func(string) error
	deleteFn  func(string) error
}

func (f *fakeQuarantineService) List(limit int) ([]QuarantineItem, error) {
	if limit > 0 && len(f.items) > limit {
		return f.items[:limit], nil
	}
	return f.items, nil
}

func (f *fakeQuarantineService) Rescan(sha256 string) (string, error) {
	if f.rescanFn != nil {
		return f.rescanFn(sha256)
	}
	return files.BlobStatusAvailable, nil
}

func (f *fakeQuarantineService) Release(sha256 string) error {
	if f.releaseFn != nil {
		return f.releaseFn(sha256)
	}
	return nil
}

func (f *fakeQuarantineService) Delete(sha256 string) error {
	if f.deleteFn != nil {
		return f.deleteFn(sha256)
	}
	return nil
}

func quarantineContext(method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

// flushHeader 在 CreateTestContext 直调 handler 的模式下，把 gin 延迟写的
// 状态码刷新到 recorder（204 等无 body 响应否则保持默认 200）。
func flushHeader(c *gin.Context) {
	if c.Writer != nil {
		c.Writer.WriteHeaderNow()
	}
}

// TestListQuarantine 列表视图与 limit 校验、服务未注入 503。
func TestListQuarantine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().UTC()
	fileID, fileName := uuid.New().String(), "malware.exe"
	h := &Handler{quarantine: &fakeQuarantineService{items: []QuarantineItem{
		{SHA256: "aa", Size: 10, MimeType: "application/x-dosexec", RefCount: 2, CreatedAt: now, FileID: &fileID, FileName: &fileName},
	}}}
	c, w := quarantineContext(http.MethodGet, "/api/v1/admin/quarantine", "")
	h.listQuarantine(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"sha256":"aa"`, `"file_name":"malware.exe"`, `"ref_count":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	// 非法 limit 400。
	c, w = quarantineContext(http.MethodGet, "/api/v1/admin/quarantine?limit=x", "")
	h.listQuarantine(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, want 400", w.Code)
	}
	// 未注入 503（不 panic）。
	c, w = quarantineContext(http.MethodGet, "/api/v1/admin/quarantine", "")
	(&Handler{}).listQuarantine(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d, want 503", w.Code)
	}
}

// TestQuarantineActionMatrix 处置动作矩阵：rescan/release（须 confirm=true）/
// delete（204）/ 无确认拒绝 / 未知动作 400 / 未注入 503 / 不存在 404。
func TestQuarantineActionMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := uuid.New()
	newHandler := func(svc *fakeQuarantineService) *Handler {
		return &Handler{quarantine: svc, audit: audit.NopRecorder{}}
	}
	call := func(h *Handler, body string) *httptest.ResponseRecorder {
		c, w := quarantineContext(http.MethodPost, "/api/v1/admin/quarantine/aa/action", body)
		c.Params = gin.Params{{Key: "sha256", Value: "aa"}}
		c.Set("user_id", admin)
		h.quarantineAction(c)
		flushHeader(c)
		return w
	}

	t.Run("rescan 通过置 available", func(t *testing.T) {
		w := call(newHandler(&fakeQuarantineService{}), `{"action":"rescan"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"available"`) {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("rescan 未通过维持 quarantined", func(t *testing.T) {
		svc := &fakeQuarantineService{rescanFn: func(string) (string, error) { return files.BlobStatusQuarantined, nil }}
		w := call(newHandler(svc), `{"action":"rescan"}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"quarantined"`) {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("release 无确认拒绝", func(t *testing.T) {
		for _, body := range []string{`{"action":"release"}`, `{"action":"release","confirm":false}`} {
			w := call(newHandler(&fakeQuarantineService{}), body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "confirm") {
				t.Fatalf("body %s: status = %d resp = %s, want 400 CONFIRM_REQUIRED", body, w.Code, w.Body.String())
			}
		}
	})

	t.Run("release 确认后成功", func(t *testing.T) {
		w := call(newHandler(&fakeQuarantineService{}), `{"action":"release","confirm":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("delete 成功 204", func(t *testing.T) {
		w := call(newHandler(&fakeQuarantineService{}), `{"action":"delete"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("未知动作 400 / 非法 JSON 400", func(t *testing.T) {
		if w := call(newHandler(&fakeQuarantineService{}), `{"action":"purge"}`); w.Code != http.StatusBadRequest {
			t.Fatalf("unknown action status = %d, want 400", w.Code)
		}
		if w := call(newHandler(&fakeQuarantineService{}), `not-json`); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid json status = %d, want 400", w.Code)
		}
	})

	t.Run("不存在 404 / 未注入 503", func(t *testing.T) {
		svc := &fakeQuarantineService{
			rescanFn:  func(string) (string, error) { return "", files.ErrNotFound },
			releaseFn: func(string) error { return files.ErrNotFound },
			deleteFn:  func(string) error { return files.ErrNotFound },
		}
		h := newHandler(svc)
		if w := call(h, `{"action":"rescan"}`); w.Code != http.StatusNotFound {
			t.Fatalf("rescan missing status = %d, want 404", w.Code)
		}
		if w := call(h, `{"action":"release","confirm":true}`); w.Code != http.StatusNotFound {
			t.Fatalf("release missing status = %d, want 404", w.Code)
		}
		if w := call(h, `{"action":"delete"}`); w.Code != http.StatusNotFound {
			t.Fatalf("delete missing status = %d, want 404", w.Code)
		}
		c, w := quarantineContext(http.MethodPost, "/api/v1/admin/quarantine/aa/action", `{"action":"rescan"}`)
		(&Handler{}).quarantineAction(c)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("unconfigured status = %d, want 503", w.Code)
		}
	})
}

// TestQuarantineActionAudit 处置动作写审计（release 为关键防线，须记录 actor）。
func TestQuarantineActionAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := &memAuditRecorder{}
	h := &Handler{quarantine: &fakeQuarantineService{}, audit: rec}
	admin := uuid.New()
	c, w := quarantineContext(http.MethodPost, "/api/v1/admin/quarantine/aa/action", `{"action":"release","confirm":true}`)
	c.Params = gin.Params{{Key: "sha256", Value: "aa"}}
	c.Set("user_id", admin)
	h.quarantineAction(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(rec.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(rec.entries))
	}
	e := rec.entries[0]
	if e.Action != audit.ActionQuarantineRelease || e.ResourceID != "aa" || e.UserID == nil || *e.UserID != admin {
		t.Fatalf("audit entry = %+v", e)
	}
}
