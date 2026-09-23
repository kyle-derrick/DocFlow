// Package ai —— summarize.go：文件 AI 摘要编排（v1 新端点 /api/v1/ai/summarize
// 与 MCP summarize_file 共用）。
//
// 内容抽取走 ExtractText（全类型）；不支持全文抽取的二进制类型回退
// 「标题 + 元信息 + 描述」占位（如实标注），再交 ChatService 生成摘要。
// 旧端点 /files/:id/ai/summary 保留 env 单 Provider 直连行为（兼容既有
// 部署与契约），新链路统一走本编排。
package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/settings"
)

// summarizeV2SystemPrompt 为 v1 摘要编排（/ai/summarize 与 MCP）的系统
// 提示（跟随文件语言、结构化输出；旧端点沿用 ai.go 的单 Provider 提示）。
const summarizeV2SystemPrompt = "你是文档摘要助手。请用与文件内容相同的语言输出摘要：先一句话概括，再列 3-6 条要点；总长 300 字内。不要输出与摘要无关的内容。"

// FileMeta 为摘要所需的文件元信息。
type FileMeta struct {
	ID          uuid.UUID
	Name        string
	Description string
	MimeType    string
	Size        int64
}

// FileSource 抽象「带读授权的文件内容读取」（生产实现由 http/mcp 层以
// files.Store + upload.Storage 适配；接口化便于单测注入内存实现）。
type FileSource interface {
	// FileWithContent 返回文件元信息与当前版本内容（≤MaxExtractBytes）；
	// 授权语义与 files.Get/CurrentVersion 一致（越权返回 not found）。
	FileWithContent(user, fileID uuid.UUID) (FileMeta, []byte, error)
}

// SummarizeFile 抽取文件文本并生成 AI 摘要（非流式；流式增量经 onDelta
// 透传，nil = 纯非流式）。返回摘要与结果元数据。
func (s *Service) SummarizeFile(ctx context.Context, user uuid.UUID, src FileSource, fileID uuid.UUID, onDelta func(string)) (string, ChatResult, error) {
	return s.SummarizeFileOpt(ctx, user, src, fileID, false, onDelta)
}

// SummarizeFileOpt 为 SummarizeFile 的带 think 版本（/ai/summarize 的
// think 请求参数；模型不具备 reasoning 能力时 Chat 内静默忽略）。
func (s *Service) SummarizeFileOpt(ctx context.Context, user uuid.UUID, src FileSource, fileID uuid.UUID, think bool, onDelta func(string)) (string, ChatResult, error) {
	meta, data, err := src.FileWithContent(user, fileID)
	if err != nil {
		return "", ChatResult{}, err
	}
	userContent := summarizeUserContent(meta, data)
	res, err := s.Chat(ctx, ChatRequest{
		Scenario: settings.AIScenarioSummary,
		Messages: []Message{{Role: "user", Content: userContent}},
		System:   summarizeV2SystemPrompt,
		Think:    think,
		Stream:   onDelta != nil,
	}, onDelta)
	if err != nil {
		return "", res, err
	}
	return res.Content, res, nil
}

// summarizeUserContent 拼装摘要输入：文件名 + 抽取文本（或占位元信息）。
func summarizeUserContent(meta FileMeta, data []byte) string {
	var body string
	if text, ok := ExtractText(meta.Name, meta.MimeType, data); ok {
		body = truncateRunes(text, MaxExtractRunes)
	} else {
		// 二进制类型（图片/音视频/压缩包等）：占位摘要输入，如实标注。
		body = fmt.Sprintf("（该文件类型暂不支持全文抽取，仅有元信息）\n文件名：%s\n大小：%d 字节\n", meta.Name, meta.Size)
		if desc := strings.TrimSpace(meta.Description); desc != "" {
			body += "描述：" + desc + "\n"
		}
		body += "请基于以上元信息生成简短说明性摘要。"
	}
	return "文件名：" + meta.Name + "\n\n" + body
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "\n…（内容过长已截断）"
}

// PromptContextPerFileRunes 单文件拼入对话上下文的字符上限（多文件场景
// 控制总上下文规模）。
const PromptContextPerFileRunes = 8000

// TruncateForPrompt 把抽取文本截断到单文件上下文上限。
func TruncateForPrompt(s string) string {
	return truncateRunes(s, PromptContextPerFileRunes)
}

// TestProvider 连接测试：发送一条 ping 消息，返回往返延迟与错误。
func (s *Service) TestProvider(ctx context.Context, providerID string) (int64, error) {
	res, err := s.Chat(ctx, ChatRequest{
		ProviderID: providerID,
		Messages:   []Message{{Role: "user", Content: "ping"}},
		System:     "Reply with the single word: pong",
		MaxTokens:  32,
	}, nil)
	return res.DurationMS, err
}
