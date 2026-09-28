// Package ai —— 推理思考（thinking）转发单测：openai_compatible 的
// delta.reasoning_content / delta.reasoning 与 anthropic 的 thinking_delta
// 经 OnThinking 流式转发，且不计入正文 Content。
package ai

import (
	"context"
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
