// ai_caps_test.go：AIModelCapabilities 最终形态（类型+能力并集，Cherry
// Studio 语义）单测——kind 归一（空/非法→chat、合法原样）、IsXxx 方法、
// 序列化单形态（kind+四能力，无旧布尔键）、providers 整体读入无 kind
// 经 EffectiveModels 归一（能力字段不丢）、ValidateAIProvider kind 白名单。
package settings

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNormalizeAIModelKind 归一规则：合法 kind 原样保留（含 trim），空/
// 非法一律归 chat（默认类型）。
func TestNormalizeAIModelKind(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"chat", AIModelKindChat},
		{"embedding", AIModelKindEmbedding},
		{"rerank", AIModelKindRerank},
		{"image", AIModelKindImage},
		{"  embedding  ", AIModelKindEmbedding}, // trim 后合法
		{"", AIModelKindChat},                   // 空 → 默认 chat
		{"  ", AIModelKindChat},                 // 纯空白 → chat
		{"weird", AIModelKindChat},              // 非法 → chat
		{"tts", AIModelKindChat},
	}
	for _, tc := range cases {
		c := AIModelCapabilities{Kind: tc.in}
		c.NormalizeAIModelKind()
		if c.Kind != tc.want {
			t.Fatalf("NormalizeAIModelKind(%q) = %q, want %q", tc.in, c.Kind, tc.want)
		}
	}
	// 归一只动 kind：能力并集字段原样保留。
	c := AIModelCapabilities{Kind: "", Reasoning: true, Vision: true, Audio: true, Video: true}
	c.NormalizeAIModelKind()
	if !c.Reasoning || !c.Vision || !c.Audio || !c.Video {
		t.Fatalf("归一不应丢能力字段: %+v", c)
	}
	// 归一幂等（重复调用稳定）。
	again := c
	again.NormalizeAIModelKind()
	if again != c {
		t.Fatalf("归一应幂等: %+v vs %+v", again, c)
	}
}

// TestAIModelCapabilitiesIsXxx IsChat/IsEmbedding/IsRerank 按 kind 判定
// （image 类型三者皆非）。
func TestAIModelCapabilitiesIsXxx(t *testing.T) {
	cases := []struct {
		kind              string
		chat, emb, rerank bool
	}{
		{AIModelKindChat, true, false, false},
		{AIModelKindEmbedding, false, true, false},
		{AIModelKindRerank, false, false, true},
		{AIModelKindImage, false, false, false},
	}
	for _, tc := range cases {
		c := AIModelCapabilities{Kind: tc.kind}
		if c.IsChat() != tc.chat || c.IsEmbedding() != tc.emb || c.IsRerank() != tc.rerank {
			t.Fatalf("kind %q: IsChat=%v IsEmbedding=%v IsRerank=%v", tc.kind, c.IsChat(), c.IsEmbedding(), c.IsRerank())
		}
	}
}

// TestAIModelCapabilitiesMarshalSingleForm 序列化单形态：只写 kind+四能力
// 键，不含旧 chat/embedding/rerank 布尔键（项目未发布，无旧形态兼容）。
func TestAIModelCapabilitiesMarshalSingleForm(t *testing.T) {
	raw, err := json.Marshal(AIModelCapabilities{Kind: AIModelKindEmbedding, Reasoning: true, Vision: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"kind":"embedding","reasoning":true,"vision":true,"audio":false,"video":false}`
	if string(raw) != want {
		t.Fatalf("marshal = %s, want %s", raw, want)
	}
	// 防御旧布尔键回归（整串相等已覆盖，此处显式断言语义）。
	for _, legacy := range []string{`"chat":`, `"chat":true`, `"embedding":true`, `"rerank":true`} {
		if strings.Contains(string(raw), legacy) {
			t.Fatalf("输出不应含旧布尔键 %s: %s", legacy, raw)
		}
	}
	// 全能力并集写出。
	raw2, err := json.Marshal(AIModelCapabilities{Kind: AIModelKindChat, Reasoning: true, Vision: true, Audio: true, Video: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want2 := `{"kind":"chat","reasoning":true,"vision":true,"audio":true,"video":true}`
	if string(raw2) != want2 {
		t.Fatalf("marshal = %s, want %s", raw2, want2)
	}
}

// TestAIModelCapabilitiesRoundTrip 写出→读入往返：kind 与能力并集稳定。
func TestAIModelCapabilitiesRoundTrip(t *testing.T) {
	orig := AIModelCapabilities{Kind: AIModelKindRerank, Reasoning: true, Vision: true, Audio: true, Video: true}
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back AIModelCapabilities
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != orig {
		t.Fatalf("round trip lost fields: %+v (raw %s)", back, raw)
	}
}

// TestAIModelCapabilitiesInProviderConfig providers JSON 整体读入：无 kind
// 的存量条目经 EffectiveModels 归一为 chat（唯一归一落点），能力字段
// 保留、PrimaryModel 等类型判定正确。
func TestAIModelCapabilitiesInProviderConfig(t *testing.T) {
	raw := `[{"id":"p1","name":"P1","kind":"openai_compatible","base_url":"https://x.example/v1","enabled":true,"models":[
		{"id":"m-no-kind","capabilities":{"vision":true}},
		{"id":"emb-m","capabilities":{"kind":"embedding"}},
		{"id":"rr-m","capabilities":{"kind":"rerank","vision":true}}
	]}]`
	var providers []AIProvider
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}
	p := providers[0]
	eff := p.EffectiveModels()
	// 无 kind 存量条目归 chat，且 vision 能力保留（归一只动 kind）。
	if got := eff[0].Capabilities.Kind; got != AIModelKindChat {
		t.Fatalf("no-kind model kind = %q, want chat", got)
	}
	if !eff[0].Capabilities.IsChat() || !eff[0].Capabilities.Vision {
		t.Fatalf("no-kind model after normalize: %+v", eff[0].Capabilities)
	}
	if !eff[1].Capabilities.IsEmbedding() || !eff[2].Capabilities.IsRerank() {
		t.Fatalf("explicit kinds lost: %+v %+v", eff[1].Capabilities, eff[2].Capabilities)
	}
	// PrimaryModel（首个 chat 类型模型）行为保持。
	if got := p.PrimaryModel(); got != "m-no-kind" {
		t.Fatalf("PrimaryModel = %q, want m-no-kind", got)
	}
	// 二次序列化（写库）单形态：kind 恒在，无旧布尔键。
	out, err := json.Marshal(p.EffectiveModels())
	if err != nil {
		t.Fatalf("marshal models: %v", err)
	}
	for _, want := range []string{`"kind":"chat"`, `"kind":"embedding"`, `"kind":"rerank"`, `"vision":true`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output missing %s: %s", want, out)
		}
	}
	for _, legacy := range []string{`"chat":true`, `"embedding":true`, `"rerank":true`} {
		if strings.Contains(string(out), legacy) {
			t.Fatalf("output must not contain legacy boolean %s: %s", legacy, out)
		}
	}
}

// TestValidateAIProviderModelKind ValidateAIProvider 拒绝 capabilities.kind
// 白名单外的取值（空 kind 合法——默认 chat 由消费路径归一）。
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
	p.Models[0].Capabilities.Kind = ""
	if err := ValidateAIProvider(p); err != nil {
		t.Fatalf("empty kind (defaults to chat) must pass validation: %v", err)
	}
}
