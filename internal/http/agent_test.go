// Package http —— Agent 默认开启语义（agentEnabled）与 /ai/status 能力
// 标志扩展测试：agent.enabled 未配置默认 true、AI 总开关关闭连带 Agent
// 不可用；/ai/status 响应从单布尔扩展为能力对象（agent/web_search/mcp/rag）。
package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/settings"
)

// newAIOffService 构造 AI 总开关显式关闭（Provider 仍在配置中）的服务。
func newAIOffService() *ai.Service {
	return ai.NewService(func() (settings.AIConfig, error) {
		cfg := settings.DefaultAIConfig()
		off := false
		cfg.Enabled = &off
		cfg.Providers = []settings.AIProvider{{ID: "mock1", Kind: settings.AIKindMock, Model: "m", Enabled: true}}
		return cfg, nil
	})
}

// TestAgentEnabledSemantics agentEnabled 判定：未配置默认 false（Docker
// 沙箱为进阶可选，需管理员显式开启；受 AI 总开关约束）；AI 总开关
// 关闭 / aiSvc 未注入 / 显式 false → false。
func TestAgentEnabledSemantics(t *testing.T) {
	on := newAIv1Service(100)
	// AI 开 + agent.enabled 未配置（回退定义默认 false）→ false。
	h := &Handler{aiSvc: on, settings: &fakeSettingsService{}}
	if h.agentEnabled() {
		t.Fatal("agent should default to disabled when agent.enabled is unconfigured")
	}
	// AI 总开关显式关闭 → Agent 一并不可用。
	if (&Handler{aiSvc: newAIOffService(), settings: &fakeSettingsService{}}).agentEnabled() {
		t.Fatal("agent must be disabled when AI master switch is off")
	}
	// aiSvc 未注入 → false。
	if (&Handler{settings: &fakeSettingsService{}}).agentEnabled() {
		t.Fatal("agent must be disabled without ai service")
	}
	// settings 未装配 → false（fail closed）。
	if (&Handler{aiSvc: on}).agentEnabled() {
		t.Fatal("agent must be disabled without settings service")
	}
	// 显式 agent.enabled=false → false；显式 true → true。
	if (&Handler{aiSvc: on, settings: &fakeSettingsService{boolKeys: map[string]bool{"agent.enabled": false}}}).agentEnabled() {
		t.Fatal("explicit agent.enabled=false must disable agent")
	}
	if !(&Handler{aiSvc: on, settings: &fakeSettingsService{boolKeys: map[string]bool{"agent.enabled": true}}}).agentEnabled() {
		t.Fatal("explicit agent.enabled=true must enable agent")
	}
}

// fakeAIStatusSettings 为 /ai/status 测试的可配置 fake（嵌入
// fakeSettingsService 提升 GetBool 等；覆盖 AIOverrides 支撑 search
// 配置、AIMCPServices 支撑 mcp 有/无场景）。
type fakeAIStatusSettings struct {
	*fakeSettingsService
	aiCfg       settings.AIConfig
	mcpServices []settings.AIMCPServiceDef
}

func (f *fakeAIStatusSettings) AIOverrides() (settings.AIConfig, bool, error) {
	return f.aiCfg, true, nil
}

func (f *fakeAIStatusSettings) AIMCPServices() ([]settings.AIMCPServiceDef, error) {
	if f.mcpServices == nil {
		return []settings.AIMCPServiceDef{}, nil
	}
	return f.mcpServices, nil
}

func (f *fakeAIStatusSettings) SetAIMCPServices(list []settings.AIMCPServiceDef, _ uuid.UUID) ([]settings.AIMCPServiceDef, error) {
	f.mcpServices = list
	return list, nil
}

func newAgentStatusRouter(svc *ai.Service, st settingsService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.aiSvc = svc
	h.settings = st
	r := gin.New()
	r.GET("/api/v1/ai/status", h.aiStatus)
	return r
}

// TestAIStatusCapabilityFlags /ai/status 响应为能力对象：enabled（总
// 开关）/ agent（agentEnabled）/ web_search（search.provider 非空）/ mcp
// （存在启用中的 ai.mcp 服务）/ rag（= enabled）。
func TestAIStatusCapabilityFlags(t *testing.T) {
	get := func(svc *ai.Service, st settingsService) map[string]bool {
		r := newAgentStatusRouter(svc, st)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/ai/status", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		var out map[string]bool
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("body 应为能力对象: %s (%v)", w.Body.String(), err)
		}
		return out
	}
	assert := func(name string, got, want map[string]bool) {
		t.Helper()
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: %s = %v, want %v (body: %v)", name, k, got[k], v, got)
			}
		}
	}
	// AI 开 + search 配置 + 启用中的 MCP 服务 → 全 true。
	cfg := settings.DefaultAIConfig()
	cfg.Search.Provider = "searxng"
	full := &fakeAIStatusSettings{fakeSettingsService: &fakeSettingsService{}, aiCfg: cfg, mcpServices: []settings.AIMCPServiceDef{
		{ID: "s1", Name: "工具站", URL: "http://a/mcp", Enabled: true},
	}}
	assert("full", get(newAIv1Service(100), full), map[string]bool{
		"enabled": true, "agent": false, "web_search": true, "mcp": true, "rag": true,
	})
	// AI 开 + search 未配 + 仅停用 MCP 条目 → web_search/mcp false。
	empty := &fakeAIStatusSettings{fakeSettingsService: &fakeSettingsService{}, aiCfg: settings.DefaultAIConfig(), mcpServices: []settings.AIMCPServiceDef{
		{ID: "s2", Name: "停用", URL: "http://b/mcp", Enabled: false},
	}}
	assert("no-search/mcp", get(newAIv1Service(100), empty), map[string]bool{
		"enabled": true, "agent": false, "web_search": false, "mcp": false, "rag": true,
	})
	// AI 总开关关 → enabled/agent/rag 均 false。
	offCfg := settings.DefaultAIConfig()
	offCfg.Search.Provider = "searxng"
	offSt := &fakeAIStatusSettings{fakeSettingsService: &fakeSettingsService{}, aiCfg: offCfg, mcpServices: []settings.AIMCPServiceDef{
		{ID: "s1", Name: "n", URL: "http://a/mcp", Enabled: true},
	}}
	assert("ai-off", get(newAIOffService(), offSt), map[string]bool{
		"enabled": false, "agent": false, "rag": false,
	})
	// agent.enabled 显式 false → agent false（enabled 仍 true）。
	disabled := &fakeAIStatusSettings{fakeSettingsService: &fakeSettingsService{boolKeys: map[string]bool{"agent.enabled": false}}, aiCfg: settings.DefaultAIConfig()}
	assert("agent-off", get(newAIv1Service(100), disabled), map[string]bool{
		"enabled": true, "agent": false,
	})
}
