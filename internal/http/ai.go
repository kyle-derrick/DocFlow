package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/upload"
)

// aiSummarizer 抽象 AI 摘要客户端（SetAI 注入；生产实现 *ai.Client）：
// Enabled=false 或未注入时端点 503 AI_DISABLED。
type aiSummarizer interface {
	Enabled() bool
	Summarize(ctx context.Context, text, filename string) (string, error)
}

// aiFileSource 抽象 AI 摘要所需的文件读取源（NewHandler 以 *files.Store 装配，
// 接口化便于单测注入内存实现；读授权复用 files.Get/CurrentVersion 的
// authorizeFileAccess 语义）。
type aiFileSource interface {
	Get(user, fileID uuid.UUID) (files.File, error)
	CurrentVersion(user, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
}

// fileAISummary POST /api/v1/files/:id/ai/summary：生成文件当前版本内容的
// AI 摘要（OpenAI 兼容 /chat/completions）。
//   - 读权限：authorizeFileAccess 同规则（个人 owner / 团队在册成员 + 路径级 ACL）；
//   - 文本类判定复用 search.IsTextIndexable 语义（mime text/*、json/xml、
//     扩展名白名单），非文本 400；
//   - 当前版本 blob 须 available（其余状态 409）；
//   - 内容 >100KB 拒绝 413 CONTENT_TOO_LARGE（不截断，避免误导性摘要）；
//   - AI 禁用 503 AI_DISABLED；上游失败（含超时）502；
//   - 高频端点：不记录审计。
func (h *Handler) fileAISummary(c *gin.Context) {
	if h.ai == nil || !h.ai.Enabled() || h.aiFiles == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai summary is disabled", "code": "AI_DISABLED"})
		return
	}
	id, ok := parseID(c, c.Param("id"))
	if !ok {
		return
	}
	user := userID(c)
	f, err := h.aiFiles.Get(user, id)
	if h.fileError(c, err) {
		return
	}
	if f.Type != "file" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a text file", "code": "NOT_TEXT_FILE"})
		return
	}
	_, blob, err := h.aiFiles.CurrentVersion(user, id)
	if h.fileError(c, err) {
		return
	}
	if blob.Status != files.BlobStatusAvailable {
		c.JSON(http.StatusConflict, gin.H{"error": "file version is not available"})
		return
	}
	if !search.IsTextIndexable(blob.MimeType, f.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "not a text file", "code": "NOT_TEXT_FILE"})
		return
	}
	if blob.Size > ai.MaxInputBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "content too large for ai summary", "code": "CONTENT_TOO_LARGE"})
		return
	}
	text, err := readAISummaryInput(h.storage, blob.StorageKey)
	if err != nil {
		if errors.Is(err, ai.ErrTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "content too large for ai summary", "code": "CONTENT_TOO_LARGE"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to read file content"})
		return
	}
	summary, err := h.ai.Summarize(c.Request.Context(), text, f.Name)
	switch {
	case err == nil:
	case errors.Is(err, ai.ErrTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "content too large for ai summary", "code": "CONTENT_TOO_LARGE"})
		return
	case errors.Is(err, ai.ErrDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai summary is disabled", "code": "AI_DISABLED"})
		return
	case errors.Is(err, ai.ErrUpstream):
		c.JSON(http.StatusBadGateway, gin.H{"error": "ai upstream request failed"})
		return
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai summary failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary})
}

// readAISummaryInput 读取 blob 内容（防御性限流读，超限按内容过长拒绝）。
func readAISummaryInput(storage upload.Storage, key string) (string, error) {
	r, err := storage.Read(key)
	if err != nil {
		return "", err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, ai.MaxInputBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > ai.MaxInputBytes {
		return "", ai.ErrTooLarge
	}
	return string(data), nil
}

// ==================== AI 能力第一版（/ai/* 与 /admin/settings/ai） ====================

// aiChatRequest 为 POST /api/v1/ai/chat 的请求体。
type aiChatRequest struct {
	ProviderID string `json:"providerId"`
	// Model 为可选模型指定（providerId+modelId）：须归属启用中的 Provider
	// 且具备 chat 能力，否则 400 AI_MODEL_NOT_ALLOWED；未传用场景默认。
	Model *struct {
		ProviderID string `json:"providerId"`
		ModelID    string `json:"modelId"`
	} `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream *bool `json:"stream"`
	// Think 开启推理思考（默认 false）：仅当所选模型 capabilities.reasoning
	//=true 时向 Provider 透传（openai_compatible: reasoning_effort=medium；
	// anthropic: thinking budget_tokens=2048）；模型不支持时静默忽略。
	Think *bool `json:"think"`
	// WebSearch 开启联网搜索增强（默认 false）：须管理端配置
	// ai.search.provider（searxng/tavily），否则静默跳过；搜索结果注入
	// system 上下文，来源经 SSE meta 事件回传。与 context.query 的 RAG
	// 模式互斥（RAG 以知识库检索为主，忽略本参数）。
	WebSearch *bool `json:"web_search"`
	// UseMCP 开启外部 MCP 工具循环（默认 false）：须管理端配置启用中的
	// MCP 服务（ai.mcp），否则静默按普通对话继续；工具执行前经 SSE
	// tool 事件回传（type/label/server/tool）。与 web_search 可同开
	//（互不干扰：搜索结果照旧注入首轮 system）；RAG 模式忽略本参数。
	UseMCP *bool `json:"use_mcp"`
	// UseFiles 开启内置平台文件工具（默认 true——用户至少有一个空间即
	// 可用；显式 false 关闭）：注入 df_* 工具（列目录/读/写/建目录/搜索，
	// 相对路径基于用户默认空间根=工作目录），与 use_mcp 工具合并进同一
	// 工具循环；每次执行经 SSE tool 事件回传并写 ai.tool 审计。依赖
	//（文件源/上传/存储/空间服务）未装配时静默跳过。RAG 模式忽略本参数。
	UseFiles *bool `json:"use_files"`
	// IncludeMemory 开启用户长期记忆注入（默认 false）：取本人最近 20 条
	// ai_memory（手动维护，migration 050）拼入 system 上下文「以下是用户
	// 的长期偏好记忆…」；未配置/无记忆静默跳过。
	IncludeMemory *bool `json:"include_memory"`
	// IncludeDocs 开启「我的文件」检索（默认 false）：普通对话模式下以
	// 最后一条 user 消息为查询，检索本人可见文档（关键词/混合按平台
	// RAG 配置，rerank 生效）注入 system 上下文，来源经 sources 事件与
	// 响应字段回传；检索失败/无命中静默跳过（对话不中断）。RAG 模式
	//（context.query）忽略——已有知识库问答语义。
	IncludeDocs *bool `json:"include_docs"`
	// WorkRoot 工作目录 folderID（可选）：use_files 工具相对路径的解析
	// 基准；空/非法回落用户默认空间根。
	WorkRoot *string `json:"work_root"`
	// Context 为可选上下文：query 触发检索增强（RAG-lite）；fileIds 把
	// 指定文件抽取文本拼入上下文（两者可并存，RAG 优先拼装）。
	Context *struct {
		Query   string   `json:"query"`
		FileIDs []string `json:"fileIds"`
	} `json:"context"`
}

// sseWriter 为 SSE 事件写出器（每事件即时 Flush）。
type sseWriter struct {
	c *gin.Context
}

func (w sseWriter) event(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = w.c.Writer.WriteString(fmt.Sprintf("event: %s\ndata: %s\n\n", name, data))
	w.c.Writer.Flush()
}

// aiRequireService 返回注入且已启用的 AI 服务；未注入或未启用（无可用
// Provider / ai.enabled=false）时 404（AI 能力对客户端完全不存在）并返回
// nil——与「全站隐藏 AI 入口」的前端语义一致。
func (h *Handler) aiRequireService(c *gin.Context) *ai.Service {
	if h.aiSvc == nil || !h.aiSvc.EnabledNow() {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return nil
	}
	return h.aiSvc
}

// aiStatus GET /api/v1/ai/status：登录用户读取 AI 能力可用性（前端据此
// 显隐全站 AI 入口与各能力入口）。本端点不随开关 404（否则无法区分
// 「关闭」与「未部署」）。响应为能力标志对象：
//   - enabled：总开关（ai.enabled + 存在启用中的 Provider）；
//   - agent：Agent 创作舱（agentEnabled 新语义：默认开启，受 AI 总开关约束）；
//   - web_search：联网搜索（管理端配置 ai.search.provider 非空）；
//   - mcp：外部 MCP 工具（ai.mcp 存在启用中的服务；读取失败容错 false）；
//   - rag：知识库问答（引用文件基于关键词检索，AI 开即可用 = enabled）。
func (h *Handler) aiStatus(c *gin.Context) {
	enabled := h.aiSvc != nil && h.aiSvc.EnabledNow()
	webSearch := false
	if sc, ok := h.aiSearchConfig(); ok {
		webSearch = sc.Provider != ""
	}
	mcpEnabled := false
	if store := h.mcpStore(); store != nil {
		if list, err := store.AIMCPServices(); err == nil {
			for _, svc := range list {
				if svc.Enabled {
					mcpEnabled = true
					break
				}
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"enabled":    enabled,
		"agent":      h.agentEnabled(),
		"web_search": webSearch,
		"mcp":        mcpEnabled,
		"rag":        enabled,
	})
}

// aiFileContentSource 把 files.Store + upload.Storage 适配为 ai.FileSource
// （读授权复用 Get/CurrentVersion 的 authorizeFileAccess 语义）。
type aiFileContentSource struct {
	files   aiFileSource
	storage upload.Storage
}

// FileWithContent 实现 ai.FileSource：当前版本 available 才返回内容。
func (s aiFileContentSource) FileWithContent(user, fileID uuid.UUID) (ai.FileMeta, []byte, error) {
	f, err := s.files.Get(user, fileID)
	if err != nil {
		return ai.FileMeta{}, nil, err
	}
	_, blob, err := s.files.CurrentVersion(user, fileID)
	if err != nil {
		return ai.FileMeta{}, nil, err
	}
	if blob.Status != files.BlobStatusAvailable {
		return ai.FileMeta{}, nil, files.ErrNoVersion
	}
	meta := ai.FileMeta{ID: f.ID, Name: f.Name, Description: f.Description, MimeType: blob.MimeType, Size: blob.Size}
	if f.Type != "file" || blob.StorageKey == "" {
		return meta, []byte{}, nil
	}
	r, err := s.storage.Read(blob.StorageKey)
	if err != nil {
		return ai.FileMeta{}, nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, ai.MaxExtractBytes+1))
	if err != nil {
		return ai.FileMeta{}, nil, err
	}
	return meta, data, nil
}

// aiChat POST /api/v1/ai/chat（scope ai:chat；每用户限流按所选 Provider
// 的 requests_per_min 执行，未配置回落全局 ai.per_user_per_min，另有
// Provider 级 daily_quota 可选日限额）：SSE 流式对话（event: meta/
// sources/delta/done/error）；stream=false 时返回 JSON。context.query
// 触发检索增强（来源经 sources 事件与响应字段回传）；context.fileIds
// 把指定文件抽取文本拼入系统上下文。可选 model {providerId, modelId}
// 指定模型（须具备 chat 能力，未传用场景默认 chat 模型）。可选 think
// 开启推理思考（模型须 reasoning 能力）；可选 web_search 开启联网搜索
// （须管理端配置 ai.search.provider，来源经 meta 事件回传，失败静默降级）。
func (h *Handler) aiChat(c *gin.Context) {
	svc := h.aiRequireService(c)
	if svc == nil {
		return
	}
	var req aiChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	// 可选 model 参数（providerId/modelId 均可只填一项；modelId 缺省时
	// 回落请求顶层 providerId）。
	targetProvider := strings.TrimSpace(req.ProviderID)
	targetModel := ""
	if req.Model != nil {
		if id := strings.TrimSpace(req.Model.ProviderID); id != "" {
			targetProvider = id
		}
		targetModel = strings.TrimSpace(req.Model.ModelID)
	}
	messages := make([]ai.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := m.Role
		if role != "user" && role != "assistant" && role != "system" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid message role", "code": "INVALID_ROLE"})
			return
		}
		if strings.TrimSpace(m.Content) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "empty message content"})
			return
		}
		messages = append(messages, ai.Message{Role: role, Content: m.Content})
	}
	// RAG 模式（context.query 非空）以 query 为最终提问，messages 仅作
	// 历史可为空（「问 AI」首轮无历史）；普通对话模式要求 messages 非空。
	ragQuery := req.Context != nil && strings.TrimSpace(req.Context.Query) != ""
	if len(messages) == 0 && !ragQuery {
		c.JSON(http.StatusBadRequest, gin.H{"error": "messages are required"})
		return
	}
	stream := true
	if req.Stream != nil {
		stream = *req.Stream
	}
	think := req.Think != nil && *req.Think
	// web_search 仅普通对话模式生效（RAG 模式以知识库检索为主，忽略）。
	webSearchOn := req.WebSearch != nil && *req.WebSearch && !ragQuery
	// use_mcp 仅普通对话模式生效（RAG 模式忽略，语义同 web_search）。
	useMCP := req.UseMCP != nil && *req.UseMCP && !ragQuery
	user := userID(c)
	// 内置平台文件工具（use_files，默认 true；用户至少有一个空间即可用，
	// 前端可显式关闭）：装配 df_* 声明与执行器（依赖未装配/无空间时
	// ptc 为 nil，静默跳过）。RAG 模式忽略（与 use_mcp 同语义）。
	useFiles := (req.UseFiles == nil || *req.UseFiles) && !ragQuery
	var platformTools []ai.PlatformTool
	var toolExec ai.ToolExecutorFunc
	if useFiles {
		// work_root：前端可指定工作目录 folderID（相对路径解析基准）；
		// 空/零值/非 UUID 回落默认空间根。读授权在 base() 经 Get 校验。
		var workRoot uuid.UUID
		if req.WorkRoot != nil {
			if id, err := uuid.Parse(strings.TrimSpace(*req.WorkRoot)); err == nil {
				workRoot = id
			}
		}
		if ptc := h.newPlatformToolCtx(c, user, workRoot); ptc != nil {
			platformTools = ai.PlatformTools()
			toolExec = ptc.executePlatformTool
		}
	}
	filesToolsOn := len(platformTools) > 0
	// 用户长期记忆注入（include_memory，默认 false）：取本人最近 20 条
	// 手动记忆拼入 system 上下文；失败/无记忆静默跳过。
	memoryOn := req.IncludeMemory != nil && *req.IncludeMemory
	if memoryOn {
		if block := h.aiMemoryContextBlock(user); block != "" {
			messages = appendContextSystem(messages, block)
		}
	}
	userSvc := h.aiServiceFor(svc, user)

	// 先行解析目标（Provider + 模型，个人池感知：显式模型先查个人池后查
	// 平台池；未显式且 prefer_personal 时个人场景默认优先）：模型不属于
	// 两池任一启用 Provider / 无 chat 能力 → 400（越权拒绝）；随后按目标
	// 执行限流（个人池命中豁免 Provider 级限流与日配额，保留全局兜底）。
	target, perr := userSvc.ResolveChatTargetFor(targetProvider, targetModel, settings.AIScenarioChat)
	if perr != nil {
		aiErrorJSON(c, perr)
		return
	}
	provider, model := target.Provider, target.Model
	if !h.aiRateLimitForTarget(c, target) {
		return
	}

	// ai.chat 审计：每次请求完成记一条（成功/失败均记；SSE 逐 delta 不记；
	// 命中个人池附 personal:true——个人 Key 用户自担，豁免平台限流）。
	auditDone := func(status, providerID, usedModel string) {
		meta, _ := json.Marshal(map[string]any{
			"provider_id": providerID, "model": usedModel, "stream": stream, "rag": ragQuery,
			"think": think, "web_search": webSearchOn, "memory": memoryOn, "personal": target.Personal,
			"mcp": useMCP, "files": filesToolsOn, "docs": req.IncludeDocs != nil && *req.IncludeDocs && !ragQuery,
		})
		h.recordAudit(c, audit.Entry{
			UserID: &user, Action: audit.ActionAIChat, ResourceType: audit.ResourceAI,
			Status: status, Metadata: string(meta),
		})
	}

	// 上下文拼装：fileIds 抽取文本进 system；query 走 RAG-lite。
	var sources []ai.Source
	if req.Context != nil {
		if len(req.Context.FileIDs) > 0 {
			block, srcs, err := h.aiFileContextBlock(userSvc, user, req.Context.FileIDs)
			if err != nil {
				aiErrorJSON(c, err)
				return
			}
			if block != "" {
				messages = appendContextSystem(messages, block)
				sources = append(sources, srcs...)
			}
		}
	}

	// 「我的文件」检索（include_docs）：普通对话模式下以最后一条 user
	// 消息为查询，检索结果注入 system（rerank 生效），来源并入 sources
	// 随流式/非流式回传；失败/无命中静默跳过。RAG 模式忽略。
	if req.IncludeDocs != nil && *req.IncludeDocs && !ragQuery {
		if q := ai.LastUserQuery(messages); q != "" {
			if hits, rerr := userSvc.Retrieve(c.Request.Context(), user, q); rerr == nil && len(hits) > 0 {
				if block, srcs := ai.DocsContextBlock(hits); block != "" {
					messages = appendContextSystem(messages, block)
					sources = append(sources, srcs...)
				}
			}
		}
	}

	// 联网搜索增强（web_search）：须管理端配置 ai.search.provider，未配置
	// 静默跳过；查询取最后一条 user 消息（截 400 字）；失败静默降级（meta
	// 标注 search_failed，对话不中断）。
	var webSources []ai.WebSearchResult
	searchFailed := false
	if webSearchOn {
		if sc, ok := h.aiSearchConfig(); ok && sc.Provider != "" {
			if q := ai.LastUserQuery(messages); q != "" {
				messages, webSources, searchFailed = userSvc.ApplyWebSearch(c.Request.Context(), sc, q, messages)
			}
		}
	}

	// SSE meta 事件载荷：web_search 生效时附 sources（成功有结果）或
	// search_failed（失败降级标注）；未开启/未配置时与原结构一致。
	chatMeta := func() gin.H {
		meta := gin.H{"provider_id": provider.ID, "provider_name": provider.Name, "model": model}
		if webSearchOn {
			if searchFailed {
				meta["search_failed"] = true
			} else if len(webSources) > 0 {
				meta["sources"] = aiWebSourcesJSON(webSources)
			}
		}
		return meta
	}

	if req.Context != nil {
		if q := strings.TrimSpace(req.Context.Query); q != "" {
			// RAG：以 query 为最终提问，messages 作为历史；sources 在检索
			// 完成后、补全开始前经 onSources 下发（检索失败经 error 事件）。
			history := messages
			if !stream {
				answer, srcs, res, err := userSvc.AskDocsOpt(c.Request.Context(), user, q, ai.AskOptions{ProviderID: provider.ID, ModelID: model}, history, nil, nil)
				if err != nil {
					auditDone(audit.StatusFailure, "", "")
					aiErrorJSON(c, err)
					return
				}
				auditDone(audit.StatusSuccess, res.ProviderID, res.Model)
				c.JSON(http.StatusOK, gin.H{"content": answer, "sources": aiSourcesJSON(srcs), "provider_id": res.ProviderID, "provider_name": res.ProviderName, "model": res.Model, "usage": aiUsageJSON(res)})
				return
			}
			h.startAIStream(c)
			w := sseWriter{c}
			w.event("meta", gin.H{"provider_id": provider.ID, "provider_name": provider.Name, "model": model})
			_, _, res, err := userSvc.AskDocsOpt(c.Request.Context(), user, q, ai.AskOptions{ProviderID: provider.ID, ModelID: model}, history, func(s []ai.Source) {
				if len(s) > 0 {
					w.event("sources", gin.H{"sources": aiSourcesJSON(s)})
				}
			}, func(text string) {
				w.event("delta", gin.H{"text": text})
			})
			if err != nil {
				auditDone(audit.StatusFailure, provider.ID, model)
				w.event("error", gin.H{"error": "ai upstream request failed", "code": aiErrorCode(err)})
				return
			}
			auditDone(audit.StatusSuccess, res.ProviderID, res.Model)
			w.event("done", gin.H{"usage": aiUsageJSON(res), "content_length": len(res.Content)})
			return
		}
	}

	if !stream {
		res, err := userSvc.Chat(c.Request.Context(), ai.ChatRequest{ProviderID: provider.ID, Model: model, Messages: messages, Think: think, UseMCP: useMCP, PlatformTools: platformTools, ToolExecutor: toolExec, Stream: false}, nil)
		if err != nil {
			auditDone(audit.StatusFailure, "", "")
			aiErrorJSON(c, err)
			return
		}
		auditDone(audit.StatusSuccess, res.ProviderID, res.Model)
		resp := gin.H{"content": res.Content, "sources": aiSourcesJSON(sources), "provider_id": res.ProviderID, "provider_name": res.ProviderName, "model": res.Model, "usage": aiUsageJSON(res)}
		if webSearchOn {
			if searchFailed {
				resp["search_failed"] = true
			}
			if len(webSources) > 0 {
				resp["web_sources"] = aiWebSourcesJSON(webSources)
			}
		}
		c.JSON(http.StatusOK, resp)
		h.maybeAutoExtractMemory(c, lastUserMessage(messages), res.Content) // 记忆自动提取（后台，静默）
		return
	}
	h.startAIStream(c)
	w := sseWriter{c}
	w.event("meta", chatMeta())
	if len(sources) > 0 {
		w.event("sources", gin.H{"sources": aiSourcesJSON(sources)})
	}
	// 工具事件（use_mcp 外部 MCP 工具与 use_files 内置平台工具共用）：
	// 每次工具执行前发一行（照 meta.sources 的 SSE 事件机制，前端进度
	// 提示格式冻结；内置工具 server 固定 docflow，tool 字段按名透传）：
	//   event: tool / data: {"type":"tool","label":"<服务Name> / <工具name>","server":"<服务ID>","tool":"<工具name>"}
	onTool := func(serverID, serverName, toolName string) {
		w.event("tool", gin.H{"type": "tool", "label": serverName + " / " + toolName, "server": serverID, "tool": toolName})
	}
	res, err := userSvc.Chat(c.Request.Context(), ai.ChatRequest{
		ProviderID: provider.ID, Model: model, Messages: messages, Think: think, UseMCP: useMCP,
		PlatformTools: platformTools, ToolExecutor: toolExec, Stream: true, OnTool: onTool,
	}, func(text string) {
		w.event("delta", gin.H{"text": text})
	})
	if err != nil {
		auditDone(audit.StatusFailure, provider.ID, model)
		w.event("error", gin.H{"error": "ai upstream request failed", "code": aiErrorCode(err)})
		return
	}
	if len(sources) > 0 {
		w.event("sources", gin.H{"sources": aiSourcesJSON(sources)})
	}
	auditDone(audit.StatusSuccess, res.ProviderID, res.Model)
	w.event("done", gin.H{"usage": aiUsageJSON(res), "content_length": len(res.Content)})
	h.maybeAutoExtractMemory(c, lastUserMessage(messages), res.Content) // 记忆自动提取（后台，静默）
}

// appendContextSystem 把上下文块并入首条 system 消息（无则新建）。
func appendContextSystem(messages []ai.Message, block string) []ai.Message {
	for i := range messages {
		if messages[i].Role == "system" {
			messages[i].Content += "\n\n" + block
			return messages
		}
	}
	out := make([]ai.Message, 0, len(messages)+1)
	out = append(out, ai.Message{Role: "system", Content: block})
	return append(out, messages...)
}

// aiFileContextBlock 抽取 fileIds 文本拼装上下文块（每文件截断；越权文件
// 跳过）。返回块文本与实际纳入的来源列表。
func (h *Handler) aiFileContextBlock(_ *ai.Service, user uuid.UUID, fileIDs []string) (string, []ai.Source, error) {
	src := aiFileContentSource{files: h.aiFiles, storage: h.storage}
	if h.aiFiles == nil || h.storage == nil {
		return "", nil, nil
	}
	var b strings.Builder
	var sources []ai.Source
	for _, raw := range fileIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		meta, data, err := src.FileWithContent(user, id)
		if err != nil {
			continue // 不存在/越权/无版本：跳过（不泄露存在性）
		}
		text, ok := ai.ExtractText(meta.Name, meta.MimeType, data)
		if !ok {
			text = fmt.Sprintf("（%s 暂不支持全文抽取；描述：%s）", meta.Name, meta.Description)
		}
		b.WriteString(fmt.Sprintf("\n文件：%s（链接 /view/%s）\n%s\n", meta.Name, meta.ID.String(), ai.TruncateForPrompt(text)))
		sources = append(sources, ai.Source{FileID: meta.ID.String(), Name: meta.Name, URL: "/view/" + meta.ID.String()})
	}
	return b.String(), sources, nil
}

// startAIStream 写出 SSE 响应头（进入流式后错误一律经 event: error）。
func (h *Handler) startAIStream(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	c.Writer.Flush()
}

func aiSourcesJSON(sources []ai.Source) []gin.H {
	out := make([]gin.H, 0, len(sources))
	for _, s := range sources {
		out = append(out, gin.H{"file_id": s.FileID, "name": s.Name, "url": s.URL})
	}
	return out
}

// aiWebSourcesJSON 网络搜索来源视图（SSE meta.sources 与非流式 web_sources）。
func aiWebSourcesJSON(sources []ai.WebSearchResult) []gin.H {
	out := make([]gin.H, 0, len(sources))
	for _, s := range sources {
		out = append(out, gin.H{"title": s.Title, "url": s.URL})
	}
	return out
}

// aiSearchConfig 读取联网搜索生效配置（system_settings 的 ai.search.* 覆盖，
// 热读取；未装配 settings 服务或读取失败按未配置处理 = 静默跳过）。
func (h *Handler) aiSearchConfig() (settings.AISearchConfig, bool) {
	if h.settings == nil {
		return settings.AISearchConfig{}, false
	}
	cfg, _, err := h.settings.AIOverrides()
	if err != nil {
		return settings.AISearchConfig{}, false
	}
	return cfg.Search, true
}

func aiUsageJSON(res ai.ChatResult) gin.H {
	return gin.H{"prompt_tokens": res.PromptTokens, "completion_tokens": res.CompletionTokens, "duration_ms": res.DurationMS}
}

// aiErrorCode 把服务错误映射为机器可读 code。
func aiErrorCode(err error) string {
	switch {
	case errors.Is(err, ai.ErrNoProvider):
		return "AI_NOT_CONFIGURED"
	case errors.Is(err, ai.ErrProviderNotFound):
		return "AI_PROVIDER_NOT_FOUND"
	case errors.Is(err, ai.ErrModelNotAllowed):
		return "AI_MODEL_NOT_ALLOWED"
	case errors.Is(err, ai.ErrUpstreamChat), errors.Is(err, ai.ErrUpstream):
		return "AI_UPSTREAM"
	default:
		return "AI_ERROR"
	}
}

// aiErrorJSON 以标准 JSON 状态码返回服务错误（流未开始前）。
func aiErrorJSON(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ai.ErrNoProvider):
		c.JSON(http.StatusBadRequest, gin.H{"error": "no ai provider configured; ask the administrator to add one", "code": "AI_NOT_CONFIGURED"})
	case errors.Is(err, ai.ErrProviderNotFound):
		c.JSON(http.StatusBadRequest, gin.H{"error": "ai provider not found", "code": "AI_PROVIDER_NOT_FOUND"})
	case errors.Is(err, ai.ErrModelNotAllowed):
		c.JSON(http.StatusBadRequest, gin.H{"error": "ai model not allowed", "code": "AI_MODEL_NOT_ALLOWED"})
	case errors.Is(err, ai.ErrUpstreamChat), errors.Is(err, ai.ErrUpstream):
		c.JSON(http.StatusBadGateway, gin.H{"error": "ai upstream request failed"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai request failed"})
	}
}

// aiSummarize POST /api/v1/ai/summarize {fileId, stream?}：抽取全类型内容
// （office/pdf/drawio/excalidraw/dfdoc/文本），交 ChatService 生成摘要；
// stream=true 时 SSE 输出（event: meta/delta/done），否则 JSON。
func (h *Handler) aiSummarize(c *gin.Context) {
	svc := h.aiRequireService(c)
	if svc == nil {
		return
	}
	if h.aiFiles == nil || h.storage == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai service is not configured", "code": "AI_DISABLED"})
		return
	}
	var req struct {
		FileID string `json:"fileId"`
		Stream *bool  `json:"stream"`
		// Think 开启推理思考（默认 false）：仅当摘要场景默认模型勾选
		// capabilities.reasoning 时透传；模型不支持时静默忽略。
		Think *bool `json:"think"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.FileID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fileId is required"})
		return
	}
	fileID, ok := parseID(c, req.FileID)
	if !ok {
		return
	}
	user := userID(c)
	stream := req.Stream != nil && *req.Stream
	think := req.Think != nil && *req.Think
	src := aiFileContentSource{files: h.aiFiles, storage: h.storage}
	userSvc := h.aiServiceFor(svc, user)
	// 摘要场景默认模型（summary 回落 chat；个人池感知——prefer_personal
	// 且个人默认命中时用个人模型并豁免 Provider 级限流）的限流配置执行。
	if t, terr := userSvc.ResolveChatTargetFor("", "", settings.AIScenarioSummary); terr == nil {
		if !h.aiRateLimitForTarget(c, t) {
			return
		}
	}
	if stream {
		h.startAIStream(c)
		w := sseWriter{c}
		summary, res, err := userSvc.SummarizeFileOpt(c.Request.Context(), user, src, fileID, think, func(text string) {
			w.event("delta", gin.H{"text": text})
		})
		if err != nil {
			w.event("error", gin.H{"error": summarizeErrorMessage(err), "code": summarizeErrorCode(err)})
			return
		}
		w.event("meta", gin.H{"provider_id": res.ProviderID, "provider_name": res.ProviderName, "model": res.Model})
		w.event("done", gin.H{"summary": summary, "usage": aiUsageJSON(res)})
		return
	}
	summary, res, err := userSvc.SummarizeFileOpt(c.Request.Context(), user, src, fileID, think, nil)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": summarizeErrorMessage(err), "code": summarizeErrorCode(err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary, "provider_id": res.ProviderID, "provider_name": res.ProviderName, "model": res.Model, "usage": aiUsageJSON(res)})
}

func summarizeErrorCode(err error) string {
	if errors.Is(err, files.ErrNotFound) {
		return "FILE_NOT_FOUND"
	}
	return aiErrorCode(err)
}

func summarizeErrorMessage(err error) string {
	if errors.Is(err, ai.ErrUpstreamChat) || errors.Is(err, ai.ErrUpstream) {
		return "ai upstream request failed"
	}
	if errors.Is(err, files.ErrNotFound) {
		return "file not found"
	}
	return "ai summarize failed"
}

// ---------- 管理端：AI 设置 CRUD / 连接测试 / 用量统计 ----------

// aiProviderView 为管理端 Provider 视图（api_key 永不回显，仅报 configured；
// models 为生效模型列表——旧配置无 models 时按 base_url+model 合成单模型）。
type aiProviderView struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Kind             string             `json:"kind"`
	BaseURL          string             `json:"base_url"`
	Model            string             `json:"model"`
	Models           []settings.AIModel `json:"models"`
	Enabled          bool               `json:"enabled"`
	APIKeyConfigured bool               `json:"api_key_configured"`
	IsEnv            bool               `json:"is_env"`
	RequestsPerMin   int                `json:"requests_per_min"`
	DailyQuota       int                `json:"daily_quota"`
}

// aiSettingsResponse 为 GET/PUT /admin/settings/ai 的响应。
type aiSettingsResponse struct {
	Enabled         bool                           `json:"enabled"`
	Providers       []aiProviderView               `json:"providers"`
	DefaultProvider string                         `json:"default_provider"`
	DefaultModels   map[string]settings.AIModelRef `json:"default_models"`
	Temperature     float64                        `json:"temperature"`
	MaxTokens       int                            `json:"max_tokens"`
	PerUserPerMin   int                            `json:"per_user_per_min"`
	RAG             settings.AIRAGConfig           `json:"rag"`
	Search          aiSearchSettingsView           `json:"search"`
	// OCR 为图片 OCR 配置（ai.ocr 键整体块回显；非敏感，无密钥字段）。
	OCR settings.AIOCRConfig `json:"ocr"`
	// Personas 为平台人设（ai.personas 键，非敏感明文回显；保存并入
	// PUT /admin/settings/ai——载荷未带 = 保持现值，语义同 rag 块）。
	Personas []settings.AIPersonaDef `json:"personas"`
	Env      gin.H                   `json:"env"`
}

// aiPersonasOrEmpty 归一 nil 为空数组（JSON 输出 [] 而非 null）。
func aiPersonasOrEmpty(list []settings.AIPersonaDef) []settings.AIPersonaDef {
	if list == nil {
		return []settings.AIPersonaDef{}
	}
	return list
}

// aiSearchSettingsView 为联网搜索配置的管理端视图：tavily_api_key 为密钥
// （只写不读），仅回显 configured 标志，绝不回显明文。
type aiSearchSettingsView struct {
	Provider               string `json:"provider"`
	SearxngURL             string `json:"searxng_url"`
	MaxResults             int    `json:"max_results"`
	TavilyAPIKeyConfigured bool   `json:"tavily_api_key_configured"`
}

func aiSearchSettingsViewOf(cfg settings.AIConfig) aiSearchSettingsView {
	s := cfg.Search
	if s.MaxResults <= 0 {
		s.MaxResults = settings.AISearchMaxResultsDefault
	}
	return aiSearchSettingsView{Provider: s.Provider, SearxngURL: s.SearxngURL, MaxResults: s.MaxResults, TavilyAPIKeyConfigured: s.TavilyAPIKey != ""}
}

// aiEffectiveSettings 合并 env 基线与 DB 覆盖，产出管理端视图。
func (h *Handler) aiEffectiveSettings() (settings.AIConfig, error) {
	if h.settings == nil {
		return h.aiEnv, nil
	}
	db, _, err := h.settings.AIOverrides()
	if err != nil {
		return settings.AIConfig{}, err
	}
	if len(db.Providers) == 0 {
		db.Providers = h.aiEnv.Providers
	}
	if db.DefaultProvider == "" {
		db.DefaultProvider = h.aiEnv.DefaultProvider
	}
	if db.RAG.Mode == "" {
		db.RAG = h.aiEnv.RAG
	}
	return db, nil
}

func aiProviderViews(cfg settings.AIConfig) []aiProviderView {
	out := make([]aiProviderView, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		out = append(out, aiProviderView{
			ID: p.ID, Name: p.Name, Kind: p.Kind, BaseURL: p.BaseURL,
			Model: p.Model, Models: p.EffectiveModels(), Enabled: p.Enabled,
			APIKeyConfigured: p.APIKey != "", IsEnv: p.ID == "env",
			RequestsPerMin: p.RequestsPerMin, DailyQuota: p.DailyQuota,
		})
	}
	return out
}

// aiModels GET /api/v1/ai/models（登录即可，非管理员）：返回启用 Provider
// 的可用模型（含能力勾选）与场景默认模型。绝不返回 api_key/base_url 等
// 密钥类信息。AI 未启用时 404（与 /ai/chat 同语义）。双轨制：用户配置了
// 个人 Provider（user_ai_prefs）时，个人池模型条目追加在平台条目之后并
// 标记 personal:true、Provider 名后缀「（个人）」（模型选择器据此区分
// 双轨来源；显式选择个人条目时解析链先查个人池）。
func (h *Handler) aiModels(c *gin.Context) {
	if h.aiSvc == nil || !h.aiSvc.EnabledNow() {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	cfg, err := h.aiSvc.Config()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load ai config"})
		return
	}
	type modelView struct {
		ID           string                       `json:"id"`
		Label        string                       `json:"label,omitempty"`
		Capabilities settings.AIModelCapabilities `json:"capabilities"`
	}
	type providerView struct {
		ID       string      `json:"id"`
		Name     string      `json:"name"`
		Kind     string      `json:"kind"`
		Models   []modelView `json:"models"`
		Personal bool        `json:"personal,omitempty"`
	}
	providers := make([]providerView, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		effModels := p.EffectiveModels()
		models := make([]modelView, 0, len(effModels))
		for _, m := range effModels {
			models = append(models, modelView{ID: m.ID, Label: m.Label, Capabilities: m.Capabilities})
		}
		providers = append(providers, providerView{ID: p.ID, Name: p.Name, Kind: p.Kind, Models: models})
	}
	// 个人池合并（best-effort：读取/解析失败静默跳过，仅平台条目）。
	if prefs, ok := h.aiPersonalFor(c); ok {
		for _, p := range prefs.Providers {
			models := make([]modelView, 0, len(p.Models))
			for _, m := range p.Models {
				models = append(models, modelView{ID: m.ID, Label: m.Label, Capabilities: m.Capabilities})
			}
			providers = append(providers, providerView{ID: p.ID, Name: p.Name + "（个人）", Kind: p.Kind, Models: models, Personal: true})
		}
	}
	defaults := gin.H{}
	for _, scenario := range settings.AIScenarios {
		if scenario == settings.AIScenarioEmbedding {
			continue // embedding 场景不参与对话补全，单独经 RAG 配置
		}
		if p, m, ok := cfg.ResolveDefaultModel(scenario); ok {
			defaults[scenario] = gin.H{"provider_id": p.ID, "model_id": m.ID}
		}
	}
	// personas 为平台人设（ai.personas，非敏感明文——管理员配给全员选用
	// 的 system 提示模板，本人可见 system_prompt 无妨）：对话入口据此合并
	// 人设下拉（平台 tag），选中后前端以其 system_prompt 作为首条 system。
	c.JSON(http.StatusOK, gin.H{"providers": providers, "default_models": defaults, "personas": h.platformPersonas()})
}

// getAISettings GET /admin/settings/ai：读生效配置（密钥掩码）。
func (h *Handler) getAISettings(c *gin.Context) {
	cfg, err := h.aiEffectiveSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to load ai settings"})
		return
	}
	c.JSON(http.StatusOK, aiSettingsResponse{
		Enabled:         cfg.EffectiveEnabled(),
		Providers:       aiProviderViews(cfg),
		DefaultProvider: cfg.DefaultProvider,
		DefaultModels:   cfg.DefaultModels,
		Temperature:     cfg.Temperature,
		MaxTokens:       cfg.MaxTokens,
		PerUserPerMin:   cfg.PerUserPerMin,
		RAG:             cfg.RAG,
		Search:          aiSearchSettingsViewOf(cfg),
		OCR:             cfg.OCR,
		Personas:        aiPersonasOrEmpty(h.platformPersonas()),
		Env: gin.H{
			"enabled":  len(h.aiEnv.Providers) > 0,
			"base_url": envOrDefault("AI_BASE_URL", "https://api.openai.com/v1"),
			"model":    envOrDefault("AI_MODEL", "gpt-4o-mini"),
		},
	})
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// putAISettings PUT /admin/settings/ai：整体保存（api_key 留空 = 保持现值）。
func (h *Handler) putAISettings(c *gin.Context) {
	if h.settings == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settings service is not configured"})
		return
	}
	var req settings.AIConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	merged, err := h.settings.SetAI(req, h.aiEnv, userID(c))
	if err != nil {
		if errors.Is(err, settings.ErrInvalidValue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_AI_SETTINGS"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to save ai settings"})
		return
	}
	c.JSON(http.StatusOK, aiSettingsResponse{
		Enabled:         merged.EffectiveEnabled(),
		Providers:       aiProviderViews(merged),
		DefaultProvider: merged.DefaultProvider,
		DefaultModels:   merged.DefaultModels,
		Temperature:     merged.Temperature,
		MaxTokens:       merged.MaxTokens,
		PerUserPerMin:   merged.PerUserPerMin,
		RAG:             merged.RAG,
		Search:          aiSearchSettingsViewOf(merged),
		OCR:             merged.OCR,
		Personas:        aiPersonasOrEmpty(merged.Personas),
		Env: gin.H{
			"enabled":  len(h.aiEnv.Providers) > 0,
			"base_url": envOrDefault("AI_BASE_URL", "https://api.openai.com/v1"),
			"model":    envOrDefault("AI_MODEL", "gpt-4o-mini"),
		},
	})
}

// testAIProvider POST /admin/settings/ai/test {providerId}：发一条 ping
// 消息，返回往返延迟与错误详情。
func (h *Handler) testAIProvider(c *gin.Context) {
	svc := h.aiRequireService(c)
	if svc == nil {
		return
	}
	var req struct {
		ProviderID string `json:"providerId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	latency, err := svc.TestProvider(ctx, req.ProviderID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error(), "code": aiErrorCode(err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "latency_ms": latency})
}

func (h *Handler) testAIRAG(c *gin.Context) {
	cfg, err := h.aiEffectiveSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "error": "unable to load ai settings"})
		return
	}
	if !cfg.EffectiveEnabled() || cfg.RAG.Mode != "hybrid" || !cfg.RAG.VectorEnabled {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": "vector RAG is disabled"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	var embedding ai.EmbeddingProvider
	if cfg.RAG.EmbeddingProvider == "mock" {
		embedding = ai.NewMockEmbeddingProvider(8)
	} else {
		// 从已配置 Provider 中解析（embedding_provider=Provider ID + 具备
		// embedding 能力的 embedding_model；旧值 openai_compatible 回落
		// 默认 Provider）。
		provider, modelID, ok := cfg.ResolveEmbeddingTarget()
		if !ok || provider.Kind != settings.AIKindOpenAICompatible {
			c.JSON(http.StatusOK, gin.H{"ok": false, "error": "embedding provider/model not configured (pick an embedding-capable model)"})
			return
		}
		embedding = ai.NewOpenAIEmbeddingProvider(provider.BaseURL, provider.APIKey, modelID, nil)
	}
	vectors, err := embedding.Embed(ctx, []string{"DocFlow connection test"})
	if err == nil && len(vectors) > 0 && len(vectors[0]) > 0 {
		qdrant := ai.NewQdrantClient(cfg.RAG.QdrantURL, cfg.RAG.CollectionPrefix+"global", 10*time.Second)
		err = qdrant.EnsureCollection(ctx, len(vectors[0]))
	}
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "embedding and Qdrant collection reachable"})
}

// adminAIUsage GET /admin/ai/usage?from&to&limit：按用户聚合的用量统计。
func (h *Handler) adminAIUsage(c *gin.Context) {
	if h.aiUsageStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai usage statistics are not configured"})
		return
	}
	parseDay := func(raw string) (time.Time, bool) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return time.Time{}, true
		}
		t, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	from, ok := parseDay(c.Query("from"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from (YYYY-MM-DD)"})
		return
	}
	to, ok := parseDay(c.Query("to"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to (YYYY-MM-DD)"})
		return
	}
	if !to.IsZero() {
		to = to.AddDate(0, 0, 1) // 含 to 当天
	}
	limit := 100
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	rows, err := h.aiUsageStore.Aggregate(from, to, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "unable to aggregate ai usage"})
		return
	}
	var totalCalls, totalTokens int64
	for _, r := range rows {
		totalCalls += r.Calls
		totalTokens += r.TotalTokens
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows, "total": gin.H{"calls": totalCalls, "tokens": totalTokens}})
}

// ---------- 每用户限流（Provider 级 requests_per_min / daily_quota 热读取） ----------

// dynamicRateLimiter 为限流值可热调整的固定窗口限流器（按 key 计数；
// limit 由每次 Allow 调用方读取配置传入；AllowDaily 为按 UTC 自然日的
// 日限额计数，key 以 "daily:" 前缀与分钟窗口隔离）。
type dynamicRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]windowCount2
	now     func() time.Time
}

type windowCount2 struct {
	start time.Time
	count int
}

func newDynamicRateLimiter() *dynamicRateLimiter {
	now := time.Now
	return &dynamicRateLimiter{buckets: make(map[string]windowCount2), now: now}
}

// Allow 返回（allowed, retryAfter）：分钟级固定窗口。
func (l *dynamicRateLimiter) Allow(key string, limit int) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if limit <= 0 {
		return true, 0
	}
	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= time.Minute {
		l.buckets[key] = windowCount2{start: now, count: 1}
		return true, 0
	}
	if b.count >= limit {
		return false, time.Minute - now.Sub(b.start)
	}
	b.count++
	l.buckets[key] = b
	return true, 0
}

// AllowDaily 返回（allowed, retryAfter）：按 UTC 自然日窗口计数。
func (l *dynamicRateLimiter) AllowDaily(key string, limit int) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if limit <= 0 {
		return true, 0
	}
	day := now.UTC().Truncate(24 * time.Hour)
	key = "daily:" + key
	b, ok := l.buckets[key]
	if !ok || !b.start.Equal(day) {
		l.buckets[key] = windowCount2{start: day, count: 1}
		return true, 0
	}
	if b.count >= limit {
		return false, day.Add(24 * time.Hour).Sub(now)
	}
	b.count++
	l.buckets[key] = b
	return true, 0
}

// aiRateLimitForTarget 按解析目标执行每用户限流（个人池感知）：命中个人
// 池（Personal=true）时跳过平台 Provider 级限流与日配额（个人 Key 用户
// 自担），仅保留全局 ai.per_user_per_min（>0 时）兜底；平台目标与
// aiRateLimit 行为一致。
func (h *Handler) aiRateLimitForTarget(c *gin.Context, t ai.ChatTarget) bool {
	if h.aiLimiter == nil {
		return true
	}
	if t.Personal {
		limit := t.Config.PerUserPerMin
		if limit <= 0 {
			limit = settings.AIPerUserPerMinDefault
		}
		user := userID(c).String()
		if ok, retry := h.aiLimiter.Allow(user, limit); !ok {
			h.aiRateLimited(c, retry)
			return false
		}
		return true
	}
	return h.aiRateLimit(c, t.Provider, t.Config)
}

// aiRateLimit 按所选 Provider 的限流配置执行每用户限流（拒绝时已写 429
// 响应并返回 false）：Provider 级 requests_per_min > 0 优先，否则全局
// ai.per_user_per_min（>0 时）兜底；daily_quota > 0 时另按「用户+Provider+
// UTC 日」计数。挂 aiChat/aiSummarize 内部（限流须感知请求体中的 Provider）。
func (h *Handler) aiRateLimit(c *gin.Context, provider settings.AIProvider, cfg settings.AIConfig) bool {
	if h.aiLimiter == nil {
		return true
	}
	limit := provider.RequestsPerMin
	if limit <= 0 && cfg.PerUserPerMin > 0 {
		limit = cfg.PerUserPerMin
	}
	if limit <= 0 {
		limit = settings.AIPerUserPerMinDefault
	}
	user := userID(c).String()
	if ok, retry := h.aiLimiter.Allow(user, limit); !ok {
		h.aiRateLimited(c, retry)
		return false
	}
	if provider.DailyQuota > 0 {
		if ok, retry := h.aiLimiter.AllowDaily(user+"|"+provider.ID, provider.DailyQuota); !ok {
			h.aiRateLimited(c, retry)
			return false
		}
	}
	return true
}

func (h *Handler) aiRateLimited(c *gin.Context, retry time.Duration) {
	c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "ai rate limit exceeded", "code": "AI_RATE_LIMITED"})
}
