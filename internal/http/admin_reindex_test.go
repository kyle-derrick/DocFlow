package http

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeReindexLister 为 reindexLister 的内存实现：模拟 files 表游标分页
// （id 升序、after 之后的 limit 条；spaceID 过滤），并记录调用参数。
type fakeReindexLister struct {
	ids     []uuid.UUID // 已按升序准备
	bySpace map[uuid.UUID][]uuid.UUID
	err     error
	calls   []string // "after=<id>:limit=<n>:space=<id|->"
}

func (f *fakeReindexLister) ReindexFileIDs(after uuid.UUID, limit int, spaceID *uuid.UUID) ([]uuid.UUID, error) {
	if f.err != nil {
		return nil, f.err
	}
	pool := f.ids
	if spaceID != nil {
		pool = f.bySpace[*spaceID]
	}
	out := make([]uuid.UUID, 0, limit)
	for _, id := range pool {
		if id.String() > after.String() && len(out) < limit {
			out = append(out, id)
		}
	}
	tag := "-"
	if spaceID != nil {
		tag = spaceID.String()
	}
	f.calls = append(f.calls, "after="+after.String()+":limit="+strconv.Itoa(limit)+":space="+tag)
	return out, nil
}

// fakeEnqueuer 为 tasks.Enqueuer 的内存实现（仅关心 search-index 计数）。
type fakeEnqueuer struct {
	searchIDs []uuid.UUID
	err       error
}

func (f *fakeEnqueuer) EnqueueCompleteUpload(uuid.UUID) error { return nil }
func (f *fakeEnqueuer) EnqueueExtractWebpkg(uuid.UUID) error  { return nil }
func (f *fakeEnqueuer) EnqueueWebhookDelivery([]byte) error   { return nil }
func (f *fakeEnqueuer) EnqueueSearchIndex(id uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.searchIDs = append(f.searchIDs, id)
	return nil
}
func (f *fakeEnqueuer) Driver() string { return "fake" }

// TestAdminPostAIReindex 基本路径：入队计数与 {queued:n} 响应。
func TestAdminPostAIReindex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	lister := &fakeReindexLister{ids: ids}
	enq := &fakeEnqueuer{}
	h := &Handler{tasks: enq, reindex: lister}
	c, w := adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", "")
	h.adminPostAIReindex(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"queued":3`) {
		t.Fatalf("body = %s, want queued:3", w.Body.String())
	}
	if len(enq.searchIDs) != 3 {
		t.Fatalf("enqueued %d, want 3", len(enq.searchIDs))
	}
	for i, id := range ids {
		if enq.searchIDs[i] != id {
			t.Fatalf("order mismatch at %d", i)
		}
	}
}

// TestAdminPostAIReindexSpaceFilter space_id 过滤透传到列表源。
func TestAdminPostAIReindexSpaceFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	space := uuid.New()
	other := uuid.New()
	inSpace := []uuid.UUID{uuid.New(), uuid.New()}
	lister := &fakeReindexLister{bySpace: map[uuid.UUID][]uuid.UUID{space: inSpace, other: {uuid.New()}}}
	enq := &fakeEnqueuer{}
	h := &Handler{tasks: enq, reindex: lister}
	c, w := adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", `{"space_id":"`+space.String()+`"}`)
	h.adminPostAIReindex(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if len(enq.searchIDs) != 2 || enq.searchIDs[0] != inSpace[0] || enq.searchIDs[1] != inSpace[1] {
		t.Fatalf("enqueued = %v, want space files only", enq.searchIDs)
	}
	if len(lister.calls) == 0 || lister.calls[0][len(lister.calls[0])-36:] != space.String() {
		t.Fatalf("space filter must be passed to lister: %v", lister.calls)
	}
}

// TestAdminPostAIReindexBatching 分批（>500）：游标推进直至取尽。
func TestAdminPostAIReindexBatching(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const total = reindexBatchSize + 200 // 700：首批 500 + 次批 200
	ids := make([]uuid.UUID, total)
	for i := range ids {
		ids[i] = uuid.New()
	}
	// fakeReindexLister 按字符串序升序模拟：先排序再分页。
	sorted := append([]uuid.UUID(nil), ids...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].String() < sorted[j-1].String(); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	lister := &fakeReindexLister{ids: sorted}
	enq := &fakeEnqueuer{}
	h := &Handler{tasks: enq, reindex: lister}
	c, w := adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", "")
	h.adminPostAIReindex(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if len(enq.searchIDs) != total {
		t.Fatalf("enqueued %d, want %d", len(enq.searchIDs), total)
	}
	if len(lister.calls) != 2 {
		t.Fatalf("list calls = %d, want 2 (500+200)", len(lister.calls))
	}
	// 第二批游标为首批最后一条（按 id 升序）。
	if lister.calls[1] != "after="+sorted[reindexBatchSize-1].String()+":limit=500:space=-" {
		t.Fatalf("second batch cursor = %s", lister.calls[1])
	}
	// 无重复入队。
	seen := map[uuid.UUID]bool{}
	for _, id := range enq.searchIDs {
		if seen[id] {
			t.Fatal("duplicate enqueue")
		}
		seen[id] = true
	}
}

// TestAdminPostAIReindexErrors 依赖缺失 503、非法 body 400、列表/入队错误 500。
func TestAdminPostAIReindexErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 依赖缺失：入队器 / 列表源任一未装配。
	for _, h := range []*Handler{{reindex: &fakeReindexLister{}}, {tasks: &fakeEnqueuer{}}, {}} {
		c, w := adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", "")
		h.adminPostAIReindex(c)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
	}
	// 非法 body。
	h := &Handler{tasks: &fakeEnqueuer{}, reindex: &fakeReindexLister{}}
	c, w := adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", "not-json")
	h.adminPostAIReindex(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body status = %d", w.Code)
	}
	// 列表源故障。
	h = &Handler{tasks: &fakeEnqueuer{}, reindex: &fakeReindexLister{err: errors.New("db down")}}
	c, w = adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", "")
	h.adminPostAIReindex(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("lister error status = %d", w.Code)
	}
	// 入队故障（中途失败返回部分计数）。
	enq := &fakeEnqueuer{err: errors.New("queue down")}
	h = &Handler{tasks: enq, reindex: &fakeReindexLister{ids: []uuid.UUID{uuid.New()}}}
	c, w = adminContext(http.MethodPost, "/api/v1/admin/settings/ai/reindex", "")
	h.adminPostAIReindex(c)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"queued":0`) {
		t.Fatalf("enqueue error = %d %s, want 500 with queued:0", w.Code, w.Body.String())
	}
}
