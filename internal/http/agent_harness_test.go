// Package http —— agent_harness_test.go：agent.harness 终值解析测试
// （任务创建时 resolveAgentHarness 的纯函数核心 agentHarnessTerminal）：
// auto 按平台默认对话 Provider 协议路由（anthropic→claude-code、openai
// 兼容→pi）、显式值原样（不触发解析）、解析失败/未知 kind（含 mock）→
// builtin 兜底。
package http

import (
	"encoding/json"
	"testing"

	"github.com/docflow/docflow/internal/agent"
	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/settings"
)

// TestAgentHarnessTerminal harness 终值解析矩阵。
func TestAgentHarnessTerminal(t *testing.T) {
	kind := func(k string) func() (string, error) {
		return func() (string, error) { return k, nil }
	}
	cases := []struct {
		name       string
		configured string
		resolve    func() (string, error)
		want       string
	}{
		{"auto×anthropic→claude-code", "auto", kind(settings.AIKindAnthropic), agent.HarnessClaudeCode},
		{"auto×openai→pi", "auto", kind(settings.AIKindOpenAICompatible), agent.HarnessPi},
		{"缺省（空）按 auto×anthropic", "", kind(settings.AIKindAnthropic), agent.HarnessClaudeCode},
		{"显式 pi 原样", "pi", kind(settings.AIKindAnthropic), agent.HarnessPi},
		{"显式 claude-code 原样", "claude-code", kind(settings.AIKindOpenAICompatible), agent.HarnessClaudeCode},
		{"显式 builtin 原样", "builtin", kind(settings.AIKindAnthropic), agent.HarnessBuiltin},
		{"解析失败→builtin", "auto", func() (string, error) { return "", ai.ErrNoProvider }, agent.HarnessBuiltin},
		{"未知 kind（mock）→builtin", "auto", kind(settings.AIKindMock), agent.HarnessBuiltin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentHarnessTerminal(tc.configured, tc.resolve); got != tc.want {
				t.Fatalf("agentHarnessTerminal(%q) = %q, want %q", tc.configured, got, tc.want)
			}
		})
	}
}

// TestAgentHarnessTerminalExplicitSkipsResolve 显式值不触发 kind 解析
// （auto 语义仅存在于配置层）。
func TestAgentHarnessTerminalExplicitSkipsResolve(t *testing.T) {
	called := false
	resolve := func() (string, error) {
		called = true
		return settings.AIKindAnthropic, nil
	}
	if got := agentHarnessTerminal("claude-code", resolve); got != agent.HarnessClaudeCode || called {
		t.Fatalf("explicit value must pass through without resolving kind: got=%q called=%v", got, called)
	}
}

// TestValidAgentHarness 请求级 harness 校验矩阵：仅具体引擎合法；auto/
// 空串/未知值非法（空由调用方在解析前区分，auto 不接受——跟随平台应
// 直接不传）。
func TestValidAgentHarness(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{agent.HarnessClaudeCode, true},
		{agent.HarnessPi, true},
		{agent.HarnessBuiltin, true},
		{" pi ", true},
		{agent.HarnessAuto, false},
		{"", false},
		{"docker", false},
	}
	for _, tc := range cases {
		if got := validAgentHarness(tc.in); got != tc.want {
			t.Fatalf("validAgentHarness(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestAgentTaskHarnessPriority 请求级 harness 优先级：请求携带具体引擎
// 时原样优先（覆盖平台显式配置与 auto 路由，且不触发 kind 解析）；请求
// 为空时回落平台配置解析（既有 agentHarnessTerminal 语义）。
func TestAgentTaskHarnessPriority(t *testing.T) {
	kind := func(k string) func() (string, error) {
		return func() (string, error) { return k, nil }
	}
	// 请求级覆盖平台显式配置（平台 pi，请求 claude-code）。
	if got := agentTaskHarness(agent.HarnessClaudeCode, agent.HarnessPi, kind(settings.AIKindAnthropic)); got != agent.HarnessClaudeCode {
		t.Fatalf("request-level harness must win: got=%q", got)
	}
	// 请求级 builtin 同样优先（用户显式选择内置 runner）。
	if got := agentTaskHarness(agent.HarnessBuiltin, "auto", kind(settings.AIKindAnthropic)); got != agent.HarnessBuiltin {
		t.Fatalf("request-level builtin must win: got=%q", got)
	}
	// 请求级不触发 kind 解析（调用方已校验具体值）。
	called := false
	if got := agentTaskHarness(agent.HarnessPi, "auto", func() (string, error) { called = true; return settings.AIKindAnthropic, nil }); got != agent.HarnessPi || called {
		t.Fatalf("request-level harness must skip kind resolve: got=%q called=%v", got, called)
	}
	// 空请求回落平台配置：auto×openai→pi。
	if got := agentTaskHarness("", "auto", kind(settings.AIKindOpenAICompatible)); got != agent.HarnessPi {
		t.Fatalf("empty request must fall back to platform config: got=%q", got)
	}
	// 空白请求视为未携带；平台显式 claude-code 原样。
	if got := agentTaskHarness("   ", agent.HarnessClaudeCode, kind(settings.AIKindOpenAICompatible)); got != agent.HarnessClaudeCode {
		t.Fatalf("blank request must fall back to explicit platform config: got=%q", got)
	}
}

// TestAgentTaskModelRefUnmarshal 请求级 model 三态解析：对象（camelCase
// 对齐 /ai/chat 传法）、对象（snake_case）、单字符串（"provider/model"
// 与裸 model）、null/空串（= 未指定零值）。
func TestAgentTaskModelRefUnmarshal(t *testing.T) {
	cases := []struct {
		name             string
		in               string
		providerID       string
		modelID          string
		mustUnmarshalErr bool
	}{
		{"对象 camelCase（对齐 /ai/chat）", `{"providerId":"prov-1","modelId":"m-1"}`, "prov-1", "m-1", false},
		{"对象 snake_case", `{"provider_id":"prov-2","model_id":"m-2"}`, "prov-2", "m-2", false},
		{"字符串 provider/model", `"prov-3/m-3"`, "prov-3", "m-3", false},
		{"裸 model 字符串", `"m-4"`, "", "m-4", false},
		{"null", `null`, "", "", false},
		{"空串", `""`, "", "", false},
		{"空对象", `{}`, "", "", false},
		{"非法类型（数字数组）", `[1,2]`, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m agentTaskModelRef
			err := json.Unmarshal([]byte(tc.in), &m)
			if tc.mustUnmarshalErr {
				if err == nil {
					t.Fatalf("unmarshal(%s) 应报错，得到 %+v", tc.in, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal(%s): %v", tc.in, err)
			}
			if m.ProviderID != tc.providerID || m.ModelID != tc.modelID {
				t.Fatalf("unmarshal(%s) = {%q %q}, want {%q %q}", tc.in, m.ProviderID, m.ModelID, tc.providerID, tc.modelID)
			}
		})
	}
}
