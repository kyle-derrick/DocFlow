// Package http —— agentsock_openai_test.go：POST /v1/chat/completions 工具
// 直连转发测试：httptest 假 openai 上游断言转发体（model 替换为平台默认/
// tools 与 tool_choice 原样/assistant tool_calls 与 role=tool 消息回放
// 原样/Authorization: Bearer）与响应体原样回传（SSE 字节流与 JSON 信封
// 逐字节一致）、目标解析错误映射（未装配 503/ErrNoProvider 503/非
// openai 兼容 502/上游 5xx 502）与审计 endpoint=openai-forward；另覆盖
// 纯文本请求在目标已装配时仍走平台引擎链路（双路分流）。
package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/settings"
	"github.com/google/uuid"
)

// agentAIOpenAIToolSSE 为假 openai 上游的流式响应（pi 形态：role 序幕 →
// tool_calls 增量 → finish_reason=tool_calls 收尾 → [DONE]）。
const agentAIOpenAIToolSSE = "data: " + `{"id":"chatcmpl-up-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-platform-model","choices":[{"index":0,"delta":{"role":"assistant","content":null},"finish_reason":null}]}` + "\n\n" +
	"data: " + `{"id":"chatcmpl-up-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-platform-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":null}]}` + "\n\n" +
	"data: " + `{"id":"chatcmpl-up-1","object":"chat.completion.chunk","created":1730000000,"model":"gpt-platform-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// agentAIOpenAIToolJSON 为假 openai 上游的非流式响应（含 tool_calls）。
const agentAIOpenAIToolJSON = `{"id":"chatcmpl-up-2","object":"chat.completion","created":1730000001,"model":"gpt-platform-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_8","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":31,"completion_tokens":9,"total_tokens":40}}`

// agentAIOpenAIPiBody 为 pi 形态的带 tools 多轮请求（assistant tool_calls
// + role=tool 结果回放；stream 以 %s 注入 true/false）。
const agentAIOpenAIPiBody = `{
  "model": "gpt-5-mini",
  "max_tokens": 1024,
  "stream": %s,
  "tool_choice": "auto",
  "temperature": 0.7,
  "tools": [{"type":"function","function":{"name":"shell","description":"Run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}}],
  "messages": [
    {"role":"system","content":"You are pi"},
    {"role":"user","content":"list files"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]},
    {"role":"tool","tool_call_id":"call_1","content":"file1\nfile2"}
  ]
}`

// agentAIOpenAIReplayBody 为无 tools 声明、仅消息回放（assistant
// tool_calls + role=tool）的请求——同样必须走直连转发。
const agentAIOpenAIReplayBody = `{
  "model": "gpt-5-mini",
  "messages": [
    {"role":"user","content":"list files"},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_2","type":"function","function":{"name":"shell","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"call_2","content":"out"}
  ]
}`

// newOpenAITestEnv 构造带引擎回调调用追踪的网关环境。
func newOpenAITestEnv(t *testing.T) (*httptest.Server, *AgentAIGateway, *bool) {
	t.Helper()
	chatCalled := false
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		chatCalled = true
		return "should not be used", "mock/m", nil
	}, nil)
	return srv, gw, &chatCalled
}

// assertOpenAIForwardedBody 断言直连转发体（model 替换/Bearer/tools 与
// tool_choice 原样/消息回放原样/temperature 不透传）。
func assertOpenAIForwardedBody(t *testing.T, raw []byte, hdr http.Header, wantStream bool, wantTools bool) {
	t.Helper()
	if auth := hdr.Get("Authorization"); auth != "Bearer sk-openai" {
		t.Fatalf("upstream Authorization = %q, want Bearer sk-openai", auth)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("upstream Content-Type = %q", ct)
	}
	var fwd struct {
		Model       string            `json:"model"`
		MaxTokens   int               `json:"max_tokens"`
		Stream      bool              `json:"stream"`
		ToolChoice  json.RawMessage   `json:"tool_choice"`
		Temperature *float64          `json:"temperature"`
		Messages    []json.RawMessage `json:"messages"`
		Tools       []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &fwd); err != nil {
		t.Fatalf("decode forwarded body: %v (%s)", err, raw)
	}
	if fwd.Model != "gpt-platform-model" {
		t.Fatalf("forwarded model = %q, want platform default (request model ignored)", fwd.Model)
	}
	if fwd.Stream != wantStream {
		t.Fatalf("forwarded stream = %v, want %v", fwd.Stream, wantStream)
	}
	if fwd.MaxTokens != 1024 {
		t.Fatalf("forwarded max_tokens = %d, want 1024", fwd.MaxTokens)
	}
	if fwd.Temperature != nil {
		t.Fatalf("temperature must not be forwarded, got %v", *fwd.Temperature)
	}
	if wantTools {
		if string(fwd.ToolChoice) != `"auto"` {
			t.Fatalf("forwarded tool_choice = %s, want \"auto\"", fwd.ToolChoice)
		}
		if len(fwd.Tools) != 1 {
			t.Fatalf("forwarded tools = %d, want 1", len(fwd.Tools))
		}
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Parameters  json.RawMessage `json:"parameters"`
				Description string          `json:"description"`
			} `json:"function"`
		}
		if err := json.Unmarshal(fwd.Tools[0], &tool); err != nil || tool.Type != "function" || tool.Function.Name != "shell" ||
			!strings.Contains(string(tool.Function.Parameters), `"cmd"`) || tool.Function.Description != "Run a shell command" {
			t.Fatalf("forwarded tool mismatch: %+v (%s)", tool, fwd.Tools[0])
		}
	} else {
		if fwd.Tools != nil || fwd.ToolChoice != nil {
			t.Fatalf("replay-only forward must omit tools/tool_choice: %s", raw)
		}
	}
	if len(fwd.Messages) != 4 {
		t.Fatalf("forwarded messages = %d, want 4: %s", len(fwd.Messages), raw)
	}
	var sys struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(fwd.Messages[0], &sys); err != nil || sys.Role != "system" || sys.Content != "You are pi" {
		t.Fatalf("system message passthrough mismatch: %+v (%s)", sys, fwd.Messages[0])
	}
	var assistant struct {
		Role      string `json:"role"`
		Content   any    `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(fwd.Messages[2], &assistant); err != nil || assistant.Role != "assistant" || assistant.Content != nil ||
		len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Type != "function" ||
		assistant.ToolCalls[0].Function.Name != "shell" || !strings.Contains(assistant.ToolCalls[0].Function.Arguments, "ls") {
		t.Fatalf("assistant tool_calls passthrough mismatch: %+v (%s)", assistant, fwd.Messages[2])
	}
	var toolMsg struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(fwd.Messages[3], &toolMsg); err != nil || toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != "file1\nfile2" {
		t.Fatalf("tool message passthrough mismatch: %+v (%s)", toolMsg, fwd.Messages[3])
	}
}

// TestAgentAIOpenAIToolsForwardStream 工具直连流式（pi 核心场景）：假
// openai 上游断言转发体（model 替换/tools 与 tool_choice 原样/assistant
// tool_calls 与 role=tool 回放原样/Bearer key/temperature 不透传），SSE
// 字节流逐字节原样回传，引擎回调不被调用。
func TestAgentAIOpenAIToolsForwardStream(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBody, upstreamHeader = raw, r.Header
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, agentAIOpenAIToolSSE)
	}))
	t.Cleanup(upstream.Close)
	srv, gw, chatCalled := newOpenAITestEnv(t)
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "sk-openai", "gpt-platform-model", nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	code, hdr, body := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "true"))
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if *chatCalled {
		t.Fatal("plain chat callback must not be used on the forward path")
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	assertOpenAIForwardedBody(t, upstreamBody, upstreamHeader, true, true)
	if body != agentAIOpenAIToolSSE {
		t.Fatalf("SSE passthrough mismatch:\ngot  %q\nwant %q", body, agentAIOpenAIToolSSE)
	}
}

// TestAgentAIOpenAIToolsForwardNonStream 工具直连非流式：上游 chat
// completion JSON（含 tool_calls）逐字节原样回传，转发体 stream=false。
func TestAgentAIOpenAIToolsForwardNonStream(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBody, upstreamHeader = raw, r.Header
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, agentAIOpenAIToolJSON)
	}))
	t.Cleanup(upstream.Close)
	srv, gw, chatCalled := newOpenAITestEnv(t)
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "sk-openai", "gpt-platform-model", nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	code, hdr, body := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "false"))
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if *chatCalled {
		t.Fatal("plain chat callback must not be used on the forward path")
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	assertOpenAIForwardedBody(t, upstreamBody, upstreamHeader, false, true)
	if body != agentAIOpenAIToolJSON {
		t.Fatalf("JSON passthrough mismatch:\ngot  %s\nwant %s", body, agentAIOpenAIToolJSON)
	}
}

// TestAgentAIOpenAIToolCallsReplayForward 无 tools 声明、仅消息回放
// （assistant tool_calls + role=tool）的请求同样走直连：转发体不含
// tools/tool_choice 字段。
func TestAgentAIOpenAIToolCallsReplayForward(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBody = raw
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, agentAIOpenAIToolJSON)
	}))
	t.Cleanup(upstream.Close)
	srv, gw, chatCalled := newOpenAITestEnv(t)
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "sk-openai", "gpt-platform-model", nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	// max_tokens 缺省（0）：转发体不带该字段。
	code, _, body := agentAIOpenAI(t, srv.URL, token, agentAIOpenAIReplayBody)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if *chatCalled {
		t.Fatal("tool_calls replay must be forwarded, not sent to the engine")
	}
	var fwd struct {
		Model     string            `json:"model"`
		MaxTokens json.RawMessage   `json:"max_tokens"`
		Messages  []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(upstreamBody, &fwd); err != nil {
		t.Fatalf("decode forwarded body: %v (%s)", err, upstreamBody)
	}
	if fwd.Model != "gpt-platform-model" || len(fwd.MaxTokens) > 0 || len(fwd.Messages) != 3 {
		t.Fatalf("replay forward mismatch: %s", upstreamBody)
	}
}

// TestAgentAIOpenAIForwardErrors 直连路径错误映射与审计：未装配目标 503、
// 目标 ErrNoProvider 503、非 openai 兼容 Provider 502（明确报错）、上游
// 5xx 502（附上游错误摘要）、审计 endpoint=openai-forward；纯文本请求在
// 目标已装配时仍走平台引擎（目标回调不触发）。
func TestAgentAIOpenAIForwardErrors(t *testing.T) {
	rec := &fakeAuditRecorder{}
	chat := func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "ok", "mock1/mock-echo", nil
	}
	srv, gw := newAgentAITestEnv(t, chat, rec)
	token := gw.Register(uuid.New(), uuid.New(), 20)
	// 未装配目标：带 tools → 503。
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "false")); code != http.StatusServiceUnavailable || !strings.Contains(raw, "passthrough") {
		t.Fatalf("unconfigured target: code=%d raw=%s, want 503", code, raw)
	}
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return "", "", "", ai.ErrNoProvider
	})
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "false")); code != http.StatusServiceUnavailable || !strings.Contains(raw, "not configured") {
		t.Fatalf("no provider: code=%d raw=%s, want 503", code, raw)
	}
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return "", "", "", ErrAgentAIOpenAIIncompatible
	})
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "false")); code != http.StatusBadGateway || !strings.Contains(raw, "openai-compatible") {
		t.Fatalf("incompatible provider: code=%d raw=%s, want 502 naming openai compatibility", code, raw)
	}
	// 上游 5xx → 502（附上游错误摘要）。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream exploded"}}`)
	}))
	t.Cleanup(upstream.Close)
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "sk", "m", nil
	})
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "false")); code != http.StatusBadGateway || !strings.Contains(raw, "upstream exploded") {
		t.Fatalf("upstream 5xx: code=%d raw=%s, want 502 with upstream message", code, raw)
	}
	// 直连路径审计 endpoint=openai-forward（失败也记）。
	entries := rec.snapshot()
	if len(entries) == 0 {
		t.Fatal("forward failures must be audited")
	}
	for _, e := range entries {
		if !strings.Contains(e.Metadata, `"endpoint":"openai-forward"`) {
			t.Fatalf("audit endpoint mismatch: %s", e.Metadata)
		}
	}
	// 目标已装配时纯文本请求仍走平台引擎（目标回调不触发）。
	targetCalled := false
	gw.SetOpenAITarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		targetCalled = true
		return "", "", "", nil
	})
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, `{"messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK || !strings.Contains(raw, "mock1/mock-echo") {
		t.Fatalf("plain path with target configured: code=%d raw=%s, want 200 via engine", code, raw)
	}
	if targetCalled {
		t.Fatal("plain requests must not resolve the forward target")
	}
}

// TestAgentAIOpenAIIncompatibleTargetDirect 校验 main.go 装配同款目标解析
// （非 openai 兼容 kind Provider → ErrAgentAIOpenAIIncompatible）经
// ai.Service 真链路的行为（mock kind 默认 Provider → 502）。
func TestAgentAIOpenAIIncompatibleTargetDirect(t *testing.T) {
	svc := newAIv1Service(100) // mock kind 默认 Provider。
	srv, gw, _ := newOpenAITestEnv(t)
	gw.SetOpenAITarget(func(ctx context.Context, user uuid.UUID) (string, string, string, error) {
		target, err := svc.ForUser(user).ResolveChatTargetFor("", "", settings.AIScenarioChat)
		if err != nil {
			return "", "", "", err
		}
		if target.Provider.Kind != settings.AIKindOpenAICompatible {
			return "", "", "", ErrAgentAIOpenAIIncompatible
		}
		return target.Provider.BaseURL, target.Provider.APIKey, target.Model, nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, fmt.Sprintf(agentAIOpenAIPiBody, "false")); code != http.StatusBadGateway || !strings.Contains(raw, "openai-compatible") {
		t.Fatalf("mock default provider should yield 502: code=%d raw=%s", code, raw)
	}
}
