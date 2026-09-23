// Package mcp —— tools_ai.go：AI 能力工具（AI 能力第一版）。
//
// 三个工具走同一 ChatService（多 Provider，scope ai:chat；JWT 不受限，
// PAT 须显式授权 ai:chat）：
//   - ask_docs：query → RAG-lite 检索增强问答（来源带 /view/{id} 链接）；
//   - summarize_file：fileId → 全类型内容抽取 + AI 摘要；
//   - ai_chat：通用多轮对话（messages 数组直传）。
package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
)

// ScopeAIChat 为 AI 工具的 PAT scope（与 auth.ScopeAIChat 同值）。
const ScopeAIChat = "ai:chat"

// AIAssistant 抽象 AI 能力（生产实现经 http 层以 ai.Service 适配注入；
// 接口化便于单测注入内存实现）。
type AIAssistant interface {
	// Enabled 表示 AI 能力当前可用（总开关 + 存在启用中的 Provider）；
	// false 时 AI 工具不出现在 tools 列表（调用已注册工具返回不可用）。
	Enabled() bool
	// AskDocs 检索增强问答（返回答案与来源列表）；topK<=0 用默认，
	// spaceID 非 nil 限定检索空间。
	AskDocs(ctx context.Context, user uuid.UUID, query string, topK int, spaceID *uuid.UUID) (string, []ai.Source, error)
	// SummarizeFile 抽取文件内容并生成摘要（读授权由实现保证）。
	SummarizeFile(ctx context.Context, user, fileID uuid.UUID) (string, error)
	// Chat 通用对话补全（非流式）。
	Chat(ctx context.Context, user uuid.UUID, messages []ai.Message) (string, error)
}

func aiAvailable(d *Deps) bool { return d.AI != nil && d.AI.Enabled() }

// aiTools 返回 AI 工具清单（名称无 df_ 前缀——与任务书一致，便于 agent
// 按通用语义发现；scope 均为 ai:chat）。
func aiTools() []Tool {
	return []Tool{
		{
			Name:        "ask_docs",
			Description: "检索增强问答（RAG-lite）：在当前用户可见的空间中全文检索与 query 相关的文件，基于命中片段生成带来源链接（/view/{file_id}）的回答。可选 space_id 限定空间、top_k 控制召回条数（默认 8，上限 10）。需部署启用 AI 与搜索。",
			Scope:       ScopeAIChat,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"query":    map[string]any{"type": "string", "description": "问题或检索关键词（必填）"},
				"space_id": map[string]any{"type": "string", "description": "限定检索的空间 ID（UUID，可选；缺省 = 全部可见空间）"},
				"top_k":    map[string]any{"type": "integer", "description": "召回条数上限（可选，1-10；缺省 8）"},
			}, "required": []string{"query"}},
			Available: aiAvailable,
			Handler:   toolAskDocs,
		},
		{
			Name:        "summarize_file",
			Description: "生成文件的 AI 摘要：支持 md/txt/源码、.dfdoc 富文本、drawio、excalidraw、docx/xlsx/pptx、pdf（简单字体）等类型的全文抽取；不支持的类型按元信息生成占位摘要。需部署启用 AI。",
			Scope:       ScopeAIChat,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"file_id": map[string]any{"type": "string", "description": "文件 file_id（UUID）"},
			}, "required": []string{"file_id"}},
			Available: aiAvailable,
			Handler:   toolSummarizeFile,
		},
		{
			Name:        "ai_chat",
			Description: "通用 AI 对话（非流式）：把 messages 数组（role/content）交由系统配置的默认 Provider 补全，适合自由问答与文本加工。",
			Scope:       ScopeAIChat,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"messages": map[string]any{"type": "array", "items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"role":    map[string]any{"type": "string", "enum": []string{"user", "assistant", "system"}},
						"content": map[string]any{"type": "string"},
					},
					"required": []string{"role", "content"},
				}},
			}, "required": []string{"messages"}},
			Available: aiAvailable,
			Handler:   toolAIChat,
		},
	}
}

func toolAskDocs(ctx context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		Query   string `json:"query"`
		SpaceID string `json:"space_id"`
		TopK    int    `json:"top_k"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return nil, badArgs("query is required")
	}
	var spaceID *uuid.UUID
	if raw := strings.TrimSpace(a.SpaceID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return nil, badArgs("space_id must be a UUID")
		}
		spaceID = &id
	}
	answer, sources, err := deps.AI.AskDocs(ctx, id.UserID, q, a.TopK, spaceID)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(sources))
	for _, s := range sources {
		items = append(items, map[string]any{"file_id": s.FileID, "name": s.Name, "url": s.URL})
	}
	return map[string]any{"query": q, "answer": answer, "sources": items, "count": len(items)}, nil
}

func toolSummarizeFile(ctx context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		FileID string `json:"file_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	fileID, err := parseUUIDField("file_id", a.FileID)
	if err != nil {
		return nil, err
	}
	summary, err := deps.AI.SummarizeFile(ctx, id.UserID, fileID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"file_id": fileID, "summary": summary}, nil
}

func toolAIChat(ctx context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error) {
	var a struct {
		Messages []ai.Message `json:"messages"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if len(a.Messages) == 0 {
		return nil, badArgs("messages must not be empty")
	}
	for _, m := range a.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			return nil, badArgs("message role must be user/assistant/system")
		}
		if strings.TrimSpace(m.Content) == "" {
			return nil, badArgs("message content must not be empty")
		}
	}
	answer, err := deps.AI.Chat(ctx, id.UserID, a.Messages)
	if err != nil {
		return nil, err
	}
	return map[string]any{"answer": answer}, nil
}
