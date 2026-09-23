// Package http —— agentsock.go：Agent 容器的平台 AI IPC socket 网关。
//
// 断网容器（NetworkMode=none）回调平台 AI 的唯一通道：后端在 unix
// domain socket `/run/docflow-ipc/ai.sock`（目录可配 env
// DOCFLOW_AGENT_IPC_DIR）上提供最小 HTTP 服务，路由 `POST /chat`（自研
// runner）、`POST /v1/chat/completions`（OpenAI Chat Completions 协议
// 兼容子集，见 agentsock_openai.go）与 `POST /v1/messages`（Anthropic
// Messages 协议兼容子集，见 agentsock_anthropic.go）：
//   - 鉴权：`Authorization: Bearer <taskToken>`；token 为创建 agent 任务
//     时签发的一次性 uuid v4（Register），内存 map 记录归属任务/用户与
//     调用计数，任务终态即 Revoke 注销（过期）；
//   - 限流：每任务调用上限（agent.ai_max_calls，注册时固化为 limit），
//     超限 429 + JSON 错误；
//   - 处理：组装 ChatRequest 走平台默认对话模型（复用 internal/ai 的
//     非流式链路，经注入的 chat 回调；零值 ChatRequest 保证禁用工具/
//     联网/记忆/思考），返回 `{"content":"...","model":"provider/model"}`；
//   - 审计：每次调用写一条 action=agent.ai 的审计记录（成功/失败均记）。
//
// socket 目录 0700、socket 文件 0600；容器侧只读挂载（docker.go 经
// named volume docflow-agent-ipc 挂到 /run/docflow-ai）。unix domain
// 不进 gin 主路由、不入 openapi。
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
)

// Agent IPC 边界常量。
const (
	// AgentAIIPCDefaultDir socket 目录默认值（compose backend 挂载点）。
	AgentAIIPCDefaultDir = "/run/docflow-ipc"
	// AgentAISockName socket 文件名。
	AgentAISockName = "ai.sock"
	// agentAIMaxMessages 单次 /chat 的消息条数上限（openai 消息数组子集）。
	agentAIMaxMessages = 64
	// agentAIMaxBodyBytes 请求体大小上限（2MiB）。
	agentAIMaxBodyBytes = 2 << 20
	// agentAIMaxTokens 单次请求 max_tokens 上限（超出按上限截断）。
	agentAIMaxTokens = 32000
	// agentAIChatTimeout 单次平台 AI 调用超时（对齐 ai.DefaultChatTimeout）。
	agentAIChatTimeout = 120 * time.Second
	// ActionAgentAI 为 agent 容器经 IPC 调用平台 AI 的审计 action。
	ActionAgentAI = "agent.ai"
)

// AgentAIChatFunc 为平台 AI 调用回调（main.go 以 ai.Service 装配：
// ForUser(user) 记账 + 系统默认对话模型；system 为请求内 system 消息
// 归位后的系统提示；onDelta 非空时平台引擎流式逐段回调（openai 兼容
// 端点 stream 分支消费），nil 即非流式；返回内容与 "provider/model"）。
type AgentAIChatFunc func(ctx context.Context, user uuid.UUID, system string, messages []ai.Message, maxTokens int, onDelta func(string)) (content, model string, err error)

// agentAIToken 为一个已签发令牌的内存记录。
type agentAITokenInfo struct {
	taskID string
	user   uuid.UUID
	calls  int64
	limit  int64
}

// AgentAIGateway 为 agent AI IPC 的令牌注册表与 /chat 处理器。
type AgentAIGateway struct {
	mu     sync.Mutex
	tokens map[string]agentAITokenInfo
	chat   AgentAIChatFunc
	// anthropic 为 POST /v1/messages 工具透传路径的目标解析回调
	// （agentsock_anthropic.go；nil 时该端点工具请求 503）。
	anthropic AgentAIAnthropicTargetFunc
	// openai 为 POST /v1/chat/completions 工具直连路径的目标解析回调
	// （agentsock_openai.go；nil 时该端点带 tools/tool_calls 请求 503）。
	openai AgentAIOpenAITargetFunc
	// client 为网关侧 HTTP 客户端（工具透传直连上游用；超时由请求
	// context 控制）。
	client *http.Client
	audit  audit.Recorder
	now    func() time.Time
}

// NewAgentAIGateway 构造网关；chat 为 nil 时 /chat 恒 503（AI 未装配）。
func NewAgentAIGateway(chat AgentAIChatFunc) *AgentAIGateway {
	return &AgentAIGateway{tokens: make(map[string]agentAITokenInfo), chat: chat, client: &http.Client{}, audit: audit.NopRecorder{}, now: time.Now}
}

// SetAuditRecorder 注入审计写入器；nil 保持 Nop。
func (g *AgentAIGateway) SetAuditRecorder(recorder audit.Recorder) {
	if recorder != nil {
		g.audit = recorder
	}
}

// Register 为任务签发一次性 AI 令牌（uuid v4）并返回明文；limit<=0 拒绝。
// 同一任务重复签发时旧令牌被覆盖（仅保留最新）。
func (g *AgentAIGateway) Register(taskID uuid.UUID, user uuid.UUID, limit int64) string {
	if limit <= 0 {
		return ""
	}
	token := uuid.New().String()
	g.mu.Lock()
	g.tokens[token] = agentAITokenInfo{taskID: taskID.String(), user: user, limit: limit}
	g.mu.Unlock()
	return token
}

// Revoke 注销任务令牌（任务终态：succeeded/failed/cancelled 时调用）。
func (g *AgentAIGateway) Revoke(taskID uuid.UUID) {
	id := taskID.String()
	g.mu.Lock()
	for token, info := range g.tokens {
		if info.taskID == id {
			delete(g.tokens, token)
		}
	}
	g.mu.Unlock()
}

// Handler 返回 /chat、/v1/chat/completions 与 /v1/messages 路由的
// http.Handler（unix socket server 消费；openai/anthropic 兼容端点分别
// 见 agentsock_openai.go / agentsock_anthropic.go）。
func (g *AgentAIGateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat", g.handleChat)
	mux.HandleFunc("/v1/chat/completions", g.handleOpenAIChatCompletions)
	mux.HandleFunc("/v1/messages", g.handleAnthropicMessages)
	return mux
}

type agentAIChatRequest struct {
	Messages  []ai.Message `json:"messages"`
	MaxTokens int          `json:"max_tokens"`
}

// handleChat 处理 POST /chat：Bearer 令牌校验 → 限流 → 平台 AI 调用 →
// 审计 + JSON 应答。错误语义：401 令牌无效/已注销、405 方法、400 载荷、
// 429 超出任务调用上限、502 AI 上游失败、503 网关未装配 AI。
func (g *AgentAIGateway) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAgentAIError(w, http.StatusMethodNotAllowed, "POST /chat only")
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
	var req agentAIChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, agentAIMaxBodyBytes)).Decode(&req); err != nil {
		writeAgentAIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// openai 消息数组子集：system 消息归位 ChatRequest.System（anthropic
	// 协议不接受 messages 内的 system role），其余按序透传。
	system, msgs, err := agentAINormalizeMessages(req.Messages)
	if err != nil {
		writeAgentAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	maxTokens, err := agentAIClampMaxTokens(req.MaxTokens)
	if err != nil {
		writeAgentAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentAIChatTimeout)
	defer cancel()
	content, model, err := g.chat(ctx, info.user, system, msgs, maxTokens, nil)
	g.auditAICall(info, model, err, "")
	if err != nil {
		writeAgentAIError(w, http.StatusBadGateway, "platform ai request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"content": content, "model": model})
}

// authorize 为 /chat 与 /v1/chat/completions 共用的入口闸门：Bearer 令牌
// 校验 + 每任务调用计数（受理时即递增，并发请求共同受 limit 约束）。
// 失败路径已写好错误响应（401/429），ok=false 时中止处理。
func (g *AgentAIGateway) authorize(w http.ResponseWriter, r *http.Request) (agentAITokenInfo, bool) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	info, ok := g.tokens[token]
	if ok {
		if info.calls >= info.limit {
			g.mu.Unlock()
			writeAgentAIError(w, http.StatusTooManyRequests, "agent ai call limit exceeded for this task")
			return agentAITokenInfo{}, false
		}
		info.calls++
		g.tokens[token] = info
	}
	g.mu.Unlock()
	if !ok {
		writeAgentAIError(w, http.StatusUnauthorized, "invalid or expired agent ai token")
		return agentAITokenInfo{}, false
	}
	return info, true
}

// agentAINormalizeMessages 校验并归一 openai 子集消息数组：条数 1-64，
// system/developer 消息归位系统提示（多段以 \n 连接；developer 为 OpenAI
// 新模型的 system 后继角色，同语义处理），user/assistant 按序透传，其余
// role 拒绝。
func agentAINormalizeMessages(in []ai.Message) (system string, msgs []ai.Message, err error) {
	if len(in) == 0 || len(in) > agentAIMaxMessages {
		return "", nil, fmt.Errorf("messages must contain 1-%d entries", agentAIMaxMessages)
	}
	var sys strings.Builder
	msgs = make([]ai.Message, 0, len(in))
	for _, m := range in {
		switch m.Role {
		case "system", "developer":
			if sys.Len() > 0 {
				sys.WriteString("\n")
			}
			sys.WriteString(m.Content)
		case "user", "assistant":
			msgs = append(msgs, ai.Message{Role: m.Role, Content: m.Content})
		default:
			return "", nil, errors.New("message role must be system/user/assistant")
		}
	}
	if len(msgs) == 0 {
		return "", nil, errors.New("messages must contain at least one user/assistant entry")
	}
	return sys.String(), msgs, nil
}

// agentAIClampMaxTokens 归一 max_tokens：负数拒绝，超上限截断。
func agentAIClampMaxTokens(n int) (int, error) {
	if n < 0 {
		return 0, errors.New("max_tokens must be >= 0")
	}
	if n > agentAIMaxTokens {
		n = agentAIMaxTokens
	}
	return n, nil
}

// auditAICall 为 /chat 与 openai 兼容端点共用的审计写入（成功/失败均记，
// 不落 prompt/回复内容，仅任务与模型元数据）；endpoint 非空时 metadata
// 追加该键（openai 端点传 "openai"），/chat 保持原三键不变。
func (g *AgentAIGateway) auditAICall(info agentAITokenInfo, model string, err error, endpoint string) {
	status := audit.StatusSuccess
	if err != nil {
		status = audit.StatusFailure
	}
	meta := map[string]string{"task_id": info.taskID, "model": model, "status": status}
	if endpoint != "" {
		meta["endpoint"] = endpoint
	}
	metadata, _ := json.Marshal(meta)
	user := info.user
	_ = g.audit.Record(audit.Entry{UserID: &user, Action: ActionAgentAI, ResourceType: audit.ResourceAI, ResourceID: info.taskID, Status: status, Metadata: string(metadata), CreatedAt: g.now().UTC()})
}

func writeAgentAIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// StartAgentIPCServer 在 dir（空则 env DOCFLOW_AGENT_IPC_DIR，再回退
// AgentAIIPCDefaultDir）下监听 ai.sock 并服务 h：目录不存在则创建
// （0700）、socket 0600。返回幂等的 stop（Shutdown + 摘除 socket 文件）。
// 失败（目录/socket 不可建）返回错误，由调用方决定是否仅告警降级。
func StartAgentIPCServer(ctx context.Context, dir string, h http.Handler) (stop func(), err error) {
	if dir == "" {
		dir = os.Getenv("DOCFLOW_AGENT_IPC_DIR")
	}
	if dir == "" {
		dir = AgentAIIPCDefaultDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent ai ipc: mkdir %s: %w", dir, err)
	}
	_ = os.Chmod(dir, 0o700)
	sockPath := filepath.Join(dir, AgentAISockName)
	// 残留 socket（上次进程未清理）先摘除，避免 address already in use。
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("agent ai ipc: listen %s: %w", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o600)
	srv := &http.Server{Handler: h}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			_ = os.Remove(sockPath)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("agent ai ipc: serve: %v", err)
			}
		}
	}()
	return stop, nil
}
