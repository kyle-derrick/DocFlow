// Package http —— agentsock_anthropic_test.go：POST /v1/messages 端点
// 测试：非流式 message 信封（fake provider）、流式标准 anthropic SSE 事件
// 序列（可解析拼装）、tools 声明与 tool_use/tool_result 块的透传转发
// （httptest 假 anthropic 上游断言转发体与流式回传）、载荷校验（多模态
// 块 400/system 块数组归一/model 忽略）与令牌/限流跨端点共享计数。
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

// agentAIAnthropic 发起一次 POST /v1/messages（Bearer token），返回状态
// 码、响应头与原始响应体。
func agentAIAnthropic(t *testing.T, base, token, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := agentAIClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(raw)
}

// agentAISSEEvent 为一条已解析的 SSE 事件（event 名 + data 载荷）。
type agentAISSEEvent struct {
	name string
	data string
}

// agentAIParseSSE 解析 event:/data: 行组（anthropic 具名事件与 openai
// data 事件两种写法均可）。
func agentAIParseSSE(body string) []agentAISSEEvent {
	var events []agentAISSEEvent
	cur := agentAISSEEvent{data: "\x00"} // data 未出现标记
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if cur.name != "" || cur.data != "\x00" {
				events = append(events, cur)
			}
			cur = agentAISSEEvent{data: "\x00"}
		}
	}
	if cur.name != "" || cur.data != "\x00" {
		events = append(events, cur)
	}
	return events
}

// agentAIAnthropicToolSSE 为假 anthropic 上游的流式响应：一次文本块 +
// 一次 tool_use 块（Claude Code 本地执行工具的模型输出形态）。
const agentAIAnthropicToolSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_up_1","type":"message","role":"assistant","model":"claude-platform-model","content":[],"usage":{"input_tokens":25,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Listing files."}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"Bash","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// agentAIAnthropicToolJSON 为假 anthropic 上游的非流式响应（含 tool_use
// content 块）。
const agentAIAnthropicToolJSON = `{"id":"msg_up_2","type":"message","role":"assistant","model":"claude-platform-model","content":[{"type":"text","text":"Running it."},{"type":"tool_use","id":"toolu_8","name":"Bash","input":{"command":"pwd"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":9}}`

// agentAIAnthropicClaudeCodeBody 为 Claude Code 形态的带 tools 多轮请求
// （assistant tool_use + user tool_result content 块数组回传）。
const agentAIAnthropicClaudeCodeBody = `{
  "model": "claude-3-5-sonnet-20241022",
  "max_tokens": 1024,
  "stream": %s,
  "system": [{"type":"text","text":"You are Claude Code","cache_control":{"type":"ephemeral"}}],
  "tools": [{"name":"Bash","description":"Run a bash command","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
  "messages": [
    {"role":"user","content":"list files"},
    {"role":"assistant","content":[{"type":"text","text":"I'll check."},{"type":"tool_use","id":"toolu_0","name":"Bash","input":{"command":"ls"}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_0","content":"file1\nfile2"}]}
  ]
}`

// TestAgentAIAnthropicMessagesNonStream 非流式全链路（fake provider）：
// 请求 model 忽略（实际模型为平台默认 mock1/mock-echo）、system 块数组
// 归一为系统提示、text 块数组 content 归一为纯文本、max_tokens 透传、
// anthropic message 信封（id=msg_<taskID>、content text 块、
// stop_reason=end_turn、usage 占位）与审计（endpoint=anthropic）。
func TestAgentAIAnthropicMessagesNonStream(t *testing.T) {
	rec := &fakeAuditRecorder{}
	var gotSystem string
	var gotMsgs []ai.Message
	var gotMax int
	var full string
	svc := newAIv1Service(100)
	srv, gw := newAgentAITestEnv(t, func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int, onDelta func(string)) (string, string, error) {
		gotSystem, gotMsgs, gotMax = system, messages, maxTokens
		res, err := svc.ForUser(user).Chat(ctx, ai.ChatRequest{System: system, Messages: messages, MaxTokens: maxTokens, Stream: onDelta != nil}, onDelta)
		if err != nil {
			return "", "", err
		}
		full = res.Content
		return res.Content, res.ProviderID + "/" + res.Model, nil
	}, rec)
	task, user := uuid.New(), uuid.New()
	token := gw.Register(task, user, 5)
	code, hdr, body := agentAIAnthropic(t, srv.URL, token, `{"model":"claude-3-5-sonnet-20241022","max_tokens":321,"system":[{"type":"text","text":"SYS-A"},{"type":"text","text":"SYS-B"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"blocks"}]},{"role":"assistant","content":"hi"},{"role":"user","content":"build a website"}]}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var out struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	if out.ID != "msg_"+task.String() || out.Type != "message" || out.Role != "assistant" {
		t.Fatalf("envelope mismatch: %+v", out)
	}
	if out.Model != "mock1/mock-echo" {
		t.Fatalf("model = %q, want platform mock1/mock-echo (request model ignored)", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != full || full == "" {
		t.Fatalf("content mismatch: %+v (engine content %q)", out.Content, full)
	}
	if out.StopReason != "end_turn" || out.Usage.InputTokens != 0 || out.Usage.OutputTokens != 0 {
		t.Fatalf("stop/usage mismatch: %q %+v", out.StopReason, out.Usage)
	}
	if gotSystem != "SYS-A\nSYS-B" {
		t.Fatalf("system normalized = %q, want SYS-A\\nSYS-B", gotSystem)
	}
	if len(gotMsgs) != 3 || gotMsgs[0].Content != "hello \nblocks" || gotMsgs[1].Role != "assistant" || gotMsgs[2].Content != "build a website" {
		t.Fatalf("engine messages = %+v", gotMsgs)
	}
	if gotMax != 321 {
		t.Fatalf("max_tokens = %d", gotMax)
	}
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != ActionAgentAI || e.ResourceID != task.String() || e.UserID == nil || *e.UserID != user || !strings.Contains(e.Metadata, `"endpoint":"anthropic"`) {
		t.Fatalf("audit entry mismatch: %+v", e)
	}
}

// TestAgentAIAnthropicMessagesStream 纯文本路径流式：标准 anthropic SSE
// 事件序列（message_start → content_block_start → ≥1 text_delta →
// content_block_stop → message_delta(end_turn) → message_stop）、text_delta
// 拼装等于引擎全文、message_start 的 message 头完整。
func TestAgentAIAnthropicMessagesStream(t *testing.T) {
	svc := newAIv1Service(100)
	var full string
	srv, gw := newAgentAITestEnv(t, func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int, onDelta func(string)) (string, string, error) {
		res, err := svc.ForUser(user).Chat(ctx, ai.ChatRequest{System: system, Messages: messages, MaxTokens: maxTokens, Stream: onDelta != nil}, onDelta)
		if err != nil {
			return "", "", err
		}
		full = res.Content
		return res.Content, res.ProviderID + "/" + res.Model, nil
	}, nil)
	task := uuid.New()
	token := gw.Register(task, uuid.New(), 5)
	code, hdr, body := agentAIAnthropic(t, srv.URL, token, `{"model":"claude-x","stream":true,"system":"BE AGENT","messages":[{"role":"user","content":"你好"}]}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	events := agentAIParseSSE(body)
	if len(events) < 5 {
		t.Fatalf("events = %d, body=%q", len(events), body)
	}
	wantNames := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	var names []string
	for _, ev := range events {
		names = append(names, ev.name)
	}
	for _, want := range wantNames {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Fatalf("missing event %q in %v", want, names)
		}
	}
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("event order = %v", names)
	}
	var start struct {
		Message struct {
			ID    string `json:"id"`
			Role  string `json:"role"`
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(events[0].data), &start); err != nil || start.Message.ID != "msg_"+task.String() || start.Message.Role != "assistant" {
		t.Fatalf("message_start mismatch: %v %+v (%s)", err, start, events[0].data)
	}
	var assembled strings.Builder
	deltas := 0
	for _, ev := range events {
		switch ev.name {
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
				t.Fatalf("delta decode: %v (%s)", err, ev.data)
			}
			if d.Index != 0 || d.Delta.Type != "text_delta" {
				t.Fatalf("unexpected delta: %+v (%s)", d, ev.data)
			}
			assembled.WriteString(d.Delta.Text)
			deltas++
		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil || d.Delta.StopReason != "end_turn" {
				t.Fatalf("message_delta mismatch: %v (%s)", err, ev.data)
			}
		case "content_block_start", "content_block_stop":
			var d struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil || d.Index != 0 {
				t.Fatalf("block event mismatch: %v (%s)", err, ev.data)
			}
		}
	}
	if deltas == 0 || assembled.String() != full {
		t.Fatalf("assembled = %q, want engine content %q (deltas=%d)", assembled.String(), full, deltas)
	}
}

// TestAgentAIAnthropicToolsForwardStream 工具透传流式（Claude Code 核心
// 场景）：假 anthropic 上游断言转发体——model 替换为平台默认（请求 model
// 忽略）、tools 原样透传（input_schema 保真）、assistant tool_use 与
// user tool_result 块原样透传、system 归一为字符串、x-api-key/
// anthropic-version 头；SSE 字节流原样回传（tool_use 块事件完整可解析）。
func TestAgentAIAnthropicToolsForwardStream(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBody, upstreamHeader = raw, r.Header
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, agentAIAnthropicToolSSE)
	}))
	t.Cleanup(upstream.Close)
	chatCalled := false
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		chatCalled = true
		return "should not be used", "mock/m", nil
	}, nil)
	gw.SetAnthropicTarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "sk-test", "claude-platform-model", nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	code, hdr, body := agentAIAnthropic(t, srv.URL, token, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "true"))
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if chatCalled {
		t.Fatal("plain chat callback must not be used on the passthrough path")
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	// 上游转发体断言。
	var fwd struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(upstreamBody, &fwd); err != nil {
		t.Fatalf("decode forwarded body: %v (%s)", err, upstreamBody)
	}
	if fwd.Model != "claude-platform-model" {
		t.Fatalf("forwarded model = %q, want platform default (request model ignored)", fwd.Model)
	}
	if !fwd.Stream || fwd.MaxTokens != 1024 || fwd.System != "You are Claude Code" {
		t.Fatalf("forwarded envelope = stream:%v max:%d system:%q", fwd.Stream, fwd.MaxTokens, fwd.System)
	}
	if len(fwd.Tools) != 1 || fwd.Tools[0].Name != "Bash" || fwd.Tools[0].Description != "Run a bash command" || !json.Valid(fwd.Tools[0].InputSchema) || !strings.Contains(string(fwd.Tools[0].InputSchema), `"command"`) {
		t.Fatalf("forwarded tools = %+v", fwd.Tools)
	}
	if len(fwd.Messages) != 3 || fwd.Messages[0].Role != "user" {
		t.Fatalf("forwarded messages = %+v", fwd.Messages)
	}
	if string(fwd.Messages[0].Content) != `"list files"` {
		t.Fatalf("string content passthrough = %s", fwd.Messages[0].Content)
	}
	var assistantBlocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(fwd.Messages[1].Content, &assistantBlocks); err != nil || len(assistantBlocks) != 2 ||
		assistantBlocks[0].Type != "text" || assistantBlocks[0].Text != "I'll check." ||
		assistantBlocks[1].Type != "tool_use" || assistantBlocks[1].ID != "toolu_0" || assistantBlocks[1].Name != "Bash" {
		t.Fatalf("assistant tool_use blocks passthrough = %+v (%s)", assistantBlocks, fwd.Messages[1].Content)
	}
	var resultBlocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(fwd.Messages[2].Content, &resultBlocks); err != nil || len(resultBlocks) != 1 ||
		resultBlocks[0].Type != "tool_result" || resultBlocks[0].ToolUseID != "toolu_0" || resultBlocks[0].Content != "file1\nfile2" {
		t.Fatalf("tool_result blocks passthrough = %+v (%s)", resultBlocks, fwd.Messages[2].Content)
	}
	if upstreamHeader.Get("x-api-key") != "sk-test" || upstreamHeader.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("upstream headers = x-api-key:%q version:%q", upstreamHeader.Get("x-api-key"), upstreamHeader.Get("anthropic-version"))
	}
	// SSE 字节流原样回传断言（tool_use 块事件完整可解析）。
	events := agentAIParseSSE(body)
	var names []string
	for _, ev := range events {
		names = append(names, ev.name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing event %q in %v", want, names)
		}
	}
	var toolStart struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	var jsonDelta struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	}
	var stopDelta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	for _, ev := range events {
		switch {
		case ev.name == "content_block_start" && strings.Contains(ev.data, `"tool_use"`):
			if err := json.Unmarshal([]byte(ev.data), &toolStart); err != nil || toolStart.ContentBlock.Type != "tool_use" || toolStart.ContentBlock.ID != "toolu_9" || toolStart.ContentBlock.Name != "Bash" {
				t.Fatalf("tool_use block start mismatch: %v (%s)", err, ev.data)
			}
		case ev.name == "content_block_delta" && strings.Contains(ev.data, "input_json_delta"):
			if err := json.Unmarshal([]byte(ev.data), &jsonDelta); err != nil || jsonDelta.Delta.PartialJSON != `{"command":"ls"}` {
				t.Fatalf("input_json_delta mismatch: %v (%s)", err, ev.data)
			}
		case ev.name == "message_delta":
			if err := json.Unmarshal([]byte(ev.data), &stopDelta); err != nil || stopDelta.Delta.StopReason != "tool_use" {
				t.Fatalf("message_delta stop_reason mismatch: %v (%s)", err, ev.data)
			}
		}
	}
	if toolStart.ContentBlock.ID == "" || jsonDelta.Delta.PartialJSON == "" {
		t.Fatalf("tool_use events not observed: %v", names)
	}
}

// TestAgentAIAnthropicToolsForwardNonStream 工具透传非流式：上游 message
// JSON（含 tool_use content 块）原样回传，转发体不带 stream 字段。
func TestAgentAIAnthropicToolsForwardNonStream(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBody = raw
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, agentAIAnthropicToolJSON)
	}))
	t.Cleanup(upstream.Close)
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "unused", "mock/m", nil
	}, nil)
	gw.SetAnthropicTarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "", "claude-platform-model", nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	code, hdr, body := agentAIAnthropic(t, srv.URL, token, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "false"))
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var fwd map[string]any
	if err := json.Unmarshal(upstreamBody, &fwd); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	if _, ok := fwd["stream"]; ok {
		t.Fatalf("non-stream forward must omit stream: %s", upstreamBody)
	}
	var out struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	if out.ID != "msg_up_2" || out.Model != "claude-platform-model" || out.StopReason != "tool_use" {
		t.Fatalf("envelope passthrough mismatch: %+v", out)
	}
	if len(out.Content) != 2 || out.Content[1].Type != "tool_use" || out.Content[1].ID != "toolu_8" || out.Content[1].Name != "Bash" || !strings.Contains(string(out.Content[1].Input), "pwd") {
		t.Fatalf("tool_use content passthrough mismatch: %+v", out.Content)
	}
}

// TestAgentAIAnthropicErrors 错误语义与共享计数：多模态 image 块 400、
// 无名工具 400、坏 JSON 400、GET 405、透传未装配 503、目标解析
// ErrNoProvider 503 / 非 anthropic Provider 502、上游 5xx 502（anthropic
// 形状错误体）；限流与 /chat 共用同一 calls 计数。
func TestAgentAIAnthropicErrors(t *testing.T) {
	chat := func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "ok", "mock/m", nil
	}
	// 载荷校验（未装配透传目标不影响 400 判定，载荷先于路由分流）。
	srv, gw := newAgentAITestEnv(t, chat, nil)
	token := gw.Register(uuid.New(), uuid.New(), 20)
	if code, _, raw := agentAIAnthropic(t, srv.URL, token, `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`); code != http.StatusBadRequest || !strings.Contains(raw, "image") {
		t.Fatalf("image block: code=%d raw=%s, want 400 naming the block type", code, raw)
	}
	if code, _, raw := agentAIAnthropic(t, srv.URL, token, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"description":"no name"}]}`); code != http.StatusBadRequest || !strings.Contains(raw, "tool name") {
		t.Fatalf("nameless tool: code=%d raw=%s, want 400", code, raw)
	}
	if code, _, _ := agentAIAnthropic(t, srv.URL, token, `{"messages":[{"role":"system","content":"x"}]}`); code != http.StatusBadRequest {
		t.Fatalf("system role in messages: code=%d, want 400", code)
	}
	if code, _, _ := agentAIAnthropic(t, srv.URL, token, `{bad json`); code != http.StatusBadRequest {
		t.Fatalf("bad json: code=%d, want 400", code)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/messages", nil)
	if res, err := agentAIClient().Do(req); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/messages should be 405, got err=%v", err)
	}
	// 带 tools 但未装配透传目标 → 503（anthropic 形状错误体）。
	if code, _, raw := agentAIAnthropic(t, srv.URL, token, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "false")); code != http.StatusServiceUnavailable || !strings.Contains(raw, `"type":"error"`) {
		t.Fatalf("passthrough unconfigured: code=%d raw=%s, want 503 anthropic error", code, raw)
	}
	// 目标解析错误映射。
	srv2, gw2 := newAgentAITestEnv(t, chat, nil)
	token2 := gw2.Register(uuid.New(), uuid.New(), 20)
	gw2.SetAnthropicTarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return "", "", "", ai.ErrNoProvider
	})
	if code, _, raw := agentAIAnthropic(t, srv2.URL, token2, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "false")); code != http.StatusServiceUnavailable || !strings.Contains(raw, "not configured") {
		t.Fatalf("no provider: code=%d raw=%s, want 503", code, raw)
	}
	gw2.SetAnthropicTarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return "", "", "", ErrAgentAIAnthropicIncompatible
	})
	if code, _, raw := agentAIAnthropic(t, srv2.URL, token2, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "false")); code != http.StatusBadGateway || !strings.Contains(raw, "anthropic-compatible") {
		t.Fatalf("incompatible provider: code=%d raw=%s, want 502 naming anthropic compatibility", code, raw)
	}
	// 上游 5xx → 502（附上游错误摘要）。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"upstream exploded"}}`)
	}))
	t.Cleanup(upstream.Close)
	gw2.SetAnthropicTarget(func(_ context.Context, _ uuid.UUID) (string, string, string, error) {
		return upstream.URL, "sk", "m", nil
	})
	if code, _, raw := agentAIAnthropic(t, srv2.URL, token2, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "false")); code != http.StatusBadGateway || !strings.Contains(raw, "upstream exploded") {
		t.Fatalf("upstream 5xx: code=%d raw=%s, want 502 with upstream message", code, raw)
	}
	// 限流共享计数：limit=3，/v1/messages 两次 + /chat 一次耗尽。
	srv3, gw3 := newAgentAITestEnv(t, chat, nil)
	token3 := gw3.Register(uuid.New(), uuid.New(), 3)
	plain := `{"messages":[{"role":"user","content":"hi"}]}`
	if code, _, _ := agentAIAnthropic(t, srv3.URL, token3, plain); code != http.StatusOK {
		t.Fatalf("call 1: code=%d, want 200", code)
	}
	if code, _ := agentAIChat(t, srv3.URL, token3, plain); code != http.StatusOK {
		t.Fatalf("cross-endpoint /chat: code=%d, want 200", code)
	}
	if code, _, _ := agentAIAnthropic(t, srv3.URL, token3, plain); code != http.StatusOK {
		t.Fatalf("call 3: code=%d, want 200", code)
	}
	if code, _, raw := agentAIAnthropic(t, srv3.URL, token3, plain); code != http.StatusTooManyRequests || !strings.Contains(raw, "error") {
		t.Fatalf("over limit: code=%d raw=%s, want 429", code, raw)
	}
	// 伪造令牌 401。
	if code, _, _ := agentAIAnthropic(t, srv3.URL, "not-a-token", plain); code != http.StatusUnauthorized {
		t.Fatalf("forged token: code=%d, want 401", code)
	}
}

// TestAgentAIAnthropicIncompatibleTargetDirect 校验 main.go 装配同款
// 目标解析（非 anthropic kind Provider → ErrAgentAIAnthropicIncompatible）
// 经 ai.Service 真链路的行为。
func TestAgentAIAnthropicIncompatibleTargetDirect(t *testing.T) {
	svc := newAIv1Service(100) // mock kind 默认 Provider。
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "ok", "mock1/mock-echo", nil
	}, nil)
	gw.SetAnthropicTarget(func(ctx context.Context, user uuid.UUID) (string, string, string, error) {
		target, err := svc.ForUser(user).ResolveChatTargetFor("", "", settings.AIScenarioChat)
		if err != nil {
			return "", "", "", err
		}
		if target.Provider.Kind != settings.AIKindAnthropic {
			return "", "", "", ErrAgentAIAnthropicIncompatible
		}
		return target.Provider.BaseURL, target.Provider.APIKey, target.Model, nil
	})
	token := gw.Register(uuid.New(), uuid.New(), 5)
	if code, _, raw := agentAIAnthropic(t, srv.URL, token, fmt.Sprintf(agentAIAnthropicClaudeCodeBody, "false")); code != http.StatusBadGateway || !strings.Contains(raw, "anthropic-compatible") {
		t.Fatalf("mock default provider should yield 502: code=%d raw=%s", code, raw)
	}
}
