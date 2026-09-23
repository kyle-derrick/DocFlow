// Package mcpclient —— 轻客户端单测：JSON 与 SSE 双响应、会话头、
// isError、超时与工具列表缓存。
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMCPServer 构造假 MCP 服务器（Streamable HTTP 单端点）。
// mode="json" 全 JSON 响应；mode="sse" 全 SSE 响应；isError=true 时
// tools/call 返回 isError。记录收到的请求供断言。
type fakeMCPObs struct {
	mu        sync.Mutex
	methods   []string // JSON-RPC method 序列
	sessionIn []string // 非会话请求（initialize 之外）携带的 MCP-Session-Id
	auths     []string
}

func (o *fakeMCPObs) record(method, session, auth string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.methods = append(o.methods, method)
	if method != "initialize" {
		o.sessionIn = append(o.sessionIn, session)
	}
	o.auths = append(o.auths, auth)
}

func fakeMCPServer(t *testing.T, mode string, callIsError bool, sessionHeader string) (*httptest.Server, *fakeMCPObs) {
	t.Helper()
	obs := &fakeMCPObs{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		obs.record(req.Method, r.Header.Get("MCP-Session-Id"), r.Header.Get("Authorization"))
		if got := r.Header.Get("Accept"); !strings.Contains(got, "text/event-stream") {
			t.Errorf("Accept = %q", got)
		}
		write := func(id int, result any) {
			if mode == "sse" {
				w.Header().Set("Content-Type", "text/event-stream")
				data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		}
		switch req.Method {
		case "initialize":
			if sessionHeader != "" {
				w.Header().Set("MCP-Session-Id", sessionHeader)
			}
			if req.ID == nil || *req.ID != 1 {
				t.Errorf("initialize id = %v, want 1", req.ID)
			}
			write(1, map[string]any{"protocolVersion": ProtocolVersion, "serverInfo": map[string]string{"name": "fake"}})
		case "notifications/initialized":
			if req.ID != nil {
				t.Errorf("notification 不应携带 id: %v", *req.ID)
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			write(2, map[string]any{"tools": []Tool{{
				Name: "echo", Description: "回显文本",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
			}}})
		case "tools/call":
			text := "hello from mcp"
			if callIsError {
				write(3, map[string]any{"content": []map[string]string{{"type": "text", "text": "boom"}}, "isError": true})
				return
			}
			write(3, map[string]any{"content": []map[string]string{{"type": "text", "text": text}, {"type": "image", "text": "ignored"}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, obs
}

func TestClientJSONFlowWithSessionAndAuth(t *testing.T) {
	srv, obs := fakeMCPServer(t, "json", false, "sess-123")
	c := &Client{URL: srv.URL, AuthHeader: "Authorization: Bearer tok1"}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" || string(tools[0].InputSchema) == "" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := c.CallTool(context.Background(), "echo", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "hello from mcp" {
		t.Fatalf("out = %q", out)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	want := []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}
	if strings.Join(obs.methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods = %v", obs.methods)
	}
	for i, s := range obs.sessionIn {
		if s != "sess-123" {
			t.Errorf("请求 %d MCP-Session-Id = %q, want sess-123", i, s)
		}
	}
	for i, a := range obs.auths {
		if a != "Bearer tok1" {
			t.Errorf("请求 %d Authorization = %q", i, a)
		}
	}
}

func TestClientSSEResponses(t *testing.T) {
	srv, _ := fakeMCPServer(t, "sse", false, "sse-sess")
	c := &Client{URL: srv.URL}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out, err := c.CallTool(context.Background(), "echo", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "hello from mcp" {
		t.Fatalf("out = %q", out)
	}
}

func TestClientCallToolIsError(t *testing.T) {
	srv, _ := fakeMCPServer(t, "json", true, "")
	c := &Client{URL: srv.URL}
	if _, err := c.CallTool(context.Background(), "echo", nil); err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "执行失败") {
		t.Fatalf("err = %v, want isError 中文错误含工具文本", err)
	}
}

func TestClientHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	_, err := c.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v, want HTTP 非 2xx 中文错误", err)
	}
}

func TestClientRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	// initialize 直接喂 RPC error。
	if _, err := c.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Fatalf("err = %v, want JSON-RPC error 透传", err)
	}
}

func TestClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, HTTP: &http.Client{Timeout: 80 * time.Millisecond}}
	_, err := c.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err = %v, want 中文超时错误", err)
	}
}

func TestListToolsCacheTTL(t *testing.T) {
	srv, obs := fakeMCPServer(t, "json", false, "sess-cache")
	c := &Client{URL: srv.URL}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools#1: %v", err)
	}
	c2 := &Client{URL: srv.URL}
	if _, err := c2.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools#2: %v", err)
	}
	obs.mu.Lock()
	got := strings.Join(obs.methods, ",")
	obs.mu.Unlock()
	if got != "initialize,notifications/initialized,tools/list" {
		t.Fatalf("methods = %q, want 缓存命中不发第二次请求", got)
	}
	// TTL 过期后再取：重新 initialize + tools/list。
	base := time.Now()
	cacheNow = func() time.Time { return base.Add(2 * toolsCacheTTL) }
	defer func() { cacheNow = time.Now }()
	c3 := &Client{URL: srv.URL}
	if _, err := c3.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools#3: %v", err)
	}
	obs.mu.Lock()
	got = strings.Join(obs.methods, ",")
	obs.mu.Unlock()
	if got != "initialize,notifications/initialized,tools/list,initialize,notifications/initialized,tools/list" {
		t.Fatalf("methods = %q, want TTL 过期后重新拉取", got)
	}
}
