package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/team"
	"github.com/docflow/docflow/internal/upload"
)

// Identity 为一次 MCP 请求的调用方身份（HTTP 层从 Bearer 凭证解析注入）。
type Identity struct {
	UserID uuid.UUID
	// IsPAT 表示凭证为 PAT（dfpat_）；JWT access token 不受 scope 限制。
	IsPAT bool
	// Scopes 为 PAT 授权 scope 列表；nil 表示不受限（JWT 或未限定 scope
	// 的 PAT），与 auth.RequireScope 的判定语义一致。
	Scopes []string
}

// Allows 判定身份是否具备 scope（files:read / files:write）：
// 非 PAT 或未限定 scope 的 PAT 不受限；限定 scope 的 PAT 逐项精确匹配。
func (id Identity) Allows(scope string) bool {
	if !id.IsPAT || id.Scopes == nil {
		return true
	}
	for _, s := range id.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// PAT scope 常量（沿用 auth.AllowedPATScopes 的取值）。
const (
	ScopeFilesRead  = "files:read"
	ScopeFilesWrite = "files:write"
)

// FileStore 抽象 MCP 文件工具所需的文件服务能力（生产实现 *files.Store；
// 接口化便于单测注入内存实现，模式同 http 包 resolver/unpacker）。
// 全部方法自带用户级授权（authorizeFile* / 团队 ACL），MCP 层不做旁路。
type FileStore interface {
	EnsureRoot(owner uuid.UUID) (files.File, error)
	List(owner uuid.UUID, parent *uuid.UUID, limit int, sort files.SortOptions) ([]files.File, error)
	Get(user, id uuid.UUID) (files.File, error)
	CreateFolderIn(user, parent uuid.UUID, name string) (files.File, error)
	Rename(user, id uuid.UUID, name string) (files.File, error)
	Copy(user, id, parent uuid.UUID, name string) (files.File, error)
	Delete(user, id uuid.UUID) error
	Restore(user, id uuid.UUID) (files.File, error)
	ListTrashScope(user uuid.UUID, scope string, teamID *uuid.UUID, limit int) ([]files.File, error)
	ListVersions(user, fileID uuid.UUID) ([]files.VersionDetail, error)
	SetCurrentVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, error)
	ReadVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, files.ObjectBlob, error)
	CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error)
	ResolveReadablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (files.File, []files.File, error)
	TeamRoot(teamID uuid.UUID) (files.File, error)
	ListTeam(teamID, parent uuid.UUID, limit int, f files.TeamListFilter) ([]files.File, error)
	GetTeamFolder(teamID, id uuid.UUID) (files.File, error)
	BatchMove(user uuid.UUID, ids []uuid.UUID, target uuid.UUID) ([]files.BatchItemResult, error)
}

// UploadService 抽象上传管线能力（生产实现 *upload.Service）：写入一律走
// UploadBytes / StartReplace+Append+Complete 管线（office 校验、病毒扫描、
// 配额、扩展名黑名单、版本与作用域均生效），MCP 层不旁路。
type UploadService interface {
	UploadBytes(user, parent uuid.UUID, name string, data []byte) (upload.UploadSession, error)
	StartReplace(user, target uuid.UUID, size int64, expected string) (upload.UploadSession, error)
	Append(id uuid.UUID, offset int64, r io.Reader) (upload.UploadSession, error)
	Complete(id uuid.UUID) (upload.UploadSession, error)
}

// ShareService 抽象分享能力（生产实现 *share.Service）。
type ShareService interface {
	ListWithFileNames(owner uuid.UUID, limit int) ([]share.ShareWithFile, error)
	CreatePublic(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, opts share.ShareOptions) (share.Share, string, error)
	CreatePrivateWithOptions(owner, fileID uuid.UUID, permission string, expiresIn time.Duration, maxDownloads *int, userIds, teamIds []uuid.UUID, opts share.ShareOptions) (share.Share, error)
	Revoke(owner, shareID uuid.UUID) (share.Share, error)
}

// TeamService 抽象团队能力（生产实现 *team.Service）。
type TeamService interface {
	ListTeams(user uuid.UUID) ([]team.Team, error)
	CanRead(userID, teamID uuid.UUID) (bool, error)
}

// SearchService 抽象全文检索能力（生产实现 *search.Store；未启用时注入
// nil，对应工具在 tools/list 中标注且调用返回结构化错误）。
type SearchService interface {
	Query(user uuid.UUID, opts search.QueryOptions) ([]search.Result, error)
}

// Deps 为 MCP 工具的服务依赖集合；nil 的能力对应工具不可用（不注册旁路）。
type Deps struct {
	Files   FileStore
	Uploads UploadService
	Storage upload.Storage
	Shares  ShareService
	Teams   TeamService
	Search  SearchService
}

// Tool 为一个 MCP 工具定义：inputSchema 手写 JSON Schema（不引 zod）；
// Scope 为所需 PAT scope（files:read / files:write，JWT 不受限）；
// Available 返回 false 时该部署未启用对应能力（tools/list 描述中标注，
// 调用返回结构化错误），nil 表示恒可用。
type Tool struct {
	Name        string
	Description string
	Scope       string
	InputSchema map[string]any
	Available   func(deps *Deps) bool
	Handler     func(ctx context.Context, deps *Deps, id Identity, args json.RawMessage) (any, error)
}

// argsError 表示工具入参非法（JSON-RPC -32602）。
type argsError struct{ message string }

func (e *argsError) Error() string { return e.message }

// badArgs 构造入参错误（工具 handler 使用）。
func badArgs(format string, a ...any) error {
	return &argsError{message: fmt.Sprintf(format, a...)}
}

// errToolUnavailable 为能力未注入的哨兵（分发层转为结构化工具错误）。
var errToolUnavailable = errors.New("capability not available on this deployment")

// Server 为 MCP 服务端：持有一组 Tool 与服务依赖，按 JSON-RPC 分发。
type Server struct {
	deps  *Deps
	tools []Tool
}

// NewServer 构造服务端并注册全部 df_ 工具。
func NewServer(deps *Deps) *Server {
	if deps == nil {
		deps = &Deps{}
	}
	s := &Server{deps: deps}
	s.tools = allTools()
	return s
}

// Tools 返回工具清单（含不可用工具；unavailable 标注由 ToolsList 处理）。
func (s *Server) Tools() []Tool { return s.tools }

// Handle 处理一个 JSON-RPC 请求体并返回应答体；notification（无 id）
// 返回 nil（调用方回 HTTP 202，不应答）。
func (s *Server) Handle(ctx context.Context, body []byte, identity Identity) []byte {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return NewError(json.RawMessage("null"), CodeParseError, "parse error", nil).Encode()
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return NewError(req.ID, CodeInvalidRequest, "invalid request: expected jsonrpc 2.0 with a method", nil).Encode()
	}
	if req.IsNotification() {
		// notification（如 initialized）：静默确认，不应答。
		return nil
	}
	switch req.Method {
	case "initialize":
		return NewResult(req.ID, s.initializeResult()).Encode()
	case "ping":
		return NewResult(req.ID, map[string]any{}).Encode()
	case "tools/list":
		return NewResult(req.ID, s.toolsListResult()).Encode()
	case "tools/call":
		return s.handleToolsCall(ctx, req, identity)
	default:
		return NewError(req.ID, CodeMethodNotFound, "method not found: "+req.Method, nil).Encode()
	}
}

func (s *Server) initializeResult() map[string]any {
	instructions := "DocFlow 文档中台 MCP 服务端。工具均以 df_ 前缀命名；scope 支持 personal（个人空间）" +
		"与 team（团队空间，需 team_id）。写入走上传管线（office 校验/扫描/配额生效）；" +
		"文本类（md/txt/xml/json/excalidraw 等）可用 df_read_file 直读、df_write_version 覆盖编辑；" +
		"drawio 的 .drawio 文件即 XML，excalidraw 的 .excalidraw 即 JSON，均可按文本直接读写。"
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": ServerName, "version": ServerVersion},
		"instructions":    instructions,
	}
}

// toolsListResult 输出工具清单：不可用工具描述尾部追加「（当前部署未启用）」。
func (s *Server) toolsListResult() map[string]any {
	type toolMeta struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	out := make([]toolMeta, 0, len(s.tools))
	for _, t := range s.tools {
		desc := t.Description
		if t.Available != nil && !t.Available(s.deps) {
			desc += "（当前部署未启用）"
		}
		out = append(out, toolMeta{Name: t.Name, Description: desc, InputSchema: t.InputSchema})
	}
	return map[string]any{"tools": out}
}

// toolContent / toolResult 为 MCP tools/call 的结果结构（text 内容 + isError）。
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

func (s *Server) handleToolsCall(ctx context.Context, req Request, identity Identity) []byte {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		return NewError(req.ID, CodeInvalidParams, "invalid params: expected {name, arguments}", nil).Encode()
	}
	var tool *Tool
	for i := range s.tools {
		if s.tools[i].Name == params.Name {
			tool = &s.tools[i]
			break
		}
	}
	if tool == nil {
		return NewError(req.ID, CodeInvalidParams, "unknown tool: "+params.Name, nil).Encode()
	}
	// PAT scope 门控（语义同 api 组 RequireScope）。
	if !identity.Allows(tool.Scope) {
		return NewError(req.ID, CodeMissingScope, "missing scope: "+tool.Scope, nil).Encode()
	}
	if tool.Available != nil && !tool.Available(s.deps) {
		return NewResult(req.ID, errorToolResult(params.Name, errToolUnavailable)).Encode()
	}
	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	result, err := tool.Handler(ctx, s.deps, identity, args)
	if err != nil {
		var ae *argsError
		if errors.As(err, &ae) {
			return NewError(req.ID, CodeInvalidParams, ae.message, nil).Encode()
		}
		// 工具执行错误（服务层语义错误）：按 MCP 规范以 isError 结果返回，
		// 便于 agent 阅读原因并自行调整策略（重名/越权/不存在等）。
		return NewResult(req.ID, errorToolResult(params.Name, err)).Encode()
	}
	data, merr := json.MarshalIndent(result, "", "  ")
	if merr != nil {
		return NewError(req.ID, CodeInternalError, "result encoding failed", nil).Encode()
	}
	return NewResult(req.ID, toolResult{Content: []toolContent{{Type: "text", Text: string(data)}}}).Encode()
}

// errorToolResult 把工具执行错误转为结构化 isError 结果（错误码 + 说明）。
func errorToolResult(name string, err error) toolResult {
	return toolResult{
		Content: []toolContent{{Type: "text", Text: errorMessage(name, err)}},
		IsError: true,
	}
}
