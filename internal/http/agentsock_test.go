// Package http —— agentsock.go 测试：Agent AI IPC 网关的令牌鉴权
// （有效/无效/注销即过期）、每任务限流（limit=2 第三次 429）、/chat 正常
// 返回（注入 chat 回调与 fake AI Provider 即 ai.Service mock）、每次调用
// 写审计（action=agent.ai）与载荷校验（method/role/system 归位/max_tokens）。
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int) (string, string, error) {
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
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int) (string, string, error) {
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
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, system string, messages []ai.Message, maxTokens int) (string, string, error) {
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
	srv, gw := newAgentAITestEnv(t, func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int) (string, string, error) {
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
	srv, gw := newAgentAITestEnv(t, func(_ context.Context, _ uuid.UUID, _ string, _ []ai.Message, _ int) (string, string, error) {
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
