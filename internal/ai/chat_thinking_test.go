// Package ai —— 推理思考（thinking）转发单测：openai_compatible 的
// delta.reasoning_content / delta.reasoning 与 anthropic 的 thinking_delta
// 经 OnThinking 流式转发，且不计入正文 Content。
package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/settings"
)

// fakeOpenAIReasoning 假 openai 兼容上游：流式返回 reasoning_content 与
// 正文交替（正文在前、推理在后，验证两路互不混入）。
func fakeOpenAIReasoning(kind string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"content":"结论："}}]}`)
		switch kind {
		case "reasoning_content":
			sseWrite(w, `{"choices":[{"delta":{"reasoning_content":"先想一步，"}}]}`)
			sseWrite(w, `{"choices":[{"delta":{"reasoning_content":"再想一步。"}}]}`)
		default:
			sseWrite(w, `{"choices":[{"delta":{"reasoning":"先想一步，"}}]}`)
			sseWrite(w, `{"choices":[{"delta":{"reasoning":"再想一步。"}}]}`)
		}
		sseWrite(w, `{"choices":[{"delta":{"content":"答案是 42。"}}]}`)
		sseWrite(w, `[DONE]`)
	}))
}

func TestChatOpenAIThinkingForward(t *testing.T) {
	for _, kind := range []string{"reasoning_content", "reasoning"} {
		up := fakeOpenAIReasoning(kind)
		defer up.Close()
		svc := NewService(func() (settings.AIConfig, error) {
			return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
		})
		var deltas, thinks []string
		res, err := svc.Chat(context.Background(), ChatRequest{
			Messages:   []Message{{Role: "user", Content: "终极问题"}},
			Stream:     true,
			OnThinking: func(s string) { thinks = append(thinks, s) },
		}, func(s string) { deltas = append(deltas, s) })
		if err != nil {
			t.Fatalf("Chat(%s): %v", kind, err)
		}
		if res.Content != "结论：答案是 42。" {
			t.Fatalf("content = %q（推理不应混入正文）", res.Content)
		}
		if got := strings.Join(thinks, ""); got != "先想一步，再想一步。" {
			t.Fatalf("thinking(%s) = %q", kind, got)
		}
		if strings.Join(deltas, "") != res.Content {
			t.Fatalf("deltas = %v", deltas)
		}
	}
}

// TestChatAnthropicThinkingForward anthropic 普通对话：thinking_delta 经
// OnThinking 转发、text_delta 照旧走正文。
func TestChatAnthropicThinkingForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"message_start","message":{"usage":{"input_tokens":9}}}`)
		sseWrite(w, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`)
		sseWrite(w, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"分析问题，"}}`)
		sseWrite(w, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"推导答案。"}}`)
		sseWrite(w, `{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`)
		sseWrite(w, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案是 42。"}}`)
		sseWrite(w, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
	}))
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "a1", Name: "Anthropic", Kind: settings.AIKindAnthropic, BaseURL: up.URL, Model: "claude-3", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	var thinks []string
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages:   []Message{{Role: "user", Content: "终极问题"}},
		Stream:     true,
		OnThinking: func(s string) { thinks = append(thinks, s) },
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "答案是 42。" {
		t.Fatalf("content = %q（thinking 不应混入正文）", res.Content)
	}
	if got := strings.Join(thinks, ""); got != "分析问题，推导答案。" {
		t.Fatalf("thinking = %q", got)
	}
}

// TestChatMockThinkingForward mock Provider：think=true 时经 OnThinking
// 流出模拟推理（前端思考区无真实 Key 演示链路）。
func TestChatMockThinkingForward(t *testing.T) {
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "mock", Name: "Mock", Kind: settings.AIKindMock, Enabled: true, Models: []settings.AIModel{{ID: "m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat, Reasoning: true}}}}}}, nil
	})
	var thinks []string
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages:   []Message{{Role: "user", Content: "你好"}},
		Think:      true,
		Stream:     true,
		OnThinking: func(s string) { thinks = append(thinks, s) },
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(thinks) == 0 || strings.Join(thinks, "") == "" {
		t.Fatalf("mock think=true 应流出模拟推理: %v", thinks)
	}
	if strings.Contains(res.Content, "模拟思考") {
		t.Fatalf("thinking 不应混入正文: %q", res.Content)
	}
}

// TestToolEventSummaryLifecycle 工具事件摘要：input/output 截断、
// error 事件携带错误文本（经 fakeMCPEcho 的成功路径 + 未知工具负路径
// 在 execTool 层直测）。
func TestToolEventSummaryTruncate(t *testing.T) {
	long := strings.Repeat("长", ToolEventSummaryMaxRunes+10)
	got := toolEventSummary(long)
	if runes := []rune(got); len(runes) != ToolEventSummaryMaxRunes+1 { // 截断 + 省略号
		t.Fatalf("summary runes = %d, want %d", len(runes), ToolEventSummaryMaxRunes+1)
	}
	if got := toolEventSummary("  {\"a\":1}  "); got != `{"a":1}` {
		t.Fatalf("summary = %q", got)
	}
}

// TestChatReasoningOnlyErrors 推理模型耗尽输出上限（reasoning-only，正文
// 为空）时必须显式报错，而不是以 done+空内容静默收尾——后者在前端表现
// 为「已深度思考」后内容全空白（回归测试：openai 工具循环流式回退非流式
// 与 anthropic 工具循环 thinking-only 轮）。
func TestChatReasoningOnlyErrors(t *testing.T) {
	// openai 兼容：流式只有 reasoning_content（无正文无工具调用）→ 回退
	// 非流式返回空 message.content → openAIToolRound 须报错。
	upOpenAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") && r.Body != nil {
			// 非流式（回退）请求：返回空正文。
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"stream":true`) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"choices":[{"delta":{"reasoning_content":"想了很多，"}}]}`)
		sseWrite(w, `{"choices":[{"delta":{"reasoning_content":"但没写正文。"}}]}`)
		sseWrite(w, `[DONE]`)
	}))
	defer upOpenAI.Close()
	svcOpenAI := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: upOpenAI.URL, Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	// 工具循环路径（use_files 场景）：PlatformTools + ToolExecutor 注入。
	exec := func(name string, args json.RawMessage) (string, error) { return `{"ok":true}`, nil }
	if _, err := svcOpenAI.Chat(context.Background(), ChatRequest{
		Messages:      []Message{{Role: "user", Content: "写点什么"}},
		Stream:        true,
		PlatformTools: PlatformTools(), ToolExecutor: exec,
		OnThinking: func(string) {},
	}, nil); err == nil {
		t.Fatalf("openai 工具循环 reasoning-only 应报错（空 done 会让前端内容全空白）")
	}

	// anthropic：thinking 块完整但无 text/tool_use 块 → chatAnthropicTools
	// 须报错（thinking-only 不再静默空正文）。
	upAnth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sseWrite(w, `{"type":"message_start","message":{"usage":{"input_tokens":9}}}`)
		sseWrite(w, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`)
		sseWrite(w, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想了很多但没有正文。"}}`)
		sseWrite(w, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`)
	}))
	defer upAnth.Close()
	svcAnth := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "a1", Name: "Anthropic", Kind: settings.AIKindAnthropic, BaseURL: upAnth.URL, Model: "claude-3", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	if _, err := svcAnth.Chat(context.Background(), ChatRequest{
		Messages:      []Message{{Role: "user", Content: "写点什么"}},
		Stream:        true,
		PlatformTools: PlatformTools(), ToolExecutor: exec,
		OnThinking: func(string) {},
	}, nil); err == nil {
		t.Fatalf("anthropic 工具循环 thinking-only 应报错（空 done 会让前端内容全空白）")
	}
}

// TestChatThinkRaisesMaxTokens think=true 时 max_tokens 抬到
// ThinkMaxTokensFloor（openai 兼容网关 max_tokens 计入 reasoning_content，
// 默认 2048 被思考耗尽 → 正文空）。
func TestChatThinkRaisesMaxTokens(t *testing.T) {
	var seen float64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			MaxTokens float64 `json:"max_tokens"`
		}
		_ = json.Unmarshal(raw, &body)
		seen = body.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"好。"}}]}`))
	}))
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{
			ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Enabled: true,
			Models: []settings.AIModel{{ID: "m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat, Reasoning: true}}},
		}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	if _, err := svc.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, Think: true, Stream: false}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if seen < ThinkMaxTokensFloor {
		t.Fatalf("think=true max_tokens = %v, want >= %d", seen, ThinkMaxTokensFloor)
	}
}

// TestChatStreamFallbackEmitsContent 流式失败回退非流式时，完整正文必须
// 经 onDelta 补发——否则 SSE 客户端只收到中断前的部分增量甚至零增量，
// 却以 done 成功收尾（「内容空白/被静默截断」的另一形态）。
func TestChatStreamFallbackEmitsContent(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"stream":true`) {
			// 流式请求：上游 500（触发回退）。
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// 非流式回退：返回完整正文。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"完整的最终回答。"}}]}`))
	}))
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	var deltas []string
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "完整的最终回答。" {
		t.Fatalf("content = %q", res.Content)
	}
	if got := strings.Join(deltas, ""); got != res.Content {
		t.Fatalf("回退非流式未经 onDelta 补发全文: deltas = %q, content = %q", got, res.Content)
	}
}
