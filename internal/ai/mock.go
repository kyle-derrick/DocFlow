// Package ai —— mock.go：内置 Mock Provider（Kind=mock）。
//
// 无真实 API Key 的开发/测试/演示环境专用：在管理端 AI 设置里新建一个
// 类型为 Mock 的 Provider 即可全链路演示（对话/摘要/RAG/编辑器 AI/MCP）。
//   - 普通对话：echo 型回复（复述最后一条用户消息 + 说明性尾注）；
//   - 摘要请求（用户内容以「文件名：」前缀标记，见 summarize 流程）：
//     返回模板摘要（含文件名与内容统计）；
//   - RAG 请求（上下文含 /view/{uuid} 链接）：按系统提示的引用要求回显
//     「来源：」段落，演示来源链接链路。
//
// 输出按小块流式回调（~20ms/块）模拟打字机效果。
package ai

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/settings"
)

// mockChunkDelay 流式块间隔（模拟网络打字机节奏）。
const mockChunkDelay = 20 * time.Millisecond

// chatMock 生成 mock 回复并流式回调。
func (s *Service) chatMock(_ context.Context, p settings.AIProvider, req ChatRequest, _ float64, _ int, onDelta func(string)) (ChatResult, error) {
	var lastUser string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Content
			break
		}
	}
	var b strings.Builder
	switch {
	case strings.HasPrefix(lastUser, "文件名："):
		b.WriteString(mockSummarize(lastUser, req.System))
	case strings.Contains(req.System, "mermaid") || strings.Contains(req.System, "Mermaid"):
		b.WriteString(mockMermaid(lastUser))
	default:
		b.WriteString("（Mock AI 回复）你刚才说：「" + firstLine(lastUser) + "」。\n\n这是一条来自内置 Mock Provider 的演示回复，用于在没有真实 API Key 的环境中验证流式渲染、来源引用与用量记录等全链路能力。")
		if sources := mockSources(req.System, lastUser); sources != "" {
			b.WriteString("\n\n" + sources)
		}
	}
	text := b.String()
	// 按可见字符切块流式输出（中文按 rune 切，保持 UTF-8 完整）。
	runes := []rune(text)
	chunk := 6
	for i := 0; i < len(runes); i += chunk {
		end := i + chunk
		if end > len(runes) {
			end = len(runes)
		}
		if onDelta != nil {
			onDelta(string(runes[i:end]))
		}
		time.Sleep(mockChunkDelay)
	}
	return ChatResult{Content: text, PromptTokens: len(lastUser) / 4, CompletionTokens: len(text) / 4}, nil
}

// mockSummarize 生成模板摘要（输入形如「文件名：xxx\n\n<内容>」）。
func mockSummarize(userContent, _ string) string {
	name, content := userContent, ""
	if idx := strings.Index(userContent, "\n"); idx >= 0 {
		name = strings.TrimSpace(userContent[:idx])
		content = userContent[idx+1:]
	}
	name = strings.TrimPrefix(strings.TrimSpace(name), "文件名：")
	lines := strings.Count(content, "\n") + 1
	chars := len([]rune(content))
	var head string
	for _, line := range strings.Split(content, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			head = t
			break
		}
	}
	if len([]rune(head)) > 40 {
		head = string([]rune(head)[:40]) + "…"
	}
	var b strings.Builder
	b.WriteString("【Mock 摘要】文件「" + name + "」共 " + strconv.Itoa(lines) + " 行、" + strconv.Itoa(chars) + " 字符。")
	if head != "" {
		b.WriteString("内容开头为：「" + head + "」。")
	}
	b.WriteString("\n\n要点：\n1. 该摘要由内置 Mock Provider 生成，用于演示 AI 摘要卡片与用量统计链路；\n2. 配置真实 Provider（OpenAI 兼容 / Anthropic）后，同一入口将返回模型生成的真实摘要。")
	return b.String()
}

// mockMermaid 生成演示用 mermaid 流程图代码（drawio AI 生成入口演示）。
func mockMermaid(prompt string) string {
	label := strings.TrimSpace(prompt)
	if idx := strings.IndexAny(label, "\n。！!？?"); idx > 0 {
		label = label[:idx]
	}
	if len([]rune(label)) > 16 {
		label = string([]rune(label)[:16])
	}
	if label == "" {
		label = "流程"
	}
	return "graph TD\n  A[开始] --> B[" + label + "]\n  B --> C{条件判断}\n  C -- 是 --> D[执行主流程]\n  C -- 否 --> E[异常处理]\n  D --> F[结束]\n  E --> F"
}

// mockSources 从上下文提取 /view/{uuid} 引用并生成「来源：」段落。
func mockSources(parts ...string) string {
	const prefix = "/view/"
	seen := make(map[string]bool)
	var names []string
	for _, part := range parts {
		for {
			idx := strings.Index(part, prefix)
			if idx < 0 {
				break
			}
			rest := part[idx+len(prefix):]
			end := 0
			for end < len(rest) && isHexRune(rune(rest[end])) {
				end++
			}
			id := rest[:end]
			if len(id) == 36 && !seen[id] {
				seen[id] = true
				names = append(names, "文件 "+id)
			}
			part = rest
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "来源：" + strings.Join(names, "、")
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-'
}

func firstLine(s string) string {
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[:idx]
	}
	if len([]rune(s)) > 60 {
		s = string([]rune(s)[:60]) + "…"
	}
	return s
}
