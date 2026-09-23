// ai_personal_test.go：个人 AI 配置端点测试（GET/PUT 掩码与继承、校验
// 400 分支、/ai/models personal 合并、显式个人解析全链路、限流豁免）。
package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
)

// fakeAIPrefsStore 为 aiPrefsStore 的内存实现（整块 JSON 存取）。
type fakeAIPrefsStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]json.RawMessage
}

func newFakeAIPrefsStore() *fakeAIPrefsStore {
	return &fakeAIPrefsStore{rows: map[uuid.UUID]json.RawMessage{}}
}

func (f *fakeAIPrefsStore) GetAIPrefs(userID uuid.UUID) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if raw, ok := f.rows[userID]; ok {
		return json.RawMessage(append([]byte(nil), raw...)), nil
	}
	return nil, nil
}

func (f *fakeAIPrefsStore) SetAIPrefs(userID uuid.UUID, prefs json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[userID] = append([]byte(nil), prefs...)
	return nil
}

// personalUpstream 构造 OpenAI 兼容假上游：记录 Authorization 与模型，
// 返回固定补全。
type personalUpstream struct {
	mu   sync.Mutex
	auth []string
	srv  *httptest.Server
}

func newPersonalUpstream() *personalUpstream {
	u := &personalUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"个人池回答"}}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`))
	}))
	return u
}

func (u *personalUpstream) lastAuth() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.auth) == 0 {
		return ""
	}
	return u.auth[len(u.auth)-1]
}

// personalPrefsPayload 构造含个人 Provider 的合法 PUT 载荷。
func personalPrefsPayload(baseURL string) string {
	return `{"providers":[{"id":"mine","name":"我的网关","kind":"openai_compatible","base_url":"` + baseURL + `","api_key":"sk-personal-1","models":[{"id":"my-chat","capabilities":{"chat":true}},{"id":"my-emb","capabilities":{"embedding":true}}]}],"default_models":{"chat":{"provider_id":"mine","model_id":"my-chat"}},"personas":[{"id":"writer","name":"写作助手","system_prompt":"你是写作助手"}],"prefer_personal":true}`
}

// newAIPersonalRouter 组装个人 AI 配置端点测试路由（store 注入 + 用户注入）。
func newAIPersonalRouter(svc *ai.Service, store *fakeAIPrefsStore, actor uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = &fakeSettingsService{}
	h.aiPrefs = store
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handle(c)
		}
	}
	r := gin.New()
	r.GET("/api/v1/ai/models", withUser(h.aiModels))
	r.GET("/api/v1/ai/personal-settings", withUser(h.getAIPersonalSettings))
	r.PUT("/api/v1/ai/personal-settings", withUser(h.putAIPersonalSettings))
	r.POST("/api/v1/ai/chat", withUser(h.aiChat))
	r.POST("/api/v1/ai/summarize", withUser(h.aiSummarize))
	return r
}

func personalJSON(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestAIPersonalSettingsRoundtripAndMask GET 空态 → PUT 保存 → GET 掩码
// 回读：api_key 绝不回显（仅 api_key_configured），其余字段往返一致。
func TestAIPersonalSettingsRoundtripAndMask(t *testing.T) {
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalRouter(newAIv1Service(100), store, actor)

	// 空态：掩码空结构（非 404/503）。
	w := personalJSON(r, http.MethodGet, "/api/v1/ai/personal-settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("空态 GET: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"providers":[]`) {
		t.Fatalf("空态应返回空 providers: %s", w.Body.String())
	}

	// PUT 保存。
	w2 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload("https://gw.example.com/v1"))
	if w2.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w2.Code, w2.Body.String())
	}
	for _, leak := range []string{"sk-personal-1", `"api_key"`} {
		if strings.Contains(w2.Body.String(), leak) {
			t.Fatalf("PUT 响应泄漏 api_key（%q）: %s", leak, w2.Body.String())
		}
	}
	if !strings.Contains(w2.Body.String(), `"api_key_configured":true`) {
		t.Fatalf("PUT 响应缺 api_key_configured: %s", w2.Body.String())
	}

	// GET 回读：掩码视图。
	w3 := personalJSON(r, http.MethodGet, "/api/v1/ai/personal-settings", "")
	if w3.Code != http.StatusOK {
		t.Fatalf("GET 回读: %d", w3.Code)
	}
	body := w3.Body.String()
	if strings.Contains(body, "sk-personal-1") || strings.Contains(body, `"api_key":"`) {
		t.Fatalf("GET 回读泄漏 api_key: %s", body)
	}
	for _, want := range []string{`"id":"mine"`, `"api_key_configured":true`, `"prefer_personal":true`, `"writer"`, `"my-chat"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("GET 回读缺 %q: %s", want, body)
		}
	}
}

// TestAIPersonalSettingsAPIKeyInherit PUT 时 provider api_key 留空 = 继承
// 现值（按 Provider ID 服务端合并）：库中仍保留原 key（掩码 configured 恒 true）。
func TestAIPersonalSettingsAPIKeyInherit(t *testing.T) {
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalRouter(newAIv1Service(100), store, actor)
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload("https://gw.example.com/v1")); w.Code != http.StatusOK {
		t.Fatalf("首次 PUT: %d %s", w.Code, w.Body.String())
	}
	// 二次 PUT：不携带 api_key（掩码回读视图天然无 key 字段）。
	noKey := `{"providers":[{"id":"mine","name":"改名网关","kind":"openai_compatible","base_url":"https://gw2.example.com/v1","models":[{"id":"my-chat","capabilities":{"chat":true}}]}],"prefer_personal":false}`
	w2 := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", noKey)
	if w2.Code != http.StatusOK {
		t.Fatalf("继承 PUT: %d %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `"api_key_configured":true`) {
		t.Fatalf("留空应继承现值（configured=true）: %s", w2.Body.String())
	}
	// 底层存储仍含原 key。
	raw, _ := store.GetAIPrefs(actor)
	if !strings.Contains(string(raw), "sk-personal-1") {
		t.Fatalf("底层存储丢失继承 key: %s", string(raw))
	}
}

// TestAIPersonalSettingsValidationBranches PUT 校验失败分支：非法 JSON、
// kind 非法、base_url 非法、default_models 引用越权、超上限——400 且
// code=INVALID_AI_PREFS、error 含字段定位。
func TestAIPersonalSettingsValidationBranches(t *testing.T) {
	actor := uuid.New()
	r := newAIPersonalRouter(newAIv1Service(100), newFakeAIPrefsStore(), actor)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"非法 JSON", `{"providers":`, "invalid request"},
		{"kind 非法", `{"providers":[{"id":"a","name":"n","kind":"mock","base_url":"https://x.example.com","models":[{"id":"m","capabilities":{"chat":true}}]}]}`, "kind"},
		{"base_url 非法", `{"providers":[{"id":"a","name":"n","kind":"anthropic","base_url":"not-a-url","models":[{"id":"m","capabilities":{"chat":true}}]}]}`, "base_url"},
		{"默认引用越权", `{"providers":[{"id":"a","name":"n","kind":"anthropic","base_url":"https://x.example.com","models":[{"id":"m","capabilities":{"chat":true}}]}],"default_models":{"chat":{"provider_id":"nope","model_id":"m"}}}`, "provider_id"},
		{"场景键非法", `{"providers":[{"id":"a","name":"n","kind":"anthropic","base_url":"https://x.example.com","models":[{"id":"m","capabilities":{"chat":true}}]}],"default_models":{"other":{"provider_id":"a","model_id":"m"}}}`, "未知场景"},
		{"persona 缺名称", `{"providers":[{"id":"a","name":"n","kind":"anthropic","base_url":"https://x.example.com","models":[{"id":"m","capabilities":{"chat":true}}]}],"personas":[{"id":"p","name":"","system_prompt":"s"}]}`, "personas[0].name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body %s 应含 %q", w.Body.String(), tc.want)
			}
		})
	}
}

// TestAIModelsPersonalMerged /ai/models：个人池条目追加（personal:true +
// 名后缀「（个人）」），api_key 不泄漏；未配置个人池时与原响应一致。
func TestAIModelsPersonalMerged(t *testing.T) {
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalRouter(newAIv1Service(100), store, actor)
	// 未配置：仅平台 mock1。
	w := personalJSON(r, http.MethodGet, "/api/v1/ai/models", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "personal") {
		t.Fatalf("未配置个人池: %d %s", w.Code, w.Body.String())
	}
	// 配置后：平台 + 个人。
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload("https://gw.example.com/v1")); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	w2 := personalJSON(r, http.MethodGet, "/api/v1/ai/models", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("models: %d %s", w2.Code, w2.Body.String())
	}
	body := w2.Body.String()
	for _, want := range []string{`"id":"mine"`, `"name":"我的网关（个人）"`, `"personal":true`, `"my-chat"`, `"id":"mock1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q: %s", want, body)
		}
	}
	if strings.Contains(body, "sk-personal-1") || strings.Contains(body, "gw.example.com") {
		t.Fatalf("models 泄漏密钥/地址: %s", body)
	}
}

// TestAIChatPersonalPoolExplicit 显式指定个人模型：请求打到个人上游
// （携带个人 api_key 与模型名）；显式平台模型不受个人池影响。
func TestAIChatPersonalPoolExplicit(t *testing.T) {
	up := newPersonalUpstream()
	defer up.srv.Close()
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	// 平台链保留 mock Provider（显式平台模型时零上游依赖）。
	r := newAIPersonalRouter(newAIv1Service(100), store, actor)
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload(up.srv.URL)); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	// 显式个人模型 → 个人上游。
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"providerId":"mine","modelId":"my-chat"},"messages":[{"role":"user","content":"你好"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("显式个人 chat: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"content":"个人池回答"`, `"provider_id":"mine"`, `"model":"my-chat"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body 缺 %q: %s", want, body)
		}
	}
	if up.lastAuth() != "Bearer sk-personal-1" {
		t.Fatalf("个人上游 Authorization = %q", up.lastAuth())
	}
	// 显式平台模型（仅 modelId 反查）：mock 回答、未触个人上游。
	w2 := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"modelId":"mock-echo"},"messages":[{"role":"user","content":"你好"}]}`)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), `"provider_id":"mock1"`) {
		t.Fatalf("显式平台 chat: %d %s", w2.Code, w2.Body.String())
	}
	// 越权：显式个人 Provider + 平台模型 → 两池均未命中该组合 → 400。
	w3 := postAI(r, "/api/v1/ai/chat", `{"stream":false,"model":{"providerId":"mine","modelId":"mock-echo"},"messages":[{"role":"user","content":"x"}]}`)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("个人 Provider+平台模型应 400: %d %s", w3.Code, w3.Body.String())
	}
}

// TestAIChatPreferPersonalDefault 未显式指定且 prefer_personal=true：
// 个人 chat 默认模型（打到个人上游）。
func TestAIChatPreferPersonalDefault(t *testing.T) {
	up := newPersonalUpstream()
	defer up.srv.Close()
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalRouter(newAIv1Service(100), store, actor)
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload(up.srv.URL)); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	w := postAI(r, "/api/v1/ai/chat", `{"stream":false,"messages":[{"role":"user","content":"你好"}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"my-chat"`) {
		t.Fatalf("个人默认 chat: %d %s", w.Code, w.Body.String())
	}
	if up.lastAuth() != "Bearer sk-personal-1" {
		t.Fatalf("应打到个人上游: %q", up.lastAuth())
	}
}

// TestAIChatPersonalRateLimitExempt 限流豁免：平台 Provider 级
// requests_per_min=1 时，显式个人池请求连续两次均放行（跳过 Provider 级
// 限流与日配额，保留全局兜底）；显式平台请求第二次 429。
func TestAIChatPersonalRateLimitExempt(t *testing.T) {
	up := newPersonalUpstream()
	defer up.srv.Close()
	cfg := settings.AIConfig{
		Providers: []settings.AIProvider{{
			ID: "plat", Name: "平台", Kind: settings.AIKindMock, Enabled: true,
			Models:         []settings.AIModel{{ID: "plat-chat", Capabilities: settings.AIModelCapabilities{Chat: true}}},
			RequestsPerMin: 1, DailyQuota: 1,
		}},
		DefaultProvider: "plat", Temperature: 0.3, MaxTokens: 512, PerUserPerMin: 100,
	}
	store := newFakeAIPrefsStore()
	actor := uuid.New()
	r := newAIPersonalRouter(newAIv1ServiceCfg(cfg), store, actor)
	if w := personalJSON(r, http.MethodPut, "/api/v1/ai/personal-settings", personalPrefsPayload(up.srv.URL)); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	personalBody := `{"stream":false,"model":{"providerId":"mine","modelId":"my-chat"},"messages":[{"role":"user","content":"x"}]}`
	platformBody := `{"stream":false,"model":{"providerId":"plat","modelId":"plat-chat"},"messages":[{"role":"user","content":"x"}]}`
	// 平台：第一次放行、第二次 429（分钟窗 + 日配额均按用户计数，桶跨
	// Provider 共享——先于个人请求执行，避免共享计数干扰）。
	if w := postAI(r, "/api/v1/ai/chat", platformBody); w.Code != http.StatusOK {
		t.Fatalf("平台第 1 次应放行: %d %s", w.Code, w.Body.String())
	}
	if w := postAI(r, "/api/v1/ai/chat", platformBody); w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "AI_RATE_LIMITED") {
		t.Fatalf("平台第 2 次应 429: %d %s", w.Code, w.Body.String())
	}
	// 个人池：紧随其后连续两次均放行（豁免 Provider 级 1/min 与 daily=1，
	// 仅全局 per_user_per_min=100 兜底未触及）。
	for i := 0; i < 2; i++ {
		if w := postAI(r, "/api/v1/ai/chat", personalBody); w.Code != http.StatusOK {
			t.Fatalf("个人第 %d 次不应被平台 Provider 级限流/日配额拦截: %d %s", i+1, w.Code, w.Body.String())
		}
	}
}
