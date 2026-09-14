package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/notify"
)

// Sign 与 RFC 4231 Test Case 1 的已知向量一致（HMAC-SHA256，Key=0x0b*20，
// Data="Hi There"），证明签名实现正确而非自证。
func TestSignKnownVector(t *testing.T) {
	key := make([]byte, 20)
	for i := range key {
		key[i] = 0x0b
	}
	want := "sha256=b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"
	if got := Sign(string(key), []byte("Hi There")); got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
}

// capturedRequest 记录一次到达回调的请求（并发安全）。
type capturedRequest struct {
	mu          sync.Mutex
	body        []byte
	signature   string
	method      string
	contentType string
}

func (c *capturedRequest) record(body []byte, sig, method, ct string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body, c.signature, c.method, c.contentType = body, sig, method, ct
}

func (c *capturedRequest) snapshot() ([]byte, string, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body, c.signature, c.method, c.contentType
}

// seedHook 建立内存 store 中启用状态的 hook（secret 固定，便于验签断言）。
func seedHook(t *testing.T, store *MemoryStore, owner uuid.UUID, url string, events ...string) Webhook {
	t.Helper()
	w := Webhook{
		ID: uuid.New(), UserID: owner, URL: url, Events: EventList(events),
		Secret: "whsec_test-secret", Enabled: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := store.Create(w); err != nil {
		t.Fatal(err)
	}
	return w
}

func deliveryPayload(hookID, userID uuid.UUID, event, title, body string, resource *uuid.UUID) []byte {
	raw, err := MarshalDelivery(hookID, userID, event, title, body, resource)
	if err != nil {
		panic(err)
	}
	return raw
}

// 成功投递：POST JSON {event,timestamp,data}、Content-Type、签名头与
// 请求体字节一致（接收方可用 secret 复算验证）；last_status=200 且
// failure_count 清零。
func TestDeliverTaskSuccessSignatureAndPayload(t *testing.T) {
	store := NewMemoryStore()
	owner := uuid.New()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.record(body, r.Header.Get(SignatureHeader), r.Method, r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hook := seedHook(t, store, owner, srv.URL, notify.EventUploadCompleted)

	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond})
	resource := uuid.New()
	raw := deliveryPayload(hook.ID, owner, notify.EventUploadCompleted, "上传完成：a.txt", "正文", &resource)
	if err := d.DeliverTask(context.Background(), raw); err != nil {
		t.Fatalf("DeliverTask: %v", err)
	}

	body, sig, method, ct := captured.snapshot()
	if method != http.MethodPost {
		t.Fatalf("method = %s, want POST", method)
	}
	if ct != "application/json" {
		t.Fatalf("content-type = %s", ct)
	}
	if want := Sign("whsec_test-secret", body); sig != want {
		t.Fatalf("signature = %q, want %q（须对实际请求体签名）", sig, want)
	}
	var payload struct {
		Event     string `json:"event"`
		Timestamp string `json:"timestamp"`
		Data      struct {
			UserID     string  `json:"user_id"`
			Title      string  `json:"title"`
			Body       string  `json:"body"`
			ResourceID *string `json:"resource_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload json: %v (%s)", err, body)
	}
	if payload.Event != notify.EventUploadCompleted || payload.Data.Title != "上传完成：a.txt" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Data.UserID != owner.String() || payload.Data.ResourceID == nil || *payload.Data.ResourceID != resource.String() {
		t.Fatalf("payload data = %+v", payload.Data)
	}

	got, err := store.Get(hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastStatus == nil || *got.LastStatus != 200 || got.FailureCount != 0 || got.LastDeliveredAt == nil {
		t.Fatalf("after success = status:%v failures:%d delivered:%v", got.LastStatus, got.FailureCount, got.LastDeliveredAt)
	}
}

// 资源为空：data.resource_id 为 null（uuid.Nil 规整）。
func TestDeliverTaskNilResourceOmitted(t *testing.T) {
	store := NewMemoryStore()
	owner := uuid.New()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.record(body, "", r.Method, "")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hook := seedHook(t, store, owner, srv.URL, notify.EventShareAccessed)

	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond})
	if err := d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventShareAccessed, "分享被下载", "b", nil)); err != nil {
		t.Fatal(err)
	}
	body, _, _, _ := captured.snapshot()
	if !strings.Contains(string(body), `"resource_id":null`) {
		t.Fatalf("payload must contain null resource_id: %s", body)
	}
}

// 退避重试：前两次 500、第三次 200 → 共 3 次尝试，最终按成功记账。
func TestDeliverRetriesThenSucceeds(t *testing.T) {
	store := NewMemoryStore()
	owner := uuid.New()
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hook := seedHook(t, store, owner, srv.URL, notify.EventUploadQuarantined)

	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond, 2 * time.Millisecond})
	if err := d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventUploadQuarantined, "t", "b", nil)); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3（1 次 + 2 次重试）", got)
	}
	w, err := store.Get(hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.FailureCount != 0 || w.LastStatus == nil || *w.LastStatus != 200 {
		t.Fatalf("after eventual success = %+v", w)
	}
}

// 连续失败：failure_count 递增并记录 last_status；成功清零。
func TestDeliverFailureCounting(t *testing.T) {
	store := NewMemoryStore()
	owner := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	hook := seedHook(t, store, owner, srv.URL, notify.EventUploadCompleted)

	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond})
	for i := 0; i < 3; i++ {
		if err := d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventUploadCompleted, "t", "b", nil)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := store.Get(hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 连续失败计数按「事件投递」计（每次内部 2 次尝试 = 1 次失败记账）。
	if w.FailureCount != 3 || w.LastStatus == nil || *w.LastStatus != 502 {
		t.Fatalf("after 3 failed deliveries = failures:%d status:%v", w.FailureCount, w.LastStatus)
	}
	if !w.Enabled {
		t.Fatal("3 failures must not disable yet (threshold 10)")
	}
}

// 自动禁用：连续 10 次失败投递后 enabled=false，且后续投递直接跳过
// （回调不再收到请求）。
func TestDeliverAutoDisableAfterConsecutiveFailures(t *testing.T) {
	store := NewMemoryStore()
	owner := uuid.New()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	hook := seedHook(t, store, owner, srv.URL, notify.EventUploadCompleted)

	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond})
	for i := 0; i < DefaultMaxConsecutiveFailures; i++ {
		if err := d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventUploadCompleted, "t", "b", nil)); err != nil {
			t.Fatal(err)
		}
	}
	w, err := store.Get(hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Enabled {
		t.Fatalf("after %d consecutive failures hook must be disabled: %+v", DefaultMaxConsecutiveFailures, w)
	}
	if w.FailureCount != DefaultMaxConsecutiveFailures {
		t.Fatalf("failure_count = %d, want %d", w.FailureCount, DefaultMaxConsecutiveFailures)
	}
	// 已禁用：再投递不产生请求。
	before := requests.Load()
	if err := d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventUploadCompleted, "t", "b", nil)); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Fatal("disabled hook must not receive further deliveries")
	}
}

// 禁用后手动重新启用并成功投递：failure_count 清零、恢复 enabled。
func TestDeliverReEnableResetsOnSuccess(t *testing.T) {
	store := NewMemoryStore()
	owner := uuid.New()
	var fail atomic.Bool
	fail.Store(true)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	hook := seedHook(t, store, owner, srv.URL, notify.EventUploadCompleted)

	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond})
	for i := 0; i < DefaultMaxConsecutiveFailures; i++ {
		_ = d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventUploadCompleted, "t", "b", nil))
	}
	svc := NewService(store)
	if _, err := svc.Update(owner, hook.ID, true); err != nil {
		t.Fatal(err)
	}
	fail.Store(false)
	if err := d.DeliverTask(context.Background(), deliveryPayload(hook.ID, owner, notify.EventUploadCompleted, "t", "b", nil)); err != nil {
		t.Fatal(err)
	}
	w, err := store.Get(hook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.FailureCount != 0 || w.LastStatus == nil || *w.LastStatus != 200 {
		t.Fatalf("after re-enable + success = %+v", w)
	}
}

// hook 已删除：投递静默丢弃（无错误、不 panic）。
func TestDeliverTaskDeletedHookSkipped(t *testing.T) {
	store := NewMemoryStore()
	d := NewDispatcher(store)
	d.SetRetryBackoff([]time.Duration{time.Millisecond})
	raw := deliveryPayload(uuid.New(), uuid.New(), notify.EventUploadCompleted, "t", "b", nil)
	if err := d.DeliverTask(context.Background(), raw); err != nil {
		t.Fatalf("deleted hook must be skipped silently: %v", err)
	}
	if err := d.DeliverTask(context.Background(), []byte("not-json")); err != nil {
		t.Fatalf("invalid payload must be skipped silently: %v", err)
	}
}
