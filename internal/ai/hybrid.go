package ai

import (
	"context"
	"strings"

	"github.com/docflow/docflow/internal/search"
	"github.com/google/uuid"
)

type HybridRetriever struct {
	Keyword        Searcher
	Embeddings     EmbeddingProvider
	Vectors        VectorStore
	CurrentVersion func(uuid.UUID) (uuid.UUID, error)
	// Resolve 热解析注入（非 nil 时优先于 Embeddings/Vectors 固定字段）：
	// 每次检索按当前热配置解析 embedding Provider 与目标向量库视图
	//（embedding 热切换免重启）。失败（含 ErrVectorDisabled——mode/开关
	// 关闭）降级纯关键词召回：向量侧本就是 best-effort 增强。
	Resolve func() (EmbeddingProvider, VectorStore, error)
}

func (r *HybridRetriever) Retrieve(ctx context.Context, user uuid.UUID, query string, topK int, spaceID *uuid.UUID) ([]search.Result, error) {
	keyword, err := r.Keyword.Query(user, search.QueryOptions{Q: strings.TrimSpace(query), Limit: topK, SpaceID: spaceID})
	if err != nil {
		return nil, err
	}
	embeddings, vectors := r.Embeddings, r.Vectors
	if r.Resolve != nil {
		if emb, store, rerr := r.Resolve(); rerr == nil {
			embeddings, vectors = emb, store
		} else {
			embeddings, vectors = nil, nil
		}
	}
	if embeddings == nil || vectors == nil || len(keyword) == 0 {
		return keyword, nil
	}
	v, err := embeddings.Embed(ctx, []string{query})
	if err != nil || len(v) == 0 || len(v[0]) == 0 {
		return keyword, nil
	}
	filter := map[string]any{}
	if spaceID != nil {
		filter["must"] = []any{map[string]any{"key": "space_id", "match": map[string]any{"value": spaceID.String()}}}
	}
	hits, err := vectors.Search(ctx, v[0], topK, filter)
	if err != nil {
		return keyword, nil
	}
	seen := make(map[uuid.UUID]bool)
	candidates := make(map[uuid.UUID]search.Result, len(keyword))
	for _, item := range keyword {
		if item.Type == "file" {
			candidates[item.ID] = item
		}
	}
	out := make([]search.Result, 0, topK)
	for _, hit := range hits {
		id, err := uuid.Parse(hit.PayloadString("file_id"))
		candidate, allowed := candidates[id]
		if err != nil || !allowed || seen[id] {
			continue
		}
		if r.CurrentVersion != nil {
			version, err := r.CurrentVersion(id)
			if err != nil || hit.PayloadString("version_id") != version.String() {
				continue
			}
		}
		seen[id] = true
		if text := hit.PayloadString("text"); text != "" {
			candidate.Snippet = text
		}
		out = append(out, candidate)
		if len(out) >= topK {
			break
		}
	}
	for _, item := range keyword {
		if item.Type == "file" && !seen[item.ID] {
			seen[item.ID] = true
			out = append(out, item)
			if len(out) >= topK {
				break
			}
		}
	}
	return out, nil
}
func (h VectorHit) PayloadString(key string) string {
	if v, ok := h.Payload[key].(string); ok {
		return v
	}
	return ""
}
