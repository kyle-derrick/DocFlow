package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// batchTestRouter 构建挂载幂等中间件的测试路由（前置中间件设置 user
// 上下文模拟认证；idempotency 作为路由中间件，与生产链路一致）。
func batchTestRouter(h *Handler) (*gin.Engine, *int) {
	gin.SetMode(gin.TestMode)
	calls := 0
	r := gin.New()
	setUser := func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, uuid.MustParse("11111111-1111-1111-1111-111111111111"))
		c.Next()
	}
	final := func(c *gin.Context) {
		calls++
		c.JSON(http.StatusOK, gin.H{"calls": calls})
	}
	r.POST("/batch/x", setUser, h.idempotency, final)
	return r, &calls
}

func postBatch(r *gin.Engine, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/batch/x", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestBatchIdempotencyReplay 同 key + 同 body：60s 内重放缓存响应（handler
// 只执行一次，带 Idempotency-Replayed 头），过期后重新执行。
func TestBatchIdempotencyReplay(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.idem = newIdemCache(80 * time.Millisecond)
	r, calls := batchTestRouter(h)
	body := `{"file_ids":["11111111-1111-1111-1111-111111111112"]}`

	w1 := postBatch(r, "key-1", body)
	if w1.Code != http.StatusOK || !strings.Contains(w1.Body.String(), `"calls":1`) {
		t.Fatalf("first call: %d %s", w1.Code, w1.Body.String())
	}
	if w1.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("first call must not be marked replayed")
	}

	// 同 key 同 body → 重放缓存，不执行 handler。
	w2 := postBatch(r, "key-1", body)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"calls":1`) {
		t.Fatalf("replay: %d %s", w2.Code, w2.Body.String())
	}
	if w2.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("replayed response must carry Idempotency-Replayed: true")
	}
	if *calls != 1 {
		t.Fatalf("handler calls = %d, want 1", *calls)
	}

	// TTL 过期后重新执行。
	time.Sleep(120 * time.Millisecond)
	w3 := postBatch(r, "key-1", body)
	if w3.Code != http.StatusOK || !strings.Contains(w3.Body.String(), `"calls":2`) {
		t.Fatalf("after ttl: %d %s", w3.Code, w3.Body.String())
	}
	if *calls != 2 {
		t.Fatalf("handler calls after ttl = %d, want 2", *calls)
	}
}

// TestBatchIdempotencyKeyBodyMismatch 同 key + 不同 body → 409。
func TestBatchIdempotencyKeyBodyMismatch(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	r, calls := batchTestRouter(h)

	w1 := postBatch(r, "key-2", `{"file_ids":["11111111-1111-1111-1111-111111111112"]}`)
	if w1.Code != http.StatusOK {
		t.Fatalf("first: %d", w1.Code)
	}
	w2 := postBatch(r, "key-2", `{"file_ids":["11111111-1111-1111-1111-111111111113"]}`)
	if w2.Code != http.StatusConflict {
		t.Fatalf("mismatched body: %d, want 409", w2.Code)
	}
	if *calls != 1 {
		t.Fatalf("handler calls = %d, want 1", *calls)
	}
}

// TestBatchIdempotencyNoKey 无 key 直接透传，每次都执行；
// 不同 key 互不影响。
func TestBatchIdempotencyNoKey(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	r, calls := batchTestRouter(h)
	body := `{"file_ids":[]}`

	for i := 0; i < 2; i++ {
		if w := postBatch(r, "", body); w.Code != http.StatusOK {
			t.Fatalf("no-key call %d: %d", i, w.Code)
		}
	}
	if w := postBatch(r, "a", body); w.Code != http.StatusOK {
		t.Fatalf("key a: %d", w.Code)
	}
	if w := postBatch(r, "b", body); w.Code != http.StatusOK {
		t.Fatalf("key b: %d", w.Code)
	}
	if *calls != 4 {
		t.Fatalf("handler calls = %d, want 4", *calls)
	}
}

// TestBatchIdempotencyUserIsolation 幂等缓存键按用户隔离：同 key 同 body
// 不同用户不得重放他人响应（需要两个用户上下文，此处直接验证缓存层）。
func TestBatchIdempotencyUserIsolation(t *testing.T) {
	cache := newIdemCache(time.Minute)
	cache.put("userA\x00k", "hashA", 200, []byte(`{"a":1}`))
	// 同 key 不同用户前缀 → 未命中。
	if _, hit, _ := cache.get("userB\x00k", "hashA"); hit {
		t.Fatal("cache must be scoped per user")
	}
	// 同 key 同用户同 hash → 命中。
	if _, hit, match := cache.get("userA\x00k", "hashA"); !hit || !match {
		t.Fatalf("hit=%v match=%v, want true/true", hit, match)
	}
	// 同 key 同用户不同 hash → 命中但不匹配（HTTP 层 409）。
	if _, hit, match := cache.get("userA\x00k", "hashB"); !hit || match {
		t.Fatalf("hit=%v match=%v, want true/false", hit, match)
	}
}

// TestParseBatchIDs 覆盖批量 ID 解析：空/超限/非法拒绝，重复去重保序。
func TestParseBatchIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	id := "11111111-1111-1111-1111-111111111112"
	newCtx := func() *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/batch/x", nil)
		return c
	}

	if _, ok := parseBatchIDs(newCtx(), nil); ok {
		t.Fatal("empty ids must be rejected")
	}
	if _, ok := parseBatchIDs(newCtx(), []string{"not-a-uuid"}); ok {
		t.Fatal("invalid uuid must be rejected")
	}
	tooMany := make([]string, batchMaxItems+1)
	for i := range tooMany {
		tooMany[i] = id
	}
	if _, ok := parseBatchIDs(newCtx(), tooMany); ok {
		t.Fatal("over-limit ids must be rejected")
	}
	ids, ok := parseBatchIDs(newCtx(), []string{id, id, "  " + id + "  "})
	if !ok || len(ids) != 1 {
		t.Fatalf("dedupe: ids = %v (len %d), ok = %v; want 1 unique id", ids, len(ids), ok)
	}
}
