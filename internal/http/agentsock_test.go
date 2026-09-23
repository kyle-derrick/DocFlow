// Package http —— agentsock.go 测试：Agent AI IPC 网关的令牌鉴权
// （有效/无效/注销即过期）、每任务限流（limit=2 第三次 429）、/chat 正常
// 返回（注入 chat 回调与 fake AI Provider 即 ai.Service mock）、每次调用
// 写审计（action=agent.ai）与载荷校验（method/role/system 归位/max_tokens）。
// 另覆盖 POST /v1/chat/completions（agentsock_openai.go）：非流式 fake
// provider 全链路、SSE 流式拼装 + [DONE]、tools 请求改走工具直连（未装配
// 目标 503；转发细节见 agentsock_openai_test.go）、401/429 与 /chat
// 共用令牌表与限流计数。
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
)

// newAgentAITestEnv 构造挂测试 server 的网关环境。
func newAgentAITestEnv(t *testing.T, chat AgentAIChatFunc, rec audit.Recorder) (*httptest.Server, *AgentAIGateway) {
	t.Helper()
	gw := NewAgentAIGateway(chat)
	if rec != nil {
		gw.SetAuditRecorder(rec)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv, gw
}

// agentAIChat 发起一次 POST /chat（Bearer token）。
func agentAIChat(t *testing.T, base, token, body string) (int, map[string]string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/chat", bytes.NewReader([]byte(body)))
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
	var out map[string]string
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func agentAIClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

// TestAgentAIChatTokenAuth 令牌鉴权：有效 token 200；伪造 token 401；
// Revoke（任务终态注销）后原 token 立即失效（401）。
func TestAgentAIChatTokenAuth(t *testing.T) {
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "ok", "mock/m", nil
	}, nil)
	task, user := uuid.New(), uuid.New()
	token := gw.Register(task, user, 10)
	if token == "" {
		t.Fatal("register should issue a token")
	}
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	if code, out := agentAIChat(t, srv.URL, token, body); code != http.StatusOK || out["content"] != "ok" {
		t.Fatalf("valid token: code=%d out=%v", code, out)
	}
	if code, _ := agentAIChat(t, srv.URL, "not-a-token", body); code != http.StatusUnauthorized {
		t.Fatalf("forged token: code=%d, want 401", code)
	}
	// 无 Authorization 头同样 401。
	if code, _ := agentAIChat(t, srv.URL, "", body); code != http.StatusUnauthorized {
		t.Fatalf("missing token: code=%d, want 401", code)
	}
	gw.Revoke(task)
	if code, _ := agentAIChat(t, srv.URL, token, body); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: code=%d, want 401", code)
	}
}

// TestAgentAIChatRateLimit 每任务限流：limit=2 时前两次 200、第三次 429。
func TestAgentAIChatRateLimit(t *testing.T) {
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "ok", "mock/m", nil
	}, nil)
	token := gw.Register(uuid.New(), uuid.New(), 2)
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		if code, _ := agentAIChat(t, srv.URL, token, body); code != http.StatusOK {
			t.Fatalf("call %d: code=%d, want 200", i+1, code)
		}
	}
	if code, out := agentAIChat(t, srv.URL, token, body); code != http.StatusTooManyRequests || out["error"] == "" {
		t.Fatalf("third call: code=%d out=%v, want 429 + json error", code, out)
	}
}

// TestAgentAIChatSuccess /chat 正常返回：system 消息归位、user/assistant
// 按序透传、max_tokens 传递，响应含 content 与 "provider/model"。
func TestAgentAIChatSuccess(t *testing.T) {
	var gotSystem string
	var gotMsgs []ai.Message
	var gotMax int
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, system string, messages []ai.Message, maxTokens int, _ func(string)) (string, string, error) {
		gotSystem, gotMsgs, gotMax = system, messages, maxTokens
		return "hello world", "prov/model-x", nil
	}, nil)
	token := gw.Register(uuid.New(), uuid.New(), 5)
	code, out := agentAIChat(t, srv.URL, token, `{"messages":[{"role":"system","content":"BE THE AGENT"},{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}],"max_tokens":777}`)
	if code != http.StatusOK || out["content"] != "hello world" || out["model"] != "prov/model-x" {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if gotSystem != "BE THE AGENT" {
		t.Fatalf("system hoisted = %q", gotSystem)
	}
	if len(gotMsgs) != 3 || gotMsgs[0].Role != "user" || gotMsgs[1].Role != "assistant" || gotMsgs[2].Content != "q2" {
		t.Fatalf("messages = %+v", gotMsgs)
	}
	if gotMax != 777 {
		t.Fatalf("max_tokens = %d", gotMax)
	}
}

// TestAgentAIChatWithFakeProvider 以 ai.Service 的 mock Provider 走完整
// 链路（main.go 同款接线）：/chat 返回 mock 回显与 "mock1/mock-echo"。
func TestAgentAIChatWithFakeProvider(t *testing.T) {
	svc := newAIv1Service(100)
	srv, gw := newAgentAITestEnv(t, func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int, _ func(string)) (string, string, error) {
		res, err := svc.ForUser(user).Chat(ctx, ai.ChatRequest{System: system, Messages: messages, MaxTokens: maxTokens}, nil)
		if err != nil {
			return "", "", err
		}
		return res.Content, res.ProviderID + "/" + res.Model, nil
	}, nil)
	token := gw.Register(uuid.New(), uuid.New(), 5)
	code, out := agentAIChat(t, srv.URL, token, `{"messages":[{"role":"user","content":"build a website"}]}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["model"] != "mock1/mock-echo" || out["content"] == "" {
		t.Fatalf("fake provider roundtrip: %v", out)
	}
}

// TestAgentAIChatAuditAndErrors 审计写入（成功与失败各一条 agent.ai）与
// 错误语义：GET 405、坏 JSON 400、坏 role 400、AI 上游失败 502。
func TestAgentAIChatAuditAndErrors(t *testing.T) {
	rec := &fakeAuditRecorder{}
	chatErr := false
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		if chatErr {
			return "", "", errors.New("upstream boom")
		}
		return "ok", "p/m", nil
	}, rec)
	task, user := uuid.New(), uuid.New()
	token := gw.Register(task, user, 10)
	if code, _ := agentAIChat(t, srv.URL, token, `{"messages":[{"role":"tool","content":"x"}]}`); code != http.StatusBadRequest {
		t.Fatalf("bad role: code=%d, want 400", code)
	}
	if code, _ := agentAIChat(t, srv.URL, token, `{bad json`); code != http.StatusBadRequest {
		t.Fatalf("bad json: code=%d, want 400", code)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/chat", nil)
	if res, err := agentAIClient().Do(req); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /chat should be 405, got err=%v", err)
	}
	if _, out := agentAIChat(t, srv.URL, token, `{"messages":[{"role":"user","content":"q"}]}`); out["content"] != "ok" {
		t.Fatalf("success call: %v", out)
	}
	chatErr = true
	if code, out := agentAIChat(t, srv.URL, token, `{"messages":[{"role":"user","content":"q"}]}`); code != http.StatusBadGateway || out["error"] == "" {
		t.Fatalf("upstream failure: code=%d out=%v, want 502 + json error", code, out)
	}
	entries := rec.snapshot()
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d, want 2 (success + failure)", len(entries))
	}
	for _, e := range entries {
		if e.Action != ActionAgentAI || e.ResourceType != audit.ResourceAI || e.ResourceID != task.String() || e.UserID == nil || *e.UserID != user {
			t.Fatalf("audit entry mismatch: %+v", e)
		}
	}
	if entries[0].Status != audit.StatusSuccess || entries[1].Status != audit.StatusFailure {
		t.Fatalf("audit statuses = %q/%q", entries[0].Status, entries[1].Status)
	}
}

// ---------- POST /v1/chat/completions（OpenAI 协议兼容子集） ----------

// agentAIOpenAI 发起一次 POST /v1/chat/completions（Bearer token），返回
// 状态码、响应头与原始响应体。
func agentAIOpenAI(t *testing.T, base, token, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader([]byte(body)))
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

// TestAgentAIOpenAIChatCompletionsNonStream 非流式全链路（fake provider）：
// 请求 model/temperature 忽略（实际模型为平台默认 mock1/mock-echo）、
// system 归位与 max_tokens 透传、响应信封（id=chatcmpl-<taskID>、
// object/choices/finish_reason/usage 占位）与审计（endpoint=openai）。
func TestAgentAIOpenAIChatCompletionsNonStream(t *testing.T) {
	rec := &fakeAuditRecorder{}
	var gotSystem string
	var gotMsgs []ai.Message
	var gotMax int
	svc := newAIv1Service(100)
	srv, gw := newAgentAITestEnv(t, func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int, onDelta func(string)) (string, string, error) {
		gotSystem, gotMsgs, gotMax = system, messages, maxTokens
		res, err := svc.ForUser(user).Chat(ctx, ai.ChatRequest{System: system, Messages: messages, MaxTokens: maxTokens, Stream: onDelta != nil}, onDelta)
		if err != nil {
			return "", "", err
		}
		return res.Content, res.ProviderID + "/" + res.Model, nil
	}, rec)
	task, user := uuid.New(), uuid.New()
	token := gw.Register(task, user, 5)
	code, hdr, body := agentAIOpenAI(t, srv.URL, token, `{"model":"gpt-5","messages":[{"role":"system","content":"BE THE AGENT"},{"role":"user","content":"build a website"}],"temperature":0.9,"max_tokens":321}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	if out.ID != "chatcmpl-"+task.String() || out.Object != "chat.completion" || out.Created == 0 {
		t.Fatalf("envelope mismatch: %+v", out)
	}
	if out.Model != "mock1/mock-echo" {
		t.Fatalf("model = %q, want platform mock1/mock-echo (request model ignored)", out.Model)
	}
	if len(out.Choices) != 1 || out.Choices[0].Index != 0 || out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" || out.Choices[0].Message.Content == "" {
		t.Fatalf("choices mismatch: %+v", out.Choices)
	}
	if out.Usage.PromptTokens != 0 || out.Usage.CompletionTokens != 0 || out.Usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v, want zero placeholder", out.Usage)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if gotSystem != "BE THE AGENT" || len(gotMsgs) != 1 || gotMsgs[0].Role != "user" || gotMsgs[0].Content != "build a website" || gotMax != 321 {
		t.Fatalf("engine call mismatch: system=%q msgs=%+v max=%d", gotSystem, gotMsgs, gotMax)
	}
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Action != ActionAgentAI || e.ResourceType != audit.ResourceAI || e.ResourceID != task.String() || e.UserID == nil || *e.UserID != user || !strings.Contains(e.Metadata, `"endpoint":"openai"`) {
		t.Fatalf("audit entry mismatch: %+v", e)
	}
}

// TestAgentAIOpenAIChatCompletionsStream 流式（fake provider 逐 delta）：
// SSE 分块拼装等于引擎全文、首块 role 序幕、收尾块 delta 空 +
// finish_reason=stop、末尾 data: [DONE]、Content-Type 为 event-stream。
func TestAgentAIOpenAIChatCompletionsStream(t *testing.T) {
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
	code, hdr, body := agentAIOpenAI(t, srv.URL, token, `{"model":"x-echo","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", code, body)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	var payloads []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			payloads = append(payloads, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(payloads) < 4 { // role 序幕 + 若干增量 + stop 块 + [DONE]
		t.Fatalf("payloads = %d, body=%q", len(payloads), body)
	}
	if payloads[len(payloads)-1] != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", payloads[len(payloads)-1])
	}
	type chunkMsg struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	chunks := payloads[:len(payloads)-1]
	var assembled strings.Builder
	for i, p := range chunks {
		var c chunkMsg
		if err := json.Unmarshal([]byte(p), &c); err != nil {
			t.Fatalf("chunk %d decode: %v (%s)", i, err, p)
		}
		if c.ID != "chatcmpl-"+task.String() || c.Object != "chat.completion.chunk" || c.Model != "x-echo" || len(c.Choices) != 1 {
			t.Fatalf("chunk %d envelope mismatch: %+v", i, c)
		}
		if i == len(chunks)-1 { // 收尾块：delta 空 + finish_reason=stop。
			if c.Choices[0].FinishReason == nil || *c.Choices[0].FinishReason != "stop" || c.Choices[0].Delta.Role != "" || c.Choices[0].Delta.Content != "" {
				t.Fatalf("final chunk mismatch: %+v", c)
			}
			continue
		}
		if c.Choices[0].FinishReason != nil {
			t.Fatalf("chunk %d premature finish_reason: %+v", i, c)
		}
		assembled.WriteString(c.Choices[0].Delta.Content)
	}
	if assembled.String() != full {
		t.Fatalf("assembled stream = %q, want engine content %q", assembled.String(), full)
	}
	var first chunkMsg
	if err := json.Unmarshal([]byte(chunks[0]), &first); err != nil || first.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk should carry delta.role=assistant: %v %+v", err, first)
	}
}

// TestAgentAIOpenAIChatCompletionsErrors 错误语义：伪造令牌 401、GET
// 405、tools 400（明确错误；tools:null 放行）、限流与 /chat 共用同一
// calls 计数（limit=3：/chat 一次 + openai 两次后第三次 429）。
func TestAgentAIOpenAIChatCompletionsErrors(t *testing.T) {
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int, _ func(string)) (string, string, error) {
		return "ok", "mock/m", nil
	}, nil)
	token := gw.Register(uuid.New(), uuid.New(), 3)
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	if code, _, _ := agentAIOpenAI(t, srv.URL, "not-a-token", body); code != http.StatusUnauthorized {
		t.Fatalf("forged token: code=%d, want 401", code)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/chat/completions", nil)
	if res, err := agentAIClient().Do(req); err != nil || res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/chat/completions should be 405, got err=%v", err)
	}
	// tools 请求改走工具直连转发（未装配 openai 目标 → 503）。
	toolsBody := `{"messages":[{"role":"user","content":"hi"}],"stream":true,"tools":[{"type":"function","function":{"name":"shell","parameters":{"type":"object"}}}]}`
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, toolsBody); code != http.StatusServiceUnavailable || !strings.Contains(raw, "passthrough") {
		t.Fatalf("tools request without target: code=%d raw=%s, want 503 (forward path unconfigured)", code, raw)
	}
	// tools:null 不视为携带工具（部分客户端显式置 null）。
	if code, _, _ := agentAIOpenAI(t, srv.URL, token, `{"messages":[{"role":"user","content":"hi"}],"tools":null}`); code != http.StatusOK {
		t.Fatalf("tools:null should pass, got %d", code)
	}
	// 与 /chat 共用计数：openai 已耗 2，/chat 为第 3 次（200），此后 429。
	if code, _ := agentAIChat(t, srv.URL, token, body); code != http.StatusOK {
		t.Fatalf("cross-endpoint /chat call: code=%d, want 200", code)
	}
	if code, _, raw := agentAIOpenAI(t, srv.URL, token, body); code != http.StatusTooManyRequests || !strings.Contains(raw, "error") {
		t.Fatalf("over limit: code=%d raw=%s, want 429 + json error", code, raw)
	}
}
