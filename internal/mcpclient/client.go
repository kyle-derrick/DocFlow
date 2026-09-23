// Package mcpclient —— 外部 MCP 服务器轻客户端（JSON-RPC 2.0 over
// Streamable HTTP，protocolVersion 2025-03-26）。
//
// 自研轻实现（不引入 SDK 依赖）：AI 对话消费平台配置的外部 MCP 工具
// （settings 的 ai.mcp 键）。会话按需建立（无长连接池）：initialize →
// notifications/initialized → tools/list / tools/call；服务器可经响应头
// MCP-Session-Id 下发会话 ID，后续请求自动回带。传输细节：
//   - 所有 POST Content-Type: application/json、Accept: application/json,
//     text/event-stream；
//   - 响应 Content-Type 含 text/event-stream 时解析 SSE 流（data: 行）取
//     id 匹配的 JSON-RPC response；普通 JSON 直接解析；
//   - AuthHeader 形如 "Authorization: Bearer x"（按首个冒号拆头名/值）；
//   - 错误（HTTP 非 2xx / JSON-RPC error / 超时）返回中文友好 error。
//
// 工具列表按 URL 包级缓存（TTL 60s，减少每轮对话 RTT）；CallTool 不缓存。
package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProtocolVersion 协商的 MCP 协议版本（Streamable HTTP 首个正式版）。
const ProtocolVersion = "2025-03-26"

// RequestTimeout 单次 MCP 请求超时（含流式响应整体）。
const RequestTimeout = 8 * time.Second

// toolsCacheTTL 工具列表缓存有效期。
const toolsCacheTTL = 60 * time.Second

// maxResponseBytes 单响应体读取上限（防异常服务器拖垮内存）。
const maxResponseBytes = 4 << 20

// Tool 为 tools/list 返回的一个工具条目（inputSchema 透传给上游模型）。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client 为单个 MCP 服务器端点（URL + 可选认证头）。值拷贝不共享会话，
// 须以指针复用（同一 Client 的 initialize 只发一次，session 内复用）。
type Client struct {
	URL        string
	AuthHeader string // 形如 "Authorization: Bearer x"，按首个冒号拆分
	HTTP       *http.Client

	mu        sync.Mutex
	sessionID string // 响应头 MCP-Session-Id（可选）
	initDone  bool   // initialize 已执行（initErr 缓存结果）
	initErr   error
}

// defaultHTTP 包级默认 HTTP 客户端（8s 超时；测试可注入自定义 Client）。
var defaultHTTP = &http.Client{Timeout: RequestTimeout}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// ---------- JSON-RPC 传输 ----------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"` // notification 无 id
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ensureSession 惰性执行 initialize + notifications/initialized（结果缓存
// 于 Client：失败同样缓存，换新 Client 重试）；记录响应头 MCP-Session-Id
// （若有）。IO 在锁外执行（rpc→setHeaders 需读锁，避免重入死锁）；同一
// Client 由同一请求 goroutine 串行使用，无并发重复初始化问题。
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	if c.initDone {
		err := c.initErr
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	id := 1
	_, hdrs, err := c.rpc(ctx, &id, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "docflow", "version": "1.0"},
	}, true)
	var sid string
	if err == nil {
		sid = hdrs.Get("MCP-Session-Id")
		if sid != "" {
			c.mu.Lock()
			c.sessionID = sid // 先落会话：紧随的 notification 即可回带
			c.mu.Unlock()
		}
		// notifications/initialized：notification 无 id，2xx 即可，body 忽略。
		err = c.notify(ctx, "notifications/initialized")
	}
	c.mu.Lock()
	c.sessionID, c.initDone, c.initErr = sid, true, err
	c.mu.Unlock()
	return err
}

// rpc 发送一个 JSON-RPC 请求并返回 result；captureHeader=true 时一并返回
// 响应头（initialize 记录 MCP-Session-Id 用）。
func (c *Client) rpc(ctx context.Context, id *int, method string, params any, captureHeader bool) (json.RawMessage, http.Header, error) {
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, nil, fmt.Errorf("构造 MCP 请求失败：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("MCP 服务地址无效：%v", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, c.transportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, nil, fmt.Errorf("MCP 服务返回错误（HTTP %d）：%s", resp.StatusCode, clip(string(raw), 200))
	}
	var result json.RawMessage
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		result, err = readSSEResponse(resp.Body, id)
		if err != nil {
			return nil, nil, err
		}
	} else {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if rerr != nil {
			return nil, nil, fmt.Errorf("读取 MCP 响应失败：%v", rerr)
		}
		result, err = decodeRPCResponse(raw, id)
		if err != nil {
			return nil, nil, err
		}
	}
	return result, resp.Header, nil
}

// notify 发送 notification（无 id，2xx 即成功，body 忽略）。
func (c *Client) notify(ctx context.Context, method string) error {
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method})
	if err != nil {
		return fmt.Errorf("构造 MCP 请求失败：%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("MCP 服务地址无效：%v", err)
	}
	c.setHeaders(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return c.transportError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP 服务返回错误（HTTP %d）", resp.StatusCode)
	}
	return nil
}

// setHeaders 统一请求头：Content-Type/Accept、认证头、会话头。
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if h := strings.TrimSpace(c.AuthHeader); h != "" {
		if name, value, ok := strings.Cut(h, ":"); ok {
			if n := strings.TrimSpace(name); n != "" {
				req.Header.Set(n, strings.TrimSpace(value))
			}
		}
	}
	c.mu.Lock()
	session := c.sessionID
	c.mu.Unlock()
	if session != "" {
		req.Header.Set("MCP-Session-Id", session)
	}
}

// transportError 归一化网络层错误（超时/连接失败 → 中文友好提示）。
func (c *Client) transportError(err error) error {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return fmt.Errorf("MCP 服务请求超时（%s），请稍后重试", RequestTimeout)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("MCP 服务请求超时（%s），请稍后重试", RequestTimeout)
	}
	return fmt.Errorf("MCP 服务无法连接：%v", err)
}

// decodeRPCResponse 解析 JSON-RPC response：error 非空报中文错误；id 不
// 匹配（错配的并发响应）同样报错。
func decodeRPCResponse(raw []byte, wantID *int) (json.RawMessage, error) {
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("MCP 响应解析失败：%v", err)
	}
	if resp.Error != nil {
		msg := resp.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("code %d", resp.Error.Code)
		}
		return nil, fmt.Errorf("MCP 调用失败：%s", msg)
	}
	if wantID != nil && resp.ID != nil && *resp.ID != *wantID {
		return nil, fmt.Errorf("MCP 响应 id 不匹配（want %d got %d）", *wantID, *resp.ID)
	}
	return resp.Result, nil
}

// readSSEResponse 解析 text/event-stream 流：逐 data: 行取 id 匹配的
// JSON-RPC response（单响应即可返回；心跳/注释行跳过）。
func readSSEResponse(r io.Reader, wantID *int) (json.RawMessage, error) {
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
		result, err := decodeRPCResponse([]byte(data), wantID)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 MCP 事件流失败：%v", err)
	}
	return nil, fmt.Errorf("MCP 事件流结束但未收到响应（id 匹配失败）")
}

// ---------- 会话级操作 ----------

// cachedTools 工具列表缓存条目（含会话 ID：TTL 内的 CallTool 复用会话，
// 免重复 initialize）。
type cachedTools struct {
	tools   []Tool
	session string
	at      time.Time
}

// toolsCache 包级工具列表缓存（key=URL，TTL 60s）。
var toolsCache sync.Map // string → cachedTools

// cacheNow 缓存 TTL 的时间源（测试可替换）。
var cacheNow = time.Now

// ListTools 返回服务器工具列表（initialize → initialized → tools/list；
// 命中缓存直接返回，减少每轮对话 RTT）。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if v, ok := toolsCache.Load(c.URL); ok {
		ct := v.(cachedTools)
		if cacheNow().Sub(ct.at) < toolsCacheTTL {
			c.mu.Lock()
			// 缓存内联恢复会话（TTL 内的 CallTool 免重复 initialize）。
			c.sessionID, c.initDone, c.initErr = ct.session, ct.session != "", nil
			c.mu.Unlock()
			return ct.tools, nil
		}
	}
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	id := 2
	raw, _, err := c.rpc(ctx, &id, "tools/list", nil, false)
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("MCP 工具列表解析失败：%v", err)
	}
	if res.Tools == nil {
		res.Tools = []Tool{}
	}
	c.mu.Lock()
	session := c.sessionID
	c.mu.Unlock()
	toolsCache.Store(c.URL, cachedTools{tools: res.Tools, session: session, at: cacheNow()})
	return res.Tools, nil
}

// CallTool 调用服务器工具（不缓存）：result.content[] 中 type=="text" 的
// text 按换行拼接返回；result.isError=true 时返回中文错误（含工具文本）。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	if err := c.ensureSession(ctx); err != nil {
		return "", err
	}
	id := 3
	raw, _, err := c.rpc(ctx, &id, "tools/call", map[string]any{"name": name, "arguments": arguments}, false)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("MCP 工具 %s 响应解析失败：%v", name, err)
	}
	var b strings.Builder
	for _, item := range res.Content {
		if item.Type != "text" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(item.Text)
	}
	if res.IsError {
		return "", fmt.Errorf("MCP 工具 %s 执行失败：%s", name, b.String())
	}
	return b.String(), nil
}

// SortedToolNames 返回工具名列表（按字典序，测试/诊断用）。
func SortedToolNames(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
