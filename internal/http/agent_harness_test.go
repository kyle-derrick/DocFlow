// Package http —— agent_harness_test.go：agent.harness 终值解析测试
// （任务创建时 resolveAgentHarness 的纯函数核心 agentHarnessTerminal）：
// auto 按平台默认对话 Provider 协议路由（anthropic→claude-code、openai
// 兼容→pi）、显式值原样（不触发解析）、解析失败/未知 kind（含 mock）→
// builtin 兜底。
package http

import (
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
