// Package http —— 外部 MCP 端点与 use_mcp 对话链路测试（/ai/mcp、
// /admin/settings/ai/mcp、/admin/settings/ai/mcp/test、aiChat SSE tool 事件）。
package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
)

// fakeMCPSettings 为 fakeSettingsService 的 ai.mcp 扩展（嵌入提升其余
// settingsService 方法；字段/方法定义在本文件，避免并行改动 admin_test.go）。
type fakeMCPSettings struct {
	*fakeSettingsService
	mcpServices []settings.AIMCPServiceDef
}

func (f *fakeMCPSettings) AIMCPServices() ([]settings.AIMCPServiceDef, error) {
	if f.mcpServices == nil {
		return []settings.AIMCPServiceDef{}, nil
	}
	return f.mcpServices, nil
}

func (f *fakeMCPSettings) SetAIMCPServices(list []settings.AIMCPServiceDef, _ uuid.UUID) ([]settings.AIMCPServiceDef, error) {
	if err := settings.ValidateAIMCPServices(list); err != nil {
		return nil, err
	}
	f.mcpServices = list
	return list, nil
}

// newMCPRouter 组装 MCP 端点测试路由（登录用户注入 + 可选 AI 服务）。
func newMCPRouter(actor uuid.UUID, svc *ai.Service, fs settingsService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = fs
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handle(c)
		}
	}
	r := gin.New()
	r.GET("/api/v1/ai/mcp", withUser(h.aiMCPList))
	r.POST("/api/v1/ai/chat", withUser(h.aiChat))
	r.GET("/api/v1/admin/settings/ai/mcp", withUser(h.adminGetAIMCPServices))
	r.PUT("/api/v1/admin/settings/ai/mcp", withUser(h.adminPutAIMCPServices))
	r.POST("/api/v1/admin/settings/ai/mcp/test", withUser(h.adminTestAIMCP))
	return r
}

func TestAIMCPListEndpoint(t *testing.T) {
	actor := uuid.New()
	fs := &fakeMCPSettings{fakeSettingsService: &fakeSettingsService{}, mcpServices: []settings.AIMCPServiceDef{
		{ID: "s1", Name: "启用服务", URL: "http://a/mcp", AuthHeader: "Authorization: Bearer x", Enabled: true},
		{ID: "s2", Name: "停用服务", URL: "http://b/mcp", Enabled: false},
	}}
	r := newMCPRouter(actor, nil, fs)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ai/mcp", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"id":"s1"`) || !strings.Contains(body, `"name":"启用服务"`) {
		t.Fatalf("body = %s", body)
	}
	if strings.Contains(body, "s2") || strings.Contains(body, "http://a/mcp") || strings.Contains(body, "Bearer") {
		t.Fatalf("不应泄露停用项/URL/凭据: %s", body)
	}
}

func TestAdminAIMCPEndpoints(t *testing.T) {
	actor := uuid.New()
	fs := &fakeMCPSettings{fakeSettingsService: &fakeSettingsService{}}
	r := newMCPRouter(actor, nil, fs)
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/ai/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	// PUT：写入 + auth_header 掩码回显。
	w := put(`{"services":[{"id":"s1","name":"服务一","url":"https://mcp.example.com/mcp","auth_header":"Authorization: Bearer secret","enabled":true}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), `"auth_header_configured":true`) {
		t.Fatalf("PUT 回显应掩码: %s", w.Body.String())
	}
	// GET：auth_header_configured 布尔。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/ai/mcp", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"url":"https://mcp.example.com/mcp"`) {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
	}
	// 非法载荷 400 INVALID_AI_MCP。
	w = put(`{"services":[{"id":"bad","name":"n","url":"notaurl"}]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_AI_MCP") {
		t.Fatalf("bad PUT status=%d body=%s", w.Code, w.Body.String())
	}
}

// mcpTestResponse /admin/settings/ai/mcp/test 的响应结构。
type mcpTestResponse struct {
	OK        bool     `json:"ok"`
	Tools     int      `json:"tools"`
	Names     []string `json:"names"`
	LatencyMS int64    `json:"latency_ms"`
	Error     string   `json:"error"`
}

// postMCPTest 向 newMCPRouter 发起 POST /admin/settings/ai/mcp/test。
func postMCPTest(r *gin.Engine, body string) (int, mcpTestResponse) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/settings/ai/mcp/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var res mcpTestResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res
}

// TestAdminAIMCPTestEndpoint 成功（ok/tools/names/latency_ms）与失败
//（上游 500、拒绝连接）路径：失败仍 200，ok=false + 中文 error 非空。
// 超时路径（10s 总预算）不单独覆盖：拒绝连接同为 mcpclient 传输层错误。
func TestAdminAIMCPTestEndpoint(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(fakeMCPHTTPEcho))
	defer mcp.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	r := newMCPRouter(uuid.New(), nil, &fakeMCPSettings{fakeSettingsService: &fakeSettingsService{}})

	code, res := postMCPTest(r, `{"url":"`+mcp.URL+`","auth_header":"Authorization: Bearer t"}`)
	if code != http.StatusOK || !res.OK || res.Tools != 1 || len(res.Names) != 1 || res.Names[0] != "echo" {
		t.Fatalf("ok case code=%d res=%+v", code, res)
	}
	if res.LatencyMS <= 0 {
		t.Fatalf("latency_ms = %d, want > 0", res.LatencyMS)
	}

	code, res = postMCPTest(r, `{"url":"`+broken.URL+`"}`)
	if code != http.StatusOK || res.OK || res.Error == "" {
		t.Fatalf("500 case code=%d res=%+v", code, res)
	}
	// 上游拒绝连接（端口未监听）：仍 200，ok=false + error 非空。
	code, res = postMCPTest(r, `{"url":"http://127.0.0.1:1/mcp"}`)
	if code != http.StatusOK || res.OK || res.Error == "" {
		t.Fatalf("refused case code=%d res=%+v", code, res)
	}
	// 空 url 400（载荷错误与连接失败区分开）。
	if code, _ = postMCPTest(r, `{"url":"  "}`); code != http.StatusBadRequest {
		t.Fatalf("empty url status = %d", code)
	}
}

// mcpRoleLookup 内存角色表（auth.RoleLookup 最小实现，403 用例用）。
type mcpRoleLookup struct{ roles map[uuid.UUID]string }

func (f *mcpRoleLookup) Role(id uuid.UUID) (string, error) {
	if role, ok := f.roles[id]; ok {
		return role, nil
	}
	return "", auth.ErrUserNotFound
}

// TestAdminAIMCPTestForbidden 非 admin 角色经 RequireRole 拦截 403，
// admin 放行（到达 handler，坏 URL 返回 200 ok=false）。
func TestAdminAIMCPTestForbidden(t *testing.T) {
	adminID, userID := uuid.New(), uuid.New()
	lookup := &mcpRoleLookup{roles: map[uuid.UUID]string{adminID: auth.RoleAdmin, userID: auth.RoleUser}}
	build := func(uid uuid.UUID) *gin.Engine {
		gin.SetMode(gin.TestMode)
		h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
		rg := gin.New()
		grp := rg.Group("/api/v1/admin/settings/ai", func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, uid)
		}, auth.RequireRole(auth.RoleAdmin, lookup))
		grp.POST("/mcp/test", h.adminTestAIMCP)
		return rg
	}
	code, _ := postMCPTest(build(userID), `{"url":"http://127.0.0.1:1/mcp"}`)
	if code != http.StatusForbidden {
		t.Fatalf("user status = %d, want 403", code)
	}
	code, res := postMCPTest(build(adminID), `{"url":"http://127.0.0.1:1/mcp"}`)
	if code != http.StatusOK || res.OK {
		t.Fatalf("admin status=%d res=%+v, want 200 ok=false", code, res)
	}
}

// fakeMCPHTTPEcho 简化假 MCP 服务器（echo 工具）。
func fakeMCPHTTPEcho(w http.ResponseWriter, r *http.Request) {
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
			"name": "echo", "description": "回显",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}}})
	case "tools/call":
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
}

// fakeOpenAIMCPLoop 假 openai 兼容上游：第 1 轮流式 tool_calls、第 2 轮文本。
func fakeOpenAIMCPLoop(w http.ResponseWriter, r *http.Request, round *int) {
	*round++
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	sse := func(payload string) { _, _ = w.Write([]byte("data: " + payload + "\n\n")) }
	if *round == 1 {
		sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"mcp_test_echo","arguments":"{\"city\":\"北京\"}"}}]}}]}`)
		sse(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		sse(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5}}`)
		sse(`[DONE]`)
		return
	}
	sse(`{"choices":[{"delta":{"content":"北京晴"}}]}`)
	sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	sse(`{"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":6}}`)
	sse(`[DONE]`)
}

// TestAIChatUseMCPSSE e2e：use_mcp=true 时 SSE 序列含 tool 事件（执行前
// 下发）与最终 delta 文本；use_mcp 缺省时无 tool 事件。
func TestAIChatUseMCPSSE(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(fakeMCPHTTPEcho))
	defer mcp.Close()
	round := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fakeOpenAIMCPLoop(w, r, &round)
	}))
	defer up.Close()
	svc := ai.NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, DefaultProvider: "o1", Temperature: 0.3, MaxTokens: 128, PerUserPerMin: 100}, nil
	})
	svc.SetMCPReader(func() []settings.AIMCPServiceDef {
		return []settings.AIMCPServiceDef{{ID: "test", Name: "测试服务", URL: mcp.URL, Enabled: true}}
	})
	actor := uuid.New()
	fs := &fakeMCPSettings{fakeSettingsService: &fakeSettingsService{}}
	r := newMCPRouter(actor, svc, fs)

	w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"北京天气"}],"use_mcp":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// tool 事件（执行前）：type/label/server/tool 冻结格式。
	if !strings.Contains(body, "event: tool") || !strings.Contains(body, `"type":"tool"`) ||
		!strings.Contains(body, `"label":"测试服务 / echo"`) || !strings.Contains(body, `"server":"test"`) || !strings.Contains(body, `"tool":"echo"`) {
		t.Fatalf("tool 事件缺失: %s", body)
	}
	events, joined := sseDeltas(body)
	has := func(name string) bool {
		for _, e := range events {
			if e == name {
				return true
			}
		}
		return false
	}
	if !has("meta") || !has("delta") || !has("done") || !has("tool") {
		t.Fatalf("events = %v", events)
	}
	if joined != "北京晴" {
		t.Fatalf("delta joined = %q", joined)
	}
	// tool 事件先于最终 delta。
	if strings.Index(body, "event: tool") > strings.Index(body, "event: delta") {
		t.Fatalf("tool 事件应在 delta 之前: %s", body)
	}
	// 第二轮上游请求含 role:tool 结果（fakeOpenAIMCPLoop 单 handler 共享
	// round 计数：两次 POST 已隐式验证；这里仅核对 round==2）。
	if round != 2 {
		t.Fatalf("upstream rounds = %d", round)
	}
}

// TestAIChatUseMCPDisabled 回归：use_mcp 缺省（false）时无 tool 事件、
// 上游单轮（不含 tools）。
func TestAIChatUseMCPDisabled(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(fakeMCPHTTPEcho))
	defer mcp.Close()
	// round 从 1 起步 = 直接命中第 2 轮文本分支（普通对话路径无工具轮）。
	round := 1
	var sawTools bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, sawTools = body["tools"]
		fakeOpenAIMCPLoop(w, r, &round)
	}))
	defer up.Close()
	svc := ai.NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, DefaultProvider: "o1", Temperature: 0.3, MaxTokens: 128, PerUserPerMin: 100}, nil
	})
	svc.SetMCPReader(func() []settings.AIMCPServiceDef {
		return []settings.AIMCPServiceDef{{ID: "test", Name: "测试服务", URL: mcp.URL, Enabled: true}}
	})
	r := newMCPRouter(uuid.New(), svc, &fakeMCPSettings{fakeSettingsService: &fakeSettingsService{}})
	w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"北京天气"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "event: tool") || sawTools {
		t.Fatalf("use_mcp 缺省不应触发工具循环: %s", w.Body.String())
	}
	_, joined := sseDeltas(w.Body.String())
	if joined != "北京晴" {
		t.Fatalf("delta joined = %q", joined)
	}
}
