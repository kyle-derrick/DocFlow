// Package http —— agentsock_openai.go：Agent IPC 网关的 OpenAI Chat
// Completions 兼容端点（POST /v1/chat/completions，与 /chat 同一 unix
// socket）。
//
// 面向 OpenAI 协议客户端（成熟 agent harness——如 pi/Codex CLI 经
// OPENAI_BASE_URL 指向本网关的 http+unix 客户端）在断网沙箱内以平台
// AI 为模型后端运行：
//   - 鉴权/限流/审计与 POST /chat 完全一致：同一令牌表、同一 calls 计数
//     （agent.ai_max_calls，三端点共同消耗），审计 action 仍 agent.ai，
//     metadata 追加 endpoint=openai（工具直连路径为 openai-forward）；
//   - 请求 model 字段忽略——安全边界：不允许容器内任选模型，一律平台
//     默认对话模型（直连转发路径 model 替换为平台默认模型）；
//     temperature 同样忽略（沿用平台全局值）；
//   - 工具调用（tools）走「直连转发」双路实现（对齐 agentsock_anthropic.go
//     的取舍）：
//     · 纯文本请求（无 tools 且消息不含 tool_calls/tool 角色回放）：
//     经既有 chat 回调走平台统一 ChatRequest 链路（行为不变）：请求
//     子集 messages（role=system/developer/user/assistant，system/
//     developer 照 /chat 归位为系统提示）、stream（SSE 增量 +
//     [DONE]）、max_tokens（同 /chat 的 0-32000 截断）；content 仅
//     接受字符串——multimodal 数组载荷按无效请求 400（子集边界）；
//     · 带 tools 或含 tool_calls/tool 角色回放的请求（pi 的常态：模型端
//     function calling 驱动其本地 Bash/文件工具，工具结果以 role=tool
//     消息随多轮循环回传）：ai.Message 纯文本结构无法承载
//     tool_calls/tool 消息，网关内经注入的 openai 回调（main.go 以
//     ai.Service.ResolveChatTargetFor 装配）解析平台默认对话目标
//     （要求 openai 兼容 kind），请求体改写（model 替换为平台默认
//     模型；tools/tool_choice/messages 原样；Authorization: Bearer
//     <provider key>）直发 <baseURL>/chat/completions，响应体
//     （JSON/SSE）原样管道回传（流式逐块 flush）。平台仍管鉴权/
//     限流/审计/模型路由；无状态标准循环（pi 本地执行工具后带
//     role=tool 结果再请求），网关不存对话。此路径不经 Service.Chat，
//     ai_usage 不记账（审计仍有完整记录）；默认 Provider 非 openai
//     兼容 kind 时明确 502。
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

// Agent IPC openai 端点边界常量（沿用项见 agentsock.go）。
const (
	// agentAIOpenAIMaxMessages 单次 /v1/chat/completions 消息条数总上限：
	// agent harness（pi）多轮工具循环会把全量历史随每轮回传，对齐
	// anthropic 端点取 512；纯文本路径仍受 agentAINormalizeMessages 的
	// 64 条子集上限约束（行为不变）。
	agentAIOpenAIMaxMessages = 512
	// agentAIOpenAIForwardTimeout 工具直连单次上游调用超时：agent 编码
	// 场景一次补全可流式生成数万 token，对齐 anthropic 透传放宽到
	// 10 分钟（普通链路 120s）。
	agentAIOpenAIForwardTimeout = 10 * time.Minute
)

// ErrAgentAIOpenAIIncompatible 表示解析出的平台默认对话 Provider 非
// OpenAI 兼容协议（本端点工具直连要求 openai 兼容上游）。
var ErrAgentAIOpenAIIncompatible = errors.New("platform default chat provider is not openai-compatible; /v1/chat/completions tool passthrough requires an openai-compatible provider")

// AgentAIOpenAITargetFunc 为 /v1/chat/completions 工具直连路径的目标解析
// 回调（main.go 以 ai.Service.ForUser(user).ResolveChatTargetFor 装配）：
// 返回平台默认对话目标的 baseURL / apiKey / model。要求目标 Provider 为
// openai 兼容 kind（否则应返回 ErrAgentAIOpenAIIncompatible）；未配置
// 任何 Provider 时返回 ai.ErrNoProvider。
type AgentAIOpenAITargetFunc func(ctx context.Context, user uuid.UUID) (baseURL, apiKey, model string, err error)

// SetOpenAITarget 注入工具直连目标解析回调；nil 保持未装配（该路径带
// tools/tool_calls 请求 503）。
func (g *AgentAIGateway) SetOpenAITarget(fn AgentAIOpenAITargetFunc) {
	if fn != nil {
		g.openai = fn
	}
}

// openAICompletionRequest 为 /v1/chat/completions 请求子集（未识别字段
// 一律忽略）。Model/Temperature 刻意不透传（见包注释）；Tools/ToolChoice
// 与消息的 ToolCalls 以 RawMessage 承载（在场检测 + 转发原样透传）。
type openAICompletionRequest struct {
	Model       string            `json:"model"`
	Messages    []openAIInMessage `json:"messages"`
	Stream      bool              `json:"stream"`
	Temperature *float64          `json:"temperature"`
	MaxTokens   int               `json:"max_tokens"`
	Tools       json.RawMessage   `json:"tools"`
	ToolChoice  json.RawMessage   `json:"tool_choice"`
}

// openAIInMessage 为消息的原样载荷：Content 以 RawMessage 承载（纯文本
// 路径校验为 string——multimodal 数组 400；直连路径原样转发，assistant
// tool_calls 消息与 role=tool 消息按协议 content 可为 null）。ToolCalls/
// ToolCallID/Name 仅在场检测（触发直连）+ 转发时透传。
type openAIInMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// agentAIOpenAINormalized 为归一结果：msgs/system 为纯文本路径的统一
// 消息（既有子集校验）；rawMsgs/tools/toolChoice 为直连路径的原样载荷；
// needsForward 表示必须走直连（带 tools 或含 tool_calls/tool 角色回放）。
type agentAIOpenAINormalized struct {
	system       string
	msgs         []ai.Message
	rawMsgs      []openAIInMessage
	tools        json.RawMessage
	toolChoice   json.RawMessage
	maxTokens    int
	stream       bool
	needsForward bool
}

// agentAIOpenAINormalize 校验并归一请求：消息条数 1-512、max_tokens（同
// /chat 的 0-32000 截断）、tools 在场检测（非 null 即触发直连，检测先于
// content 校验——工具回放消息的 content 按协议可为数组/null，原样转发）；
// 纯文本路径进一步按既有子集归一（content 仅字符串、system/developer
// 归位、64 条上限——复用 agentAINormalizeMessages，行为不变）。
func agentAIOpenAINormalize(req *openAICompletionRequest) (*agentAIOpenAINormalized, error) {
	norm := &agentAIOpenAINormalized{rawMsgs: req.Messages, tools: req.Tools, toolChoice: req.ToolChoice, maxTokens: req.MaxTokens, stream: req.Stream}
	var err error
	if norm.maxTokens, err = agentAIClampMaxTokens(req.MaxTokens); err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 || len(req.Messages) > agentAIOpenAIMaxMessages {
		return nil, fmt.Errorf("messages must contain 1-%d entries", agentAIOpenAIMaxMessages)
	}
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		norm.needsForward = true
	}
	for _, m := range req.Messages {
		if m.Role == "tool" || (m.Role == "assistant" && len(m.ToolCalls) > 0 && string(m.ToolCalls) != "null") {
			norm.needsForward = true
			break
		}
	}
	if norm.needsForward {
		return norm, nil
	}
	plain := make([]ai.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		var content string
		if len(m.Content) > 0 && string(m.Content) != "null" {
			if jerr := json.Unmarshal(m.Content, &content); jerr != nil {
				return nil, errors.New("message content must be a string (multimodal arrays are not supported on the plain path)")
			}
		}
		plain = append(plain, ai.Message{Role: m.Role, Content: content})
	}
	if norm.system, norm.msgs, err = agentAINormalizeMessages(plain); err != nil {
		return nil, err
	}
	return norm, nil
}

// openAICompletionMessage / openAICompletionChoice / openAIUsage /
// openAICompletionResponse 为非流式响应（choices[0].message.content +
// finish_reason:stop；usage 平台回调不回传令牌计数，恒 0 占位——OpenAI
// 客户端仅信息性消费，harness 不依赖）。
type openAICompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAICompletionChoice struct {
	Index        int                     `json:"index"`
	Message      openAICompletionMessage `json:"message"`
	FinishReason string                  `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAICompletionResponse struct {
	ID      string                   `json:"id"`
	Object  string                   `json:"object"`
	Created int64                    `json:"created"`
	Model   string                   `json:"model"`
	Choices []openAICompletionChoice `json:"choices"`
	Usage   openAIUsage              `json:"usage"`
}

// openAIChunkDelta / openAIChunkChoice / openAIChunk 为流式增量块
// （object=chat.completion.chunk；非收尾块 finish_reason 为 null，收尾块
// delta 空 + finish_reason:"stop"）。Model 为请求值回显——增量阶段平台
// 实际模型尚未返回，OpenAI 客户端对该字段仅信息性消费（实际服务模型以
// 非流式响应与审计为准）。
type openAIChunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type openAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type openAIChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []openAIChunkChoice `json:"choices"`
}

// handleOpenAIChatCompletions 处理 POST /v1/chat/completions（协议子集与
// 双路实现见包注释）。错误语义与 /chat 一致：401 令牌无效/已注销、405
// 方法、400 载荷（含纯文本路径 multimodal 数组）、429 超出任务调用上限、
// 502 AI 上游失败/默认 Provider 非 openai 兼容（工具直连路径）、503 网关
// 未装配 AI 或直连未配置。
func (g *AgentAIGateway) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentAIError(w, http.StatusMethodNotAllowed, "POST /v1/chat/completions only")
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
	var req openAICompletionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentAIMaxBodyBytes)).Decode(&req); err != nil {
		writeAgentAIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	norm, err := agentAIOpenAINormalize(&req)
	if err != nil {
		writeAgentAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if norm.needsForward {
		g.serveOpenAIForward(w, r, info, norm)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentAIChatTimeout)
	defer cancel()
	if norm.stream {
		g.serveOpenAIStream(w, ctx, info, req.Model, norm.system, norm.msgs, norm.maxTokens)
		return
	}
	content, model, err := g.chat(ctx, info.user, norm.system, norm.msgs, norm.maxTokens, nil)
	g.auditAICall(info, model, err, "openai")
	if err != nil {
		writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(openAICompletionResponse{
		ID:      "chatcmpl-" + info.taskID,
		Object:  "chat.completion",
		Created: g.now().Unix(),
		Model:   model,
		Choices: []openAICompletionChoice{{Index: 0, Message: openAICompletionMessage{Role: "assistant", Content: content}, FinishReason: "stop"}},
		Usage:   openAIUsage{},
	})
}

// serveOpenAIForward 处理工具直连路径：解析目标（openai 回调，要求
// openai 兼容 kind）→ 组装 OpenAI 原生请求（model 替换为平台默认；
// tools/tool_choice/messages 原样透传；max_tokens 缺省不送；Bearer 鉴权）
// → 直发 <baseURL>/chat/completions → 响应体（JSON 或 SSE 字节流）原样
// 回传（流式逐块 flush，复用 anthropic 端点的通用管道）。上游非 2xx 汇
// 为 502（附上游错误摘要——OpenAI 错误信封 {"error":{"message"}} 与
// anthropic 同形，复用其摘要提取）；审计 endpoint=openai-forward。
func (g *AgentAIGateway) serveOpenAIForward(w http.ResponseWriter, r *http.Request, info agentAITokenInfo, norm *agentAIOpenAINormalized) {
	if g.openai == nil {
		writeAgentAIError(w, http.StatusServiceUnavailable, "tool passthrough is not configured on this gateway")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentAIOpenAIForwardTimeout)
	defer cancel()
	baseURL, apiKey, model, err := g.openai(ctx, info.user)
	if err != nil {
		g.auditAICall(info, model, err, "openai-forward")
		if errors.Is(err, ai.ErrNoProvider) {
			writeAgentAIError(w, http.StatusServiceUnavailable, "platform ai is not configured")
			return
		}
		if errors.Is(err, ErrAgentAIOpenAIIncompatible) {
			writeAgentAIError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
		return
	}
	body := map[string]any{
		"model":    model,
		"messages": norm.rawMsgs,
	}
	if len(norm.tools) > 0 && string(norm.tools) != "null" {
		body["tools"] = json.RawMessage(norm.tools)
	}
	if len(norm.toolChoice) > 0 && string(norm.toolChoice) != "null" {
		body["tool_choice"] = json.RawMessage(norm.toolChoice)
	}
	if norm.maxTokens > 0 {
		body["max_tokens"] = norm.maxTokens
	}
	if norm.stream {
		body["stream"] = true
	}
	payload, err := json.Marshal(body)
	if err != nil {
		g.auditAICall(info, model, err, "openai-forward")
		writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
		return
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		g.auditAICall(info, model, err, "openai-forward")
		writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := g.client.Do(httpReq)
	if err != nil {
		g.auditAICall(info, model, err, "openai-forward")
		writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		g.auditAICall(info, model, fmt.Errorf("upstream status %d", resp.StatusCode), "openai-forward")
		writeAgentAIError(w, http.StatusBadGateway, "upstream ai error: "+agentAIAnthropicUpstreamError(raw))
		return
	}
	if norm.stream {
		g.auditAICall(info, model, g.pipeAnthropicSSE(w, resp.Body), "openai-forward")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, io.LimitReader(resp.Body, agentAIAnthropicMaxBodyBytes))
	g.auditAICall(info, model, copyErr, "openai-forward")
}

// serveOpenAIStream 处理 stream=true：惰性开流（首个事件写出时才提交
// 200——平台失败且零增量时可回退标准 502 JSON 错误），每个 delta 一块
// chat.completion.chunk（首块带 delta.role=assistant，照 OpenAI 流规范），
// 成功收尾 delta:{} + finish_reason:"stop" 块，最后 data: [DONE]。
func (g *AgentAIGateway) serveOpenAIStream(w http.ResponseWriter, ctx context.Context, info agentAITokenInfo, requestModel, system string, msgs []ai.Message, maxTokens int) {
	sse := &agentAISSEWriter{w: w}
	id := "chatcmpl-" + info.taskID
	created := g.now().Unix()
	chunk := func(delta openAIChunkDelta, finish *string) openAIChunk {
		return openAIChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: requestModel, Choices: []openAIChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}}}
	}
	roleSent := false
	onDelta := func(text string) {
		if text == "" {
			return
		}
		if !roleSent { // 首个增量前补 role 序幕块（照 OpenAI 流规范）。
			roleSent = sse.writeEvent(chunk(openAIChunkDelta{Role: "assistant"}, nil))
		}
		sse.writeEvent(chunk(openAIChunkDelta{Content: text}, nil))
	}
	_, model, err := g.chat(ctx, info.user, system, msgs, maxTokens, onDelta)
	g.auditAICall(info, model, err, "openai")
	if err != nil {
		if !sse.started { // 零增量：尚未提交 SSE 头，回退标准 JSON 错误。
			writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
			return
		}
		// 已开流只能截断（无 [DONE]），客户端按流异常处理。
		log.Printf("agent ai ipc: openai stream aborted (task %s): %v", info.taskID, err)
		return
	}
	if !roleSent { // 空补全兜底：保证 stop 前有序幕块。
		roleSent = sse.writeEvent(chunk(openAIChunkDelta{Role: "assistant"}, nil))
	}
	stop := "stop"
	sse.writeEvent(chunk(openAIChunkDelta{}, &stop))
	sse.writeEvent("[DONE]")
}

// agentAISSEWriter 惰性提交 SSE 响应：首个事件写出时才设置头并 200
// （此前发生错误仍可改走标准 JSON 错误响应），逐事件 Flush 推送。
type agentAISSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
	dead    bool
}

// writeEvent 写一条 `data: <payload>\n\n` 事件：payload 为 string 时原样
// 写出（[DONE] 终止标记），否则 JSON 序列化；写出失败（客户端断开）置
// dead 后续调用为 no-op（请求上下文随断开取消，上游随之中止）。
func (s *agentAISSEWriter) writeEvent(payload any) bool {
	if s.dead {
		return false
	}
	var data string
	switch v := payload.(type) {
	case string:
		data = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		data = string(b)
	}
	return s.writeRaw("data: " + data + "\n\n")
}

// writeRaw 写出一段已格式化的 SSE 文本（含收尾空行），负责惰性头提交与
// 逐段 flush；openai 的 data 事件与 anthropic 的具名事件（见
// agentsock_anthropic.go writeNamedEvent）共用此底层。
func (s *agentAISSEWriter) writeRaw(raw string) bool {
	if s.dead {
		return false
	}
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.WriteHeader(http.StatusOK)
		if f, ok := s.w.(http.Flusher); ok {
			s.flusher = f
		}
		s.started = true
	}
	if _, err := fmt.Fprint(s.w, raw); err != nil {
		s.dead = true
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}
