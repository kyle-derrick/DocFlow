// ai_caps_test.go：AIModelCapabilities 类型+能力并集重构（Cherry Studio
// 语义）的兼容迁移测试——旧对象读入自动映射 kind（含多 true 优先级与
// 全 false 默认）、新写出双形态（kind+旧布尔）、kind 权威读入、复合
// 字面量构造（Kind 为空）经旧布尔推导、ValidateAI 的 kind 白名单。
package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustUnmarshalCaps 反序列化 JSON 到 AIModelCapabilities（失败即 fatal）。
func mustUnmarshalCaps(t *testing.T, raw string) AIModelCapabilities {
	t.Helper()
	var c AIModelCapabilities
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return c
}

// TestAIModelCapabilitiesLegacyMapping 旧对象（无 kind、有 chat/embedding/
// rerank 布尔）读入自动映射 kind；多 true 按优先 embedding>rerank>chat
// （旧 UI chat 为默认勾选噪音，embedding/rerank 为主动勾选）；全 false
// （含 vision-only）默认 chat。
func TestAIModelCapabilitiesLegacyMapping(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind string
	}{
		{"chat only", `{"chat":true}`, AIModelKindChat},
		{"embedding only", `{"embedding":true}`, AIModelKindEmbedding},
		{"rerank only", `{"rerank":true}`, AIModelKindRerank},
		// 多 true：embedding > rerank > chat 优先级（bge-m3 旧数据
		// {chat:true,embedding:true} 必须归 embedding，否则 default
		// embedding model 校验误拒）。
		{"embedding beats chat", `{"chat":true,"embedding":true,"rerank":true}`, AIModelKindEmbedding},
		{"rerank beats chat", `{"chat":true,"rerank":true}`, AIModelKindRerank},
		{"embedding beats rerank", `{"embedding":true,"rerank":true}`, AIModelKindEmbedding},
		// 全 false / 全缺省：默认 chat（旧 vision-only 归一 chat+vision 能力）。
		{"all false defaults chat", `{"chat":false,"embedding":false,"rerank":false,"vision":true}`, AIModelKindChat},
		{"empty object defaults chat", `{}`, AIModelKindChat},
		// 旧 reasoning 能力保留。
		{"keeps reasoning", `{"chat":true,"reasoning":true}`, AIModelKindChat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustUnmarshalCaps(t, tc.raw)
			if c.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q (raw %s)", c.Kind, tc.kind, tc.raw)
			}
			// 归一后旧布尔与 kind 恒一致（内存态一致性）。
			if want := tc.kind == AIModelKindChat; c.Chat != want {
				t.Fatalf("chat = %v, want %v", c.Chat, want)
			}
			if want := tc.kind == AIModelKindEmbedding; c.Embedding != want {
				t.Fatalf("embedding = %v, want %v", c.Embedding, want)
			}
			if want := tc.kind == AIModelKindRerank; c.Rerank != want {
				t.Fatalf("rerank = %v, want %v", c.Rerank, want)
			}
		})
	}
	// 旧 vision/reasoning 能力字段读入保留。
	if c := mustUnmarshalCaps(t, `{"chat":true,"vision":true,"reasoning":true}`); !c.Vision || !c.Reasoning {
		t.Fatalf("vision/reasoning lost: %+v", c)
	}
}

// TestAIModelCapabilitiesKindAuthoritative 新形态读入：kind 为权威（载荷
// 携带的旧布尔与 kind 冲突时被忽略并按 kind 派生），能力并集保留。
func TestAIModelCapabilitiesKindAuthoritative(t *testing.T) {
	c := mustUnmarshalCaps(t, `{"kind":"embedding","chat":true,"reasoning":true,"audio":true}`)
	if c.Kind != AIModelKindEmbedding {
		t.Fatalf("kind = %q, want embedding", c.Kind)
	}
	if c.Chat {
		t.Fatalf("chat derived from kind must be false for embedding")
	}
	if !c.Embedding {
		t.Fatalf("embedding derived from kind must be true")
	}
	if !c.Reasoning || !c.Audio {
		t.Fatalf("capability union lost: %+v", c)
	}
	// 未知 kind 按缺失处理（旧布尔推导，默认 chat）。
	if c := mustUnmarshalCaps(t, `{"kind":"weird"}`); c.Kind != AIModelKindChat {
		t.Fatalf("unknown kind should fall back to chat, got %q", c.Kind)
	}
}

// TestAIModelCapabilitiesMarshalDualForm 新写出双形态：kind+能力并集在前，
// 旧布尔由 kind 派生（已部署旧前端按 chat 布尔过滤不受影响）。
func TestAIModelCapabilitiesMarshalDualForm(t *testing.T) {
	cases := []struct {
		name string
		caps AIModelCapabilities
		want string
	}{
		{
			"chat+reasoning writes dual form",
			AIModelCapabilities{Kind: AIModelKindChat, Reasoning: true},
			`{"kind":"chat","reasoning":true,"vision":false,"audio":false,"video":false,"chat":true,"embedding":false,"rerank":false}`,
		},
		{
			"embedding derives legacy booleans",
			AIModelCapabilities{Kind: AIModelKindEmbedding},
			`{"kind":"embedding","reasoning":false,"vision":false,"audio":false,"video":false,"chat":false,"embedding":true,"rerank":false}`,
		},
		{
			"rerank with vision/audio/video union",
			AIModelCapabilities{Kind: AIModelKindRerank, Vision: true, Audio: true, Video: true},
			`{"kind":"rerank","reasoning":false,"vision":true,"audio":true,"video":true,"chat":false,"embedding":false,"rerank":true}`,
		},
		{
			"image kind",
			AIModelCapabilities{Kind: AIModelKindImage},
			`{"kind":"image","reasoning":false,"vision":false,"audio":false,"video":false,"chat":false,"embedding":false,"rerank":false}`,
		},
		// 复合字面量构造（Kind 为空，如旧代码 {Chat: true}）：序列化时按
		// 旧布尔推导 kind，保证读旧布尔的调用方与写出的 kind 一致。
		{
			"legacy literal {Chat:true} derives kind=chat",
			AIModelCapabilities{Chat: true},
			`{"kind":"chat","reasoning":false,"vision":false,"audio":false,"video":false,"chat":true,"embedding":false,"rerank":false}`,
		},
		{
			"legacy literal {Embedding:true} derives kind=embedding",
			AIModelCapabilities{Embedding: true},
			`{"kind":"embedding","reasoning":false,"vision":false,"audio":false,"video":false,"chat":false,"embedding":true,"rerank":false}`,
		},
		{
			"legacy literal {Rerank:true} derives kind=rerank",
			AIModelCapabilities{Rerank: true},
			`{"kind":"rerank","reasoning":false,"vision":false,"audio":false,"video":false,"chat":false,"embedding":false,"rerank":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.caps)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Fatalf("marshal = %s, want %s", raw, tc.want)
			}
		})
	}
}

// TestAIModelCapabilitiesRoundTrip 写出→读入往返：kind 与能力并集稳定，
// 且读入后旧布尔与 kind 一致（双形态自洽）。
func TestAIModelCapabilitiesRoundTrip(t *testing.T) {
	orig := AIModelCapabilities{Kind: AIModelKindChat, Reasoning: true, Vision: true, Audio: true, Video: true}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back AIModelCapabilities
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Kind != AIModelKindChat || !back.Reasoning || !back.Vision || !back.Audio || !back.Video || !back.Chat {
		t.Fatalf("round trip lost fields: %+v (raw %s)", back, raw)
	}
	// 旧形态写出同样可被旧语义消费：仅旧布尔的读法（模拟已部署前端）。
	var legacy struct {
		Chat      bool `json:"chat"`
		Embedding bool `json:"embedding"`
		Rerank    bool `json:"rerank"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if !legacy.Chat || legacy.Embedding || legacy.Rerank {
		t.Fatalf("legacy view mismatch: %+v", legacy)
	}
}

// TestAIModelCapabilitiesInProviderConfig providers JSON 整体读入即迁移：
// 旧配置（capabilities 为旧布尔对象）反序列化后模型 kind 已归一，且
// PrimaryModel 等按旧布尔读的解析函数行为不变。
func TestAIModelCapabilitiesInProviderConfig(t *testing.T) {
	raw := `[{"id":"p1","name":"P1","kind":"openai_compatible","base_url":"https://x.example/v1","enabled":true,"models":[
		{"id":"chat-m","capabilities":{"chat":true,"reasoning":true}},
		{"id":"emb-m","capabilities":{"embedding":true}},
		{"id":"rr-m","capabilities":{"rerank":true}}
	]}]`
	var providers []AIProvider
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	p := providers[0]
	if got := p.Models[0].Capabilities.Kind; got != AIModelKindChat {
		t.Fatalf("chat-m kind = %q", got)
	}
	if got := p.Models[1].Capabilities.Kind; got != AIModelKindEmbedding {
		t.Fatalf("emb-m kind = %q", got)
	}
	if got := p.Models[2].Capabilities.Kind; got != AIModelKindRerank {
		t.Fatalf("rr-m kind = %q", got)
	}
	// 旧布尔读取路径（PrimaryModel 首个 chat 模型）行为保持。
	if got := p.PrimaryModel(); got != "chat-m" {
		t.Fatalf("PrimaryModel = %q, want chat-m", got)
	}
	// 二次序列化（写库）产出双形态。
	out, err := json.Marshal(providers)
	if err != nil {
		t.Fatalf("marshal providers: %v", err)
	}
	for _, want := range []string{`"kind":"chat"`, `"kind":"embedding"`, `"kind":"rerank"`, `"chat":true`, `"embedding":true`, `"rerank":true`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("dual-form output missing %s: %s", want, out)
		}
	}
}

// TestValidateAIProviderModelKind ValidateAIProvider 拒绝 capabilities.kind
// 白名单外的取值（UnmarshalJSON 已归一，此处防御直接构造）。
func TestValidateAIProviderModelKind(t *testing.T) {
	p := AIProvider{ID: "p1", Name: "P1", Kind: AIKindOpenAICompatible, BaseURL: "https://x.example/v1",
		Models: []AIModel{{ID: "m", Capabilities: AIModelCapabilities{Kind: "tts"}}}}
	err := ValidateAIProvider(p)
	if err == nil || !strings.Contains(err.Error(), "capabilities.kind") {
		t.Fatalf("expected capabilities.kind validation error, got %v", err)
	}
	p.Models[0].Capabilities.Kind = AIModelKindImage
	if err := ValidateAIProvider(p); err != nil {
		t.Fatalf("valid kind rejected: %v", err)
	}
}
