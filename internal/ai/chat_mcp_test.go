// Package ai —— MCP 工具循环单测（use_mcp：openai/anthropic 双轮、
// 回归降级、轮次上限）。
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
)

// fakeMCPEcho 构造假 MCP 服务器（单工具 echo，返回 "echo:<city>"），
// callCount 统计 tools/call 次数。
func fakeMCPEcho(t *testing.T, callCount *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		write := func(id int, result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		}
		switch req.Method {
		case "initialize":
			write(*req.ID, map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			write(*req.ID, map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "回显城市天气",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}},
			}}})
		case "tools/call":
			atomic.AddInt32(callCount, 1)
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			var args struct {
				City string `json:"city"`
			}
			_ = json.Unmarshal(params.Arguments, &args)
			write(*req.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": "echo:" + args.City}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// mcpReaderOf 构造返回指定服务列表的 mcpReader。
func mcpReaderOf(svc ...settings.AIMCPServiceDef) func() []settings.AIMCPServiceDef {
	return func() []settings.AIMCPServiceDef { return svc }
}

// captureRequests 记录假上游收到的每个请求 body。
type capturedBodies struct {
	bodies []map[string]any
}

func (c *capturedBodies) add(r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	c.bodies = append(c.bodies, body)
}

// messagesOf 提取请求 body 的 messages（断言用）。
func messagesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	return msgs
}

// sseWrite 写一行 SSE data。
func sseWrite(w http.ResponseWriter, payload string) {
	_, _ = w.Write([]byte("data: " + payload + "\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// fakeOpenAIToolLoop 假 openai 兼容上游：第 1 轮流式返回 tool_calls
// （finish_reason=tool_calls），第 2 轮返回文本。之后各轮恒返回
// toolCallsForever（轮次上限用例）。
func fakeOpenAIToolLoop(t *testing.T, cap *capturedBodies, toolCallsForever bool) *httptest.Server {
	t.Helper()
	var rounds int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r)
		n := atomic.AddInt32(&rounds, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 1 || toolCallsForever {
			sseWrite(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"mcp_test_echo","arguments":"{\"ci"}}]}}]}`)
			sseWrite(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"北京\"}"}}]}}]}`)
			sseWrite(w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			sseWrite(w, `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)
			sseWrite(w, `[DONE]`)
			return
		}
		sseWrite(w, `{"choices":[{"delta":{"content":"北京天气"}}]}`)
		sseWrite(w, `{"choices":[{"delta":{"content":"晴"}}]}`)
		sseWrite(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		sseWrite(w, `{"choices":[],"usage":{"prompt_tokens":22,"completion_tokens":9}}`)
		sseWrite(w, `[DONE]`)
	}))
}

func TestChatOpenAIMCPToolLoop(t *testing.T) {
	var mcpCalls int32
	mcp := fakeMCPEcho(t, &mcpCalls)
	defer mcp.Close()
	cap := &capturedBodies{}
	up := fakeOpenAIToolLoop(t, cap, false)
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	svc.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "test", Name: "测试服务", URL: mcp.URL, Enabled: true}))
	var deltas []string
	var toolEvents []string
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "北京天气如何"}},
		UseMCP:   true, Stream: true,
		OnTool: func(serverID, serverName, toolName string) {
			toolEvents = append(toolEvents, serverID+"|"+serverName+"|"+toolName)
		},
	}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// 最终文本来自第二轮。
	if res.Content != "北京天气晴" {
		t.Fatalf("content = %q", res.Content)
	}
	if strings.Join(deltas, "") != "北京天气晴" {
		t.Fatalf("deltas = %v", deltas)
	}
	// 工具事件：执行前回调（服务 ID/名称/工具名）。
	if len(toolEvents) != 1 || toolEvents[0] != "test|测试服务|echo" {
		t.Fatalf("toolEvents = %v", toolEvents)
	}
	if atomic.LoadInt32(&mcpCalls) != 1 {
		t.Fatalf("mcp calls = %d", mcpCalls)
	}
	if len(cap.bodies) != 2 {
		t.Fatalf("upstream rounds = %d, want 2", len(cap.bodies))
	}
	// 第一轮请求：带 tools 与 tool_choice。
	first := cap.bodies[0]
	toolsJSON, _ := json.Marshal(first["tools"])
	if !strings.Contains(string(toolsJSON), "mcp_test_echo") || first["tool_choice"] != "auto" {
		t.Fatalf("round1 tools/tool_choice 异常: %s %v", toolsJSON, first["tool_choice"])
	}
	// 第二轮 messages：assistant tool_calls + tool 结果回喂。
	msgs := messagesOf(t, cap.bodies[1])
	last := msgs[len(msgs)-1]
	if last["role"] != "tool" || last["tool_call_id"] != "call_1" || last["content"] != "echo:北京" {
		t.Fatalf("round2 tool message = %v", last)
	}
	asst := msgs[len(msgs)-2]
	if asst["role"] != "assistant" {
		t.Fatalf("round2 assistant missing: %v", asst)
	}
	callsJSON, _ := json.Marshal(asst["tool_calls"])
	var asstCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(callsJSON, &asstCalls); err != nil || len(asstCalls) != 1 {
		t.Fatalf("round2 assistant tool_calls = %s", callsJSON)
	}
	if asstCalls[0].ID != "call_1" || asstCalls[0].Function.Name != "mcp_test_echo" {
		t.Fatalf("round2 assistant tool_calls = %s", callsJSON)
	}
	var fnArgsStr string
	var fnArgs struct {
		City string `json:"city"`
	}
	if err := json.Unmarshal(asstCalls[0].Function.Arguments, &fnArgsStr); err != nil ||
		json.Unmarshal([]byte(fnArgsStr), &fnArgs) != nil || fnArgs.City != "北京" {
		t.Fatalf("arguments = %s", asstCalls[0].Function.Arguments)
	}
	// usage 按轮累计（一次对话一条用量记录）。
	if res.PromptTokens != 33 || res.CompletionTokens != 16 {
		t.Fatalf("usage = %d/%d, want 33/16", res.PromptTokens, res.CompletionTokens)
	}
}

// TestChatOpenAIMCPNoToolsRegression 回归：UseMCP=false 或无可用服务时
// 请求不含 tools，与现状完全一致。
func TestChatOpenAIMCPNoToolsRegression(t *testing.T) {
	srv := fakeOpenAISSE(t)
	defer srv.Close()
	cfg := func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: srv.URL, APIKey: "k1", Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	}
	// UseMCP=false。
	svc := NewService(cfg)
	svc.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "test", Name: "n", URL: "http://127.0.0.1:1/mcp", Enabled: true}))
	res, err := svc.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, UseMCP: false, Stream: true}, nil)
	if err != nil || res.Content != "你好，世界" {
		t.Fatalf("UseMCP=false res=%+v err=%v", res, err)
	}
	// UseMCP=true 但未注入 reader（nil）→ 普通对话。
	svc2 := NewService(cfg)
	res, err = svc2.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, UseMCP: true, Stream: true}, nil)
	if err != nil || res.Content != "你好，世界" {
		t.Fatalf("nil reader res=%+v err=%v", res, err)
	}
	// UseMCP=true 但 reader 返回空列表 → 普通对话。
	svc3 := NewService(cfg)
	svc3.SetMCPReader(mcpReaderOf())
	res, err = svc3.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, UseMCP: true, Stream: true}, nil)
	if err != nil || res.Content != "你好，世界" {
		t.Fatalf("empty services res=%+v err=%v", res, err)
	}
	// UseMCP=true 但服务全禁用 → 普通对话。
	svc4 := NewService(cfg)
	svc4.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "test", Name: "n", URL: "http://127.0.0.1:1/mcp", Enabled: false}))
	res, err = svc4.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, UseMCP: true, Stream: true}, nil)
	if err != nil || res.Content != "你好，世界" {
		t.Fatalf("disabled services res=%+v err=%v", res, err)
	}
}

// TestChatOpenAIMCPRoundLimit 轮次上限：上游恒请求工具 → 恰执行
// MCPToolMaxRounds 次后停止并输出提示。
func TestChatOpenAIMCPRoundLimit(t *testing.T) {
	var mcpCalls int32
	mcp := fakeMCPEcho(t, &mcpCalls)
	defer mcp.Close()
	up := fakeOpenAIToolLoop(t, &capturedBodies{}, true)
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	svc.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "test", Name: "测试服务", URL: mcp.URL, Enabled: true}))
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "hi"}}, UseMCP: true, Stream: true,
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := atomic.LoadInt32(&mcpCalls); got != MCPToolMaxRounds {
		t.Fatalf("mcp calls = %d, want %d", got, MCPToolMaxRounds)
	}
	if !strings.Contains(res.Content, "工具调用轮次上限") {
		t.Fatalf("content = %q, want 上限提示", res.Content)
	}
}

// fakeAnthropicToolLoop 假 anthropic 上游：第 1 轮流式返回 text + tool_use
// 块（stop_reason=tool_use），第 2 轮返回纯文本。
func fakeAnthropicToolLoop(t *testing.T, cap *capturedBodies) *httptest.Server {
	t.Helper()
	var rounds int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r)
		n := atomic.AddInt32(&rounds, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			sseWrite(w, `{"type":"message_start","message":{"usage":{"input_tokens":9}}}`)
			sseWrite(w, `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
			sseWrite(w, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我先查一下。"}}`)
			sseWrite(w, `{"type":"content_block_stop","index":0}`)
			sseWrite(w, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_1","name":"mcp_test_echo"}}`)
			sseWrite(w, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
			sseWrite(w, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"上海\"}"}}`)
			sseWrite(w, `{"type":"content_block_stop","index":1}`)
			sseWrite(w, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}`)
			sseWrite(w, `{"type":"message_stop"}`)
			return
		}
		sseWrite(w, `{"type":"message_start","message":{"usage":{"input_tokens":30}}}`)
		sseWrite(w, `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
		sseWrite(w, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"上海多云"}}`)
		sseWrite(w, `{"type":"content_block_stop","index":0}`)
		sseWrite(w, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`)
		sseWrite(w, `{"type":"message_stop"}`)
	}))
}

func TestChatAnthropicMCPToolLoop(t *testing.T) {
	var mcpCalls int32
	mcp := fakeMCPEcho(t, &mcpCalls)
	defer mcp.Close()
	cap := &capturedBodies{}
	up := fakeAnthropicToolLoop(t, cap)
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "a1", Name: "Anthropic", Kind: settings.AIKindAnthropic, BaseURL: up.URL, Model: "claude-3", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	svc.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "test", Name: "测试服务", URL: mcp.URL, Enabled: true}))
	var deltas []string
	var toolEvents []string
	res, err := svc.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "上海天气如何"}},
		UseMCP:   true, Stream: true,
		OnTool: func(serverID, serverName, toolName string) {
			toolEvents = append(toolEvents, serverID+"|"+serverName+"|"+toolName)
		},
	}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// 两轮文本拼接（第一轮模型先说话再调工具）。
	if res.Content != "我先查一下。上海多云" {
		t.Fatalf("content = %q", res.Content)
	}
	if strings.Join(deltas, "") != res.Content {
		t.Fatalf("deltas = %v", deltas)
	}
	if len(toolEvents) != 1 || toolEvents[0] != "test|测试服务|echo" {
		t.Fatalf("toolEvents = %v", toolEvents)
	}
	if atomic.LoadInt32(&mcpCalls) != 1 {
		t.Fatalf("mcp calls = %d", mcpCalls)
	}
	// 第一轮请求带 tools。
	toolsJSON, _ := json.Marshal(cap.bodies[0]["tools"])
	if !strings.Contains(string(toolsJSON), "mcp_test_echo") || !strings.Contains(string(toolsJSON), "input_schema") {
		t.Fatalf("round1 tools = %s", toolsJSON)
	}
	// 第二轮 messages：assistant 原始块 + user tool_result。
	msgs := messagesOf(t, cap.bodies[1])
	asst, usr := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if asst["role"] != "assistant" {
		t.Fatalf("round2 tail-1 role = %v", asst["role"])
	}
	asstBlocks, _ := json.Marshal(asst["content"])
	if !strings.Contains(string(asstBlocks), `"tool_use"`) || !strings.Contains(string(asstBlocks), `"tu_1"`) || !strings.Contains(string(asstBlocks), "我先查一下") {
		t.Fatalf("round2 assistant blocks = %s", asstBlocks)
	}
	if usr["role"] != "user" {
		t.Fatalf("round2 tail role = %v", usr["role"])
	}
	results, _ := json.Marshal(usr["content"])
	if !strings.Contains(string(results), `"tool_use_id":"tu_1"`) || !strings.Contains(string(results), "echo:上海") {
		t.Fatalf("round2 tool_result = %s", results)
	}
	// usage 按轮累计。
	if res.PromptTokens != 39 || res.CompletionTokens != 14 {
		t.Fatalf("usage = %d/%d, want 39/14", res.PromptTokens, res.CompletionTokens)
	}
}

// TestMCPToolFnName 工具名归一化：非法字符 → _、超长截断、前缀。
func TestMCPToolFnName(t *testing.T) {
	if got := mcpToolFnName("test", "echo"); got != "mcp_test_echo" {
		t.Fatalf("fn = %q", got)
	}
	if got := mcpToolFnName("my server", "weather.query"); got != "mcp_my_server_weather_query" {
		t.Fatalf("fn = %q", got)
	}
	long := mcpToolFnName(strings.Repeat("s", 40), strings.Repeat("t", 60))
	if len(long) > mcpToolFnNameMax || !strings.HasPrefix(long, mcpToolFnPrefix) {
		t.Fatalf("fn = %q len=%d", long, len(long))
	}
	// 非法 JSON schema 兜底为 object。
	if got := fmt.Sprint(mcpToolSchema(json.RawMessage(`{bad`))); !strings.Contains(got, "map[type:object]") {
		t.Fatalf("schema fallback = %v", got)
	}
}

// fakeMCPAuthEcho 假 MCP 服务器（单工具 echo），记录收到的认证头（个人
// MCP 多头透传断言用）。
func fakeMCPAuthEcho(t *testing.T, gotAuth, gotKey *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		*gotKey = r.Header.Get("X-Api-Key")
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		respond := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
		}
		switch req.Method {
		case "initialize":
			respond(map[string]any{"protocolVersion": "2025-03-26"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			respond(map[string]any{"tools": []map[string]any{{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object"}}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestMCPCollectPersonalMerged use_mcp 工具收集合并平台+个人服务：个人
// 服务（WithPersonalPrefs 挂载，仅本人对话生效）与平台服务并存；个人服务
// 多认证头透传；平台服务失败跳过不阻塞个人服务。
func TestMCPCollectPersonalMerged(t *testing.T) {
	var gotAuth, gotKey string
	personal := fakeMCPAuthEcho(t, &gotAuth, &gotKey)
	defer personal.Close()

	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: "http://127.0.0.1:1", Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	// 平台 reader：一个不可达服务（失败跳过）——仅剩个人服务的工具可用。
	svc.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "plat", Name: "平台坏站", URL: "http://127.0.0.1:1/mcp", Enabled: true}))
	clone := svc.WithPersonalPrefs(auth.AIPersonalPrefs{
		MCPServers: []auth.AIPersonalMCPServer{{
			ID: "mine", Name: "我的工具站", URL: personal.URL,
			AuthHeaders: map[string]string{"Authorization": "Bearer p-tok", "X-Api-Key": "p-key"},
		}},
	})
	ts := clone.collectMCPTools(context.Background())
	if ts == nil {
		t.Fatal("应收集到个人服务的工具")
	}
	ref, ok := ts.byFn["mcp_mine_echo"]
	if !ok {
		t.Fatalf("缺个人工具 mcp_mine_echo: %v", ts.byFn)
	}
	if ref.service.ID != "mine" || ref.service.Name != "我的工具站" || !ref.service.Enabled {
		t.Fatalf("个人服务定义异常: %+v", ref.service)
	}
	if gotAuth != "Bearer p-tok" || gotKey != "p-key" {
		t.Fatalf("个人认证头未透传: Authorization=%q X-Api-Key=%q", gotAuth, gotKey)
	}

	// 未挂载个人配置（原始 svc）：仅平台坏站 → 零工具 nil（个人服务仅本
	// 人对话生效的负向断言）。
	if ts2 := svc.collectMCPTools(context.Background()); ts2 != nil {
		t.Fatalf("未挂载个人配置不应出现个人工具: %v", ts2.byFn)
	}
}

// TestChatMCPPlatformAndPersonalLoop 全链路：平台服务不可达跳过，个人
// 服务工具进入对话工具循环并被调用（openai 双轮）。
func TestChatMCPPlatformAndPersonalLoop(t *testing.T) {
	var mcpCalls int32
	personal := fakeMCPEcho(t, &mcpCalls)
	defer personal.Close()
	cap := &capturedBodies{}
	up := fakeOpenAIToolLoop(t, cap, false)
	defer up.Close()
	svc := NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, Temperature: 0.3, MaxTokens: 128}, nil
	})
	svc.SetMCPReader(mcpReaderOf(settings.AIMCPServiceDef{ID: "platbad", Name: "平台坏站", URL: "http://127.0.0.1:1/mcp", Enabled: true}))
	clone := svc.WithPersonalPrefs(auth.AIPersonalPrefs{
		// ID=test：fakeOpenAIToolLoop 固定请求 mcp_test_echo。
		MCPServers: []auth.AIPersonalMCPServer{{ID: "test", Name: "我的服务", URL: personal.URL}},
	})
	var toolEvents []string
	res, err := clone.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "北京天气如何"}},
		UseMCP:   true, Stream: true,
		OnTool: func(serverID, serverName, toolName string) {
			toolEvents = append(toolEvents, serverID+"|"+serverName+"|"+toolName)
		},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Content != "北京天气晴" {
		t.Fatalf("content = %q", res.Content)
	}
	if len(toolEvents) != 1 || toolEvents[0] != "test|我的服务|echo" {
		t.Fatalf("toolEvents = %v", toolEvents)
	}
	if atomic.LoadInt32(&mcpCalls) != 1 {
		t.Fatalf("mcp calls = %d", mcpCalls)
	}
}
