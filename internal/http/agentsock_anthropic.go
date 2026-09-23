// Package http —— agentsock_anthropic.go：Agent IPC 网关的 Anthropic
// Messages 兼容端点（POST /v1/messages，与 /chat 同一 unix socket）。
//
// 面向 Anthropic 协议客户端（Claude Code harness——claude -p 经
// ANTHROPIC_BASE_URL 指向本网关的 http+unix 客户端）在断网沙箱内以平台
// AI 为模型后端运行。鉴权/限流/审计与 POST /chat 完全一致：同一令牌表、
// 同一 calls 计数（三端点共同消耗），审计 action 仍 agent.ai，metadata
// 追加 endpoint=anthropic。请求 model 字段忽略——安全边界同 openai 端点
// （不允许容器内任选模型），一律平台默认对话模型。
//
// 工具调用（本端点核心价值）走「透传」双路实现（取舍见下）：
//   - 纯文本请求（无 tools 且消息不含 tool_use/tool_result 块）：经既有
//     chat 回调走平台统一 ChatRequest 链路（任意 Provider kind 可用，含
//     mock；用量记账/重试/非流式回退全部继承），网关仅做协议封装——
//     非流式 Anthropic message JSON 信封 / 流式标准 Anthropic SSE 序列
//     （message_start → content_block_start/delta/stop → message_delta
//     → message_stop，增量经 onDelta）；
//   - 带 tools 或含 tool_use/tool_result 块的请求（Claude Code 的常态：
//     模型端 function calling 驱动其本地 Bash/文件工具，工具结果以
//     content 块数组随多轮循环回传）：平台引擎的工具循环是服务端执行
//     （MCP/内置工具经 chatToolContext 闭环），ai.Message 为纯文本结构
//     无法承载 tool_use/tool_result 块，且引擎不暴露 per-protocol 底层
//     调用、无「仅声明不执行」的透传模式——因此网关内实现独立的
//     anthropic HTTP 客户端直连目标 Provider：经注入的 anthropic 回调
//     （main.go 以 ai.Service.ResolveChatTargetFor 装配）解析平台默认
//     对话目标（要求 anthropic kind），拿到 baseURL/apiKey/model 后把
//     请求以 anthropic 原生形状转发（tools 声明与 content 块数组原样
//     透传，model 替换为平台默认模型），响应体（JSON 或 SSE 字节流）
//     原样回传客户端。平台仍管鉴权/限流/审计/模型路由；无状态标准
//     循环（Claude Code 本地执行工具后带 tool_result 再请求），网关不
//     存对话。注意：此路径不经 Service.Chat，ai_usage 不记账（审计仍有
//     完整记录）；默认 Provider 非 anthropic kind 时明确 502。
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/ai"
	"github.com/google/uuid"
)

// Agent IPC anthropic 端点边界常量（沿用项见 agentsock.go）。
const (
	// agentAIAnthropicMaxMessages 单次 /v1/messages 消息条数上限：agent
	// harness（Claude Code）多轮工具循环会把全量历史随每轮回传，64 不够。
	agentAIAnthropicMaxMessages = 512
	// agentAIAnthropicMaxBodyBytes 请求体上限（8MiB）：长会话历史 + 工具
	// 结果回传显著大于普通对话（/chat 与 openai 端点 2MiB）。
	agentAIAnthropicMaxBodyBytes = 8 << 20
	// agentAIAnthropicMaxTools 单次请求工具声明数上限（Claude Code 内置
	// + MCP 工具聚合通常 <64）。
	agentAIAnthropicMaxTools = 128
	// agentAIAnthropicForwardTimeout 工具透传单次上游调用超时：agent 编码
	// 场景一次补全可流式生成数万 token，放宽到 10 分钟（普通链路 120s）。
	agentAIAnthropicForwardTimeout = 10 * time.Minute
)

// ErrAgentAIAnthropicIncompatible 表示解析出的平台默认对话 Provider 非
// anthropic 协议（本端点工具透传要求 anthropic 兼容上游直连）。
var ErrAgentAIAnthropicIncompatible = errors.New("platform default chat provider is not anthropic-compatible; /v1/messages tool passthrough requires an anthropic provider")

// AgentAIAnthropicTargetFunc 为 /v1/messages 工具透传路径的目标解析回调
// （main.go 以 ai.Service.ForUser(user).ResolveChatTargetFor 装配）：
// 返回平台默认对话目标的 baseURL / apiKey / model。要求目标 Provider 为
// anthropic kind（否则应返回 ErrAgentAIAnthropicIncompatible）；未配置
// 任何 Provider 时返回 ai.ErrNoProvider。
type AgentAIAnthropicTargetFunc func(ctx context.Context, user uuid.UUID) (baseURL, apiKey, model string, err error)

// SetAnthropicTarget 注入工具透传目标解析回调；nil 保持未装配（该路径
// 请求 503）。
func (g *AgentAIGateway) SetAnthropicTarget(fn AgentAIAnthropicTargetFunc) {
	if fn != nil {
		g.anthropic = fn
	}
}

// ---------- 请求解析（Anthropic Messages 子集） ----------

// anthropicMessagesRequest 为 /v1/messages 请求子集（未识别字段一律忽略；
// temperature/tool_choice 等同 openai 端点忽略，沿用平台全局值）。
// System/Content 以 RawMessage 承载：string 或块数组二形，归一见
// agentAIAnthropicNormalize。Model 刻意不透传（安全边界，见包注释）。
type anthropicMessagesRequest struct {
	Model      string               `json:"model"`
	System     json.RawMessage      `json:"system"`
	Messages   []anthropicInMessage `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	Stream     bool                 `json:"stream"`
	Tools      []anthropicInTool    `json:"tools"`
	ToolChoice json.RawMessage      `json:"tool_choice"`
}

type anthropicInMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicInTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicOutMessage / anthropicOutTool 为转发上游的消息与工具声明
// （content 以 RawMessage 原样透传——string 或 content 块数组 JSON 保真；
// input_schema 空/非法回退 {"type":"object"}，同引擎 mcpToolSchema）。
type anthropicOutMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicOutTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// agentAIAnthropicNormalized 为归一结果：msgs 为纯文本路径的统一消息
// （text 块以 \n 连接）；rawMsgs/tools 为透传路径的原样载荷；
// needsForward 表示必须走透传（带 tools 或含 tool_use/tool_result 块）。
type agentAIAnthropicNormalized struct {
	system       string
	msgs         []ai.Message
	rawMsgs      []anthropicOutMessage
	tools        []anthropicOutTool
	maxTokens    int
	stream       bool
	needsForward bool
}

// agentAIAnthropicNormalize 校验并归一请求：system（string 或 text 块
// 数组 → 以 \n 连接的系统提示，非 text 块 400）、messages（1-512 条，
// role 仅 user/assistant——system 归位 system 字段，anthropic 协议不
// 接受 messages 内 system role；content 为 string 或块数组，仅接受
// text/tool_use/tool_result 块——多模态 image 等明确 400）、max_tokens
// （同 /chat 的 0-32000 截断）、tools（name 必填、数上限 128）。
func agentAIAnthropicNormalize(req *anthropicMessagesRequest) (*agentAIAnthropicNormalized, error) {
	norm := &agentAIAnthropicNormalized{maxTokens: req.MaxTokens, stream: req.Stream}
	var err error
	if norm.maxTokens, err = agentAIClampMaxTokens(req.MaxTokens); err != nil {
		return nil, err
	}
	if norm.system, err = agentAIAnthropicSystem(req.System); err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 || len(req.Messages) > agentAIAnthropicMaxMessages {
		return nil, fmt.Errorf("messages must contain 1-%d entries", agentAIAnthropicMaxMessages)
	}
	norm.msgs = make([]ai.Message, 0, len(req.Messages))
	norm.rawMsgs = make([]anthropicOutMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, errors.New("message role must be user or assistant (system belongs in the system field)")
		}
		plain, toolBlocks, err := agentAIAnthropicContent(m.Content)
		if err != nil {
			return nil, err
		}
		norm.msgs = append(norm.msgs, ai.Message{Role: m.Role, Content: plain})
		norm.rawMsgs = append(norm.rawMsgs, anthropicOutMessage{Role: m.Role, Content: m.Content})
		if toolBlocks {
			norm.needsForward = true
		}
	}
	if len(req.Tools) > agentAIAnthropicMaxTools {
		return nil, fmt.Errorf("too many tools: max %d", agentAIAnthropicMaxTools)
	}
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, errors.New("tool name is required")
		}
		schema := t.InputSchema
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		norm.tools = append(norm.tools, anthropicOutTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if len(norm.tools) > 0 {
		norm.needsForward = true
	}
	return norm, nil
}

// agentAIAnthropicSystem 归一 system 字段：缺省/null → ""；JSON string →
// 原文；text 块数组 → 各块 text 以 \n 连接（Claude Code 的多段系统提示
// 含 cache_control 等附加字段，忽略）；其他块类型（image 等）400。
func agentAIAnthropicSystem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("system must be a string or an array of text blocks")
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("unsupported system block type %q: only text blocks are supported", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// agentAIAnthropicContent 归一单条消息 content：string → 纯文本；块数组
// → text 块文本以 \n 连接（plain），tool_use/tool_result 块置
// hasToolBlocks（透传路径原样转发，plain 路径不可承载）；其余块类型
// （image 等多模态）明确 400——沙箱边界。tool_result 内嵌 content 为块
// 数组时同样仅接受 text 块。
func agentAIAnthropicContent(raw json.RawMessage) (plain string, hasToolBlocks bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, errors.New("message content is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, false, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", false, errors.New("message content must be a string or an array of content blocks")
	}
	if len(blocks) == 0 {
		return "", false, errors.New("message content must not be empty")
	}
	var texts []string
	for _, b := range blocks {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &head) != nil || head.Type == "" {
			return "", false, errors.New("content block requires a type field")
		}
		switch head.Type {
		case "text":
			var t struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &t)
			texts = append(texts, t.Text)
		case "tool_use", "tool_result":
			hasToolBlocks = true
			if head.Type == "tool_result" {
				if err := agentAIAnthropicToolResultContent(b); err != nil {
					return "", false, err
				}
			}
		default:
			return "", false, fmt.Errorf("unsupported content block type %q: only text/tool_use/tool_result blocks are supported (multimodal input is unavailable in this sandbox)", head.Type)
		}
	}
	return strings.Join(texts, "\n"), hasToolBlocks, nil
}

// agentAIAnthropicToolResultContent 校验 tool_result 块内嵌 content：
// string/缺省放行；块数组仅接受 text 块（Claude Code 工具结果可携带
// 截图 image 块——沙箱内不可用，明确 400）。
func agentAIAnthropicToolResultContent(block json.RawMessage) error {
	var tr struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(block, &tr); err != nil || len(tr.Content) == 0 || string(tr.Content) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(tr.Content, &s); err == nil {
		return nil
	}
	var nested []json.RawMessage
	if err := json.Unmarshal(tr.Content, &nested); err != nil {
		return errors.New("tool_result content must be a string or an array of text blocks")
	}
	for _, nb := range nested {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(nb, &head) != nil || head.Type != "text" {
			return fmt.Errorf("unsupported block inside tool_result content: only text blocks are supported (multimodal tool results are unavailable in this sandbox)")
		}
	}
	return nil
}

// ---------- 端点处理 ----------

// handleAnthropicMessages 处理 POST /v1/messages（协议子集与双路实现见
// 包注释）。错误语义与 /chat 一致：401 令牌无效/已注销、405 方法、400
// 载荷（含多模态块）、429 超出任务调用上限、502 AI 上游失败/默认 Provider
// 非 anthropic、503 网关未装配 AI 或透传未配置。错误体为 anthropic 形状
// {"type":"error","error":{type,message}}（鉴权/限流共用闸门的错误体除外）。
func (g *AgentAIGateway) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentAIError(w, http.StatusMethodNotAllowed, "POST /v1/messages only")
		return
	}
	info, ok := g.authorize(w, r)
	if !ok {
		return
	}
	if g.chat == nil {
		writeAgentAIError(w, http.StatusServiceUnavailable, "platform ai is not configured")
		return
	}
	var req anthropicMessagesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentAIAnthropicMaxBodyBytes)).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body")
		return
	}
	norm, err := agentAIAnthropicNormalize(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if norm.needsForward {
		g.serveAnthropicForward(w, r, info, norm)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentAIChatTimeout)
	defer cancel()
	if norm.stream {
		g.serveAnthropicPlainStream(w, ctx, info, req.Model, norm)
		return
	}
	content, model, err := g.chat(ctx, info.user, norm.system, norm.msgs, norm.maxTokens, nil)
	g.auditAICall(info, model, err, "anthropic")
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "platform ai request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(anthropicMessageEnvelope{ID: "msg_" + info.taskID, Type: "message", Role: "assistant", Model: model, Content: []anthropicTextBlock{{Type: "text", Text: content}}, StopReason: "end_turn"})
}

// serveAnthropicForward 处理工具透传路径：解析目标（anthropic 回调）→
// 组装 anthropic 原生请求（model 替换为平台默认；system 归一字符串；
// messages content 块与 tools 原样透传；max_tokens 缺省按上限）→ 直连
// 上游 → 响应体（JSON 或 SSE 字节流）原样回传。上游非 2xx 汇为 502
// （附上游错误摘要）；流式回传中断（客户端断开/上游断流）记审计失败。
func (g *AgentAIGateway) serveAnthropicForward(w http.ResponseWriter, r *http.Request, info agentAITokenInfo, norm *agentAIAnthropicNormalized) {
	if g.anthropic == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "tool passthrough is not configured on this gateway")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentAIAnthropicForwardTimeout)
	defer cancel()
	baseURL, apiKey, model, err := g.anthropic(ctx, info.user)
	if err != nil {
		g.auditAICall(info, model, err, "anthropic")
		if errors.Is(err, ai.ErrNoProvider) {
			writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "platform ai is not configured")
			return
		}
		if errors.Is(err, ErrAgentAIAnthropicIncompatible) {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "platform ai request failed")
		return
	}
	maxTokens := norm.maxTokens
	if maxTokens <= 0 { // anthropic 上游必填 max_tokens：缺省按网关上限。
		maxTokens = agentAIMaxTokens
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   norm.rawMsgs,
	}
	if norm.system != "" {
		body["system"] = norm.system
	}
	if len(norm.tools) > 0 {
		body["tools"] = norm.tools
	}
	if norm.stream {
		body["stream"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		g.auditAICall(info, model, err, "anthropic")
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "platform ai request failed")
		return
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		g.auditAICall(info, model, err, "anthropic")
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "platform ai request failed")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("x-api-key", apiKey)
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := g.client.Do(httpReq)
	if err != nil {
		g.auditAICall(info, model, err, "anthropic")
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "platform ai request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		g.auditAICall(info, model, fmt.Errorf("upstream status %d", resp.StatusCode), "anthropic")
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream ai error: "+agentAIAnthropicUpstreamError(raw))
		return
	}
	if norm.stream {
		g.auditAICall(info, model, g.pipeAnthropicSSE(w, resp.Body), "anthropic")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, io.LimitReader(resp.Body, agentAIAnthropicMaxBodyBytes))
	g.auditAICall(info, model, copyErr, "anthropic")
}

// pipeAnthropicSSE 把上游 SSE 字节流原样转发给客户端（逐读块 flush）；
// EOF 为正常收尾（nil），读写错误（客户端断开/上游断流）返回错误交由
// 调用方记审计。
func (g *AgentAIGateway) pipeAnthropicSSE(w http.ResponseWriter, body io.Reader) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// agentAIAnthropicUpstreamError 提取上游错误摘要：优先解析 anthropic
// 错误信封的 error.message，否则原样截断。
func agentAIAnthropicUpstreamError(raw []byte) string {
	var up struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &up) == nil && up.Error != nil && up.Error.Message != "" {
		return up.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// ---------- 纯文本路径的协议封装 ----------

// anthropicTextBlock / anthropicUsage / anthropicMessageEnvelope 为非流式
// message 响应信封（usage 平台回调不回传令牌计数，恒 0 占位——anthropic
// 客户端仅信息性消费，harness 不依赖）。
type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicMessageEnvelope struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []anthropicTextBlock `json:"content"`
	StopReason   string               `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        anthropicUsage       `json:"usage"`
}

// anthropicStreamMessage 为 message_start 的 message 头（增量阶段平台实际
// 模型尚未返回，model 回显请求值——anthropic 客户端对该字段仅信息性消费）。
type anthropicStreamMessage struct {
	ID           string               `json:"id"`
	Type         string               `json:"type"`
	Role         string               `json:"role"`
	Model        string               `json:"model"`
	Content      []anthropicTextBlock `json:"content"`
	StopReason   *string              `json:"stop_reason"`
	StopSequence *string              `json:"stop_sequence"`
	Usage        anthropicUsage       `json:"usage"`
}

type anthropicEventMessageStart struct {
	Type    string                 `json:"type"`
	Message anthropicStreamMessage `json:"message"`
}

type anthropicEventBlockStart struct {
	Type         string             `json:"type"`
	Index        int                `json:"index"`
	ContentBlock anthropicTextBlock `json:"content_block"`
}

type anthropicTextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicEventBlockDelta struct {
	Type  string             `json:"type"`
	Index int                `json:"index"`
	Delta anthropicTextDelta `json:"delta"`
}

type anthropicEventBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type anthropicStopDelta struct {
	StopReason   string  `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type anthropicEventMessageDelta struct {
	Type  string             `json:"type"`
	Delta anthropicStopDelta `json:"delta"`
	Usage anthropicUsage     `json:"usage"`
}

// writeNamedEvent 写一条 anthropic 具名 SSE 事件（event: <type> +
// data: <json>），底层复用 agentAISSEWriter 的惰性提交与 flush。
func (s *agentAISSEWriter) writeNamedEvent(event string, payload any) bool {
	if s.dead {
		return false
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return s.writeRaw("event: " + event + "\ndata: " + string(b) + "\n\n")
}

// serveAnthropicPlainStream 处理纯文本路径 stream=true：惰性开流（首个
// 事件写出时才提交 200——平台失败且零增量时可回退标准 502 JSON 错误），
// 标准 anthropic 事件序列：message_start → （首个增量时）content_block_start
// → 逐 delta content_block_delta(text_delta) → content_block_stop →
// message_delta(end_turn) → message_stop。零增量成功收尾同样产出完整
// 空文本块序列（合法 anthropic 流）。
func (g *AgentAIGateway) serveAnthropicPlainStream(w http.ResponseWriter, ctx context.Context, info agentAITokenInfo, requestModel string, norm *agentAIAnthropicNormalized) {
	sse := &agentAISSEWriter{w: w}
	head := anthropicEventMessageStart{Type: "message_start", Message: anthropicStreamMessage{
		ID: "msg_" + info.taskID, Type: "message", Role: "assistant", Model: requestModel,
		Content: []anthropicTextBlock{}, Usage: anthropicUsage{},
	}}
	blockStart := anthropicEventBlockStart{Type: "content_block_start", Index: 0, ContentBlock: anthropicTextBlock{Type: "text"}}
	blockOpen := false
	onDelta := func(text string) {
		if text == "" {
			return
		}
		if !sse.started {
			sse.writeNamedEvent("message_start", head)
		}
		if !blockOpen {
			blockOpen = sse.writeNamedEvent("content_block_start", blockStart)
		}
		sse.writeNamedEvent("content_block_delta", anthropicEventBlockDelta{Type: "content_block_delta", Index: 0, Delta: anthropicTextDelta{Type: "text_delta", Text: text}})
	}
	_, model, err := g.chat(ctx, info.user, norm.system, norm.msgs, norm.maxTokens, onDelta)
	g.auditAICall(info, model, err, "anthropic")
	if err != nil {
		if !sse.started { // 零增量：尚未提交 SSE 头，回退标准错误。
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "platform ai request failed")
			return
		}
		// 已开流只能截断（无收尾事件），客户端按流异常处理。
		log.Printf("agent ai ipc: anthropic stream aborted (task %s): %v", info.taskID, err)
		return
	}
	if !sse.started {
		sse.writeNamedEvent("message_start", head)
	}
	if !blockOpen {
		sse.writeNamedEvent("content_block_start", blockStart)
	}
	sse.writeNamedEvent("content_block_stop", anthropicEventBlockStop{Type: "content_block_stop", Index: 0})
	sse.writeNamedEvent("message_delta", anthropicEventMessageDelta{
		Type:  "message_delta",
		Delta: anthropicStopDelta{StopReason: "end_turn"},
		Usage: anthropicUsage{},
	})
	sse.writeNamedEvent("message_stop", map[string]string{"type": "message_stop"})
}

// writeAnthropicError 写 anthropic 形状错误体（{"type":"error","error":
// {type,message}}）；type 按状态码映射（400 invalid_request_error /
// 401 authentication_error / 429 rate_limit_error / 其余 api_error）。
func writeAnthropicError(w http.ResponseWriter, code int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
