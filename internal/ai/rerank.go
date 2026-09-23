// Package ai —— rerank.go：RAG 检索结果重排序（ai.rag.rerank_provider /
// rerank_model 配置的运行时消费层；管理端此前仅可配置、无人读取）。
//
// 协议采用 Cohere/Jina 事实标准的 rerank HTTP 形状——OpenAI 无官方 rerank
// 端点，主流商用（Cohere/Jina/SiliconFlow）与自托管（TEI、xinference、
// vLLM 兼容网关）rerank 服务均兼容此形状，openai_compatible Provider 直接
// 复用其 base_url/api_key：
//
//	POST {base}/rerank  {"model":..., "query":..., "documents":[{"text":...}], "top_n":N}
//	200 {"results":[{"index":0,"relevance_score":0.9}, ...]}
//
// 降级语义：重排是检索质量的增强而非关键路径——未配置、Provider 不可用
//（不存在/禁用/anthropic 无 rerank API/mock 不走网络）、请求失败、超时、
// 响应异常（index 越界/长度不符——防恶意响应）一律静默返回原序，仅记
// log，绝不阻断问答链路。
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/settings"
)

// RerankTimeout 单次重排请求超时（超时即降级原序）。
const RerankTimeout = 8 * time.Second

// RerankMaxDocs 单次重排送出的文档数上限：超出部分不参与重排、按原序
// 拼回结果尾部（避免超长 payload 打爆 rerank 服务）。
const RerankMaxDocs = 32

// rerankDocument 为请求 documents 数组元素（Cohere/Jina 形状只要求 text）。
type rerankDocument struct {
	Text string `json:"text"`
}

// rerankRequest 为 POST {base}/rerank 请求体。
type rerankRequest struct {
	Model     string           `json:"model"`
	Query     string           `json:"query"`
	Documents []rerankDocument `json:"documents"`
	TopN      int              `json:"top_n"`
}

// rerankResult 为响应 results 元素：index 对应请求 documents 下标。
type rerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

// rerankResponse 为 POST {base}/rerank 响应体。
type rerankResponse struct {
	Results []rerankResult `json:"results"`
}

// rerankHits 用配置的重排序模型对混合检索结果重排；未配置/Provider 不可用
// /失败/超时一律静默返回原序（重排是增强而非关键路径），失败记 log。
func (s *Service) rerankHits(ctx context.Context, query string, hits []search.Result) []search.Result {
	if len(hits) <= 1 {
		return hits
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return hits
	}
	cfg, err := s.Config()
	if err != nil {
		return hits
	}
	providerID := strings.TrimSpace(cfg.RAG.RerankProvider)
	model := strings.TrimSpace(cfg.RAG.RerankModel)
	if providerID == "" || model == "" {
		return hits
	}
	provider, ok := cfg.ProviderByID(providerID)
	// 不存在/禁用，或 Kind 不支持 /rerank 协议（anthropic 无 rerank API；
	// mock 不调网络直接原序）：直通。
	if !ok || !provider.Enabled || provider.Kind != settings.AIKindOpenAICompatible {
		return hits
	}
	// 单次文档数截断：>RerankMaxDocs 只送前 RerankMaxDocs 条（按现有序）。
	send := len(hits)
	if send > RerankMaxDocs {
		send = RerankMaxDocs
	}
	docs := make([]rerankDocument, send)
	for i := 0; i < send; i++ {
		// 文件名前缀增强语义（rerank 模型对标题高度敏感；snippet 为命中
		// 上下文，去掉 ts_headline 的 [[..]] 高亮标记）。
		snippet := strings.TrimSpace(stripSnippetMarks(hits[i].Snippet))
		docs[i] = rerankDocument{Text: hits[i].Name + "\n" + snippet}
	}
	payload, err := json.Marshal(rerankRequest{Model: model, Query: q, Documents: docs, TopN: send})
	if err != nil {
		log.Printf("[ai:rerank] marshal request: %v", err)
		return hits
	}
	rctx, cancel := context.WithTimeout(ctx, RerankTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimSuffix(provider.BaseURL, "/")+"/rerank", bytes.NewReader(payload))
	if err != nil {
		log.Printf("[ai:rerank] build request: %v", err)
		return hits
	}
	req.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("[ai:rerank] request %s: %v", providerID, err)
		return hits
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		log.Printf("[ai:rerank] status %d: %s", resp.StatusCode, truncateBytes(raw, 200))
		return hits
	}
	var out rerankResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		log.Printf("[ai:rerank] decode response: %v", err)
		return hits
	}
	// 响应校验（防恶意/异常响应）：results 条数须与送出文档数一致，index
	// 全部落在 [0, send) 且不重复——合起来保证是原集合的一个完整排列。
	if len(out.Results) != send {
		log.Printf("[ai:rerank] results len = %d, want %d（原序直通）", len(out.Results), send)
		return hits
	}
	seen := make([]bool, send)
	for _, r := range out.Results {
		if r.Index < 0 || r.Index >= send || seen[r.Index] {
			log.Printf("[ai:rerank] invalid result index %d（原序直通）", r.Index)
			return hits
		}
		seen[r.Index] = true
	}
	// 按 relevance_score 降序取 index 重排。
	sort.SliceStable(out.Results, func(i, j int) bool {
		return out.Results[i].RelevanceScore > out.Results[j].RelevanceScore
	})
	reordered := make([]search.Result, 0, len(hits))
	for _, r := range out.Results {
		reordered = append(reordered, hits[r.Index])
	}
	// 未参与重排的尾部条目按原序拼回（保持原序在后面）。
	return append(reordered, hits[send:]...)
}
