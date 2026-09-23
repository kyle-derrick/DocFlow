// Package ai —— search.go：联网搜索增强（chat 的 web_search 参数）。
//
// 成熟方案接入（不自研爬虫）：SearXNG（自托管元搜索引擎，JSON API）与
// Tavily（搜索 API）两种实现，经 settings 的 ai.search.* 键热配置。
// 搜索失败静默降级（不阻断对话，HTTP 层经 SSE meta 的 search_failed
// 标注）；查询取对话中最后一条用户消息（截 400 字），结果以
// 「[n] 标题 + 链接 + 摘要」system 上下文块注入消息头部。
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/settings"
)

// WebSearchTimeout 单次搜索请求超时（超时/网络错误静默降级）。
const WebSearchTimeout = 8 * time.Second

// WebSearchQueryMaxRunes 搜索查询长度上限（runes，取最后一条用户消息）。
const WebSearchQueryMaxRunes = 400

// WebSearchSnippetMaxRunes 单条结果摘要截断上限（runes）。
const WebSearchSnippetMaxRunes = 500

// TavilyBaseURL Tavily 搜索 API 基地址（测试可注入 httptest 地址覆盖）。
const TavilyBaseURL = "https://api.tavily.com"

// WebSearchResult 为一条网络搜索结果（Content 为截断后的摘要）。
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content,omitempty"`
}

// WebSearcher 抽象联网搜索（生产实现 SearxngSearcher / TavilySearcher；
// 命名区别于 rag.go 的 Searcher——那是站内文档检索）。
type WebSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error)
}

// ---------- SearXNG ----------

// SearxngSearcher 为 SearXNG JSON API 实现（GET {base}/search?q=&format=json）。
type SearxngSearcher struct {
	BaseURL string
	Client  *http.Client
}

type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (s *SearxngSearcher) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if maxResults <= 0 {
		maxResults = settings.AISearchMaxResultsDefault
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("language", "zh")
	endpoint := strings.TrimSuffix(s.BaseURL, "/") + "/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: WebSearchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("searxng status %d: %s", resp.StatusCode, truncateBytes(raw, 200))
	}
	var out searxngResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]WebSearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		results = append(results, WebSearchResult{Title: r.Title, URL: r.URL, Content: clipRunes(r.Content, WebSearchSnippetMaxRunes)})
		if len(results) >= maxResults {
			break
		}
	}
	return results, nil
}

// ---------- Tavily ----------

// TavilySearcher 为 Tavily 搜索 API 实现（POST {base}/search）。
type TavilySearcher struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (s *TavilySearcher) Search(ctx context.Context, query string, maxResults int) ([]WebSearchResult, error) {
	if maxResults <= 0 {
		maxResults = settings.AISearchMaxResultsDefault
	}
	base := s.BaseURL
	if base == "" {
		base = TavilyBaseURL
	}
	body, err := json.Marshal(map[string]any{
		"api_key":     s.APIKey,
		"query":       query,
		"max_results": maxResults,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/search", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: WebSearchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("tavily status %d: %s", resp.StatusCode, truncateBytes(raw, 200))
	}
	var out tavilyResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]WebSearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		results = append(results, WebSearchResult{Title: r.Title, URL: r.URL, Content: clipRunes(r.Content, WebSearchSnippetMaxRunes)})
	}
	return results, nil
}

// NewWebSearcher 按配置构造联网搜索实现（provider 空/未知 = nil = 禁用）。
func NewWebSearcher(cfg settings.AISearchConfig) WebSearcher {
	switch cfg.Provider {
	case settings.AISearchProviderSearxng:
		return &SearxngSearcher{BaseURL: cfg.SearxngURL}
	case settings.AISearchProviderTavily:
		return &TavilySearcher{APIKey: cfg.TavilyAPIKey}
	default:
		return nil
	}
}

// ---------- chat 集成（查询提取 / 上下文注入 / 静默降级） ----------

// LastUserQuery 返回对话中最后一条 user 消息（截 400 字；无则空串）——
// web_search 的搜索查询提取。
func LastUserQuery(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			q := strings.TrimSpace(messages[i].Content)
			if q == "" {
				return ""
			}
			return clipRunes(q, WebSearchQueryMaxRunes)
		}
	}
	return ""
}

// clipRunes 无尾注截断（区别于 summarize 的 truncateRunes——搜索查询与
// 摘要片段不应带「内容过长」尾注）。
func clipRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// BuildWebSearchContext 把搜索结果拼为 system 上下文块（[n] 标题 + 链接 +
// 摘要），供注入消息头部。
func BuildWebSearchContext(results []WebSearchResult) string {
	var b strings.Builder
	b.WriteString("以下是网络搜索结果，供参考。回答时可引用编号 [n] 与对应链接；搜索结果可能过时或不准确，请结合问题自行判断。\n")
	for i, r := range results {
		fmt.Fprintf(&b, "\n[%d] %s\n%s\n", i+1, r.Title, r.URL)
		if snippet := strings.TrimSpace(r.Content); snippet != "" {
			b.WriteString("摘要：" + snippet + "\n")
		}
	}
	return b.String()
}

// ApplyWebSearch 执行联网搜索并注入上下文：查询取 messages 中最后一条
// user 消息（截 400 字；query 非空时优先，RAG 模式由 HTTP 层传检索 query）。
//   - cfg.Provider 为空（未配置/禁用）或无可用查询：静默跳过（原样返回）；
//   - 搜索失败（超时/网络/非 2xx）：failed=true，不注入、不报错（对话继续）；
//   - 成功：上下文块注入消息头部（并入既有 system 或新建首条 system）。
func (s *Service) ApplyWebSearch(ctx context.Context, cfg settings.AISearchConfig, query string, messages []Message) ([]Message, []WebSearchResult, bool) {
	searcher := NewWebSearcher(cfg)
	if searcher == nil || strings.TrimSpace(query) == "" {
		return messages, nil, false
	}
	sctx, cancel := context.WithTimeout(ctx, WebSearchTimeout)
	defer cancel()
	results, err := searcher.Search(sctx, query, cfg.MaxResults)
	if err != nil {
		return messages, nil, true // 搜索失败：静默降级（不阻断对话）
	}
	if len(results) == 0 {
		return messages, nil, false
	}
	return prependSystemMessage(messages, BuildWebSearchContext(results)), results, false
}

// prependSystemMessage 把上下文块并入首条 system 消息（无则新建于头部）。
func prependSystemMessage(messages []Message, block string) []Message {
	for i := range messages {
		if messages[i].Role == "system" {
			messages[i].Content += "\n\n" + block
			return messages
		}
	}
	out := make([]Message, 0, len(messages)+1)
	out = append(out, Message{Role: "system", Content: block})
	return append(out, messages...)
}
