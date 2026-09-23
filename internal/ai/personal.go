// personal.go：个人 AI Provider 池（双轨制解析链）。
//
// 双轨制模型解析顺序（chat/summarize 与 /ai/models 消费）：
//  1. 请求显式指定模型（providerId/modelId 任一非空）：先查个人池后查
//     平台池——个人池命中（模型归属该 Provider 且具备 chat 能力）即用，
//     未命中回落平台既有解析链；两池均未命中返回 ErrModelNotAllowed；
//  2. 未显式指定：prefer_personal=true 且个人 default_models 命中场景
//     （chat/summary/edit，无效回落 chat）→ 用个人模型；否则平台链不变。
//
// 个人 Provider 经同一 openai_compatible/anthropic 请求构造（复用
// chatOpenAI/chatAnthropic：ResolvePersonalProvider 把个人条目适配为与
// 平台同构的 settings.AIProvider）。限流豁免（跳过 Provider 级限流与日
// 配额，个人 Key 用户自担）由 HTTP 层按解析结果的 Personal 标志执行；
// 用量/审计以 personal:true 记录。
package ai

import (
	"fmt"
	"strings"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/settings"
)

// ResolvePersonalProvider 把个人 Provider 条目适配为与平台同构的
// settings.AIProvider（Kind 仅 openai_compatible/anthropic，恒视为启用，
// 无 Provider 级限流配置）。id 未命中返回 ok=false。
func ResolvePersonalProvider(prefs auth.AIPersonalPrefs, id string) (settings.AIProvider, bool) {
	prov, ok := prefs.PersonalProviderByID(id)
	if !ok {
		return settings.AIProvider{}, false
	}
	models := make([]settings.AIModel, len(prov.Models))
	for i, m := range prov.Models {
		models[i] = settings.AIModel{ID: m.ID, Label: m.Label, Capabilities: m.Capabilities}
	}
	return settings.AIProvider{
		ID: prov.ID, Name: prov.Name, Kind: prov.Kind,
		BaseURL: prov.BaseURL, APIKey: prov.APIKey,
		Models: models, Enabled: true,
	}, true
}

// WithPersonalPrefs 返回挂载个人配置的浅拷贝（config/client 等只读字段
// 共享；ForUser 之后链式调用，每请求构造，无并发状态）。
func (s *Service) WithPersonalPrefs(prefs auth.AIPersonalPrefs) *Service {
	clone := *s
	clone.personal = prefs
	return &clone
}

// PersonalPrefs 返回挂载的个人配置（未挂载返回零值与 false）。
func (s *Service) PersonalPrefs() (auth.AIPersonalPrefs, bool) {
	if len(s.personal.Providers) == 0 {
		return auth.AIPersonalPrefs{}, false
	}
	return s.personal, true
}

// ChatTarget 为个人池感知的解析结果：Personal=true 表示命中个人池
// （Provider 为个人条目的同构适配；HTTP 层据此豁免 Provider 级限流与
// 日配额并在审计/用量打 personal 标记）。
type ChatTarget struct {
	Provider settings.AIProvider
	Model    string
	Config   settings.AIConfig
	Personal bool
}

// ResolveChatTargetFor 解析对话目标（个人池感知版；HTTP 层经
// ForUser+WithPersonalPrefs 的服务克隆调用，替代裸 ResolveChatTarget）。
// 解析顺序见文件头注释。错误：ErrNoProvider/ErrProviderNotFound/
// ErrModelNotAllowed。
func (s *Service) ResolveChatTargetFor(providerID, modelID, scenario string) (ChatTarget, error) {
	prefs, hasPersonal := s.PersonalPrefs()
	target := strings.TrimSpace(providerID)
	mid := strings.TrimSpace(modelID)

	// 显式指定模型（或 Provider）：先查个人池，未命中回落平台链。
	if target != "" || mid != "" {
		if hasPersonal {
			if t, ok := resolvePersonalTarget(prefs, target, mid, scenario); ok {
				cfg, err := s.Config()
				if err != nil {
					return ChatTarget{}, fmt.Errorf("%w: read config: %v", ErrUpstreamChat, err)
				}
				t.Config = cfg
				return t, nil
			}
		}
		provider, model, cfg, err := s.resolveChatTarget(target, mid, scenario)
		if err != nil {
			return ChatTarget{}, err
		}
		return ChatTarget{Provider: provider, Model: model, Config: cfg}, nil
	}

	// 未显式指定：prefer_personal 且个人场景默认命中 → 个人池。
	if hasPersonal && prefs.PreferPersonal {
		if t, ok := personalDefaultTarget(prefs, scenario); ok {
			cfg, err := s.Config()
			if err != nil {
				return ChatTarget{}, fmt.Errorf("%w: read config: %v", ErrUpstreamChat, err)
			}
			t.Config = cfg
			return t, nil
		}
	}
	provider, model, cfg, err := s.resolveChatTarget("", "", scenario)
	if err != nil {
		return ChatTarget{}, err
	}
	return ChatTarget{Provider: provider, Model: model, Config: cfg}, nil
}

// resolvePersonalTarget 在个人池中解析显式目标：
//   - 指定 providerID：命中个人 Provider 且模型（缺省取场景默认/主模型）
//     具备 chat 能力 → 命中；模型不归属或无 chat 能力 → 不命中（回落
//     平台链，两池均未命中由平台链报 ErrModelNotAllowed）；
//   - 仅指定 modelID：按模型归属反查个人池（须具备 chat 能力）。
func resolvePersonalTarget(prefs auth.AIPersonalPrefs, target, mid, scenario string) (ChatTarget, bool) {
	if target != "" {
		provider, ok := ResolvePersonalProvider(prefs, target)
		if !ok {
			return ChatTarget{}, false
		}
		if mid == "" {
			model := personalDefaultModel(prefs, provider, scenario)
			if model == "" {
				return ChatTarget{}, false
			}
			return ChatTarget{Provider: provider, Model: model, Personal: true}, true
		}
		if m, ok := provider.ModelWithID(mid); ok && m.Capabilities.Chat {
			return ChatTarget{Provider: provider, Model: m.ID, Personal: true}, true
		}
		return ChatTarget{}, false
	}
	// 仅 modelID：反查个人池。
	for _, p := range prefs.Providers {
		provider, _ := ResolvePersonalProvider(prefs, p.ID)
		if m, ok := provider.ModelWithID(mid); ok && m.Capabilities.Chat {
			return ChatTarget{Provider: provider, Model: m.ID, Personal: true}, true
		}
	}
	return ChatTarget{}, false
}

// personalDefaultTarget 解析个人池场景默认模型（scenario 空 = chat；
// summary/edit 无效回落 chat；命中须具备 chat 能力）。
func personalDefaultTarget(prefs auth.AIPersonalPrefs, scenario string) (ChatTarget, bool) {
	if scenario == "" || scenario == settings.AIScenarioEmbedding {
		scenario = settings.AIScenarioChat
	}
	for _, s := range []string{scenario, settings.AIScenarioChat} {
		ref, ok := prefs.DefaultModels[s]
		if !ok {
			continue
		}
		provider, ok := ResolvePersonalProvider(prefs, ref.ProviderID)
		if !ok {
			continue
		}
		if m, ok := provider.ModelWithID(ref.ModelID); ok && m.Capabilities.Chat {
			return ChatTarget{Provider: provider, Model: m.ID, Personal: true}, true
		}
	}
	return ChatTarget{}, false
}

// personalDefaultModel 返回个人 Provider 的场景默认模型（default_models
// 命中同 Provider 优先），否则首个 chat 能力模型，再否则首个模型。
func personalDefaultModel(prefs auth.AIPersonalPrefs, provider settings.AIProvider, scenario string) string {
	if scenario == "" {
		scenario = settings.AIScenarioChat
	}
	for _, s := range []string{scenario, settings.AIScenarioChat} {
		if ref, ok := prefs.DefaultModels[s]; ok && ref.ProviderID == provider.ID {
			if m, ok2 := provider.ModelWithID(ref.ModelID); ok2 && m.Capabilities.Chat {
				return m.ID
			}
		}
	}
	return provider.PrimaryModel()
}
