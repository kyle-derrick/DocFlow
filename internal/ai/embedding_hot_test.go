package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/settings"
	"github.com/google/uuid"
)

func hotConfig(provider, model string) settings.AIConfig {
	return settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "p1", Kind: settings.AIKindOpenAICompatible, BaseURL: "http://embedding.example", APIKey: "k1", Enabled: true,
			Models: []settings.AIModel{
				{ID: "m1", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
				{ID: "m2", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
				{ID: "mc", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
			},
		}},
		RAG: settings.AIRAGConfig{CollectionPrefix: "docflow_", EmbeddingProvider: provider, EmbeddingModel: model},
	}
}

func hotService(cfg settings.AIConfig) *Service {
	return NewService(func() (settings.AIConfig, error) { return cfg, nil })
}

// TestResolveEmbeddingCollection 派生名稳定性：同 provider+model 恒定同名、
// 换模型/换 provider 换名、mock 路径固定 g_mock8。
func TestResolveEmbeddingCollection(t *testing.T) {
	svc := hotService(hotConfig("p1", "m1"))
	p1, c1, err := svc.ResolveEmbedding()
	if err != nil {
		t.Fatal(err)
	}
	if _, c2, err := svc.ResolveEmbedding(); err != nil || c1 != c2 {
		t.Fatalf("same config must derive same collection: %q vs %q (%v)", c1, c2, err)
	}
	// 同配置复用缓存实例（provider 客户端无状态，按配置指纹缓存）。
	if p2, _, _ := svc.ResolveEmbedding(); p1 != p2 {
		t.Fatal("cached provider instance expected for identical config")
	}
	hash := hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("p1::m1")); return s[:] }())
	if c1 != "docflow_g_"+hash[:10] {
		t.Fatalf("collection %q != prefix+g_+sha256-10hex (%s)", c1, hash[:10])
	}
	// 换模型即换库。
	if _, c3, _ := hotService(hotConfig("p1", "m2")).ResolveEmbedding(); c3 == c1 {
		t.Fatalf("switching model must switch collection (%q)", c3)
	}
	// 换 provider 即换库。
	cfg := hotConfig("p2", "m1")
	cfg.Providers = append(hotConfig("p1", "m1").Providers, settings.AIProvider{ID: "p2", Kind: settings.AIKindOpenAICompatible, BaseURL: "http://x", Enabled: true, Models: []settings.AIModel{{ID: "m1", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}}}})
	if _, c4, _ := hotService(cfg).ResolveEmbedding(); c4 == c1 {
		t.Fatalf("switching provider must switch collection (%q)", c4)
	}
	// mock 路径：内置 8 维 + 固定派生名。
	mock, cm, err := hotService(hotConfig("mock", "")).ResolveEmbedding()
	if err != nil || cm != "docflow_g_mock8" || mock.Dimensions() != 8 {
		t.Fatalf("mock path: %q dims=%d %v", cm, mock.Dimensions(), err)
	}
}

// TestResolveEmbeddingUnconfigured 未配置 / 目标无效时报错。
func TestResolveEmbeddingUnconfigured(t *testing.T) {
	if _, _, err := hotService(hotConfig("missing", "m1")).ResolveEmbedding(); err == nil {
		t.Fatal("unknown provider must error")
	}
	if _, _, err := hotService(hotConfig("p1", "mc")).ResolveEmbedding(); err == nil {
		t.Fatal("model without embedding capability must error")
	}
	// 非 OpenAI 兼容 Provider（anthropic）不支持 /embeddings 直连。
	kind := hotConfig("p1", "m1")
	kind.Providers[0].Kind = settings.AIKindAnthropic
	if _, _, err := hotService(kind).ResolveEmbedding(); err == nil {
		t.Fatal("non openai_compatible provider must error")
	}
	// 配置读取失败。
	svc := NewService(func() (settings.AIConfig, error) { return settings.AIConfig{}, errors.New("db down") })
	if _, _, err := svc.ResolveEmbedding(); err == nil {
		t.Fatal("config read failure must propagate")
	}
}

// recordingStore 记录调用（带自身标签），验证热切换后写入新库。
type recordingStore struct {
	label  string
	events []string
}

func (s *recordingStore) EnsureCollection(_ context.Context, dims int) error {
	s.events = append(s.events, s.label+":ensure:"+strconv.Itoa(dims))
	return nil
}
func (s *recordingStore) Upsert(_ context.Context, points []VectorPoint) error {
	s.events = append(s.events, s.label+":upsert:"+strconv.Itoa(len(points)))
	return nil
}
func (s *recordingStore) Search(context.Context, []float32, int, map[string]any) ([]VectorHit, error) {
	return nil, nil
}
func (s *recordingStore) DeleteFile(context.Context, uuid.UUID) error { return nil }

// TestVectorIndexerHotSwitch 热切换后索引写入新 collection（Resolve 返回
// 的视图即新库；旧库视图不再被写）。
func TestVectorIndexerHotSwitch(t *testing.T) {
	old, fresh := &recordingStore{label: "old"}, &recordingStore{label: "fresh"}
	current := old
	ix := &VectorIndexer{
		ChunkSize: 8, Overlap: 0,
		Resolve: func() (EmbeddingProvider, VectorStore, error) {
			return NewMockEmbeddingProvider(8), current, nil
		},
	}
	ctx := context.Background()
	if err := ix.IndexVector(ctx, uuid.New(), uuid.New(), nil, "a.txt", "hello world foo"); err != nil {
		t.Fatal(err)
	}
	current = fresh // 模拟管理端切换 embedding 模型 → 新派生 collection。
	if err := ix.IndexVector(ctx, uuid.New(), uuid.New(), nil, "b.txt", "hello world bar"); err != nil {
		t.Fatal(err)
	}
	if len(old.events) != 2 || len(fresh.events) != 2 { // ensure + upsert 各一次
		t.Fatalf("old=%v fresh=%v", old.events, fresh.events)
	}
	// 未启用向量（热配置关闭）：安静跳过，零向量库调用。
	touched := &recordingStore{label: "disabled"}
	ix.Resolve = func() (EmbeddingProvider, VectorStore, error) { return nil, nil, ErrVectorDisabled }
	if err := ix.IndexVector(ctx, uuid.New(), uuid.New(), nil, "c.txt", "hello"); err != nil {
		t.Fatalf("disabled must skip silently: %v", err)
	}
	if len(touched.events) != 0 {
		t.Fatalf("disabled must not touch vector store: %v", touched.events)
	}
	// 配置错误（未配置 Provider）：原样上抛。
	cfgErr := errors.New("rag embedding provider \"x\" not configured")
	ix.Resolve = func() (EmbeddingProvider, VectorStore, error) { return nil, nil, cfgErr }
	if err := ix.IndexVector(ctx, uuid.New(), uuid.New(), nil, "d.txt", "hello"); !errors.Is(err, cfgErr) {
		t.Fatalf("config error must propagate, got %v", err)
	}
}

// TestQdrantScopePaths Scope 视图按指定 collection 工作（与构造期固定名互不干扰）。
func TestQdrantScopePaths(t *testing.T) {
	paths := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths[r.Method+" "+r.URL.Path]++
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/collections/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Write([]byte(`{"status":"ok","result":[]}`))
		}
	}))
	defer srv.Close()
	q := NewQdrantClient(srv.URL, "fixed", 0)
	ctx := context.Background()
	scoped := q.Scope("g_hot1")
	if err := scoped.EnsureCollection(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if err := scoped.Upsert(ctx, []VectorPoint{{ID: uuid.New().String(), Vector: make([]float32, 8)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Search(ctx, make([]float32, 8), 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := scoped.DeleteFile(ctx, uuid.New()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GET /collections/g_hot1", "PUT /collections/g_hot1", "PUT /collections/g_hot1/points", "POST /collections/g_hot1/points/search", "POST /collections/g_hot1/points/delete"} {
		if paths[want] == 0 {
			t.Fatalf("missing request %s (got %v)", want, paths)
		}
	}
	if len(paths) != 5 {
		t.Fatalf("unexpected extra requests: %v", paths)
	}
}

// TestHybridRetrieverHotResolve 检索侧热解析：Resolve 失败（含未启用）降级
// 纯关键词、成功走向量合并（片段来自新库命中）。
func TestHybridRetrieverHotResolve(t *testing.T) {
	allowed := uuid.New()
	kw := keywordStub{[]search.Result{{ID: allowed, Type: "file", Name: "doc"}}}
	r := &HybridRetriever{
		Keyword: kw,
		Resolve: func() (EmbeddingProvider, VectorStore, error) { return nil, nil, ErrVectorDisabled },
	}
	out, err := r.Retrieve(context.Background(), uuid.New(), "q", 5, nil)
	if err != nil || len(out) != 1 || out[0].ID != allowed || out[0].Snippet != "" {
		t.Fatalf("keyword fallback: %+v %v", out, err)
	}
	hr := &HybridRetriever{
		Keyword: kw,
		Resolve: func() (EmbeddingProvider, VectorStore, error) {
			return NewMockEmbeddingProvider(8), vectorStub{[]VectorHit{{Payload: map[string]any{"file_id": allowed.String(), "text": "hot"}}}}, nil
		},
	}
	out, err = hr.Retrieve(context.Background(), uuid.New(), "q", 5, nil)
	if err != nil || len(out) != 1 || !strings.Contains(out[0].Snippet, "hot") {
		t.Fatalf("hot resolve merge: %+v %v", out, err)
	}
}
