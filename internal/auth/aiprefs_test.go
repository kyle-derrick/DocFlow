// aiprefs_test.go：个人 AI 配置校验与掩码视图测试（user_ai_prefs 结构）。
package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/settings"
)

func validPrefs() AIPersonalPrefs {
	return AIPersonalPrefs{
		Providers: []AIPersonalProvider{{
			ID: "mine", Name: "我的 DeepSeek", Kind: AIPersonalKindOpenAICompatible,
			BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-personal",
			Models: []AIPersonalModel{
				{ID: "deepseek-chat", Label: "对话", Capabilities: settings.AIModelCapabilities{Chat: true, Reasoning: true}},
				{ID: "deepseek-emb", Capabilities: settings.AIModelCapabilities{Embedding: true}},
			},
		}},
		DefaultModels: map[string]AIPersonalModelRef{
			"chat":      {ProviderID: "mine", ModelID: "deepseek-chat"},
			"embedding": {ProviderID: "mine", ModelID: "deepseek-emb"},
		},
		Personas:       []AIPersonalPersona{{ID: "writer", Name: "写作助手", SystemPrompt: "你是写作助手"}},
		PreferPersonal: true,
	}
}

func TestValidateAIPersonalPrefsValid(t *testing.T) {
	if err := ValidateAIPersonalPrefs(validPrefs()); err != nil {
		t.Fatalf("合法配置应通过: %v", err)
	}
	// 空配置（用户未配置任何个人 Provider）合法。
	if err := ValidateAIPersonalPrefs(AIPersonalPrefs{}); err != nil {
		t.Fatalf("空配置应通过: %v", err)
	}
}

func TestValidateAIPersonalPrefsProviderBranches(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AIPersonalPrefs)
		want string // 错误消息须包含的定位片段
	}{
		{"providers 超上限", func(p *AIPersonalPrefs) {
			for i := 0; i < AIPersonalMaxProviders+1; i++ {
				p.Providers = append(p.Providers, AIPersonalProvider{ID: string(rune('a' + i)), Name: "n", Kind: AIPersonalKindAnthropic, BaseURL: "https://x.example.com", Models: []AIPersonalModel{{ID: "m"}}})
			}
		}, "providers 最多"},
		{"id 为空", func(p *AIPersonalPrefs) { p.Providers[0].ID = " " }, "providers[0].id"},
		{"id 非法字符", func(p *AIPersonalPrefs) { p.Providers[0].ID = "bad id!" }, "providers[0].id"},
		{"id 重复", func(p *AIPersonalPrefs) {
			p.Providers = append(p.Providers, p.Providers[0])
		}, "重复"},
		{"name 为空", func(p *AIPersonalPrefs) { p.Providers[0].Name = "" }, "providers[0].name"},
		{"kind 非法", func(p *AIPersonalPrefs) { p.Providers[0].Kind = "mock" }, "providers[0].kind"},
		{"kind 平台枚举外", func(p *AIPersonalPrefs) { p.Providers[0].Kind = "openai" }, "providers[0].kind"},
		{"base_url 缺失", func(p *AIPersonalPrefs) { p.Providers[0].BaseURL = "" }, "base_url"},
		{"base_url 非绝对", func(p *AIPersonalPrefs) { p.Providers[0].BaseURL = "api.deepseek.com/v1" }, "base_url"},
		{"base_url 非 http(s)", func(p *AIPersonalPrefs) { p.Providers[0].BaseURL = "ftp://x.example.com" }, "base_url"},
		{"api_key 过长", func(p *AIPersonalPrefs) { p.Providers[0].APIKey = strings.Repeat("k", 513) }, "api_key"},
		{"models 为空", func(p *AIPersonalPrefs) { p.Providers[0].Models = nil }, "至少配置 1 个模型"},
		{"models 超上限", func(p *AIPersonalPrefs) {
			p.Providers[0].Models = nil
			for i := 0; i < AIPersonalMaxModels+1; i++ {
				p.Providers[0].Models = append(p.Providers[0].Models, AIPersonalModel{ID: "m" + string(rune('a'+i%26)) + string(rune('0'+i/26))})
			}
		}, "最多"},
		{"model id 重复", func(p *AIPersonalPrefs) {
			p.Providers[0].Models = append(p.Providers[0].Models, p.Providers[0].Models[0])
		}, "重复"},
		{"model id 为空", func(p *AIPersonalPrefs) { p.Providers[0].Models[0].ID = "" }, "models[0].id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPrefs()
			tc.mut(&p)
			err := ValidateAIPersonalPrefs(p)
			if err == nil {
				t.Fatalf("应报错")
			}
			if !errors.Is(err, ErrInvalidAIPrefs) {
				t.Fatalf("错误须包裹 ErrInvalidAIPrefs: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 应含定位 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestValidateAIPersonalPrefsDefaultModelsAndPersonas(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*AIPersonalPrefs)
		want string
	}{
		{"未知场景键", func(p *AIPersonalPrefs) {
			p.DefaultModels["translate"] = AIPersonalModelRef{ProviderID: "mine", ModelID: "deepseek-chat"}
		}, "未知场景"},
		{"provider 不存在", func(p *AIPersonalPrefs) {
			p.DefaultModels["summary"] = AIPersonalModelRef{ProviderID: "nope", ModelID: "deepseek-chat"}
		}, "provider_id"},
		{"模型不在 Provider", func(p *AIPersonalPrefs) {
			p.DefaultModels["summary"] = AIPersonalModelRef{ProviderID: "mine", ModelID: "no-such"}
		}, "model_id"},
		{"chat 场景无 chat 能力", func(p *AIPersonalPrefs) {
			p.DefaultModels["edit"] = AIPersonalModelRef{ProviderID: "mine", ModelID: "deepseek-emb"}
		}, "chat 能力"},
		{"embedding 场景无 embedding 能力", func(p *AIPersonalPrefs) {
			p.DefaultModels["embedding"] = AIPersonalModelRef{ProviderID: "mine", ModelID: "deepseek-chat"}
		}, "embedding 能力"},
		{"personas 超上限", func(p *AIPersonalPrefs) {
			for i := 0; i < AIPersonalMaxPersonas+1; i++ {
				p.Personas = append(p.Personas, AIPersonalPersona{ID: "p" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Name: "n", SystemPrompt: "s"})
			}
		}, "personas"},
		{"persona id 重复", func(p *AIPersonalPrefs) {
			p.Personas = append(p.Personas, p.Personas[0])
		}, "重复"},
		{"persona name 为空", func(p *AIPersonalPrefs) { p.Personas[0].Name = " " }, "personas[0].name"},
		{"system_prompt 超长", func(p *AIPersonalPrefs) {
			p.Personas[0].SystemPrompt = strings.Repeat("字", AIPersonalMaxPromptRunes+1)
		}, "system_prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPrefs()
			tc.mut(&p)
			err := ValidateAIPersonalPrefs(p)
			if err == nil || !errors.Is(err, ErrInvalidAIPrefs) {
				t.Fatalf("应报 ErrInvalidAIPrefs 语义错误: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 应含定位 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestAIPersonalPrefsMaskedNoKeyLeak 掩码视图：api_key 绝不回显，仅报
// api_key_configured；其余字段深拷贝（改视图不影响原配置）。
func TestAIPersonalPrefsMaskedNoKeyLeak(t *testing.T) {
	p := validPrefs()
	view := p.Masked()
	if len(view.Providers) != 1 {
		t.Fatalf("providers = %d", len(view.Providers))
	}
	v := view.Providers[0]
	if v.APIKeyConfigured != true {
		t.Fatalf("api_key_configured = %v, want true", v.APIKeyConfigured)
	}
	if v.Models[0].ID != "deepseek-chat" || v.Models[0].Capabilities.Reasoning != true {
		t.Fatalf("models 深拷贝丢失: %+v", v.Models)
	}
	if view.Personas[0].Name != "写作助手" || view.PreferPersonal != true {
		t.Fatalf("personas/prefer_personal 丢失: %+v", view)
	}
	if view.DefaultModels["chat"].ModelID != "deepseek-chat" {
		t.Fatalf("default_models 丢失: %+v", view.DefaultModels)
	}
	// 无 key 的 Provider：configured=false。
	p.Providers[0].APIKey = ""
	if p.Masked().Providers[0].APIKeyConfigured != false {
		t.Fatal("无 key 时 api_key_configured 应为 false")
	}
}

// TestAIPersonalPrefsMemoryAuto memory_auto 布尔：存量 JSON 无此字段反序列
// 化为 false（默认关闭，无需迁移）；开启后 JSON 往返保留、校验通过、掩码
// 视图原样回显（非密钥，不受掩码逻辑触碰）；默认关闭时视图不输出该字段。
func TestAIPersonalPrefsMemoryAuto(t *testing.T) {
	// 存量 JSON（无 memory_auto 字段）→ false。
	var legacy AIPersonalPrefs
	if err := json.Unmarshal([]byte(`{"providers":[],"prefer_personal":true}`), &legacy); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if legacy.MemoryAuto {
		t.Fatal("存量 JSON 无 memory_auto 应反序列化为 false")
	}
	// 开启：JSON 往返 + 校验通过 + 掩码视图原样回显。
	p := validPrefs()
	p.MemoryAuto = true
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back AIPersonalPrefs
	if err := json.Unmarshal(raw, &back); err != nil || !back.MemoryAuto {
		t.Fatalf("JSON 往返应保留 memory_auto: err=%v on=%v", err, back.MemoryAuto)
	}
	if err := ValidateAIPersonalPrefs(p); err != nil {
		t.Fatalf("开启 memory_auto 不引入新校验: %v", err)
	}
	if !p.Masked().MemoryAuto {
		t.Fatal("掩码视图应原样回显 memory_auto（非密钥）")
	}
	// 默认关闭：视图 omitempty 不输出该字段。
	if b, err := json.Marshal(AIPersonalPrefs{}.Masked()); err != nil || strings.Contains(string(b), "memory_auto") {
		t.Fatalf("默认关闭时视图不应输出 memory_auto: %s (%v)", b, err)
	}
}
