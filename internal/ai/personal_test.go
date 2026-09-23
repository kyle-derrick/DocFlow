// personal_test.go：双轨制解析链顺序测试（显式个人 → 显式平台 →
// prefer_personal 默认 → 平台回落）。
package ai

import (
	"errors"
	"testing"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
)

func personalTestPrefs() auth.AIPersonalPrefs {
	return auth.AIPersonalPrefs{
		Providers: []auth.AIPersonalProvider{{
			ID: "mine", Name: "我的网关", Kind: settings.AIKindOpenAICompatible,
			BaseURL: "https://gw.example.com/v1", APIKey: "sk-mine",
			Models: []auth.AIPersonalModel{
				{ID: "my-chat", Capabilities: settings.AIModelCapabilities{Chat: true}},
				{ID: "my-emb", Capabilities: settings.AIModelCapabilities{Embedding: true}},
			},
		}},
		DefaultModels: map[string]auth.AIPersonalModelRef{
			"chat":    {ProviderID: "mine", ModelID: "my-chat"},
			"summary": {ProviderID: "mine", ModelID: "my-chat"},
		},
	}
}

func personalTestPlatformCfg() settings.AIConfig {
	return settings.AIConfig{
		Providers: []settings.AIProvider{
			{ID: "plat", Name: "平台", Kind: settings.AIKindMock, Model: "plat-chat", Enabled: true,
				Models: []settings.AIModel{{ID: "plat-chat", Capabilities: settings.AIModelCapabilities{Chat: true}}}},
			// 与个人池同 ID 的平台 Provider（冲突场景：显式指定时个人优先）。
			{ID: "mine", Name: "平台同名", Kind: settings.AIKindMock, Enabled: true,
				Models: []settings.AIModel{{ID: "plat-chat", Capabilities: settings.AIModelCapabilities{Chat: true}}}},
		},
		DefaultProvider: "plat",
		Temperature:     settings.AITemperatureDefault,
		MaxTokens:       settings.AIMaxTokensDefault,
		PerUserPerMin:   100,
	}
}

func newPersonalTestService(preferPersonal bool) *Service {
	prefs := personalTestPrefs()
	prefs.PreferPersonal = preferPersonal
	return NewService(func() (settings.AIConfig, error) { return personalTestPlatformCfg(), nil }).
		WithPersonalPrefs(prefs)
}

func TestResolveChatTargetForExplicitPersonalFirst(t *testing.T) {
	svc := newPersonalTestService(false)
	// 显式 providerId+modelId 命中个人池。
	tgt, err := svc.ResolveChatTargetFor("mine", "my-chat", settings.AIScenarioChat)
	if err != nil || !tgt.Personal {
		t.Fatalf("显式个人: err=%v personal=%v", err, tgt.Personal)
	}
	if tgt.Provider.ID != "mine" || tgt.Model != "my-chat" || tgt.Provider.APIKey != "sk-mine" {
		t.Fatalf("解析结果 = %+v", tgt)
	}
	// 仅 modelId（反查）：命中个人池。
	tgt2, err := svc.ResolveChatTargetFor("", "my-chat", settings.AIScenarioChat)
	if err != nil || !tgt2.Personal || tgt2.Provider.ID != "mine" {
		t.Fatalf("反查个人: err=%v tgt=%+v", err, tgt2)
	}
	// 显式 providerId、modelId 缺省：个人 Provider 场景默认/主模型。
	tgt3, err := svc.ResolveChatTargetFor("mine", "", settings.AIScenarioSummary)
	if err != nil || !tgt3.Personal || tgt3.Model != "my-chat" {
		t.Fatalf("个人主模型: err=%v tgt=%+v", err, tgt3)
	}
}

func TestResolveChatTargetForExplicitFallsBackToPlatform(t *testing.T) {
	svc := newPersonalTestService(false)
	// 模型不在个人池 → 平台链（modelId 反查平台）。
	tgt, err := svc.ResolveChatTargetFor("", "plat-chat", settings.AIScenarioChat)
	if err != nil {
		t.Fatalf("回落平台: %v", err)
	}
	if tgt.Personal {
		t.Fatalf("应命中平台池: %+v", tgt)
	}
	// ID 冲突（同 ID 个人+平台）：显式 providerId=mine+平台模型 → 个人池
	// 未命中该模型，回落平台同名 Provider。
	tgt2, err := svc.ResolveChatTargetFor("mine", "plat-chat", settings.AIScenarioChat)
	if err != nil {
		t.Fatalf("冲突回落: %v", err)
	}
	if tgt2.Personal || tgt2.Provider.ID != "mine" || tgt2.Provider.Kind != settings.AIKindMock {
		t.Fatalf("冲突应回落平台同名 Provider: %+v", tgt2)
	}
	// 两池均未命中 → ErrModelNotAllowed。
	_, err = svc.ResolveChatTargetFor("", "no-such-model", settings.AIScenarioChat)
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("未知模型应 ErrModelNotAllowed: %v", err)
	}
	// 个人池模型无 chat 能力（embedding）→ 不命中个人，平台亦无 → 拒绝。
	_, err = svc.ResolveChatTargetFor("mine", "my-emb", settings.AIScenarioChat)
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("无 chat 能力应 ErrModelNotAllowed: %v", err)
	}
}

func TestResolveChatTargetForPreferPersonalDefault(t *testing.T) {
	// prefer_personal=true 且个人 chat 默认存在 → 未显式时用个人默认。
	svc := newPersonalTestService(true)
	tgt, err := svc.ResolveChatTargetFor("", "", settings.AIScenarioChat)
	if err != nil || !tgt.Personal || tgt.Model != "my-chat" {
		t.Fatalf("个人默认: err=%v tgt=%+v", err, tgt)
	}
	// summary 场景默认命中个人（无效场景回落 chat 同链）。
	tgtS, err := svc.ResolveChatTargetFor("", "", settings.AIScenarioSummary)
	if err != nil || !tgtS.Personal {
		t.Fatalf("summary 个人默认: err=%v", err)
	}
	// prefer_personal=false → 平台链不变（默认 Provider 主模型）。
	svcOff := newPersonalTestService(false)
	tgt2, err := svcOff.ResolveChatTargetFor("", "", settings.AIScenarioChat)
	if err != nil || tgt2.Personal || tgt2.Provider.ID != "plat" {
		t.Fatalf("平台默认: err=%v tgt=%+v", err, tgt2)
	}
	// prefer_personal=true 但无 default_models → 平台链。
	prefs := personalTestPrefs()
	prefs.PreferPersonal = true
	prefs.DefaultModels = nil
	svcNoDefault := NewService(func() (settings.AIConfig, error) { return personalTestPlatformCfg(), nil }).WithPersonalPrefs(prefs)
	tgt3, err := svcNoDefault.ResolveChatTargetFor("", "", settings.AIScenarioChat)
	if err != nil || tgt3.Personal {
		t.Fatalf("无个人默认应回落平台: err=%v tgt=%+v", err, tgt3)
	}
	// 未挂载个人池（零值）：与旧行为一致。
	svcBare := NewService(func() (settings.AIConfig, error) { return personalTestPlatformCfg(), nil })
	tgt4, err := svcBare.ResolveChatTargetFor("", "", settings.AIScenarioChat)
	if err != nil || tgt4.Personal || tgt4.Provider.ID != "plat" {
		t.Fatalf("裸服务平台链: err=%v tgt=%+v", err, tgt4)
	}
}

// TestResolvePersonalProviderIsomorphic 适配为平台同构 Provider：字段映射
// 正确、恒启用、无 Provider 级限流配置（限流豁免由 HTTP 层执行）。
func TestResolvePersonalProviderIsomorphic(t *testing.T) {
	prefs := personalTestPrefs()
	p, ok := ResolvePersonalProvider(prefs, "mine")
	if !ok {
		t.Fatal("未命中")
	}
	if !p.Enabled || p.Kind != settings.AIKindOpenAICompatible || p.BaseURL != prefs.Providers[0].BaseURL || p.APIKey != "sk-mine" {
		t.Fatalf("同构映射 = %+v", p)
	}
	if len(p.Models) != 2 || p.Models[0].ID != "my-chat" {
		t.Fatalf("models = %+v", p.Models)
	}
	if _, ok := ResolvePersonalProvider(prefs, "nope"); ok {
		t.Fatal("未知 id 应未命中")
	}
}
