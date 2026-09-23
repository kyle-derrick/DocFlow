// Package ai —— search_test.go：联网搜索（searxng/tavily 解析、
// ApplyWebSearch 跳过/注入/降级）与推理思考透传（think payload 两分支、
// reasoning 能力解析与校验）单测。
package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/docflow/docflow/internal/settings"
)

// ---------- searxng / tavily 解析（httptest mock） ----------

// TestSearxngSearcherParse searxng JSON API：GET {base}/search 带
// q/format=json/language=zh；结果按序解析、content 截 500 字、maxResults 截断。
func TestSearxngSearcherParse(t *testing.T) {
	long := strings.Repeat("摘", 600) // 600 runes > 500 截断
	var gotQuery, gotFormat, gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %q, want /search", r.URL.Path)
		}
		gotQuery, gotFormat, gotLang = r.URL.Query().Get("q"), r.URL.Query().Get("format"), r.URL.Query().Get("language")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[` +
			`{"title":"DocFlow 文档","url":"https://example.com/a","content":"` + long + `"},` +
			`{"title":"第二条","url":"https://example.com/b","content":"摘要 B"},` +
			`{"title":"","url":"","content":"无 URL 条目应丢弃"},` +
			`{"title":"第三条","url":"https://example.com/c","content":"摘要 C"}]}`))
	}))
	defer srv.Close()
	s := &SearxngSearcher{BaseURL: srv.URL + "/"} // 尾斜杠容错
	results, err := s.Search(context.Background(), "docflow 部署", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotQuery != "docflow 部署" || gotFormat != "json" || gotLang != "zh" {
		t.Fatalf("query params = %q %q %q", gotQuery, gotFormat, gotLang)
	}
	if len(results) != 2 { // maxResults=2 截断
		t.Fatalf("results = %d, want 2（maxResults 截断）", len(results))
	}
	if results[0].Title != "DocFlow 文档" || results[0].URL != "https://example.com/a" {
		t.Fatalf("results[0] = %+v", results[0])
	}
	if got := utf8.RuneCountInString(results[0].Content); got != WebSearchSnippetMaxRunes {
		t.Fatalf("content runes = %d, want %d（截断）", got, WebSearchSnippetMaxRunes)
	}
	if results[1].URL != "https://example.com/b" {
		t.Fatalf("results[1] = %+v", results[1])
	}
}

// TestSearxngSearcherErrorStatus 非 2xx 返回错误（上层静默降级用）。
func TestSearxngSearcherErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	s := &SearxngSearcher{BaseURL: srv.URL}
	if _, err := s.Search(context.Background(), "q", 5); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want status error", err)
	}
}

// TestTavilySearcherParse tavily API：POST {base}/search 请求体携带
// api_key/query/max_results；结果解析 + content 截断。
func TestTavilySearcherParse(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/search" {
			t.Errorf("request = %s %s, want POST /search", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"Tavily 结果","url":"https://t.example/x","content":"内容 T"}]}`))
	}))
	defer srv.Close()
	s := &TavilySearcher{BaseURL: srv.URL, APIKey: "tvly-key"}
	results, err := s.Search(context.Background(), "搜索词", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotBody["api_key"] != "tvly-key" || gotBody["query"] != "搜索词" || gotBody["max_results"] != float64(5) {
		t.Fatalf("body = %v", gotBody)
	}
	if len(results) != 1 || results[0].Title != "Tavily 结果" || results[0].URL != "https://t.example/x" {
		t.Fatalf("results = %+v", results)
	}
}

// TestNewWebSearcherDisabled 未配置/未知 provider 返回 nil（禁用）。
func TestNewWebSearcherDisabled(t *testing.T) {
	if got := NewWebSearcher(settings.AISearchConfig{}); got != nil {
		t.Fatalf("空配置应返回 nil, got %T", got)
	}
	if got := NewWebSearcher(settings.AISearchConfig{Provider: "bogus"}); got != nil {
		t.Fatalf("未知 provider 应返回 nil, got %T", got)
	}
	if got := NewWebSearcher(settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: "http://s:8080"}); got == nil {
		t.Fatal("searxng 应构造实现")
	}
	if got := NewWebSearcher(settings.AISearchConfig{Provider: settings.AISearchProviderTavily, TavilyAPIKey: "k"}); got == nil {
		t.Fatal("tavily 应构造实现")
	}
}

// ---------- ApplyWebSearch：跳过 / 注入 / 降级 ----------

// TestApplyWebSearchSkipsWhenDisabled 未配置 provider：静默跳过，messages 原样。
func TestApplyWebSearchSkipsWhenDisabled(t *testing.T) {
	svc := NewService(mockConfig())
	msgs := []Message{{Role: "user", Content: "问一个需要联网的问题"}}
	out, sources, failed := svc.ApplyWebSearch(context.Background(), settings.AISearchConfig{}, "查询", msgs)
	if len(out) != 1 || out[0].Role != "user" || failed || sources != nil {
		t.Fatalf("out=%v sources=%v failed=%v, want 原样返回", out, sources, failed)
	}
	// 空查询同样跳过。
	out, sources, failed = svc.ApplyWebSearch(context.Background(), settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: "http://127.0.0.1:1"}, "  ", msgs)
	if len(out) != 1 || failed || sources != nil {
		t.Fatalf("空查询应跳过: out=%v failed=%v", out, failed)
	}
}

// TestApplyWebSearchDegradation 搜索失败（上游不可达）：failed=true、
// messages 原样（不注入）、无来源——对话不阻断。
func TestApplyWebSearchDegradation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	svc := NewService(mockConfig())
	msgs := []Message{{Role: "user", Content: "联网查一下"}}
	cfg := settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: srv.URL, MaxResults: 5}
	out, sources, failed := svc.ApplyWebSearch(context.Background(), cfg, "联网查一下", msgs)
	if !failed {
		t.Fatal("上游 5xx 应 failed=true（降级标注）")
	}
	if len(out) != 1 || out[0].Role != "user" || sources != nil {
		t.Fatalf("失败不应注入上下文: out=%v sources=%v", out, sources)
	}
}

// TestApplyWebSearchInjectsContext 成功：上下文块注入头部 system（含
// 「网络搜索结果」与 [n] 标题/链接/摘要），返回来源列表。
func TestApplyWebSearchInjectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"标题一","url":"https://a.example/1","content":"摘要一"}]}`))
	}))
	defer srv.Close()
	svc := NewService(mockConfig())
	msgs := []Message{{Role: "user", Content: "帮查资料"}, {Role: "user", Content: "最后的问题"}}
	cfg := settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: srv.URL, MaxResults: 5}
	out, sources, failed := svc.ApplyWebSearch(context.Background(), cfg, "最后的问题", msgs)
	if failed || len(sources) != 1 {
		t.Fatalf("failed=%v sources=%+v", failed, sources)
	}
	if len(out) != 3 || out[0].Role != "system" {
		t.Fatalf("应在头部注入 system: out=%v", out)
	}
	sys := out[0].Content
	for _, want := range []string{"网络搜索结果", "[1] 标题一", "https://a.example/1", "摘要：摘要一"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system 上下文缺 %q: %q", want, sys)
		}
	}
	// 既有 system 消息时并入（不新建）。
	msgs2 := []Message{{Role: "system", Content: "已有提示"}, {Role: "user", Content: "q"}}
	out2, _, failed2 := svc.ApplyWebSearch(context.Background(), cfg, "q", msgs2)
	if failed2 || len(out2) != 2 || out2[0].Role != "system" || !strings.HasPrefix(out2[0].Content, "已有提示") {
		t.Fatalf("应并入既有 system: out=%v", out2)
	}
}

// TestLastUserQueryTruncation 查询提取：取最后一条 user 消息并截 400 字。
func TestLastUserQueryTruncation(t *testing.T) {
	long := strings.Repeat("问", 450)
	msgs := []Message{{Role: "user", Content: "第一问"}, {Role: "assistant", Content: "答"}, {Role: "user", Content: long}}
	q := LastUserQuery(msgs)
	if got := utf8.RuneCountInString(q); got != WebSearchQueryMaxRunes {
		t.Fatalf("query runes = %d, want %d", got, WebSearchQueryMaxRunes)
	}
	if LastUserQuery([]Message{{Role: "assistant", Content: "无用户消息"}}) != "" {
		t.Fatal("无 user 消息应返回空串")
	}
}

// ---------- think 透传 payload（openai / anthropic 两分支） ----------

// reasoningProviderCfg 构造带 reasoning 模型的 OpenAI/Anthropic 假上游配置。
func reasoningProviderCfg(kind, model string, reasoning bool, baseURL string) settings.AIConfig {
	return settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "p1", Name: "P1", Kind: kind, BaseURL: baseURL, Enabled: true,
			Models: []settings.AIModel{{
				ID:           model,
				Capabilities: settings.AIModelCapabilities{Chat: true, Reasoning: reasoning},
			}},
		}},
		Temperature: 0.3, MaxTokens: 2048, PerUserPerMin: 20,
	}
}

// TestThinkOpenAIPayload openai_compatible：think=true 且模型 reasoning →
// 请求体 reasoning_effort=medium；模型不支持时静默忽略（无该字段）。
func TestThinkOpenAIPayload(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	// 模型勾选 reasoning：透传 reasoning_effort=medium。
	svc := NewService(func() (settings.AIConfig, error) {
		return reasoningProviderCfg(settings.AIKindOpenAICompatible, "o-test", true, srv.URL), nil
	})
	if _, err := svc.Chat(context.Background(), ChatRequest{Model: "o-test", Think: true, Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := bodies[0]["reasoning_effort"]; got != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium", got)
	}
	// 模型未勾选 reasoning：静默忽略 think（无 reasoning_effort 字段）。
	svc2 := NewService(func() (settings.AIConfig, error) {
		return reasoningProviderCfg(settings.AIKindOpenAICompatible, "plain", false, srv.URL), nil
	})
	before := len(bodies)
	if _, err := svc2.Chat(context.Background(), ChatRequest{Model: "plain", Think: true, Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Chat unsupported think: %v", err)
	}
	if _, present := bodies[before]["reasoning_effort"]; present {
		t.Fatalf("模型不支持时应静默忽略 think: %v", bodies[before])
	}
}

// TestThinkAnthropicPayload anthropic：think=true → thinking{enabled,2048}
// 且 max_tokens 抬高到 >budget；max_tokens 已充足时不变；模型不支持时忽略。
func TestThinkAnthropicPayload(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	newSvc := func(model string, reasoning bool, maxTokens int) *Service {
		cfg := reasoningProviderCfg(settings.AIKindAnthropic, model, reasoning, srv.URL)
		cfg.MaxTokens = maxTokens
		return NewService(func() (settings.AIConfig, error) { return cfg, nil })
	}
	// 默认 max_tokens=2048 ≤ budget：自动抬高（>2048）。
	svc := newSvc("claude-think", true, 2048)
	if _, err := svc.Chat(context.Background(), ChatRequest{Model: "claude-think", Think: true, Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	thinking, _ := bodies[0]["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(AnthropicThinkBudgetTokens) {
		t.Fatalf("thinking = %v", bodies[0]["thinking"])
	}
	if mt, _ := bodies[0]["max_tokens"].(float64); mt <= AnthropicThinkBudgetTokens {
		t.Fatalf("max_tokens = %v, want > %d（抬高）", bodies[0]["max_tokens"], AnthropicThinkBudgetTokens)
	}
	// max_tokens 已充足（4096 > 2048）：保持不变。
	svc2 := newSvc("claude-think", true, 4096)
	if _, err := svc2.Chat(context.Background(), ChatRequest{Model: "claude-think", Think: true, Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if mt, _ := bodies[1]["max_tokens"].(float64); mt != 4096 {
		t.Fatalf("max_tokens = %v, want 4096（已充足不变）", bodies[1]["max_tokens"])
	}
	// 模型不支持 reasoning：think 静默忽略（无 thinking 字段）。
	svc3 := newSvc("claude-plain", false, 2048)
	if _, err := svc3.Chat(context.Background(), ChatRequest{Model: "claude-plain", Think: true, Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Chat unsupported think: %v", err)
	}
	if _, present := bodies[2]["thinking"]; present {
		t.Fatalf("模型不支持时应忽略 think: %v", bodies[2])
	}
	// think=false：不透传。
	svc4 := newSvc("claude-think", true, 2048)
	if _, err := svc4.Chat(context.Background(), ChatRequest{Model: "claude-think", Messages: []Message{{Role: "user", Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Chat no think: %v", err)
	}
	if _, present := bodies[3]["thinking"]; present {
		t.Fatalf("think=false 不应透传: %v", bodies[3])
	}
}

// ---------- reasoning 能力解析与校验 ----------

// TestReasoningCapabilityParse capabilities.reasoning JSON 解析、
// EffectiveModels 旧单模型合成默认 false、/ai/models 链路（ModelWithID）。
func TestReasoningCapabilityParse(t *testing.T) {
	raw := `[{"id":"p","name":"P","kind":"openai_compatible","base_url":"https://api.example.com/v1","enabled":true,` +
		`"models":[{"id":"o1","capabilities":{"chat":true,"reasoning":true}},{"id":"g1","capabilities":{"chat":true}}]}]`
	var providers []settings.AIProvider
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m, ok := providers[0].ModelWithID("o1"); !ok || !m.Capabilities.Reasoning {
		t.Fatalf("o1 reasoning 解析失败: %+v", m)
	}
	if m, ok := providers[0].ModelWithID("g1"); !ok || m.Capabilities.Reasoning {
		t.Fatalf("g1 reasoning 默认应为 false: %+v", m)
	}
	// 旧配置（无 models）合成单模型：reasoning=false（think 被忽略）。
	legacy := settings.AIProvider{Model: "gpt-old"}
	if m, _ := legacy.ModelWithID("gpt-old"); m.Capabilities.Reasoning {
		t.Fatalf("旧单模型合成 reasoning 应为 false: %+v", m)
	}
}

// TestValidateAISearch ai.search.* 校验：provider 取值、searxng url、
// tavily key、max_results 范围；合法配置通过。
func TestValidateAISearch(t *testing.T) {
	base := settings.AIConfig{
		Providers:   []settings.AIProvider{{ID: "m1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true}},
		Temperature: 0.3, MaxTokens: 2048, PerUserPerMin: 20,
	}
	good := base
	good.Search = settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: "http://searxng:8080", MaxResults: 5}
	if err := settings.ValidateAI(good); err != nil {
		t.Fatalf("searxng 合法配置应通过: %v", err)
	}
	goodTavily := base
	goodTavily.Search = settings.AISearchConfig{Provider: settings.AISearchProviderTavily, TavilyAPIKey: "tvly-x", MaxResults: 5}
	if err := settings.ValidateAI(goodTavily); err != nil {
		t.Fatalf("tavily 合法配置应通过: %v", err)
	}
	if err := settings.ValidateAI(base); err != nil { // provider 空 = 禁用，合法
		t.Fatalf("默认（禁用）应通过: %v", err)
	}
	badProvider := base
	badProvider.Search = settings.AISearchConfig{Provider: "google"}
	if err := settings.ValidateAI(badProvider); err == nil {
		t.Fatal("未知 provider 应报错")
	}
	badURL := base
	badURL.Search = settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: ""}
	if err := settings.ValidateAI(badURL); err == nil {
		t.Fatal("searxng 缺 url 应报错")
	}
	badKey := base
	badKey.Search = settings.AISearchConfig{Provider: settings.AISearchProviderTavily}
	if err := settings.ValidateAI(badKey); err == nil {
		t.Fatal("tavily 缺 api_key 应报错")
	}
	badMax := base
	badMax.Search = settings.AISearchConfig{Provider: settings.AISearchProviderSearxng, SearxngURL: "http://s", MaxResults: 11}
	if err := settings.ValidateAI(badMax); err == nil {
		t.Fatal("max_results 超上限应报错")
	}
}
