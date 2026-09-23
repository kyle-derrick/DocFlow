// Package ai —— rerank_test.go：RAG rerank 重排单测——协议形状（model/
// query/documents/top_n、Bearer 鉴权、文件名前缀与 snippet 去标记）、乱序
// results 重排与 sources 顺序跟随、Provider 不可用直通、失败/超时/越界降级、
// >32 条截断。
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/settings"
	"github.com/google/uuid"
)

// rerankCfgFor 构造「chat 走 mock + rerank 指向 baseURL 的 openai_compatible
// Provider（模型 rerank-x，APIKey rk）」的配置；mutate 可按用例调整。
func rerankCfgFor(baseURL string, mutate func(*settings.AIConfig)) ConfigProvider {
	return func() (settings.AIConfig, error) {
		cfg := settings.AIConfig{
			Providers: []settings.AIProvider{
				{ID: "m1", Name: "Mock", Kind: settings.AIKindMock, Model: "mock-echo", Enabled: true},
				{ID: "rr", Name: "Rerank", Kind: settings.AIKindOpenAICompatible, BaseURL: baseURL, APIKey: "rk", Enabled: true,
					Models: []settings.AIModel{{ID: "rerank-x", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindRerank}}}},
			},
			DefaultProvider: "m1",
			Temperature:     settings.AITemperatureDefault,
			MaxTokens:       settings.AIMaxTokensDefault,
			PerUserPerMin:   settings.AIPerUserPerMinDefault,
		}
		cfg.RAG.RerankProvider = "rr"
		cfg.RAG.RerankModel = "rerank-x"
		if mutate != nil {
			mutate(&cfg)
		}
		return cfg, nil
	}
}

// rerankHitsOf 构造 n 条文件命中（doc-00..doc-{n-1}）。
func rerankHitsOf(n int) []search.Result {
	hits := make([]search.Result, n)
	for i := range hits {
		hits[i] = search.Result{ID: uuid.New(), Type: "file", Name: fmt.Sprintf("doc-%02d", i), Snippet: fmt.Sprintf("内容 %02d", i)}
	}
	return hits
}

// assertSameOrder 断言 out 与 hits 元素逐一相同（原序直通）。
func assertSameOrder(t *testing.T, hits, out []search.Result) {
	t.Helper()
	if len(out) != len(hits) {
		t.Fatalf("len = %d, want %d", len(out), len(hits))
	}
	for i := range hits {
		if out[i].ID != hits[i].ID {
			t.Fatalf("out[%d] = %s, want %s（原序）", i, out[i].Name, hits[i].Name)
		}
	}
}

// newRerankCounter 构造 /rerank 计数假服务（任何请求均计数并返回空 results）。
func newRerankCounter() (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	return srv, &calls
}

// TestRerankHitsNotConfigured 未配置 rerank（provider/model 任一为空）或
// 单条命中：直通且不调网络。
func TestRerankHitsNotConfigured(t *testing.T) {
	srv, calls := newRerankCounter()
	defer srv.Close()
	hits := rerankHitsOf(3)
	clear := func(c *settings.AIConfig) { c.RAG.RerankProvider, c.RAG.RerankModel = "", "" }
	noModel := func(c *settings.AIConfig) { c.RAG.RerankModel = "" }
	for name, mutate := range map[string]func(*settings.AIConfig){"both empty": clear, "model empty": noModel} {
		out := NewService(rerankCfgFor(srv.URL, mutate)).rerankHits(context.Background(), "查询", hits)
		assertSameOrder(t, hits, out)
		if got := calls.Load(); got != 0 {
			t.Fatalf("%s: rerank 调用 %d 次, want 0", name, got)
		}
	}
	// 单条命中：直接返回。
	svc := NewService(rerankCfgFor(srv.URL, nil))
	out := svc.rerankHits(context.Background(), "查询", hits[:1])
	if len(out) != 1 || out[0].ID != hits[0].ID {
		t.Fatalf("单条应原样返回: %+v", out)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("单条不应调网: %d 次", got)
	}
}

// TestRerankHitsReorders 伪 /rerank 服务：校验请求体形状（POST /rerank、
// Bearer、model/query/top_n、documents 文本=文件名+snippet 去高亮标记）；
// 返回乱序 results → 按 relevance_score 降序重排。
func TestRerankHitsReorders(t *testing.T) {
	var body struct {
		Model     string `json:"model"`
		Query     string `json:"query"`
		TopN      int    `json:"top_n"`
		Documents []struct {
			Text string `json:"text"`
		} `json:"documents"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rerank" {
			t.Errorf("request = %s %s, want POST /rerank", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer rk" {
			t.Errorf("Authorization = %q, want Bearer rk", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// 乱序返回：score 降序应为 index 2 → 0 → 1。
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.3},{"index":2,"relevance_score":0.9},{"index":0,"relevance_score":0.5}]}`))
	}))
	defer srv.Close()
	hits := rerankHitsOf(3)
	hits[0].Snippet = "[[内容 00]]" // ts_headline 高亮标记应被去掉
	out := NewService(rerankCfgFor(srv.URL, nil)).rerankHits(context.Background(), "  部署文档  ", hits)
	if body.Model != "rerank-x" || body.Query != "部署文档" || body.TopN != 3 {
		t.Fatalf("body 头部 = model=%q query=%q top_n=%d", body.Model, body.Query, body.TopN)
	}
	if len(body.Documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(body.Documents))
	}
	for i, d := range body.Documents {
		want := fmt.Sprintf("doc-%02d\n内容 %02d", i, i)
		if d.Text != want {
			t.Fatalf("documents[%d] = %q, want %q（文件名前缀 + 去标记 snippet）", i, d.Text, want)
		}
	}
	// 重排结果：2 → 0 → 1。
	wantOrder := []string{"doc-02", "doc-00", "doc-01"}
	for i, name := range wantOrder {
		if out[i].Name != name {
			t.Fatalf("out[%d] = %s, want %s（out=%v）", i, out[i].Name, name, out)
		}
	}
}

// TestAskDocsRerankSourcesOrder 端到端：AskDocsOpt 检索后重排，onSources
// 与返回 sources 的顺序 = 重排后顺序（上下文块同序）。
func TestAskDocsRerankSourcesOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// score 降序：index 2 → 0 → 3 → 1。
		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":0.98},{"index":0,"relevance_score":0.9},{"index":3,"relevance_score":0.5},{"index":1,"relevance_score":0.1}]}`))
	}))
	defer srv.Close()
	hits := rerankHitsOf(4)
	svc := NewService(rerankCfgFor(srv.URL, nil))
	svc.SetSearcher(keywordStub{results: hits})
	var callback []Source
	_, sources, _, err := svc.AskDocsOpt(context.Background(), uuid.New(), "问题", AskOptions{}, nil, func(s []Source) { callback = s }, nil)
	if err != nil {
		t.Fatalf("AskDocsOpt: %v", err)
	}
	wantOrder := []string{"doc-02", "doc-00", "doc-03", "doc-01"}
	for i, name := range wantOrder {
		hit := hitByName(t, hits, name)
		if len(callback) != 4 || callback[i].Name != name || callback[i].FileID != hit.ID.String() {
			t.Fatalf("callback[%d] = %+v, want %s（重排顺序）", i, callback[i], name)
		}
		if len(sources) != 4 || sources[i].Name != name || sources[i].FileID != hit.ID.String() {
			t.Fatalf("sources[%d] = %+v, want %s（重排顺序）", i, sources[i], name)
		}
	}
}

func hitByName(t *testing.T, hits []search.Result, name string) search.Result {
	t.Helper()
	for _, h := range hits {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("hit %s not found", name)
	return search.Result{}
}

// TestRerankHitsSkipsUnusableProvider provider 不存在/禁用/anthropic/mock：
// 直通原序且不调网络。
func TestRerankHitsSkipsUnusableProvider(t *testing.T) {
	srv, calls := newRerankCounter()
	defer srv.Close()
	cases := map[string]func(*settings.AIConfig){
		"missing":   func(c *settings.AIConfig) { c.RAG.RerankProvider = "nope" },
		"disabled":  func(c *settings.AIConfig) { c.Providers[1].Enabled = false },
		"anthropic": func(c *settings.AIConfig) { c.Providers[1].Kind = settings.AIKindAnthropic },
		"mock":      func(c *settings.AIConfig) { c.Providers[1].Kind = settings.AIKindMock },
	}
	for name, mutate := range cases {
		hits := rerankHitsOf(3)
		out := NewService(rerankCfgFor(srv.URL, mutate)).rerankHits(context.Background(), "查询", hits)
		assertSameOrder(t, hits, out)
		if got := calls.Load(); got != 0 {
			t.Fatalf("%s: rerank 调用 %d 次, want 0", name, got)
		}
	}
}

// TestRerankHitsDegradation 上游 500 / 超时 / index 越界 / 长度不符 /
// 重复 index：一律原序返回不 panic。
func TestRerankHitsDegradation(t *testing.T) {
	newSvc := func(status int, body string, delay time.Duration) *Service {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if delay > 0 {
				time.Sleep(delay)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return NewService(rerankCfgFor(srv.URL, nil))
	}
	hits := rerankHitsOf(3)
	// 500 → 原序。
	assertSameOrder(t, hits, newSvc(http.StatusInternalServerError, `{"error":"boom"}`, 0).rerankHits(context.Background(), "q", hits))
	// 超时（父 ctx 短 deadline + 慢上游；rerank 自身 8s 超时同路径降级）。
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	assertSameOrder(t, hits, newSvc(http.StatusOK, `{}`, 500*time.Millisecond).rerankHits(ctx, "q", hits))
	// index 越界 → 原序。
	assertSameOrder(t, hits, newSvc(http.StatusOK, `{"results":[{"index":0,"relevance_score":1},{"index":7,"relevance_score":0.9},{"index":1,"relevance_score":0.5}]}`, 0).rerankHits(context.Background(), "q", hits))
	// 负 index → 原序。
	assertSameOrder(t, hits, newSvc(http.StatusOK, `{"results":[{"index":-1,"relevance_score":1},{"index":0,"relevance_score":0.9},{"index":1,"relevance_score":0.5}]}`, 0).rerankHits(context.Background(), "q", hits))
	// 长度不符（缺 1 条）→ 原序。
	assertSameOrder(t, hits, newSvc(http.StatusOK, `{"results":[{"index":0,"relevance_score":1},{"index":1,"relevance_score":0.9}]}`, 0).rerankHits(context.Background(), "q", hits))
	// 重复 index → 原序。
	assertSameOrder(t, hits, newSvc(http.StatusOK, `{"results":[{"index":0,"relevance_score":1},{"index":0,"relevance_score":0.9},{"index":1,"relevance_score":0.5}]}`, 0).rerankHits(context.Background(), "q", hits))
	// 非法 JSON → 原序。
	assertSameOrder(t, hits, newSvc(http.StatusOK, `not-json`, 0).rerankHits(context.Background(), "q", hits))
}

// TestRerankHitsTruncatesAt32 >32 条截断：只送前 32（校验 documents 与
// top_n），重排后尾部未参与条目按原序拼回。
func TestRerankHitsTruncatesAt32(t *testing.T) {
	var docs, topN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TopN      int `json:"top_n"`
			Documents []struct {
				Text string `json:"text"`
			} `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		docs, topN = len(body.Documents), body.TopN
		w.Header().Set("Content-Type", "application/json")
		// 前 32 条完全倒序（score：index 31 > 30 > ... > 0）。
		var b strings.Builder
		b.WriteString(`{"results":[`)
		for i := 0; i < RerankMaxDocs; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"index":%d,"relevance_score":%d}`, RerankMaxDocs-1-i, RerankMaxDocs-i)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()
	hits := rerankHitsOf(40)
	out := NewService(rerankCfgFor(srv.URL, nil)).rerankHits(context.Background(), "查询", hits)
	if docs != RerankMaxDocs || topN != RerankMaxDocs {
		t.Fatalf("documents=%d top_n=%d, want %d/%d（截断）", docs, topN, RerankMaxDocs, RerankMaxDocs)
	}
	if len(out) != len(hits) {
		t.Fatalf("len = %d, want %d（不丢条目）", len(out), len(hits))
	}
	// 前 32 = 倒序；尾部 8 条按原序拼回。
	for i := 0; i < RerankMaxDocs; i++ {
		want := hits[RerankMaxDocs-1-i]
		if out[i].ID != want.ID {
			t.Fatalf("out[%d] = %s, want %s（倒序重排）", i, out[i].Name, want.Name)
		}
	}
	for i := RerankMaxDocs; i < len(hits); i++ {
		if out[i].ID != hits[i].ID {
			t.Fatalf("out[%d] = %s, want %s（尾部原序拼回）", i, out[i].Name, hits[i].Name)
		}
	}
}
