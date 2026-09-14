package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/webhook"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// webhooksTestEnv 构造注入内存 webhook 服务的 Handler。
type webhooksTestEnv struct {
	h     *Handler
	svc   *webhook.Service
	repo  *webhook.MemoryStore
	audit *fakeAuditRecorder
}

// fakeAuditRecorder 记录审计条目（webhook.create/delete 断言用）。
type fakeAuditRecorder struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (f *fakeAuditRecorder) Record(e audit.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeAuditRecorder) snapshot() []audit.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]audit.Entry(nil), f.entries...)
}

func newWebhooksTestEnv(t *testing.T) *webhooksTestEnv {
	t.Helper()
	store := webhook.NewMemoryStore()
	svc := webhook.NewService(store)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	recorder := &fakeAuditRecorder{}
	h.SetAuditRecorder(recorder)
	h.SetWebhooks(svc)
	return &webhooksTestEnv{h: h, svc: svc, repo: store, audit: recorder}
}

// callWebhooks 以 uid 身份调用 webhook 端点（绕过认证中间件，聚焦本人维度语义）。
func callWebhooks(h *Handler, method, path, body string, uid uuid.UUID) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, uid)
			handler(c)
		}
	}
	router.POST("/api/v1/webhooks", withUser(h.createWebhook))
	router.GET("/api/v1/webhooks", withUser(h.listWebhooks))
	router.PATCH("/api/v1/webhooks/:id", withUser(h.updateWebhook))
	router.DELETE("/api/v1/webhooks/:id", withUser(h.deleteWebhook))
	w := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	router.ServeHTTP(w, req)
	return w
}

// 创建：201 + 一次性 secret（whsec_ 前缀）+ 初始状态；审计 webhook.create；
// 重复 URL 409；非法 URL/事件/请求体 400。
func TestCreateWebhookEndpoint(t *testing.T) {
	env := newWebhooksTestEnv(t)
	uid := uuid.New()

	body := `{"url":"https://example.com/hook","events":["upload.completed","share.accessed"]}`
	w := callWebhooks(env.h, http.MethodPost, "/api/v1/webhooks", body, uid)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	resp := w.Body.String()
	if !strings.Contains(resp, `"secret":"whsec_`) {
		t.Fatalf("201 must include one-time secret: %s", resp)
	}
	if !strings.Contains(resp, `"enabled":true`) || !strings.Contains(resp, `"failure_count":0`) || !strings.Contains(resp, `"last_status":null`) {
		t.Fatalf("initial state wrong: %s", resp)
	}
	entries := env.audit.snapshot()
	if len(entries) != 1 || entries[0].Action != audit.ActionWebhookCreate || entries[0].ResourceType != audit.ResourceWebhook {
		t.Fatalf("audit entries = %+v, want webhook.create", entries)
	}

	// 重复 URL：409。
	if w := callWebhooks(env.h, http.MethodPost, "/api/v1/webhooks", body, uid); w.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", w.Code)
	}
	// 非法 URL：400。
	if w := callWebhooks(env.h, http.MethodPost, "/api/v1/webhooks", `{"url":"ftp://x","events":["upload.completed"]}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid url status = %d, want 400", w.Code)
	}
	// 非法事件（空/未知）：400。
	if w := callWebhooks(env.h, http.MethodPost, "/api/v1/webhooks", `{"url":"https://example.com/hook2","events":[]}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("empty events status = %d, want 400", w.Code)
	}
	if w := callWebhooks(env.h, http.MethodPost, "/api/v1/webhooks", `{"url":"https://example.com/hook2","events":["nope"]}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown event status = %d, want 400", w.Code)
	}
	// 非法 JSON：400。
	if w := callWebhooks(env.h, http.MethodPost, "/api/v1/webhooks", `{`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", w.Code)
	}
}

// 列表：本人维度、不含 secret、含投递状态字段。
func TestListWebhooksEndpoint(t *testing.T) {
	env := newWebhooksTestEnv(t)
	uid, other := uuid.New(), uuid.New()
	if _, _, err := env.svc.Create(uid, "https://example.com/mine", []string{"upload.completed"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.svc.Create(other, "https://example.com/theirs", []string{"file.updated"}); err != nil {
		t.Fatal(err)
	}

	w := callWebhooks(env.h, http.MethodGet, "/api/v1/webhooks", "", uid)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "example.com/mine") || strings.Contains(body, "example.com/theirs") {
		t.Fatalf("list must contain only own webhooks: %s", body)
	}
	if strings.Contains(body, "whsec_") {
		t.Fatalf("list must never contain secret: %s", body)
	}
	if !strings.Contains(body, `"last_status"`) || !strings.Contains(body, `"failure_count"`) || !strings.Contains(body, `"enabled"`) {
		t.Fatalf("list must include delivery status fields: %s", body)
	}
}

// 启停：本人 200 返回生效值；非属主/不存在 404；非法 body/ID 400。
func TestUpdateWebhookEndpoint(t *testing.T) {
	env := newWebhooksTestEnv(t)
	uid, other := uuid.New(), uuid.New()
	hook, _, err := env.svc.Create(uid, "https://example.com/hook", []string{"share.accessed"})
	if err != nil {
		t.Fatal(err)
	}

	if w := callWebhooks(env.h, http.MethodPatch, "/api/v1/webhooks/"+hook.ID.String(), `{"enabled":false}`, other); w.Code != http.StatusNotFound {
		t.Fatalf("foreign patch status = %d, want 404", w.Code)
	}
	if w := callWebhooks(env.h, http.MethodPatch, "/api/v1/webhooks/"+uuid.New().String(), `{"enabled":true}`, uid); w.Code != http.StatusNotFound {
		t.Fatalf("nonexistent status = %d, want 404", w.Code)
	}
	w := callWebhooks(env.h, http.MethodPatch, "/api/v1/webhooks/"+hook.ID.String(), `{"enabled":false}`, uid)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("own patch = %d %s", w.Code, w.Body.String())
	}
	if w := callWebhooks(env.h, http.MethodPatch, "/api/v1/webhooks/"+hook.ID.String(), `{}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("missing enabled status = %d, want 400", w.Code)
	}
	if w := callWebhooks(env.h, http.MethodPatch, "/api/v1/webhooks/not-a-uuid", `{"enabled":true}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", w.Code)
	}
}

// 删除：本人 204 幂等语义（重复/非属主 404）；审计 webhook.delete。
func TestDeleteWebhookEndpoint(t *testing.T) {
	env := newWebhooksTestEnv(t)
	uid, other := uuid.New(), uuid.New()
	hook, _, err := env.svc.Create(uid, "https://example.com/hook", []string{"upload.quarantined"})
	if err != nil {
		t.Fatal(err)
	}

	if w := callWebhooks(env.h, http.MethodDelete, "/api/v1/webhooks/"+hook.ID.String(), "", other); w.Code != http.StatusNotFound {
		t.Fatalf("foreign delete status = %d, want 404", w.Code)
	}
	if w := callWebhooks(env.h, http.MethodDelete, "/api/v1/webhooks/"+hook.ID.String(), "", uid); w.Code != http.StatusNoContent {
		t.Fatalf("own delete status = %d, want 204", w.Code)
	}
	if w := callWebhooks(env.h, http.MethodDelete, "/api/v1/webhooks/"+hook.ID.String(), "", uid); w.Code != http.StatusNotFound {
		t.Fatalf("repeat delete status = %d, want 404", w.Code)
	}
	entries := env.audit.snapshot()
	// 该测试经 service 直建 hook（无 HTTP create 审计），仅断言删除审计。
	if len(entries) != 1 || entries[0].Action != audit.ActionWebhookDelete || entries[0].ResourceID != hook.ID.String() {
		t.Fatalf("audit entries = %+v, want webhook.delete for %s", entries, hook.ID)
	}
}

// 未注入服务时端点 503（生产恒注入；此处仅防御性语义）。
func TestWebhooksNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	router := gin.New()
	router.GET("/api/v1/webhooks", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, uuid.New())
		h.listWebhooks(c)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
