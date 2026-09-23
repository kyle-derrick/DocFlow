// Package ai —— rag.go：检索增强（RAG-lite，无向量库版本）。
//
// 检索层复用 search.Repo 抽象（生产 pg ILIKE+tsvector 或 Meilisearch，
// 部署按 SEARCH_DRIVER 装配——本文件只依赖 Query 接口，后续换 Qdrant
// 向量检索时替换该注入即可，调用方零改动）。
//
// 流程：query → 按当前用户权限过滤的全文检索 top-k（默认 5）→ 拼装
// 「文件名 + 片段 + file_id + /view/{id} 链接」上下文块 → 系统提示要求
// 答案后列「来源：文件名(链接)」→ 常规对话补全（流式）。
package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/search"
)

// RAGTopKDefault 检索增强默认召回条数。
const RAGTopKDefault = 8

// RAGTopKMax 检索增强召回上限（与 search.QueryLimitMax 一致由实现截断）。
const RAGTopKMax = 10

// Source 为回答引用的来源文件（前端渲染可点击链接）。
type Source struct {
	FileID string `json:"file_id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
}

// Searcher 抽象检索能力（生产实现 *search.Store；后续向量库替换边界）。
type Searcher interface {
	Query(user uuid.UUID, opts search.QueryOptions) ([]search.Result, error)
}

type HybridSearcher interface {
	Retrieve(ctx context.Context, user uuid.UUID, query string, topK int, spaceID *uuid.UUID) ([]search.Result, error)
}

// ragSystemPrompt 为检索增强的系统提示（引用格式约定）。
const ragSystemPrompt = `你是 DocFlow 知识库助手。请优先依据下方「参考资料」回答用户问题；资料不足时如实说明，不要编造。
回答务必在末尾列出引用的来源文件，格式为：
来源：
- 文件名 (/view/{file_id})
只列出实际参考的文件。`

// Retrieve 执行检索增强召回（用户权限过滤由 search 层保证）。
func (s *Service) Retrieve(ctx context.Context, user uuid.UUID, query string) ([]search.Result, error) {
	return s.RetrieveOpt(ctx, user, query, 0, nil)
}

// RetrieveOpt 带选项召回：topK<=0 用默认；spaceID 非 nil 限定空间
// （ask_docs 的 space_id 参数；权限过滤仍由 search 层保证）。
func (s *Service) RetrieveOpt(ctx context.Context, user uuid.UUID, query string, topK int, spaceID *uuid.UUID) ([]search.Result, error) {
	if s.searcher == nil {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if topK <= 0 || topK > RAGTopKMax {
		topK = RAGTopKDefault
	}
	if s.hybrid != nil && s.ragConfig != nil {
		cfg := s.ragConfig()
		if cfg.Mode == "hybrid" && cfg.VectorEnabled {
			return s.hybrid.Retrieve(ctx, user, strings.TrimSpace(query), topK, spaceID)
		}
	}
	return s.searcher.Query(user, search.QueryOptions{Q: strings.TrimSpace(query), SpaceID: spaceID, Limit: topK})
}

// SetSearcher 注入检索源（幂等；nil 保持无 RAG）。
func (s *Service) SetSearcher(searcher Searcher) {
	if searcher != nil {
		s.searcher = searcher
	}
}

// AskDocs 检索增强问答：query → RAG → 对话补全，返回答案与来源列表。
// history 为对话历史（不含本次提问；nil = 单轮）；onSources 在检索完成后、
// 补全开始前回调来源列表（SSE 场景先行下发 sources 事件）；onDelta 流式
// 增量（nil = 非流式）。
func (s *Service) AskDocs(ctx context.Context, user uuid.UUID, query string, history []Message, onSources func([]Source), onDelta func(string)) (string, []Source, ChatResult, error) {
	return s.AskDocsOpt(ctx, user, query, AskOptions{}, history, onSources, onDelta)
}

// AskOptions 检索增强选项（零值 = 默认全空间、RAGTopKDefault 条召回）。
type AskOptions struct {
	// TopK 召回条数上限（<=0 或超 RAGTopKMax 时取默认/截断）。
	TopK int
	// SpaceID 限定检索空间（nil = 全部可见空间）。
	SpaceID *uuid.UUID
	// ProviderID/ModelID 对话目标（空 = 场景默认 chat 模型）。模型须归属
	// 启用中的 Provider 且具备 chat 能力（Chat 内校验）。
	ProviderID string
	ModelID    string
}

// AskDocsOpt 为 AskDocs 的带选项版本（top_k / space_id；MCP ask_docs 用）。
func (s *Service) AskDocsOpt(ctx context.Context, user uuid.UUID, query string, opt AskOptions, history []Message, onSources func([]Source), onDelta func(string)) (string, []Source, ChatResult, error) {
	results, err := s.RetrieveOpt(ctx, user, query, opt.TopK, opt.SpaceID)
	if err != nil {
		return "", nil, ChatResult{}, fmt.Errorf("%w: search: %v", ErrUpstreamChat, err)
	}
	// 配置了 rerank 模型时对检索结果重排（未配置/失败静默原序）；
	// sources 顺序与上下文排序均跟随重排结果。
	results = s.rerankHits(ctx, query, results)
	var sources []Source
	for _, r := range results {
		if r.Type != "file" {
			continue
		}
		sources = append(sources, Source{FileID: r.ID.String(), Name: r.Name, URL: "/view/" + r.ID.String()})
	}
	if onSources != nil {
		onSources(sources)
	}
	sys := ragSystemPrompt
	if len(sources) > 0 {
		var b strings.Builder
		b.WriteString(sys)
		b.WriteString("\n\n参考资料（按相关度排序）：\n")
		for i, r := range results {
			if r.Type != "file" {
				continue
			}
			b.WriteString(fmt.Sprintf("\n[%d] 文件名：%s\n链接：/view/%s\n", i+1, r.Name, r.ID.String()))
			if snippet := strings.TrimSpace(stripSnippetMarks(r.Snippet)); snippet != "" {
				b.WriteString("片段：" + snippet + "\n")
			}
		}
		sys = b.String()
	} else {
		sys += "\n\n（本次检索未命中任何资料，请按通用知识回答并说明未检索到相关文档。）"
	}
	msgs := make([]Message, 0, len(history)+1)
	for _, m := range history {
		if strings.TrimSpace(m.Content) != "" && (m.Role == "user" || m.Role == "assistant") {
			msgs = append(msgs, m)
		}
	}
	msgs = append(msgs, Message{Role: "user", Content: query})
	res, err := s.Chat(ctx, ChatRequest{
		ProviderID: opt.ProviderID,
		Model:      opt.ModelID,
		Messages:   msgs,
		System:     sys,
		Stream:     onDelta != nil,
	}, onDelta)
	if err != nil {
		return "", sources, res, err
	}
	return res.Content, sources, res, nil
}

// DocsContextBlock 把检索结果拼装为「我的文件」上下文块与来源列表
// （include_docs 用：普通对话模式下把检索到的本人可见文档片段注入
// system 上下文，语料拼装与 AskDocsOpt 的参考资料块一致）。无 file
// 结果时返回空串（调用方跳过注入，对话不中断）。
func DocsContextBlock(results []search.Result) (string, []Source) {
	var b strings.Builder
	var sources []Source
	idx := 0
	for _, r := range results {
		if r.Type != "file" {
			continue
		}
		idx++
		if idx == 1 {
			b.WriteString("以下是按相关度排序的用户的文档检索结果（回答时可参考，并优先依据这些资料）：")
		}
		sources = append(sources, Source{FileID: r.ID.String(), Name: r.Name, URL: "/view/" + r.ID.String()})
		b.WriteString(fmt.Sprintf("\n\n[%d] 文件名：%s\n链接：/view/%s\n", idx, r.Name, r.ID.String()))
		if snippet := strings.TrimSpace(stripSnippetMarks(r.Snippet)); snippet != "" {
			b.WriteString("片段：" + snippet + "\n")
		}
	}
	return b.String(), sources
}

// stripSnippetMarks 去掉 ts_headline 的 [[..]] 高亮标记。
func stripSnippetMarks(s string) string {
	s = strings.ReplaceAll(s, "[[", "")
	return strings.ReplaceAll(s, "]]", "")
}
