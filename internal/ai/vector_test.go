package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/search"
	"github.com/google/uuid"
)

func TestChunkBoundaries(t *testing.T) {
	chunks := ChunkText("你好世界再见", 4, 2)
	if len(chunks) != 2 || chunks[0].Text != "你好世界" || chunks[1].Text != "世界再见" {
		t.Fatalf("chunks: %+v", chunks)
	}
	if len(ChunkText("  ", 4, 2)) != 0 {
		t.Fatal("empty text must not be indexed")
	}
}

func TestMockEmbedding(t *testing.T) {
	p := NewMockEmbeddingProvider(8)
	v, err := p.Embed(context.Background(), []string{"same", "same", "different"})
	if err != nil || len(v) != 3 || len(v[0]) != 8 || v[0][0] != v[1][0] || v[0][0] == v[2][0] {
		t.Fatalf("embed: %v %v", v, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Embed(ctx, []string{"a"}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestQdrantHTTP(t *testing.T) {
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls[r.Method+" "+r.URL.Path]++
		switch r.Method + " " + r.URL.Path {
		case "GET /collections/docs":
			w.WriteHeader(http.StatusNotFound)
		case "PUT /collections/docs":
			w.Write([]byte(`{"status":"ok"}`))
		case "PUT /collections/docs/points":
			var body struct {
				Points []struct {
					ID string `json:"id"`
				} `json:"points"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Points) != 1 || body.Points[0].ID == "" {
				t.Errorf("invalid points: %+v %v", body, err)
			}
			w.Write([]byte(`{"status":"ok"}`))
		case "POST /collections/docs/points/search":
			w.Write([]byte(`{"result":[{"id":"x","score":0.9,"payload":{"file_id":"a"}}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	q := NewQdrantClient(srv.URL, "docs", 0)
	ctx := context.Background()
	if err := q.EnsureCollection(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if err := q.Upsert(ctx, []VectorPoint{{ID: uuid.New().String(), Vector: make([]float32, 8), Payload: map[string]any{"file_id": "a"}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := q.Search(ctx, make([]float32, 8), 1, nil)
	if err != nil || len(hits) != 1 || hits[0].PayloadString("file_id") != "a" {
		t.Fatalf("hits: %+v %v", hits, err)
	}
	if calls["PUT /collections/docs"] != 1 {
		t.Fatalf("calls: %+v", calls)
	}
}

type keywordStub struct{ results []search.Result }

func (s keywordStub) Query(_ uuid.UUID, _ search.QueryOptions) ([]search.Result, error) {
	return s.results, nil
}

type vectorStub struct{ hits []VectorHit }

func (s vectorStub) EnsureCollection(context.Context, int) error { return nil }
func (s vectorStub) Upsert(context.Context, []VectorPoint) error { return nil }
func (s vectorStub) DeleteFile(context.Context, uuid.UUID) error { return nil }
func (s vectorStub) Search(context.Context, []float32, int, map[string]any) ([]VectorHit, error) {
	return s.hits, nil
}
func TestHybridPermissionAndDedup(t *testing.T) {
	allowed, denied := uuid.New(), uuid.New()
	r := HybridRetriever{Keyword: keywordStub{[]search.Result{{ID: allowed, Type: "file", Name: "live"}}}, Embeddings: NewMockEmbeddingProvider(8), Vectors: vectorStub{[]VectorHit{
		{Payload: map[string]any{"file_id": denied.String(), "text": "secret"}},
		{Payload: map[string]any{"file_id": allowed.String(), "text": "context", "name": "stale"}},
		{Payload: map[string]any{"file_id": allowed.String(), "text": "duplicate"}},
	}}}
	out, err := r.Retrieve(context.Background(), uuid.New(), "query", 5, nil)
	if err != nil || len(out) != 1 || out[0].ID != allowed || out[0].Name != "live" || !strings.Contains(out[0].Snippet, "context") {
		t.Fatalf("results: %+v %v", out, err)
	}
}
