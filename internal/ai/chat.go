// Package ai —— AI 能力第一版核心服务（chat.go）：
//
// ChatService 统一承载多 Provider 的对话补全：
//   - Provider 来自 system_settings 的 ai.providers（管理端 CRUD），env
//     （AI_*）作为兜底基线合成 id=env 的 Provider（见 main.go 接线）；
//   - Kind=openai_compatible：POST {base}/chat/completions（OpenAI/
//     DeepSeek/Qwen/Ollama/vLLM 等兼容协议通吃），SSE 流式透传 delta，
//     上游不支持流式时自动回退非流式；网络错误/5xx 重试 1 次；
//   - Kind=anthropic：POST {base}/v1/messages（x-api-key 头），SSE
//     content_block_delta 透传，同样带非流式回退与 1 次重试；
//   - Kind=mock：内置假 Provider（开发/测试/无 Key 演示）：echo 型固定
//     流式响应；识别「文件名：」前缀的摘要请求返回模板摘要；识别上下文
//     中的 /view/{uuid} 链接并按系统提示要求回显「来源：」段落。
//
// 错误语义：ErrNoProvider（未配置任何可用 Provider，HTTP 400）/
// ErrUpstream（上游失败，HTTP 502）。每用户限流（ai.per_user_per_min）
// 与用量记录（ai_usage 表）由 HTTP/MCP 层接线，本文件保持纯服务。
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/mcpclient"
	"github.com/docflow/docflow/internal/settings"
	"github.com/google/uuid"
)

var (
	// ErrNoProvider 表示未配置任何可用 AI Provider（HTTP 400 AI_NOT_CONFIGURED）。
	ErrNoProvider = errors.New("no ai provider configured")
	// ErrProviderNotFound 表示指定的 providerId 不存在或未启用（HTTP 400）。
	ErrProviderNotFound = errors.New("ai provider not found")
	// ErrModelNotAllowed 表示指定的模型不属于启用中的 Provider 或不具备
	// chat 能力（HTTP 400 AI_MODEL_NOT_ALLOWED——含请求显式带 model 参数
	// 的越权场景）。
	ErrModelNotAllowed = errors.New("ai model not allowed")
	// ErrUpstreamChat 上游对话请求失败（非 2xx / 超时 / 网络/解析错误，HTTP 502）。
	ErrUpstreamChat = errors.New("ai upstream chat failed")
)

// DefaultChatTimeout 单次对话请求（含流式整体）的超时。
const DefaultChatTimeout = 120 * time.Second

// AnthropicThinkBudgetTokens anthropic thinking 的 budget_tokens（思考
// 上限）；anthropic 要求 max_tokens > budget_tokens，不满足时自动抬高。
const AnthropicThinkBudgetTokens = 2048

// Message 为对话消息条目（role: system/user/assistant）。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 为一次对话补全请求。
type ChatRequest struct {
	// ProviderID 指定 Provider（空 = 默认 Provider）。
	ProviderID string
	// Model 指定 Provider 下的模型 ID（空 = 场景默认/主模型）；必须属于
	// 启用中的 Provider 且具备 chat 能力，否则 ErrModelNotAllowed。
	Model string
	// Scenario 场景（chat/summary/edit）：Provider 与 Model 均空时用于
	// 选择场景默认模型（summary/edit 无效时回落 chat）。
	Scenario string
	// Messages 对话历史（不含 system；System 单独注入在最前）。
	Messages []Message
	// System 系统提示（可空）。
	System string
	// Temperature 覆盖全局温度（nil = 用全局配置）。
	Temperature *float64
	// MaxTokens 覆盖全局 max_tokens（0 = 用全局配置）。
	MaxTokens int
	// Think 开启推理思考（默认 false）：仅当所选模型 capabilities.reasoning
	//=true 时生效（openai_compatible 透传 reasoning_effort，anthropic 透传
	// thinking）；模型不支持时静默忽略（不报错）。
	Think bool
	// UseMCP 开启外部 MCP 工具循环（默认 false）：读取平台配置的启用
	// MCP 服务（ai.mcp）并合并当前用户自备的个人 MCP 服务（user_ai_prefs
	// 的 mcp_servers，仅本人对话生效），聚合其工具供上游模型调用，模型
	// 请求工具时执行并把结果回喂继续补全（上限 MCPToolMaxRounds 轮）。
	// 无可用服务/工具时静默按普通对话继续。
	UseMCP bool
	// PlatformTools 为内置平台文件工具声明（use_files；HTTP 层注入
	// PlatformTools() 全集）。与 UseMCP 可同开——两类工具合并进同一条
	// 工具循环（df_ 前缀内置工具 + mcp_ 前缀外部工具）。
	PlatformTools []PlatformTool
	// ToolExecutor 为内置平台工具执行器回调（name+参数 JSON → 结果
	// 字符串）：ai 包不依赖 http/service 层，执行桥经此回调注入（见
	// internal/http/ai_platform_tools.go 的 executePlatformTool）。仅当
	// PlatformTools 非空时参与分发。
	ToolExecutor ToolExecutorFunc
	// OnTool 为工具执行前回调（serverID/服务名/工具名；HTTP 层经 SSE
	// event:tool 下发前端进度提示）。可为 nil。
	OnTool ToolEventNotifier
	// Stream true 时经 onDelta 流式回调增量。
	Stream bool
}

// ChatResult 为一次对话补全的结果元数据（内容经 onDelta 或 Content 返回）。
type ChatResult struct {
	Content          string
	ProviderID       string
	ProviderName     string
	ProviderKind     string
	Model            string
	PromptTokens     int
	CompletionTokens int
	DurationMS       int64
	// Personal 表示本次命中用户个人 Provider 池（双轨制；HTTP 层据此在
	// 审计/用量打 personal 标记）。
	Personal bool
}

// ConfigProvider 返回当前生效 AI 配置（热读取，DB 覆盖 → env 基线）。
type ConfigProvider func() (settings.AIConfig, error)

// Service 为 AI 对话服务（多 Provider，SSE 流式）。
type Service struct {
	config     ConfigProvider
	client     *http.Client
	usage      UsageSink
	usageStore *UsageStore
	searcher   Searcher
	hybrid     HybridSearcher
	ragConfig  func() settings.AIRAGConfig
	now        func() time.Time
	// storage 为对象存储读取器（OCR 读图片 bytes 用；main 装配注入，
	// nil = 未配置，ExtractImageText 对此报错由调用方降级）。
	storage StorageReader
	// personal 为当前请求用户的个人 Provider 池（双轨制；ForUser 之后经
	// WithPersonalPrefs 挂载，零值 = 无个人池，仅平台链解析）。
	personal auth.AIPersonalPrefs
	// mcpReader 热读取平台 MCP 服务配置（ai.mcp 键；main 装配注入，
	// nil = 未配置 use_mcp 恒降级普通对话）。ForUser/WithPersonalPrefs
	// 浅拷贝天然携带（个人池 Provider 同样可用 MCP 工具）。
	mcpReader func() []settings.AIMCPServiceDef
}

// NewService 构造服务；cfg 为生效配置读取器（每次请求热读取）。
func NewService(cfg ConfigProvider) *Service {
	return &Service{config: cfg, client: &http.Client{Timeout: DefaultChatTimeout}, usage: NopUsageSink, now: time.Now}
}

// SetUsageSink 注入用量记录器（ai_usage 表）；nil 保持 Nop。
func (s *Service) SetUsageSink(sink UsageSink) {
	if sink != nil {
		s.usage = sink
	}
}

// SetUsageStore 注入用量存储（ForUser 按请求关联用户用）。
func (s *Service) SetUsageStore(store *UsageStore) {
	if store != nil {
		s.usageStore = store
	}
}

func (s *Service) SetHybridRetriever(r HybridSearcher, cfg func() settings.AIRAGConfig) {
	s.hybrid, s.ragConfig = r, cfg
}

// SetMCPReader 注入平台 MCP 服务配置读取器（ai.mcp 键热读取；use_mcp
// 对话的工具来源）。nil 忽略（保持未配置语义）。
func (s *Service) SetMCPReader(r func() []settings.AIMCPServiceDef) {
	if r != nil {
		s.mcpReader = r
	}
}

// ForUser 返回以指定用户记账的浅拷贝（config/client 等只读字段共享，
// usage 替换为带 UserID 的包装；每请求构造，无并发状态）。
func (s *Service) ForUser(user uuid.UUID) *Service {
	clone := *s
	if s.usageStore != nil {
		clone.usage = UserUsageSink{Store: s.usageStore, User: user}
	}
	return &clone
}

// Config 返回当前生效配置（调用方读取限流等）。
func (s *Service) Config() (settings.AIConfig, error) {
	if s.config == nil {
		return settings.DefaultAIConfig(), nil
	}
	return s.config()
}

// EnabledNow 返回 AI 能力当前是否启用（总开关 ai.enabled + 存在启用中的
// Provider）：false 时全部 /ai/* 网关端点 404、MCP AI 工具隐藏、前端入口
// 隐藏。读取失败按未启用处理（fail closed）。
func (s *Service) EnabledNow() bool {
	cfg, err := s.Config()
	if err != nil {
		return false
	}
	return cfg.EffectiveEnabled()
}

// ResolveProvider 解析目标 Provider（不发起请求；HTTP 层用于 SSE meta 事件）。
func (s *Service) ResolveProvider(id string) (settings.AIProvider, settings.AIConfig, error) {
	return s.resolveProvider(id)
}

// resolveProvider 解析目标 Provider：显式 ID 未命中返回 ErrProviderNotFound；
// 无可用 Provider 返回 ErrNoProvider。
func (s *Service) resolveProvider(id string) (settings.AIProvider, settings.AIConfig, error) {
	cfg, err := s.Config()
	if err != nil {
		return settings.AIProvider{}, cfg, fmt.Errorf("%w: read config: %v", ErrUpstreamChat, err)
	}
	target := strings.TrimSpace(id)
	var found *settings.AIProvider
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		if !p.Enabled {
			continue
		}
		if target == "" && p.ID == cfg.DefaultProvider {
			found = &cfg.Providers[i]
			break
		}
		if target != "" && p.ID == target {
			found = &cfg.Providers[i]
			break
		}
	}
	if found == nil && target == "" {
		// 无默认（或默认未启用）：取第一个启用项。
		for i := range cfg.Providers {
			if cfg.Providers[i].Enabled {
				found = &cfg.Providers[i]
				break
			}
		}
	}
	if found == nil {
		if target != "" {
			return settings.AIProvider{}, cfg, ErrProviderNotFound
		}
		return settings.AIProvider{}, cfg, ErrNoProvider
	}
	return *found, cfg, nil
}

// ResolveChatTarget 解析对话目标（Provider + 模型，不发请求）：HTTP 层
// 用于 SSE meta 事件与按 Provider 的限流取值。错误：ErrNoProvider/
// ErrProviderNotFound/ErrModelNotAllowed。
func (s *Service) ResolveChatTarget(providerID, modelID, scenario string) (settings.AIProvider, string, settings.AIConfig, error) {
	return s.resolveChatTarget(providerID, modelID, scenario)
}

// resolveChatTarget 解析对话目标：
//   - 显式 modelID：须归属启用中的 Provider 且具备 chat 能力（同时指定
//     providerID 时须匹配），否则 ErrModelNotAllowed（越权拒绝）；
//   - 仅 providerID：该 Provider 的场景默认模型（default_models 命中同
//     Provider 时优先），否则主模型；
//   - 均空：场景默认模型（scenario 空 = chat；summary/edit 无效回落
//     chat）→ 默认 Provider 主模型。
func (s *Service) resolveChatTarget(providerID, modelID, scenario string) (settings.AIProvider, string, settings.AIConfig, error) {
	cfg, err := s.Config()
	if err != nil {
		return settings.AIProvider{}, "", cfg, fmt.Errorf("%w: read config: %v", ErrUpstreamChat, err)
	}
	target := strings.TrimSpace(providerID)
	mid := strings.TrimSpace(modelID)
	if mid != "" && target == "" {
		// 未指定 Provider：按模型归属反查（仅启用中的 Provider）。
		for _, p := range cfg.Providers {
			if p.Enabled {
				if _, ok := p.ModelWithID(mid); ok {
					target = p.ID
					break
				}
			}
		}
		if target == "" {
			return settings.AIProvider{}, "", cfg, ErrModelNotAllowed
		}
	}
	if target == "" && mid == "" {
		// 场景默认（chat/summary/edit）。
		if p, m, ok := cfg.ResolveDefaultModel(scenario); ok {
			return p, m.ID, cfg, nil
		}
		return settings.AIProvider{}, "", cfg, ErrNoProvider
	}
	provider, _, err := s.resolveProvider(target)
	if err != nil {
		return settings.AIProvider{}, "", cfg, err
	}
	if mid == "" {
		mid = providerDefaultModel(cfg, provider, scenario)
	} else if m, ok := provider.ModelWithID(mid); !ok || !m.Capabilities.IsChat() {
		return settings.AIProvider{}, "", cfg, ErrModelNotAllowed
	}
	return provider, mid, cfg, nil
}

// providerDefaultModel 返回 Provider 的默认模型：场景默认（default_models
// 命中同 Provider）优先，否则主模型。
func providerDefaultModel(cfg settings.AIConfig, p settings.AIProvider, scenario string) string {
	if scenario == "" {
		scenario = settings.AIScenarioChat
	}
	for _, s := range []string{scenario, settings.AIScenarioChat} {
		if ref, ok := cfg.DefaultModels[s]; ok && ref.ProviderID == p.ID {
			if m, ok2 := p.ModelWithID(ref.ModelID); ok2 && m.Capabilities.IsChat() {
				return m.ID
			}
		}
	}
	return p.PrimaryModel()
}

// Chat 执行一次对话补全：流式时逐 delta 调 onDelta（可为 nil），返回结果
// 元数据（Content 为完整拼装文本）。错误：ErrNoProvider/ErrProviderNotFound/
// ErrModelNotAllowed/ErrUpstreamChat。解析链个人池感知（见 personal.go）：
// 显式模型先查个人池后查平台池；未显式且 prefer_personal 时个人场景默认
// 优先；命中个人池时 ChatResult.Personal=true。
func (s *Service) Chat(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error) {
	target, err := s.ResolveChatTargetFor(req.ProviderID, req.Model, req.Scenario)
	if err != nil {
		return ChatResult{}, err
	}
	provider, model, cfg := target.Provider, target.Model, target.Config
	// 推理思考门控：仅所选模型勾选 capabilities.reasoning 时透传 think；
	// 模型不支持（含旧配置合成单模型）静默忽略，不报错。
	if req.Think {
		if m, ok := provider.ModelWithID(model); !ok || !m.Capabilities.Reasoning {
			req.Think = false
		}
	}
	temperature := cfg.Temperature
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = cfg.MaxTokens
	}
	// MCP 工具收集（use_mcp）+ 内置平台工具（use_files，HTTP 层注入）：
	// 任一来源可用即进入工具循环（两类工具合并进 chatToolContext）；
	// MCP 单服务失败跳过并 log，全部失败/零工具 → 仅剩内置工具或普通
	// 对话继续（现有路径零改动）。mock Provider 不参与工具循环（无真实上游）。
	var toolset *chatToolContext
	if req.UseMCP || (len(req.PlatformTools) > 0 && req.ToolExecutor != nil) {
		toolset = s.buildToolContext(ctx, req)
	}
	start := s.now()
	var out ChatResult
	switch provider.Kind {
	case settings.AIKindMock:
		out, err = s.chatMock(ctx, provider, req, temperature, maxTokens, onDelta)
	case settings.AIKindOpenAICompatible:
		if toolset != nil {
			out, err = s.chatOpenAITools(ctx, provider, model, req, temperature, maxTokens, onDelta, toolset)
		} else {
			out, err = s.chatOpenAI(ctx, provider, model, req, temperature, maxTokens, onDelta)
		}
	case settings.AIKindAnthropic:
		if toolset != nil {
			out, err = s.chatAnthropicTools(ctx, provider, model, req, temperature, maxTokens, onDelta, toolset)
		} else {
			out, err = s.chatAnthropic(ctx, provider, model, req, temperature, maxTokens, onDelta)
		}
	default:
		return ChatResult{}, fmt.Errorf("%w: unsupported provider kind %q", ErrUpstreamChat, provider.Kind)
	}
	out.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		return out, err
	}
	out.ProviderID = provider.ID
	out.ProviderName = provider.Name
	out.ProviderKind = provider.Kind
	out.Model = model
	out.Personal = target.Personal
	s.recordUsage(provider, model, req, out)
	return out, nil
}

// recordUsage best-effort 记录一次用量（失败不影响主流程）。
func (s *Service) recordUsage(provider settings.AIProvider, model string, req ChatRequest, out ChatResult) {
	promptChars := 0
	for _, m := range req.Messages {
		promptChars += len(m.Content)
	}
	promptTokens := out.PromptTokens
	if promptTokens == 0 {
		promptTokens = promptChars / 4
	}
	completionTokens := out.CompletionTokens
	if completionTokens == 0 {
		completionTokens = len(out.Content) / 4
	}
	_ = s.usage.Record(UsageEntry{
		ProviderID: provider.ID, Model: model,
		Personal:     out.Personal,
		PromptTokens: promptTokens, CompletionTokens: completionTokens,
		DurationMS: out.DurationMS, CreatedAt: s.now().UTC(),
	})
}

// ---------- openai_compatible ----------

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIToolCallDelta 为流式 delta.tool_calls 元素（index 渐进聚合；
// id/name 首块全量、arguments 分片拼接——OpenAI 兼容协议语义）。
type openAIToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string                `json:"content"`
			ToolCalls []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content   string                `json:"content"`
			ToolCalls []openAIToolCallAgged `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// openAIToolCallAgged 为非流式响应 message.tool_calls 元素（工具循环
// 非流式回退路径解码；type 恒 function，无需读）。
type openAIToolCallAgged struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatOpenAI 调 {base}/chat/completions（model 为本次解析出的模型 ID）：
// stream=true 走 SSE，失败（上游不支持流式/连接中断）回退非流式重试一次。
func (s *Service) chatOpenAI(ctx context.Context, p settings.AIProvider, model string, req ChatRequest, temperature float64, maxTokens int, onDelta func(string)) (ChatResult, error) {
	msgs := make([]openAIMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, openAIMessage{Role: m.Role, Content: m.Content})
	}
	body := map[string]any{
		"model":       model,
		"messages":    msgs,
		"temperature": temperature,
		"max_tokens":  maxTokens,
	}
	if req.Think {
		// 推理思考透传（OpenAI o 系列/Qwen3 等；DeepSeek R1 系忽略该
		// 字段即可，不影响请求）。
		body["reasoning_effort"] = "medium"
	}
	if req.Stream {
		body["stream"] = true
		// 请求最终 chunk 附带 usage（OpenAI 兼容网关广泛支持；不支持的
		// 网关会忽略该字段，用量按字符近似回退）。
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + "/chat/completions"
	do := func(stream bool) (ChatResult, error) {
		if !stream {
			delete(body, "stream")
			delete(body, "stream_options")
			payload, err = json.Marshal(body)
			if err != nil {
				return ChatResult{}, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return ChatResult{}, fmt.Errorf("%w: build request: %v", ErrUpstreamChat, err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		resp, err := s.client.Do(httpReq)
		if err != nil {
			return ChatResult{}, fmt.Errorf("%w: %v", ErrUpstreamChat, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return ChatResult{}, fmt.Errorf("%w: status %d: %s", ErrUpstreamChat, resp.StatusCode, truncateBytes(raw, 200))
		}
		if stream {
			return consumeOpenAISSE(resp.Body, onDelta)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return ChatResult{}, fmt.Errorf("%w: read response: %v", ErrUpstreamChat, err)
		}
		var out openAIResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return ChatResult{}, fmt.Errorf("%w: decode response: %v", ErrUpstreamChat, err)
		}
		if out.Error != nil {
			return ChatResult{}, fmt.Errorf("%w: %s", ErrUpstreamChat, out.Error.Message)
		}
		if len(out.Choices) == 0 {
			return ChatResult{}, fmt.Errorf("%w: empty completion", ErrUpstreamChat)
		}
		res := ChatResult{Content: strings.TrimSpace(out.Choices[0].Message.Content)}
		if out.Usage != nil {
			res.PromptTokens, res.CompletionTokens = out.Usage.PromptTokens, out.Usage.CompletionTokens
		}
		return res, nil
	}
	res, err := do(req.Stream)
	if req.Stream && err != nil {
		// 流式失败回退非流式（一次）。
		res, err = do(false)
	}
	return res, err
}

// consumeOpenAISSE 解析 text/event-stream：逐 data: JSON 取 delta.content。
func consumeOpenAISSE(r io.Reader, onDelta func(string)) (ChatResult, error) {
	var res ChatResult
	var content strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	emit := func(text string) {
		if text == "" {
			return
		}
		content.WriteString(text)
		if onDelta != nil {
			onDelta(text)
		}
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 跳过无法解析的 chunk（兼容网关注释行/心跳）
		}
		if chunk.Error != nil {
			return res, fmt.Errorf("%w: %s", ErrUpstreamChat, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			res.PromptTokens, res.CompletionTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			emit(c.Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("%w: read stream: %v", ErrUpstreamChat, err)
	}
	res.Content = strings.TrimSpace(content.String())
	if res.Content == "" {
		return res, fmt.Errorf("%w: empty stream response", ErrUpstreamChat)
	}
	return res, nil
}

// ---------- anthropic ----------

// anthropicEvent 为 anthropic SSE 事件（仅解码，不序列化）。扩展字段
// （Index/ContentBlock/PartialJSON/Signature/StopReason）服务工具循环的
// tool_use/thinking 块聚合，普通对话路径只消费 text_delta/usage。
type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	// ContentBlock 为 content_block_start 的块头（tool_use: id/name；
	// text/thinking: type）。
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// PartialJSON 为 input_json_delta 的参数分片（tool_use）。
		PartialJSON string `json:"partial_json"`
		// Signature 为 thinking 块的签名分片（anthropic 要求带 tool_use
		// 的 assistant 消息在 thinking 开启时回传签名块）。
		Signature  string `json:"signature"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicContentBlock 为 messages 响应的 content 元素（text 普通文本 /
// thinking 推理 / tool_use 工具调用；工具循环回传 assistant 原始块用）。
type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type anthropicResponse struct {
	Content []anthropicContentBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chatAnthropic 调 {base}/v1/messages（anthropic-version 2023-06-01；
// model 为本次解析出的模型 ID）。
func (s *Service) chatAnthropic(ctx context.Context, p settings.AIProvider, model string, req ChatRequest, temperature float64, maxTokens int, onDelta func(string)) (ChatResult, error) {
	msgs := make([]map[string]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{
		"model":       model,
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"messages":    msgs,
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Think {
		// anthropic 要求 max_tokens > budget_tokens：不满足时抬高
		//（budget 之上另留 1024 tokens 给正文回答）。
		if maxTokens <= AnthropicThinkBudgetTokens {
			maxTokens = AnthropicThinkBudgetTokens + 1024
		}
		body["max_tokens"] = maxTokens
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": AnthropicThinkBudgetTokens}
	}
	if req.Stream {
		body["stream"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
	}
	url := strings.TrimSuffix(p.BaseURL, "/") + "/v1/messages"
	do := func(stream bool) (ChatResult, error) {
		if !stream {
			delete(body, "stream")
			payload, err = json.Marshal(body)
			if err != nil {
				return ChatResult{}, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return ChatResult{}, fmt.Errorf("%w: build request: %v", ErrUpstreamChat, err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			httpReq.Header.Set("x-api-key", p.APIKey)
		}
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		resp, err := s.client.Do(httpReq)
		if err != nil {
			return ChatResult{}, fmt.Errorf("%w: %v", ErrUpstreamChat, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return ChatResult{}, fmt.Errorf("%w: status %d: %s", ErrUpstreamChat, resp.StatusCode, truncateBytes(raw, 200))
		}
		if stream {
			return consumeAnthropicSSE(resp.Body, onDelta)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return ChatResult{}, fmt.Errorf("%w: read response: %v", ErrUpstreamChat, err)
		}
		var out anthropicResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return ChatResult{}, fmt.Errorf("%w: decode response: %v", ErrUpstreamChat, err)
		}
		if out.Error != nil {
			return ChatResult{}, fmt.Errorf("%w: %s", ErrUpstreamChat, out.Error.Message)
		}
		var b strings.Builder
		for _, c := range out.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		res := ChatResult{Content: strings.TrimSpace(b.String()), PromptTokens: out.Usage.InputTokens, CompletionTokens: out.Usage.OutputTokens}
		if res.Content == "" {
			return res, fmt.Errorf("%w: empty completion", ErrUpstreamChat)
		}
		return res, nil
	}
	res, err := do(req.Stream)
	if req.Stream && err != nil {
		res, err = do(false)
	}
	return res, err
}

// consumeAnthropicSSE 解析 anthropic 流：content_block_delta 增量、
// message_start/message_delta 汇总 usage。
func consumeAnthropicSSE(r io.Reader, onDelta func(string)) (ChatResult, error) {
	var res ChatResult
	var content strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Error != nil {
			return res, fmt.Errorf("%w: %s", ErrUpstreamChat, ev.Error.Message)
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
				content.WriteString(ev.Delta.Text)
				if onDelta != nil {
					onDelta(ev.Delta.Text)
				}
			}
		case "message_start":
			res.PromptTokens = ev.Message.Usage.InputTokens
		case "message_delta":
			res.CompletionTokens = ev.Usage.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("%w: read stream: %v", ErrUpstreamChat, err)
	}
	res.Content = strings.TrimSpace(content.String())
	if res.Content == "" {
		return res, fmt.Errorf("%w: empty stream response", ErrUpstreamChat)
	}
	return res, nil
}

func truncateBytes(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ---------- MCP 工具循环（use_mcp） ----------

// 工具循环边界。
const (
	// MCPToolMaxRounds 工具执行轮次上限（防死循环）：达到上限后模型仍
	// 请求工具时不再执行，输出已达成文本（无文本则提示已达上限）。
	MCPToolMaxRounds = 5
	// MCPToolResultMaxRunes 单次工具结果注入对话的长度上限（rune 计，
	// 超长截断尾注）。
	MCPToolResultMaxRunes = 8000
	// mcpToolFnNameMax openai function name 长度上限（anthropic 工具名
	// 同样受限，共用）。
	mcpToolFnNameMax = 64
	// mcpToolFnPrefix 归一化工具名前缀（防与上游 Provider 原生函数名冲突）。
	mcpToolFnPrefix = "mcp_"
)

// ToolEventNotifier 为工具执行前回调（serverID/服务名/工具名）——HTTP
// 层据此在每次工具执行前发 SSE event:tool（前端进度提示）。
type ToolEventNotifier func(serverID, serverName, toolName string)

// mcpToolRef 为一个可调用工具的完整引用（归属服务 + 工具定义 + 客户端）。
type mcpToolRef struct {
	service settings.AIMCPServiceDef
	tool    mcpclient.Tool
	client  *mcpclient.Client
}

// mcpToolContext 为一次 use_mcp 对话聚合的工具集（归一化函数名 → 引用；
// 同一服务的多个工具共享一个 mcpclient.Client——会话/工具列表缓存复用）。
type mcpToolContext struct {
	byFn map[string]mcpToolRef
}

// mcpServiceEntry 为参与一次对话聚合的 MCP 服务（平台配置 + 个人自备合并
// 后的统一形态）：def 与平台 ai.mcp 同构（个人条目恒 Enabled=true），
// headers 为个人服务的多认证头集合（平台服务为空，走 def.AuthHeader）。
type mcpServiceEntry struct {
	def     settings.AIMCPServiceDef
	headers map[string]string
}

// mcpServices 合并平台配置（mcpReader 热读取）与当前用户自备的个人 MCP
// 服务（personal.MCPServers，仅本人对话生效；经 WithPersonalPrefs 挂载，
// 未挂载即空）。个人服务恒视为启用；两池工具名归一化后重名由既有 dedup
// 逻辑跳过。
func (s *Service) mcpServices() []mcpServiceEntry {
	var services []mcpServiceEntry
	if s.mcpReader != nil {
		for _, svc := range s.mcpReader() {
			services = append(services, mcpServiceEntry{def: svc})
		}
	}
	for _, m := range s.personal.MCPServers {
		if strings.TrimSpace(m.URL) == "" {
			continue
		}
		services = append(services, mcpServiceEntry{
			def:     settings.AIMCPServiceDef{ID: m.ID, Name: m.Name, URL: m.URL, Enabled: true},
			headers: m.AuthHeaders,
		})
	}
	return services
}

// collectMCPTools 收集启用 MCP 服务（平台 + 个人合并）的工具列表：单服务
// 失败跳过并 log；无可用服务 / 全部失败 / 零工具返回 nil（调用方按普通
// 对话继续）。
func (s *Service) collectMCPTools(ctx context.Context) *mcpToolContext {
	services := s.mcpServices()
	if len(services) == 0 {
		return nil
	}
	ts := &mcpToolContext{byFn: make(map[string]mcpToolRef)}
	for _, entry := range services {
		svc := entry.def
		if !svc.Enabled || strings.TrimSpace(svc.URL) == "" {
			continue
		}
		cli := &mcpclient.Client{URL: svc.URL, AuthHeader: svc.AuthHeader, Headers: entry.headers}
		cctx, cancel := context.WithTimeout(ctx, mcpclient.RequestTimeout)
		tools, err := cli.ListTools(cctx)
		cancel()
		if err != nil {
			log.Printf("ai mcp: 服务 %s(%s) 工具列表获取失败，已跳过: %v", svc.Name, svc.ID, err)
			continue
		}
		for _, tl := range tools {
			if strings.TrimSpace(tl.Name) == "" {
				continue
			}
			fn := mcpToolFnName(svc.ID, tl.Name)
			if _, dup := ts.byFn[fn]; dup {
				log.Printf("ai mcp: 工具 %q 归一化后与既有工具重名，已跳过", fn)
				continue
			}
			ts.byFn[fn] = mcpToolRef{service: svc, tool: tl, client: cli}
		}
	}
	if len(ts.byFn) == 0 {
		return nil
	}
	return ts
}

// mcpToolFnName 生成上游可见的工具名：mcp_{serverID}_{toolName}，非法
// 字符归一为 _，超 64 字符截断。
func mcpToolFnName(serverID, toolName string) string {
	name := mcpToolFnPrefix + sanitizeToolToken(serverID) + "_" + sanitizeToolToken(toolName)
	if len(name) > mcpToolFnNameMax {
		name = name[:mcpToolFnNameMax]
	}
	return name
}

func sanitizeToolToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// mcpToolSchema 规整参数 schema：空/非法时回退 {"type":"object"}；
// 合法 JSON 原样透传（json.RawMessage 内联序列化）。
func mcpToolSchema(raw json.RawMessage) any {
	if len(raw) == 0 || !json.Valid(raw) {
		return map[string]any{"type": "object"}
	}
	return raw
}

// openAITools 构造 openai tools 参数（function 定义，按函数名排序保证
// 请求确定性）。
func (ts *mcpToolContext) openAITools() []map[string]any {
	fns := make([]string, 0, len(ts.byFn))
	for fn := range ts.byFn {
		fns = append(fns, fn)
	}
	sort.Strings(fns)
	out := make([]map[string]any, 0, len(fns))
	for _, fn := range fns {
		ref := ts.byFn[fn]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        fn,
				"description": ref.tool.Description,
				"parameters":  mcpToolSchema(ref.tool.InputSchema),
			},
		})
	}
	return out
}

// anthropicTools 构造 anthropic tools 参数（input_schema 透传）。
func (ts *mcpToolContext) anthropicTools() []map[string]any {
	fns := make([]string, 0, len(ts.byFn))
	for fn := range ts.byFn {
		fns = append(fns, fn)
	}
	sort.Strings(fns)
	out := make([]map[string]any, 0, len(fns))
	for _, fn := range fns {
		ref := ts.byFn[fn]
		out = append(out, map[string]any{
			"name":         fn,
			"description":  ref.tool.Description,
			"input_schema": mcpToolSchema(ref.tool.InputSchema),
		})
	}
	return out
}

// execMCPTool 执行一个工具调用并返回注入对话的结果字符串（永不因工具
// 失败中断对话——错误文本作为结果回喂模型，由模型决定重试/换路/告知
// 用户，agent 循环的标准鲁棒性做法）。结果截 8000 字符。
func (s *Service) execMCPTool(ctx context.Context, ts *mcpToolContext, fn string, args json.RawMessage, notify ToolEventNotifier) string {
	ref, ok := ts.byFn[fn]
	if !ok {
		return "错误：未知工具 " + fn + "（不在可用 MCP 工具集中）"
	}
	if notify != nil {
		notify(ref.service.ID, ref.service.Name, ref.tool.Name)
	}
	cctx, cancel := context.WithTimeout(ctx, mcpclient.RequestTimeout)
	defer cancel()
	result, err := ref.client.CallTool(cctx, ref.tool.Name, args)
	if err != nil {
		log.Printf("ai mcp: 工具 %s 调用失败: %v", fn, err)
		return "工具调用失败：" + err.Error()
	}
	if runes := []rune(result); len(runes) > MCPToolResultMaxRunes {
		return string(runes[:MCPToolResultMaxRunes]) + "…（工具结果过长已截断）"
	}
	return result
}

// ---------- 内置平台工具合并（use_files + use_mcp 共用循环） ----------

// ToolExecutorFunc 为内置平台工具执行器回调（name+参数 JSON → 结果字符串；
// HTTP 层闭包注入，见 internal/http/ai_platform_tools.go）。结果应为
// {"ok":true,...}/{"ok":false,"error":"..."} JSON 字符串，由执行方保证。
type ToolExecutorFunc func(name string, argsJSON json.RawMessage) (string, error)

// chatToolContext 为一次工具对话聚合的完整工具集：外部 MCP 工具（mcp_*
// 前缀，可为 nil）+ 内置平台文件工具（df_* 前缀，可为空）。两类工具
// 合并进同一条 openai/anthropic 工具循环，执行时按名称分发（内置优先）。
type chatToolContext struct {
	mcp      *mcpToolContext
	platform []PlatformTool
	exec     ToolExecutorFunc
}

// buildToolContext 聚合一次对话的工具来源：use_mcp 收集 MCP 工具（单服务
// 失败跳过；零工具时 mcp 为 nil），PlatformTools+ToolExecutor 为内置平台
// 工具。两类均无返回 nil（调用方按普通对话继续）。
func (s *Service) buildToolContext(ctx context.Context, req ChatRequest) *chatToolContext {
	tc := &chatToolContext{}
	if req.UseMCP {
		tc.mcp = s.collectMCPTools(ctx)
	}
	if len(req.PlatformTools) > 0 && req.ToolExecutor != nil {
		tc.platform = req.PlatformTools
		tc.exec = req.ToolExecutor
	}
	if tc.mcp == nil && tc.platform == nil {
		return nil
	}
	return tc
}

// openAITools 合并构造 openai tools 参数（MCP 工具按函数名排序 + 内置
// 平台工具按声明序追加，整体确定性）。
func (tc *chatToolContext) openAITools() []map[string]any {
	var out []map[string]any
	if tc.mcp != nil {
		out = append(out, tc.mcp.openAITools()...)
	}
	for _, t := range tc.platform {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Params,
			},
		})
	}
	return out
}

// anthropicTools 合并构造 anthropic tools 参数（input_schema 同构）。
func (tc *chatToolContext) anthropicTools() []map[string]any {
	var out []map[string]any
	if tc.mcp != nil {
		out = append(out, tc.mcp.anthropicTools()...)
	}
	for _, t := range tc.platform {
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Params,
		})
	}
	return out
}

// execTool 执行一次工具调用并返回注入对话的结果字符串：先查内置平台
// 工具名（PlatformToolNames）走 ToolExecutor 回调，未命中回落 MCP 工具
// 路径（execMCPTool）。与 MCP 一致的鲁棒性：工具失败不中断对话，错误
// 文本作为结果回喂模型；结果截 MCPToolResultMaxRunes 字符。
func (s *Service) execTool(ctx context.Context, tc *chatToolContext, fn string, args json.RawMessage, notify ToolEventNotifier) string {
	if tc.exec != nil && PlatformToolNames()[fn] {
		if notify != nil {
			// 内置平台工具的进度事件：server 固定 docflow/平台文件，
			// 前端 SSE tool 事件格式与 MCP 工具一致（tool 字段按名透传）。
			notify("docflow", "平台文件", fn)
		}
		result, err := tc.exec(fn, args)
		if err != nil {
			log.Printf("ai platform tool: 工具 %s 调用失败: %v", fn, err)
			return "工具调用失败：" + err.Error()
		}
		if runes := []rune(result); len(runes) > MCPToolResultMaxRunes {
			return string(runes[:MCPToolResultMaxRunes]) + "…（工具结果过长已截断）"
		}
		return result
	}
	if tc.mcp != nil {
		return s.execMCPTool(ctx, tc.mcp, fn, args, notify)
	}
	return "错误：未知工具 " + fn + "（不在可用工具集中）"
}

// toolLoopFinish 为两套循环共用的轮次上限收尾：输出已达成文本，无文本
// 时提示已达上限（提示文本同样经 emit 流出）。
func toolLoopFinish(emit func(string), content strings.Builder) string {
	out := strings.TrimSpace(content.String())
	if out == "" {
		out = "（已达工具调用轮次上限，未能生成最终回答；请基于已有工具结果继续提问。）"
		emit(out)
	}
	return out
}

// openAIToolCall 为聚合完成的一次工具调用（arguments 为 JSON 字符串，
// 原样回传 assistant 消息与转发 MCP）。
type openAIToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// openAIRound 为 openai 工具循环单轮结果。
type openAIRound struct {
	Content                        string
	ToolCalls                      []openAIToolCall
	PromptTokens, CompletionTokens int
}

// chatOpenAITools openai_compatible 工具循环：请求带 tools+tool_choice:
// auto（MCP 外部工具与内置平台工具 df_* 合并装配）；finish_reason=
// tool_calls → 逐调用发工具事件、按名称分发执行（内置优先，见 execTool）、
// 追加 {assistant tool_calls}+{tool 结果} 消息后下一轮（同一请求形状带
// tools）。流式轮 delta.content 照旧透传（onDelta）、delta.tool_calls 按
// index 渐进聚合；流式失败回退非流式（同 chatOpenAI）。think 每轮透传。
// usage 按轮累计到最终 ChatResult（一次对话经既有 recordUsage 记一条用量）。
func (s *Service) chatOpenAITools(ctx context.Context, p settings.AIProvider, model string, req ChatRequest, temperature float64, maxTokens int, onDelta func(string), set *chatToolContext) (ChatResult, error) {
	tools := set.openAITools()
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	var res ChatResult
	var content strings.Builder
	emit := func(text string) {
		if text == "" {
			return
		}
		content.WriteString(text)
		if onDelta != nil {
			onDelta(text)
		}
	}
	for execs := 0; ; {
		round, err := s.openAIToolRound(ctx, p, model, req, temperature, maxTokens, msgs, tools, emit)
		if err != nil {
			res.Content = strings.TrimSpace(content.String())
			return res, err
		}
		res.PromptTokens += round.PromptTokens
		res.CompletionTokens += round.CompletionTokens
		if len(round.ToolCalls) == 0 {
			res.Content = strings.TrimSpace(content.String())
			return res, nil
		}
		if execs >= MCPToolMaxRounds {
			res.Content = toolLoopFinish(emit, content)
			return res, nil
		}
		execs++
		// assistant 回传：tool_calls 以 openai 原始形状回填（content 置
		// null——纯工具调用回合无正文）。
		calls := make([]map[string]any, 0, len(round.ToolCalls))
		for _, tc := range round.ToolCalls {
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": tc.Arguments,
				},
			})
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls})
		for _, tc := range round.ToolCalls {
			args := json.RawMessage(strings.TrimSpace(tc.Arguments))
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": tc.ID, "content": s.execTool(ctx, set, tc.Name, args, req.OnTool)})
		}
	}
}

// openAIToolRound 执行一轮 openai 兼容请求（带 tools；stream 失败回退
// 非流式一次）。emit 在流式时逐 delta 调用、非流式时对整段文本调用。
func (s *Service) openAIToolRound(ctx context.Context, p settings.AIProvider, model string, req ChatRequest, temperature float64, maxTokens int, msgs []map[string]any, tools []map[string]any, emit func(string)) (openAIRound, error) {
	url := strings.TrimSuffix(p.BaseURL, "/") + "/chat/completions"
	do := func(stream bool) (openAIRound, error) {
		body := map[string]any{
			"model":       model,
			"messages":    msgs,
			"temperature": temperature,
			"max_tokens":  maxTokens,
			"tools":       tools,
			"tool_choice": "auto",
		}
		if req.Think {
			// 推理思考每轮透传（同 chatOpenAI）。
			body["reasoning_effort"] = "medium"
		}
		if stream {
			body["stream"] = true
			body["stream_options"] = map[string]any{"include_usage": true}
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return openAIRound{}, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return openAIRound{}, fmt.Errorf("%w: build request: %v", ErrUpstreamChat, err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
		}
		resp, err := s.client.Do(httpReq)
		if err != nil {
			return openAIRound{}, fmt.Errorf("%w: %v", ErrUpstreamChat, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return openAIRound{}, fmt.Errorf("%w: status %d: %s", ErrUpstreamChat, resp.StatusCode, truncateBytes(raw, 200))
		}
		if stream {
			return consumeOpenAISSETools(resp.Body, emit)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return openAIRound{}, fmt.Errorf("%w: read response: %v", ErrUpstreamChat, err)
		}
		var out openAIResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return openAIRound{}, fmt.Errorf("%w: decode response: %v", ErrUpstreamChat, err)
		}
		if out.Error != nil {
			return openAIRound{}, fmt.Errorf("%w: %s", ErrUpstreamChat, out.Error.Message)
		}
		if len(out.Choices) == 0 {
			return openAIRound{}, fmt.Errorf("%w: empty completion", ErrUpstreamChat)
		}
		round := openAIRound{Content: strings.TrimSpace(out.Choices[0].Message.Content)}
		for _, tc := range out.Choices[0].Message.ToolCalls {
			round.ToolCalls = append(round.ToolCalls, openAIToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		if out.Usage != nil {
			round.PromptTokens, round.CompletionTokens = out.Usage.PromptTokens, out.Usage.CompletionTokens
		}
		emit(round.Content) // 非流式轮的整段文本同样经 emit 累计/流出
		return round, nil
	}
	round, err := do(req.Stream)
	if req.Stream && err != nil {
		round, err = do(false)
	}
	return round, err
}

// consumeOpenAISSETools 解析工具循环轮的 SSE 流：delta.content 照旧
// 透传（有待定 tool_calls 聚合时不透传，防与工具参数交错乱序——文本
// 仍累计）、delta.tool_calls 按 index 渐进聚合（id 首块全量、name/
// arguments 分片拼接）。
func consumeOpenAISSETools(r io.Reader, emit func(string)) (openAIRound, error) {
	var round openAIRound
	var content strings.Builder
	calls := make(map[int]*openAIToolCall)
	var order []int
	pending := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	get := func(i int) *openAIToolCall {
		if c, ok := calls[i]; ok {
			return c
		}
		c := &openAIToolCall{}
		calls[i] = c
		order = append(order, i)
		return c
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 跳过无法解析的 chunk（兼容网关注释行/心跳）
		}
		if chunk.Error != nil {
			return round, fmt.Errorf("%w: %s", ErrUpstreamChat, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			round.PromptTokens, round.CompletionTokens = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != "" {
				content.WriteString(c.Delta.Content)
				if !pending {
					emit(c.Delta.Content)
				}
			}
			for _, tc := range c.Delta.ToolCalls {
				pending = true
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				call := get(idx)
				if tc.ID != "" {
					call.ID = tc.ID
				}
				call.Name += tc.Function.Name
				call.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return round, fmt.Errorf("%w: read stream: %v", ErrUpstreamChat, err)
	}
	sort.Ints(order)
	for _, i := range order {
		round.ToolCalls = append(round.ToolCalls, *calls[i])
	}
	round.Content = strings.TrimSpace(content.String())
	if len(round.ToolCalls) == 0 && round.Content == "" {
		return round, fmt.Errorf("%w: empty stream response", ErrUpstreamChat)
	}
	return round, nil
}

// anthropicRound 为 anthropic 工具循环单轮结果（Blocks 为 assistant 的
// 原始 content 块序列，回传下一轮用）。
type anthropicRound struct {
	Blocks                         []anthropicContentBlock
	PromptTokens, CompletionTokens int
	StopReason                     string
}

// chatAnthropicTools anthropic 工具循环（MCP 外部工具与内置平台工具
// df_* 合并装配）：stop_reason=tool_use → 按名称分发执行（内置优先，
// 见 execTool）缓冲的 tool_use 块 → messages 追加 assistant 原始 content
// 块（text+thinking+tool_use——anthropic 要求 thinking 开启时签名块随
// tool_use 回传）与 user 的 tool_result 块 → 下一轮。think 每轮透传
// （max_tokens 抬高逻辑同 chatAnthropic）；text delta 照旧透传；usage 按
// 轮累计。内部消息以 map[string]any 承载（content 可为 string 或块数组，
// string 字段的 Message 无法表达——对现有 Message/chatAnthropic 零侵入）。
func (s *Service) chatAnthropicTools(ctx context.Context, p settings.AIProvider, model string, req ChatRequest, temperature float64, maxTokens int, onDelta func(string), set *chatToolContext) (ChatResult, error) {
	tools := set.anthropicTools()
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	if req.Think && maxTokens <= AnthropicThinkBudgetTokens {
		maxTokens = AnthropicThinkBudgetTokens + 1024
	}
	var res ChatResult
	var content strings.Builder
	emit := func(text string) {
		if text == "" {
			return
		}
		content.WriteString(text)
		if onDelta != nil {
			onDelta(text)
		}
	}
	for execs := 0; ; {
		round, err := s.anthropicToolRound(ctx, p, model, req, temperature, maxTokens, msgs, tools, emit)
		if err != nil {
			res.Content = strings.TrimSpace(content.String())
			return res, err
		}
		res.PromptTokens += round.PromptTokens
		res.CompletionTokens += round.CompletionTokens
		var toolBlocks, blocks []anthropicContentBlock
		for _, b := range round.Blocks {
			if b.Type == "tool_use" {
				toolBlocks = append(toolBlocks, b)
				blocks = append(blocks, b)
				continue
			}
			if b.Type == "text" && strings.TrimSpace(b.Text) == "" {
				continue // 空文本块剔除（部分网关补占位空块）
			}
			blocks = append(blocks, b)
		}
		if len(toolBlocks) == 0 {
			res.Content = strings.TrimSpace(content.String())
			return res, nil
		}
		if execs >= MCPToolMaxRounds {
			res.Content = toolLoopFinish(emit, content)
			return res, nil
		}
		execs++
		// assistant 回传原始块 + user 的 tool_result 块（一个 user 消息
		// 承载全部结果，anthropic 多工具并行语义）。
		msgs = append(msgs, map[string]any{"role": "assistant", "content": blocks})
		results := make([]map[string]any, 0, len(toolBlocks))
		for _, tb := range toolBlocks {
			args := tb.Input
			if len(args) == 0 || !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			results = append(results, map[string]any{
				"type":        "tool_result",
				"tool_use_id": tb.ID,
				"content":     s.execTool(ctx, set, tb.Name, args, req.OnTool),
			})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": results})
	}
}

// anthropicToolRound 执行一轮 anthropic 请求（带 tools；stream 失败回退
// 非流式一次）。emit 对 text delta 逐段调用、非流式时对整段文本调用。
func (s *Service) anthropicToolRound(ctx context.Context, p settings.AIProvider, model string, req ChatRequest, temperature float64, maxTokens int, msgs []map[string]any, tools []map[string]any, emit func(string)) (anthropicRound, error) {
	url := strings.TrimSuffix(p.BaseURL, "/") + "/v1/messages"
	do := func(stream bool) (anthropicRound, error) {
		body := map[string]any{
			"model":       model,
			"max_tokens":  maxTokens,
			"temperature": temperature,
			"messages":    msgs,
			"tools":       tools,
		}
		if req.System != "" {
			body["system"] = req.System
		}
		if req.Think {
			// thinking 每轮透传（同 chatAnthropic；max_tokens 已在循环
			// 入口抬高一次，各轮复用）。
			body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": AnthropicThinkBudgetTokens}
		}
		if stream {
			body["stream"] = true
		}
		payload, err := json.Marshal(body)
		if err != nil {
			return anthropicRound{}, fmt.Errorf("%w: marshal request: %v", ErrUpstreamChat, err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return anthropicRound{}, fmt.Errorf("%w: build request: %v", ErrUpstreamChat, err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if p.APIKey != "" {
			httpReq.Header.Set("x-api-key", p.APIKey)
		}
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		resp, err := s.client.Do(httpReq)
		if err != nil {
			return anthropicRound{}, fmt.Errorf("%w: %v", ErrUpstreamChat, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return anthropicRound{}, fmt.Errorf("%w: status %d: %s", ErrUpstreamChat, resp.StatusCode, truncateBytes(raw, 200))
		}
		if stream {
			return consumeAnthropicSSETools(resp.Body, emit)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return anthropicRound{}, fmt.Errorf("%w: read response: %v", ErrUpstreamChat, err)
		}
		var out anthropicResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return anthropicRound{}, fmt.Errorf("%w: decode response: %v", ErrUpstreamChat, err)
		}
		if out.Error != nil {
			return anthropicRound{}, fmt.Errorf("%w: %s", ErrUpstreamChat, out.Error.Message)
		}
		round := anthropicRound{Blocks: out.Content, PromptTokens: out.Usage.InputTokens, CompletionTokens: out.Usage.OutputTokens}
		for _, b := range out.Content {
			if b.Type == "text" {
				emit(b.Text)
			}
		}
		if len(round.Blocks) == 0 {
			return round, fmt.Errorf("%w: empty completion", ErrUpstreamChat)
		}
		return round, nil
	}
	round, err := do(req.Stream)
	if req.Stream && err != nil {
		round, err = do(false)
	}
	return round, err
}

// consumeAnthropicSSETools 解析工具循环轮的 anthropic 流：text_delta 照旧
// 透传；content_block_start 缓冲块（text/thinking/tool_use）、
// input_json_delta 拼接工具参数、thinking_delta/signature_delta 聚合推理
// 块（随 tool_use 回传满足 thinking 签名约束）、message_delta 取 usage
// 与 stop_reason。
func consumeAnthropicSSETools(r io.Reader, emit func(string)) (anthropicRound, error) {
	var round anthropicRound
	blocks := make(map[int]*anthropicContentBlock)
	inputs := make(map[int]*strings.Builder)
	var order []int
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	get := func(i int, blockType string) *anthropicContentBlock {
		if b, ok := blocks[i]; ok {
			return b
		}
		b := &anthropicContentBlock{Type: blockType}
		blocks[i] = b
		inputs[i] = &strings.Builder{}
		order = append(order, i)
		return b
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Error != nil {
			return round, fmt.Errorf("%w: %s", ErrUpstreamChat, ev.Error.Message)
		}
		switch ev.Type {
		case "message_start":
			round.PromptTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			b := get(ev.Index, ev.ContentBlock.Type)
			b.ID, b.Name = ev.ContentBlock.ID, ev.ContentBlock.Name
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				b := get(ev.Index, "text")
				b.Text += ev.Delta.Text
				emit(ev.Delta.Text)
			case "thinking_delta":
				get(ev.Index, "thinking").Thinking += ev.Delta.Text
			case "signature_delta":
				get(ev.Index, "thinking").Signature += ev.Delta.Signature
			case "input_json_delta":
				get(ev.Index, "tool_use")
				inputs[ev.Index].WriteString(ev.Delta.PartialJSON)
			}
		case "message_delta":
			round.CompletionTokens = ev.Usage.OutputTokens
			if ev.Delta.StopReason != "" {
				round.StopReason = ev.Delta.StopReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return round, fmt.Errorf("%w: read stream: %v", ErrUpstreamChat, err)
	}
	sort.Ints(order)
	hasText := false
	for _, i := range order {
		b := *blocks[i]
		if b.Type == "tool_use" {
			raw := json.RawMessage(inputs[i].String())
			if len(raw) == 0 || !json.Valid(raw) {
				raw = json.RawMessage("{}") // 参数聚合为空/非法（如模型无参调用）兜底
			}
			b.Input = raw
		}
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			hasText = true
		}
		round.Blocks = append(round.Blocks, b)
	}
	if len(round.Blocks) == 0 || (!hasText && round.StopReason == "") {
		return round, fmt.Errorf("%w: empty stream response", ErrUpstreamChat)
	}
	return round, nil
}
