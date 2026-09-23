package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

type VectorPoint struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}
type VectorHit struct {
	ID      string
	Score   float32
	Payload map[string]any
}
type VectorStore interface {
	EnsureCollection(context.Context, int) error
	Upsert(context.Context, []VectorPoint) error
	Search(context.Context, []float32, int, map[string]any) ([]VectorHit, error)
	DeleteFile(context.Context, uuid.UUID) error
}

type QdrantClient struct {
	baseURL, collection string
	client              *http.Client
}

type VectorIndexer struct {
	Embeddings EmbeddingProvider
	Vectors    VectorStore
	ChunkSize  int
	Overlap    int
	// Resolve 热解析注入（非 nil 时优先于 Embeddings/Vectors 固定字段）：
	// 每次索引按当前热配置解析 embedding Provider 与目标向量库视图
	//（collection 已随视图固定，Ensure/Upsert 不会跨库）。返回
	// ErrVectorDisabled 时安静跳过（RAG 未启用，零向量库连接），其余
	// 错误原样上抛（task:search-index 记日志）。
	Resolve func() (EmbeddingProvider, VectorStore, error)
}

func (ix *VectorIndexer) IndexVector(ctx context.Context, fileID, versionID uuid.UUID, spaceID *uuid.UUID, name, content string) error {
	embeddings, vectors := ix.Embeddings, ix.Vectors
	if ix.Resolve != nil {
		emb, store, err := ix.Resolve()
		if err != nil {
			if errors.Is(err, ErrVectorDisabled) {
				return nil
			}
			return err
		}
		embeddings, vectors = emb, store
	}
	if embeddings == nil || vectors == nil {
		return nil
	}
	chunks := ChunkText(content, ix.ChunkSize, ix.Overlap)
	if len(chunks) == 0 {
		return nil
	}
	texts := make([]string, len(chunks))
	for i := range chunks {
		texts[i] = chunks[i].Text
	}
	v, err := embeddings.Embed(ctx, texts)
	if err != nil {
		return err
	}
	points := make([]VectorPoint, 0, len(chunks))
	for i, chunk := range chunks {
		if i >= len(v) || len(v[i]) == 0 {
			return fmt.Errorf("embedding missing for chunk %d", i)
		}
		payload := map[string]any{"file_id": fileID.String(), "version_id": versionID.String(), "name": name, "text": chunk.Text, "chunk_index": chunk.Index}
		if spaceID != nil {
			payload["space_id"] = spaceID.String()
		}
		points = append(points, VectorPoint{ID: uuid.NewSHA1(fileID, []byte(versionID.String()+":"+fmt.Sprint(chunk.Index))).String(), Vector: v[i], Payload: payload})
	}
	if err := vectors.EnsureCollection(ctx, len(v[0])); err != nil {
		return err
	}
	return vectors.Upsert(ctx, points)
}

func NewQdrantClient(baseURL, collection string, timeout time.Duration) *QdrantClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &QdrantClient{baseURL: strings.TrimSuffix(baseURL, "/"), collection: collection, client: &http.Client{Timeout: timeout}}
}

// Scope 返回以指定 collection 工作的 VectorStore 视图（共享底层 HTTP
// client 与超时；视图无状态、可并发使用）。embedding 热切换后按派生名
// （ResolveEmbedding）取新视图即换库，旧库数据保留；单例 QdrantClient
// 复用，不重建连接配置。
func (q *QdrantClient) Scope(collection string) VectorStore {
	return &scopedQdrantStore{client: q, collection: collection}
}

type scopedQdrantStore struct {
	client     *QdrantClient
	collection string
}

func (s *scopedQdrantStore) EnsureCollection(ctx context.Context, dimensions int) error {
	return s.client.ensureCollection(ctx, s.collection, dimensions)
}
func (s *scopedQdrantStore) Upsert(ctx context.Context, points []VectorPoint) error {
	return s.client.upsert(ctx, s.collection, points)
}
func (s *scopedQdrantStore) Search(ctx context.Context, vector []float32, limit int, filter map[string]any) ([]VectorHit, error) {
	return s.client.search(ctx, s.collection, vector, limit, filter)
}
func (s *scopedQdrantStore) DeleteFile(ctx context.Context, fileID uuid.UUID) error {
	return s.client.deleteFile(ctx, s.collection, fileID)
}
func (q *QdrantClient) request(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := q.client.Do(req)
	if err != nil {
		// 连接类错误给出可操作提示（no such host/refused 通常是 Qdrant 服务
		// 未随 profile 启动，用户在设置面板测试连接时最常见）。
		msg := err.Error()
		friendly := ""
		if strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "dial tcp") {
			friendly = "（Qdrant 服务似乎未启动：请在部署机执行 docker compose --profile ai-vector up -d 后重试）"
		}
		return fmt.Errorf("qdrant %s: %w%s", method, err, friendly)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &qdrantStatusError{method: method, path: path, status: resp.StatusCode, body: truncate(data, 300)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("qdrant decode: %w", err)
		}
	}
	return nil
}

type qdrantStatusError struct {
	method, path string
	status       int
	body         string
}

func (e *qdrantStatusError) Error() string {
	return fmt.Sprintf("qdrant %s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

// EnsureCollection 幂等确保构造期固定 collection 存在且维度一致（维度
// 不符报错——按 embedding 模型返回维度自动建库，见 ensureCollection）。
func (q *QdrantClient) EnsureCollection(ctx context.Context, dimensions int) error {
	return q.ensureCollection(ctx, q.collection, dimensions)
}

func (q *QdrantClient) ensureCollection(ctx context.Context, collection string, dimensions int) error {
	if dimensions < 1 {
		return fmt.Errorf("embedding dimensions must be positive")
	}
	var current struct {
		Result struct {
			Config struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	err := q.request(ctx, http.MethodGet, "/collections/"+collection, nil, &current)
	if err == nil {
		if current.Result.Config.Params.Vectors.Size != dimensions {
			return fmt.Errorf("qdrant collection dimension %d != embedding dimension %d", current.Result.Config.Params.Vectors.Size, dimensions)
		}
		return nil
	}
	statusErr, ok := err.(*qdrantStatusError)
	if !ok || statusErr.status != http.StatusNotFound {
		return err
	}
	return q.request(ctx, http.MethodPut, "/collections/"+collection, map[string]any{"vectors": map[string]any{"size": dimensions, "distance": "Cosine"}}, nil)
}

func (q *QdrantClient) DeleteFile(ctx context.Context, fileID uuid.UUID) error {
	return q.deleteFile(ctx, q.collection, fileID)
}

func (q *QdrantClient) deleteFile(ctx context.Context, collection string, fileID uuid.UUID) error {
	if fileID == uuid.Nil {
		return fmt.Errorf("file ID must not be empty")
	}
	return q.request(ctx, http.MethodPost, "/collections/"+collection+"/points/delete?wait=true", map[string]any{"filter": map[string]any{"must": []any{map[string]any{"key": "file_id", "match": map[string]any{"value": fileID.String()}}}}}, nil)
}

func (q *QdrantClient) Upsert(ctx context.Context, points []VectorPoint) error {
	return q.upsert(ctx, q.collection, points)
}

func (q *QdrantClient) upsert(ctx context.Context, collection string, points []VectorPoint) error {
	if len(points) == 0 {
		return nil
	}
	p := make([]map[string]any, len(points))
	for i, x := range points {
		p[i] = map[string]any{"id": x.ID, "vector": x.Vector, "payload": x.Payload}
	}
	return q.request(ctx, http.MethodPut, "/collections/"+collection+"/points?wait=true", map[string]any{"points": p}, nil)
}

func (q *QdrantClient) Search(ctx context.Context, vector []float32, limit int, filter map[string]any) ([]VectorHit, error) {
	return q.search(ctx, q.collection, vector, limit, filter)
}

func (q *QdrantClient) search(ctx context.Context, collection string, vector []float32, limit int, filter map[string]any) ([]VectorHit, error) {
	var out struct {
		Result []struct {
			ID      any            `json:"id"`
			Score   float32        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	err := q.request(ctx, http.MethodPost, "/collections/"+collection+"/points/search", map[string]any{"vector": vector, "limit": limit, "with_payload": true, "filter": filter}, &out)
	if err != nil {
		return nil, err
	}
	hits := make([]VectorHit, len(out.Result))
	for i, h := range out.Result {
		hits[i] = VectorHit{ID: fmt.Sprint(h.ID), Score: h.Score, Payload: h.Payload}
	}
	return hits, nil
}
