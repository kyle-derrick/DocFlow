// Package mcp 实现 DocFlow 的 MCP（Model Context Protocol）服务端：
// JSON-RPC 2.0 over HTTP 的 Streamable HTTP 简化形态——单次请求-响应
// （不做 session 与 SSE 长连），支持 initialize / ping / tools/list /
// tools/call 四个方法。鉴权与权限完全复用现有服务层（PAT dfpat_ 或用户
// access token；PAT scope 沿用 files:read / files:write 常量）。
package mcp

import "encoding/json"

// ProtocolVersion 为 initialize 返回的 MCP 协议版本。
const ProtocolVersion = "2025-03-26"

// ServerName / ServerVersion 为 serverInfo 字段取值。
const (
	ServerName    = "docflow"
	ServerVersion = "1.0.0"
)

// JSON-RPC 2.0 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// 服务端自定义错误码。
const (
	// CodeUnauthorized 认证失败（Bearer 凭证缺失或无效），HTTP 层 401。
	CodeUnauthorized = -32001
	// CodeMissingScope PAT scope 不足（工具要求的 files:read/files:write
	// 未授权），语义与 api 组 RequireScope 的 403 一致。
	CodeMissingScope = -32003
)

// Request 为单个 JSON-RPC 请求；id 缺失（notification）时不应答。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// IsNotification 判定请求是否为 notification（无 id 或 id 为 null）。
func (r Request) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Error 为 JSON-RPC error 对象（实现 error 接口，便于作为错误透传）。
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// NewError 构造错误应答。
func NewError(id json.RawMessage, code int, message string, data any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: message, Data: data}}
}

// Response 为 JSON-RPC 响应（Result 与 Error 互斥）。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// NewResult 构造成功应答。
func NewResult(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

// Encode 序列化应答；序列化失败兜底 internal error（id 为 null）。
func (r *Response) Encode() []byte {
	data, err := json.Marshal(r)
	if err != nil {
		fallback, _ := json.Marshal(NewError(json.RawMessage("null"), CodeInternalError, "response encoding failed", nil))
		return fallback
	}
	return data
}

// UnauthorizedResponse 返回认证失败的 JSON-RPC 应答体（-32001），供 HTTP
// 中间件在进入 Server 分发前拒绝未认证请求（HTTP 状态码由调用方决定为 401）。
func UnauthorizedResponse() []byte {
	return NewError(json.RawMessage("null"), CodeUnauthorized, "unauthorized: missing or invalid Bearer credential (dfpat_ PAT or access token)", nil).Encode()
}
