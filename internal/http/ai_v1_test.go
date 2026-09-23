// Package http —— AI 能力第一版端点测试（/ai/chat SSE、/ai/summarize、
// 管理端设置与限流；mock Provider 全链路）。
package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
)

// newAIv1Service 构造含 mock Provider 的 AI 服务。
func newAIv1Service(perUserPerMin int) *ai.Service {
	return newAIv1ServiceCfg(settings.AIConfig{
		Providers:       []settings.AIProvider{{ID: "mock1", Name: "Mock Provider", Kind: settings.AIKindMock, Model: "mock-echo", Enabled: true}},
		DefaultProvider: "mock1",
		Temperature:     settings.AITemperatureDefault,
		MaxTokens:       settings.AIMaxTokensDefault,
		PerUserPerMin:   perUserPerMin,
	})
}

func newAIv1ServiceCfg(cfg settings.AIConfig) *ai.Service {
	return ai.NewService(func() (settings.AIConfig, error) { return cfg, nil })
}

// newAIv1Router 组装 AI v1 端点测试路由（用户注入 + 可选文件源）。限流
// 在 handler 内按所选 Provider 配置执行（不再挂中间件）。
func newAIv1Router(svc *ai.Service, actor uuid.UUID, src *fakeAIFiles, content string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = &fakeSettingsService{}
	h.aiFiles = src
	if content != "" && src != nil {
		h.storage = newMemStorage()
		if err := h.storage.Put(src.blob.StorageKey, strings.NewReader(content)); err != nil {
			panic(err)
		}
	}
	r := gin.New()
	r.GET("/api/v1/ai/status", h.aiStatus)
	r.GET("/api/v1/ai/models", h.aiModels)
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handle(c)
		}
	}
	r.POST("/api/v1/ai/chat", withUser(h.aiChat))
	r.POST("/api/v1/ai/summarize", withUser(h.aiSummarize))
	r.GET("/api/v1/admin/settings/ai", func(c *gin.Context) { h.getAISettings(c) })
	r.PUT("/api/v1/admin/settings/ai", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, actor)
		h.putAISettings(c)
	})
	r.POST("/api/v1/admin/settings/ai/test", func(c *gin.Context) { h.testAIProvider(c) })
	r.GET("/api/v1/admin/ai/usage", func(c *gin.Context) { h.adminAIUsage(c) })
	return r
}

func postAI(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// sseDeltas 解析 SSE 输出：返回事件名列表与 delta 文本拼接。
func sseDeltas(body string) (events []string, joined string) {
	var deltaText strings.Builder
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if after, ok := strings.CutPrefix(line, "event: "); ok {
				name = after
			} else if after, ok := strings.CutPrefix(line, "data: "); ok {
				data = after
			}
		}
		if name == "" {
			continue
		}
		events = append(events, name)
		if name == "delta" {
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err == nil {
				deltaText.WriteString(payload.Text)
			}
		}
	}
	return events, deltaText.String()
}

// TestAIChatSSEMock 流式：200 + text/event-stream，meta/delta/done 事件齐备。
func TestAIChatSSEMock(t *testing.T) {
	actor := uuid.New()
	r := newAIv1Router(newAIv1Service(100), actor, nil, "")
	w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"介绍 DocFlow"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %s", ct)
	}
	events, joined := sseDeltas(w.Body.String())
	has := func(name string) bool {
		for _, e := range events {
			if e == name {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"meta", "delta", "done"} {
		if !has(want) {
			t.Fatalf("SSE 缺 %q 事件: %v", want, events)
		}
	}
	if !strings.Contains(joined, "介绍 DocFlow") || !strings.Contains(joined, "Mock") {
		t.Fatalf("delta 拼接 = %q", joined)
	}
}

// TestAIChatNonStreamMock 非流式：JSON 返回 content/provider 元信息。
func TestAIChatNonStreamMock(t *testing.T) {
	actor := uuid.New()
	r := newAIv1Router(newAIv1Service(100), actor, nil, "")
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"messages":[{"role":"user","content":"你好"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"content"`, `"provider_id":"mock1"`, `"model":"mock-echo"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q: %s", want, body)
		}
	}
}

// TestAIChatValidation 空 messages 400；非法 role 400；未配 Provider 404
// （AI 未启用 = 网关对客户端不存在，与前端入口隐藏语义一致）。
func TestAIChatValidation(t *testing.T) {
	actor := uuid.New()
	r := newAIv1Router(newAIv1Service(100), actor, nil, "")
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty messages: status = %d", w.Code)
	}
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"bogus","content":"x"}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad role: status = %d", w.Code)
	}
	empty := newAIv1Router(ai.NewService(func() (settings.AIConfig, error) { return settings.DefaultAIConfig(), nil }), actor, nil, "")
	w := postAI(empty, "/api/v1/ai/chat", `{"stream":false,"messages":[{"role":"user","content":"x"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no provider: status = %d body %s", w.Code, w.Body.String())
	}
}

// TestAIStatusAndEnabledGate /ai/status 反映生效开关；总开关显式关闭时
// /ai/chat 404（Provider 仍在配置中）。
func TestAIStatusAndEnabledGate(t *testing.T) {
	actor := uuid.New()
	r := newAIv1Router(newAIv1Service(100), actor, nil, "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/ai/status", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("status enabled: %d %s", w.Code, w.Body.String())
	}
	off := false
	gated := newAIv1Router(ai.NewService(func() (settings.AIConfig, error) {
		cfg := settings.DefaultAIConfig()
		cfg.Enabled = &off
		cfg.Providers = []settings.AIProvider{{ID: "mock1", Kind: settings.AIKindMock, Model: "m", Enabled: true}}
		return cfg, nil
	}), actor, nil, "")
	w2 := httptest.NewRecorder()
	gated.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/ai/status", nil))
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"enabled":false`) {
		t.Fatalf("status disabled: %d %s", w2.Code, w2.Body.String())
	}
	if w3 := postAI(gated, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w3.Code != http.StatusNotFound {
		t.Fatalf("gated chat: status = %d body %s", w3.Code, w3.Body.String())
	}
}

// TestAIChatRAGEmptyHistory RAG 首轮（messages 空、context.query 非空）
// 不应被 messages-required 校验拒绝（「问 AI」入口场景）。
func TestAIChatRAGEmptyHistory(t *testing.T) {
	actor := uuid.New()
	r := newAIv1Router(newAIv1Service(100), actor, nil, "")
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"messages":[],"context":{"query":"DocFlow 是什么"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rag empty history: status = %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"sources"`) {
		t.Fatalf("body 缺 sources: %s", w.Body.String())
	}
}

// TestAIChatRateLimited 每用户限流（ai.per_user_per_min=2）：第 3 次 429。
func TestAIChatRateLimited(t *testing.T) {
	actor := uuid.New()
	r := newAIv1Router(newAIv1Service(2), actor, nil, "")
	for i := 0; i < 2; i++ {
		if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusOK {
			t.Fatalf("第 %d 次不应限流: %d", i+1, w.Code)
		}
	}
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "AI_RATE_LIMITED") {
		t.Fatalf("第三次应 429: %d %s", w.Code, w.Body.String())
	}
	// 其他用户不受影响。
	other := newAIv1Router(newAIv1Service(2), uuid.New(), nil, "")
	if w := postAI(other, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusOK {
		t.Fatalf("独立用户不应被牵连: %d", w.Code)
	}
}

// TestAISummarizeV2 全类型摘要：md 文件 200 + Mock 摘要模板；不存在 404。
func TestAISummarizeV2(t *testing.T) {
	src := newAISource("text/markdown", "报告.md", "available", 20)
	if src.blob.Status != "available" {
		t.Fatal("测试前置失败")
	}
	r := newAIv1Router(newAIv1Service(100), src.owner, src, "# 季度报告\n营收增长 20%")
	w := postAI(r, "/api/v1/ai/summarize", `{"fileId":"`+src.file.ID.String()+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "【Mock 摘要】") || !strings.Contains(body, "报告.md") {
		t.Fatalf("body = %s", body)
	}
	missing := newAIv1Router(newAIv1Service(100), uuid.New(), &fakeAIFiles{owner: uuid.New(), getErr: files.ErrNotFound}, "x")
	w2 := postAI(missing, "/api/v1/ai/summarize", `{"fileId":"`+uuid.New().String()+`"}`)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("missing file: status = %d body %s", w2.Code, w2.Body.String())
	}
}

// TestAISummarizeV2Stream 流式摘要：SSE delta/done。
func TestAISummarizeV2Stream(t *testing.T) {
	src := newAISource("text/plain", "a.txt", "available", 3)
	r := newAIv1Router(newAIv1Service(100), src.owner, src, "hello")
	w := postAI(r, "/api/v1/ai/summarize", `{"fileId":"`+src.file.ID.String()+`","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: delta") || !strings.Contains(body, "event: done") {
		t.Fatalf("SSE 缺事件:\n%s", body)
	}
}

// TestAdminAISettingsCRUD GET 读默认 / PUT 保存 / 测试连接。
func TestAdminAISettingsCRUD(t *testing.T) {
	r := newAIv1Router(newAIv1Service(100), uuid.New(), nil, "")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/ai", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	// PUT：新增一个 openai_compatible Provider（fakeSettingsService 原样返回）。
	w2 := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/ai", strings.NewReader(`{"providers":[{"id":"p1","name":"DeepSeek","kind":"openai_compatible","base_url":"https://api.deepseek.com/v1","api_key":"sk-x","model":"deepseek-chat","enabled":true}],"default_provider":"p1","temperature":0.3,"max_tokens":2048,"per_user_per_min":20}`))
	putReq.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, putReq)
	if w2.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w2.Code, w2.Body.String())
	}
	// 连接测试：mock1 存在 → ok（非流式 ping 走 mock echo）。
	w3 := postAI(r, "/api/v1/admin/settings/ai/test", `{"providerId":"mock1"}`)
	if w3.Code != http.StatusOK || !strings.Contains(w3.Body.String(), `"ok":true`) {
		t.Fatalf("test mock1: %d %s", w3.Code, w3.Body.String())
	}
	// 连接测试：不存在的 Provider → ok=false。
	w4 := postAI(r, "/api/v1/admin/settings/ai/test", `{"providerId":"nope"}`)
	if w4.Code != http.StatusOK || !strings.Contains(w4.Body.String(), `"ok":false`) {
		t.Fatalf("test nope: %d %s", w4.Code, w4.Body.String())
	}
}

// TestAdminAIUsageUnconfigured 未注入用量存储：503。
func TestAdminAIUsageUnconfigured(t *testing.T) {
	r := newAIv1Router(newAIv1Service(100), uuid.New(), nil, "")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/ai/usage", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
}

// aiModelsTestConfig 构造多模型测试配置（含 api_key，验证不泄漏）。
func aiModelsTestConfig() settings.AIConfig {
	return settings.AIConfig{
		Providers: []settings.AIProvider{
			{
				ID: "p1", Name: "DeepSeek", Kind: settings.AIKindOpenAICompatible,
				BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-secret-key", Enabled: true,
				Models: []settings.AIModel{
					{ID: "deepseek-chat", Label: "对话", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
					{ID: "deepseek-emb", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
				},
			},
			{ID: "p2", Name: "Off", Kind: settings.AIKindMock, Model: "off-model", Enabled: false},
		},
		DefaultProvider: "p1",
		DefaultModels:   map[string]settings.AIModelRef{"chat": {ProviderID: "p1", ModelID: "deepseek-chat"}},
		Temperature:     0.3, MaxTokens: 1024, PerUserPerMin: 100,
	}
}

// TestAIModelsNoKeyLeak GET /ai/models：只回启用 Provider 的模型与能力，
// 绝不含 api_key/base_url；停用 Provider 不出现。
func TestAIModelsNoKeyLeak(t *testing.T) {
	r := newAIv1Router(newAIv1ServiceCfg(aiModelsTestConfig()), uuid.New(), nil, "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/ai/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"id":"p1"`, `"deepseek-chat"`, `"kind":"embedding"`, `"default_models"`, `"chat"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q: %s", want, body)
		}
	}
	for _, leak := range []string{"sk-secret-key", "api_key", "base_url", `"p2"`, "off-model"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body 泄漏 %q: %s", leak, body)
		}
	}
}

// TestAIChatModelParamValidAndRejected model 参数：合法（启用 Provider 的
// chat 能力模型）→ 200 且 meta/结果回显所选模型；越权（不属于该 Provider /
// 无 chat 能力 / 未知模型）→ 400 AI_MODEL_NOT_ALLOWED。
func TestAIChatModelParamValidAndRejected(t *testing.T) {
	cfg := aiModelsTestConfig()
	cfg.Providers = append(cfg.Providers, settings.AIProvider{
		ID: "mock1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true,
		Models: []settings.AIModel{
			{ID: "mock-chat", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}},
			{ID: "mock-emb", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindEmbedding}},
		},
	})
	r := newAIv1Router(newAIv1ServiceCfg(cfg), uuid.New(), nil, "")
	// 合法：mock1/mock-chat。
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"providerId":"mock1","modelId":"mock-chat"},"messages":[{"role":"user","content":"你好"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"mock-chat"`) {
		t.Fatalf("valid model: %d %s", w.Code, w.Body.String())
	}
	// 越权 1：模型不属于指定 Provider。
	if w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"providerId":"mock1","modelId":"deepseek-chat"},"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "AI_MODEL_NOT_ALLOWED") {
		t.Fatalf("foreign model: %d %s", w.Code, w.Body.String())
	}
	// 越权 2：模型存在但无 chat 能力。
	if w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"providerId":"mock1","modelId":"mock-emb"},"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "AI_MODEL_NOT_ALLOWED") {
		t.Fatalf("no chat capability: %d %s", w.Code, w.Body.String())
	}
	// 越权 3：未知模型。
	if w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"modelId":"no-such-model"},"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "AI_MODEL_NOT_ALLOWED") {
		t.Fatalf("unknown model: %d %s", w.Code, w.Body.String())
	}
	// 越权 4：停用 Provider 的模型。
	if w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"providerId":"p2","modelId":"off-model"},"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("disabled provider model: %d %s", w.Code, w.Body.String())
	}
}

// TestAIChatProviderRateLimit Provider 级限流：requests_per_min=1 优先于
// 全局（100），第 2 次 429。
func TestAIChatProviderRateLimit(t *testing.T) {
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "mock1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true,
			Models:         []settings.AIModel{{ID: "m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}}},
			RequestsPerMin: 1,
		}},
		DefaultProvider: "mock1", Temperature: 0.3, MaxTokens: 512, PerUserPerMin: 100,
	}
	r := newAIv1Router(newAIv1ServiceCfg(cfg), uuid.New(), nil, "")
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusOK {
		t.Fatalf("第 1 次不应限流: %d %s", w.Code, w.Body.String())
	}
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "AI_RATE_LIMITED") {
		t.Fatalf("Provider 级第 2 次应 429: %d %s", w.Code, w.Body.String())
	}
}

// TestAIChatDailyQuota Provider 级日限额：daily_quota=1，分钟限额宽松时
// 第 2 次仍 429。
func TestAIChatDailyQuota(t *testing.T) {
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "mock1", Name: "Mock", Kind: settings.AIKindMock, Enabled: true,
			Models:         []settings.AIModel{{ID: "m", Capabilities: settings.AIModelCapabilities{Kind: settings.AIModelKindChat}}},
			RequestsPerMin: 100, DailyQuota: 1,
		}},
		DefaultProvider: "mock1", Temperature: 0.3, MaxTokens: 512, PerUserPerMin: 100,
	}
	r := newAIv1Router(newAIv1ServiceCfg(cfg), uuid.New(), nil, "")
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusOK {
		t.Fatalf("第 1 次不应限流: %d %s", w.Code, w.Body.String())
	}
	if w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"x"}]}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("超日限额应 429: %d %s", w.Code, w.Body.String())
	}
}

// ---------- 平台人设（/ai/models 响应 personas / admin ai 视图） ----------

// newAIExtrasRouter 构造带可配置 fake settings 的 AI 端点测试路由（支撑
// 平台人设与记忆注入路径的测试；模式同 newAIv1Router）。
func newAIExtrasRouter(svc *ai.Service, st settingsService, mem aiMemoryStore, actor uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = st
	h.aiMemory = mem
	r := gin.New()
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handle(c)
		}
	}
	r.GET("/api/v1/ai/models", h.aiModels)
	r.POST("/api/v1/ai/chat", withUser(h.aiChat))
	r.GET("/api/v1/admin/settings/ai", h.getAISettings)
	r.PUT("/api/v1/admin/settings/ai", withUser(h.putAISettings))
	return r
}

// TestAIModelsPersonas GET /ai/models：响应含平台 personas（id/name/
// system_prompt 非敏感明文，本人可见）；密钥类字段仍不泄漏。
func TestAIModelsPersonas(t *testing.T) {
	st := &fakeSettingsService{personas: []settings.AIPersonaDef{
		{ID: "plat-fin", Name: "财务分析", SystemPrompt: "你是严谨的财务分析助手"},
	}}
	r := newAIExtrasRouter(newAIv1Service(100), st, nil, uuid.New())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/ai/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"personas"`, "plat-fin", "财务分析", "你是严谨的财务分析助手"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q: %s", want, body)
		}
	}
	// 未配置时 personas 为空数组（非 null）。
	r2 := newAIExtrasRouter(newAIv1Service(100), &fakeSettingsService{}, nil, uuid.New())
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/ai/models", nil))
	if !strings.Contains(w2.Body.String(), `"personas":[]`) {
		t.Fatalf("空配置应回显空数组: %s", w2.Body.String())
	}
}

// TestAdminAISettingsPersonasView GET/PUT /admin/settings/ai：视图回显
// 平台 personas（明文非敏感）；PUT 带 personas 保存后回显、不带则回显
// 空数组（保持现值语义由 settings.Store 的 SetAI 承担，见
// TestSetAIPersonasViaSetAI）。
func TestAdminAISettingsPersonasView(t *testing.T) {
	st := &fakeSettingsService{personas: []settings.AIPersonaDef{
		{ID: "plat-fin", Name: "财务分析", SystemPrompt: "你是严谨的财务分析助手"},
	}}
	r := newAIExtrasRouter(newAIv1Service(100), st, nil, uuid.New())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/ai", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "plat-fin") || !strings.Contains(w.Body.String(), `"personas"`) {
		t.Fatalf("GET 视图应含 personas: %d %s", w.Code, w.Body.String())
	}
	// PUT 带 personas（fakeSettingsService.SetAI 原样返回载荷）。
	put := httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/ai", strings.NewReader(`{"providers":[],"personas":[{"id":"plat-law","name":"法务审校","system_prompt":"你是法务审校助手"}]}`))
	put.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, put)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "plat-law") {
		t.Fatalf("PUT 应回显保存后的 personas: %d %s", w2.Code, w2.Body.String())
	}
	// PUT 不带 personas → 回显空数组（非 null）。
	put2 := httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/ai", strings.NewReader(`{"providers":[]}`))
	put2.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, put2)
	if w3.Code != http.StatusOK || !strings.Contains(w3.Body.String(), `"personas":[]`) {
		t.Fatalf("PUT 未带 personas 应回显空数组: %d %s", w3.Code, w3.Body.String())
	}
}

// ---------- 用户记忆（aiMemoryContextBlock 注入格式 / 条数上限） ----------

// fakeAIMemoryStore 为 aiMemoryStore 的内存实现（items 须按 created_at
// 倒序存放——最新在前，与 UserStore.ListAIMemory 一致）。
type fakeAIMemoryStore struct {
	items []auth.AIMemory
	err   error
}

func (f *fakeAIMemoryStore) ListAIMemory(_ uuid.UUID, limit int) ([]auth.AIMemory, error) {
	if f.err != nil {
		return nil, f.err
	}
	if limit <= 0 || limit > len(f.items) {
		limit = len(f.items)
	}
	return f.items[:limit], nil
}

func (f *fakeAIMemoryStore) CreateAIMemory(_ uuid.UUID, kind, content string) (auth.AIMemory, error) {
	if err := auth.ValidateAIMemory(kind, content); err != nil {
		return auth.AIMemory{}, err
	}
	item := auth.AIMemory{ID: uuid.New(), Kind: auth.AIMemoryKindManual, Content: content}
	f.items = append([]auth.AIMemory{item}, f.items...)
	return item, nil
}

func (f *fakeAIMemoryStore) DeleteAIMemory(_, _ uuid.UUID) (bool, error) { return false, nil }

// UpdateAIMemory 与 UserStore 语义对齐：先校验（失败返回 ErrInvalidAIMemory
// 语义错误、found=false），按 id 匹配更新 content（kind/ID 不变）并回读；
// 不存在 found=false。
func (f *fakeAIMemoryStore) UpdateAIMemory(_ uuid.UUID, id uuid.UUID, content string) (auth.AIMemory, bool, error) {
	if err := auth.ValidateAIMemory(auth.AIMemoryKindManual, content); err != nil {
		return auth.AIMemory{}, false, err
	}
	for i := range f.items {
		if f.items[i].ID == id {
			f.items[i].Content = strings.TrimSpace(content)
			return f.items[i], true, nil
		}
	}
	return auth.AIMemory{}, false, nil
}

// newAIMemoryRouter 组装记忆端点测试路由（用户注入；模式同 newAIv1Router，
// 仅注册被测的 PUT /ai/memory/:id）。
func newAIMemoryRouter(mem aiMemoryStore, actor uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiMemory = mem
	r := gin.New()
	r.PUT("/api/v1/ai/memory/:id", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, actor)
		h.aiMemoryUpdate(c)
	})
	return r
}

// TestAIMemoryUpdate PUT /ai/memory/:id：成功 200 回显 {memory}（kind 不
// 变、存储已更新）；不存在 404；校验失败（超长/空 content）400
// INVALID_AI_MEMORY。
func TestAIMemoryUpdate(t *testing.T) {
	actor := uuid.New()
	id := uuid.New()
	mem := &fakeAIMemoryStore{items: []auth.AIMemory{
		{ID: id, UserID: actor, Kind: auth.AIMemoryKindManual, Content: "旧偏好"},
	}}
	r := newAIMemoryRouter(mem, actor)
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/ai/memory/"+id.String(), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	// 成功：200 + {memory} 回显新 content，kind/ID 不变，fake 存储同步更新。
	w := put(`{"content":"偏好简洁中文回答并附引用来源"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update: status = %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"memory"`, "偏好简洁中文回答并附引用来源", `"kind":"manual"`, `"id":"` + id.String() + `"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q: %s", want, body)
		}
	}
	if got := mem.items[0].Content; got != "偏好简洁中文回答并附引用来源" || mem.items[0].Kind != auth.AIMemoryKindManual {
		t.Fatalf("存储应已更新且 kind 不变: %+v", mem.items[0])
	}
	// 不存在：404。
	missing := httptest.NewRequest(http.MethodPut, "/api/v1/ai/memory/"+uuid.New().String(), strings.NewReader(`{"content":"x"}`))
	missing.Header.Set("Content-Type", "application/json")
	w404 := httptest.NewRecorder()
	r.ServeHTTP(w404, missing)
	if w404.Code != http.StatusNotFound || !strings.Contains(w404.Body.String(), "ai memory not found") {
		t.Fatalf("missing: status = %d body %s", w404.Code, w404.Body.String())
	}
	// 校验失败：超长（>2000 字符）与空 content 均 400 INVALID_AI_MEMORY。
	for name, content := range map[string]string{
		"超长": strings.Repeat("记", auth.AIMemoryMaxContentRunes+1),
		"空":  "",
	} {
		if w := put(`{"content":"` + content + `"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "INVALID_AI_MEMORY") {
			t.Fatalf("%s content: status = %d body %s", name, w.Code, w.Body.String())
		}
	}
	if got := mem.items[0].Content; got != "偏好简洁中文回答并附引用来源" {
		t.Fatalf("校验失败不得改写存储: %q", got)
	}
}

// TestAIMemoryContextBlockTruncation 注入总量截断：4 条 ×1800 字符（合计
// 7200 > 6000）→ 只注入最新 3 条（5400 + 首行说明与前缀 ≤ 阈值），最旧
// 一条丢弃；正序不变；单条超长（正常链路限 2000 不会出现）仍保底注入。
func TestAIMemoryContextBlockTruncation(t *testing.T) {
	items := make([]auth.AIMemory, 4)
	for i := range items { // items[0] 最新（List 倒序），每条恰 1800 rune
		prefix := fmt.Sprintf("记忆%02d:", 3-i)
		items[i] = auth.AIMemory{ID: uuid.New(), Kind: auth.AIMemoryKindManual,
			Content: prefix + strings.Repeat("字", 1800-len([]rune(prefix)))}
	}
	h := &Handler{aiMemory: &fakeAIMemoryStore{items: items}}
	block := h.aiMemoryContextBlock(uuid.New())
	// 只含最新 3 条（记忆03/02/01），最旧的 记忆00 被截断。
	for _, want := range []string{"记忆03", "记忆02", "记忆01"} {
		if !strings.Contains(block, want) {
			t.Fatalf("块 缺 %q（最新 3 条应注入）", want)
		}
	}
	if strings.Contains(block, "记忆00") {
		t.Fatal("超过总量上限的最旧一条不应注入")
	}
	// 正序：记忆01（注入中最旧）在记忆03（最新）之前。
	if strings.Index(block, "记忆01") > strings.Index(block, "记忆03") {
		t.Fatal("截断后仍应按时间正序注入")
	}
	// 总量：块 rune 数 ≤ 阈值 + 首行说明 + 3 条条目前缀（"\n- "）。
	nl := strings.IndexByte(block, '\n')
	if nl <= 0 {
		t.Fatal("块应含首行说明")
	}
	if got := len([]rune(block)); got > aiMemoryContextMaxRunes+len([]rune(block[:nl]))+3*len("\n- ") {
		t.Fatalf("注入块 %d rune 超出阈值（%d + 首行 %d + 前缀）", got, aiMemoryContextMaxRunes, len([]rune(block[:nl])))
	}
	// 保底：单条超长（>阈值）仍注入该条，维持「至少一条」的原样注入逻辑。
	solo := auth.AIMemory{ID: uuid.New(), Kind: auth.AIMemoryKindManual,
		Content: strings.Repeat("超", aiMemoryContextMaxRunes+500)}
	b := (&Handler{aiMemory: &fakeAIMemoryStore{items: []auth.AIMemory{solo}}}).aiMemoryContextBlock(uuid.New())
	if !strings.HasPrefix(b, "以下是用户的长期偏好记忆") || !strings.Contains(b, "超") {
		t.Fatal("单条超长仍应保底注入")
	}
}

// TestAIMemoryContextBlock 注入格式与条数上限：头部说明 + 逐条「- 」列出、
// 取最近 20 条且按时间正序（阅读自然）；无记忆/读取失败/未装配返回空串。
func TestAIMemoryContextBlock(t *testing.T) {
	// 25 条倒序（最新在前）：记忆24 … 记忆00。
	items := make([]auth.AIMemory, 25)
	for i := range items {
		items[i] = auth.AIMemory{ID: uuid.New(), Kind: auth.AIMemoryKindManual, Content: fmt.Sprintf("记忆%02d", 24-i)}
	}
	h := &Handler{aiMemory: &fakeAIMemoryStore{items: items}}
	block := h.aiMemoryContextBlock(uuid.New())
	if !strings.HasPrefix(block, "以下是用户的长期偏好记忆") {
		t.Fatalf("应以头部说明开头: %q", block)
	}
	// 条数上限：最近 20 条（记忆05..记忆24），更旧的（记忆00..记忆04）不注入。
	for _, want := range []string{"\n- 记忆05", "\n- 记忆24"} {
		if !strings.Contains(block, want) {
			t.Fatalf("块 缺 %q: %q", want, block)
		}
	}
	if strings.Contains(block, "记忆04") || strings.Contains(block, "记忆00") {
		t.Fatalf("超过 20 条的旧记忆不应注入: %q", block)
	}
	// 时间正序：记忆05（最旧）在记忆24（最新）之前。
	if strings.Index(block, "记忆05") > strings.Index(block, "记忆24") {
		t.Fatalf("应按时间正序注入: %q", block)
	}

	// 与既有 persona/system 合并：块并入首条 system（appendContextSystem）。
	msgs := appendContextSystem([]ai.Message{{Role: "system", Content: "你是写作助手"}, {Role: "user", Content: "你好"}}, block)
	if len(msgs) != 2 || msgs[0].Role != "system" ||
		!strings.HasPrefix(msgs[0].Content, "你是写作助手") || !strings.Contains(msgs[0].Content, "以下是用户的长期偏好记忆") {
		t.Fatalf("记忆块应并入首条 system（persona 在前、记忆在后）: %+v", msgs)
	}
	// 无 system 时新建首条（不破坏消息顺序）。
	msgs2 := appendContextSystem([]ai.Message{{Role: "user", Content: "你好"}}, block)
	if len(msgs2) != 2 || msgs2[0].Role != "system" || !strings.Contains(msgs2[0].Content, "- 记忆24") {
		t.Fatalf("无 system 时应新建首条: %+v", msgs2)
	}

	// 静默降级：未装配 / 读取失败 / 无记忆。
	if (&Handler{}).aiMemoryContextBlock(uuid.New()) != "" {
		t.Fatal("未装配存储应返回空串")
	}
	hErr := &Handler{aiMemory: &fakeAIMemoryStore{err: errors.New("db down")}}
	if hErr.aiMemoryContextBlock(uuid.New()) != "" {
		t.Fatal("读取失败应返回空串")
	}
	hEmpty := &Handler{aiMemory: &fakeAIMemoryStore{}}
	if hEmpty.aiMemoryContextBlock(uuid.New()) != "" {
		t.Fatal("无记忆应返回空串")
	}
}

// TestAIChatIncludeMemory include_memory=true：记忆注入链路不阻塞对话
// （mock Provider 正常回复）；未开启时不注入（链路等价，不另测）。
func TestAIChatIncludeMemory(t *testing.T) {
	mem := &fakeAIMemoryStore{items: []auth.AIMemory{
		{ID: uuid.New(), Kind: auth.AIMemoryKindManual, Content: "偏好简洁中文回答"},
	}}
	r := newAIExtrasRouter(newAIv1Service(100), &fakeSettingsService{}, mem, uuid.New())
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"include_memory":true,"messages":[{"role":"system","content":"你是写作助手"},{"role":"user","content":"你好"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Mock AI 回复") {
		t.Fatalf("include_memory 对话应正常: %d %s", w.Code, w.Body.String())
	}
}
