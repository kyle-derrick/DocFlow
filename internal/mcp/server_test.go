package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/space"
)

// ---------- 测试替身 ----------

// fakeFiles 是 FileStore 的最小内存实现：多数方法返回 notFound，
// CreateFolderIn 记录调用供断言。
type fakeFiles struct {
	root     files.File
	created  []string
	notFound bool
}

func (f *fakeFiles) DefaultSpaceRoot(user uuid.UUID) (files.File, error) {
	if f.root.OwnerID == user {
		return f.root, nil
	}
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) ListSpace(spaceID, parent uuid.UUID, limit int, sort files.SpaceListFilter) ([]files.File, error) {
	return nil, nil
}
func (f *fakeFiles) Get(user, id uuid.UUID) (files.File, error) {
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) CreateFolderIn(user, parent uuid.UUID, name string) (files.File, error) {
	if f.notFound {
		return files.File{}, files.ErrNotFound
	}
	f.created = append(f.created, name)
	return files.File{ID: uuid.New(), Name: name, OwnerID: user, SpaceID: f.root.SpaceID, Type: "folder"}, nil
}
func (f *fakeFiles) Rename(user, id uuid.UUID, name string) (files.File, error) {
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) Copy(user, id, parent uuid.UUID, name string) (files.File, error) {
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) Delete(user, id uuid.UUID) error { return files.ErrNotFound }
func (f *fakeFiles) Restore(user, id uuid.UUID) (files.File, error) {
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) ListTrashSpace(user, spaceID uuid.UUID, limit int) ([]files.File, error) {
	return nil, nil
}
func (f *fakeFiles) ListVersions(user, fileID uuid.UUID) ([]files.VersionDetail, error) {
	return nil, files.ErrNotFound
}
func (f *fakeFiles) SetCurrentVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, error) {
	return files.File{}, files.FileVersion{}, files.ErrNotFound
}
func (f *fakeFiles) ReadVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, files.ObjectBlob, error) {
	return files.File{}, files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
}
func (f *fakeFiles) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
}
func (f *fakeFiles) ResolveReadablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (files.File, []files.File, error) {
	return files.File{}, nil, files.ErrNotFound
}
func (f *fakeFiles) SpaceRoot(spaceID uuid.UUID) (files.File, error) {
	if f.root.SpaceID == spaceID {
		return f.root, nil
	}
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) GetSpaceFolder(spaceID, id uuid.UUID) (files.File, error) {
	return files.File{}, files.ErrNotFound
}
func (f *fakeFiles) BatchMove(user uuid.UUID, ids []uuid.UUID, target uuid.UUID) ([]files.BatchItemResult, error) {
	return nil, files.ErrNotFound
}

// fakeSpaces 是 SpaceService 的最小内存实现（默认空间解析与读权限）。
type fakeSpaces struct {
	owner   uuid.UUID
	spaceID uuid.UUID
}

func (s *fakeSpaces) ListSpaces(user uuid.UUID) ([]space.Space, error) { return nil, nil }
func (s *fakeSpaces) CanRead(user, spaceID uuid.UUID) (bool, error) {
	return user == s.owner && spaceID == s.spaceID, nil
}
func (s *fakeSpaces) DefaultSpace(owner uuid.UUID) (space.Space, error) {
	if owner == s.owner {
		return space.Space{ID: s.spaceID, Name: "默认空间", OwnerID: owner}, nil
	}
	return space.Space{}, space.ErrNotFound
}

// fakeSearch 记录查询关键词的搜索替身。
type fakeSearch struct {
	queries []string
}

func (s *fakeSearch) Query(user uuid.UUID, opts search.QueryOptions) ([]search.Result, error) {
	s.queries = append(s.queries, opts.Q)
	return []search.Result{{ID: uuid.New(), Name: opts.Q + ".md", Type: "file", UpdatedAt: time.Now()}}, nil
}

// newTestServer 构造带最小依赖的服务端与测试用户。
func newTestServer(withSearch bool) (*Server, *fakeFiles, *fakeSearch) {
	user := uuid.New()
	spaceID := uuid.New()
	ff := &fakeFiles{root: files.File{ID: uuid.New(), Name: "根目录", OwnerID: user, SpaceID: spaceID, Type: "folder", IsRoot: true}}
	fs := &fakeSearch{}
	deps := &Deps{Files: ff, Spaces: &fakeSpaces{owner: user, spaceID: spaceID}}
	if withSearch {
		deps.Search = fs
	}
	return NewServer(deps), ff, fs
}

func testIdentity() Identity { return Identity{UserID: uuid.New()} }

// call 发起一次 tools/call 并解析为通用 map。
func call(t *testing.T, s *Server, id Identity, name string, arguments string) map[string]any {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + arguments + `}}`
	raw := s.Handle(context.Background(), []byte(body), id)
	if raw == nil {
		t.Fatal("expected a response for a request with id")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, raw)
	}
	return out
}

// ---------- 协议层测试 ----------

// initialize：协议版本、capabilities.tools、serverInfo。
func TestInitialize(t *testing.T) {
	s, _, _ := newTestServer(false)
	body := `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"tester","version":"0"}}}`
	raw := s.Handle(context.Background(), []byte(body), testIdentity())
	var out struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, raw)
	}
	if out.JSONRPC != "2.0" || out.ID != 7 {
		t.Fatalf("envelope = %s id=%d", out.JSONRPC, out.ID)
	}
	if out.Result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("protocolVersion = %q", out.Result.ProtocolVersion)
	}
	if _, ok := out.Result.Capabilities["tools"]; !ok {
		t.Fatalf("capabilities.tools missing: %v", out.Result.Capabilities)
	}
	if out.Result.ServerInfo.Name != ServerName || out.Result.ServerInfo.Version == "" {
		t.Fatalf("serverInfo = %+v", out.Result.ServerInfo)
	}
}

// tools/list：至少 20 个工具；文件工具 df_ 前缀，AI 工具
// （ask_docs/summarize_file/ai_chat，任务书命名）除外；inputSchema 为 object。
func TestToolsListAtLeast20(t *testing.T) {
	s, _, _ := newTestServer(false)
	raw := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), testIdentity())
	var out struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Result.Tools) < 20 {
		t.Fatalf("tools count = %d, want >= 20", len(out.Result.Tools))
	}
	aiTools := map[string]bool{"ask_docs": false, "summarize_file": false, "ai_chat": false}
	seen := map[string]bool{}
	for _, tool := range out.Result.Tools {
		if _, isAI := aiTools[tool.Name]; !strings.HasPrefix(tool.Name, "df_") && !isAI {
			t.Fatalf("tool %q unexpected name (df_ prefix or known AI tool required)", tool.Name)
		}
		if _, isAI := aiTools[tool.Name]; isAI {
			aiTools[tool.Name] = true
		}
		if seen[tool.Name] {
			t.Fatalf("duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("tool %q missing description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("tool %q inputSchema.type = %v", tool.Name, tool.InputSchema["type"])
		}
	}
	// 关键工具在场。
	for _, name := range []string{"df_list_files", "df_write_file", "df_read_file", "df_resolve_path", "df_create_share", "df_search_files"} {
		if !seen[name] {
			t.Fatalf("tool %q missing from tools/list", name)
		}
	}
	// AI 工具（任务书命名）全部在场。
	for name, present := range aiTools {
		if !present {
			t.Fatalf("ai tool %q missing from tools/list", name)
		}
	}
}

// JSON-RPC 错误码：parse error / invalid request / method not found / invalid params。
func TestJSONRPCErrorCodes(t *testing.T) {
	s, _, _ := newTestServer(false)
	cases := []struct {
		name string
		body string
		code int
	}{
		{"parse-error", `{"jsonrpc":"2.0","id":1,"method":`, CodeParseError},
		{"invalid-request", `{"jsonrpc":"1.0","id":1,"method":"initialize"}`, CodeInvalidRequest},
		{"no-method", `{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{"method-not-found", `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`, CodeMethodNotFound},
		{"unknown-tool", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"df_nope"}}`, CodeInvalidParams},
		{"missing-tool-name", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{}}`, CodeInvalidParams},
		{"bad-args", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"df_get_file","arguments":{"file_id":"not-a-uuid"}}}`, CodeInvalidParams},
	}
	for _, tc := range cases {
		raw := s.Handle(context.Background(), []byte(tc.body), testIdentity())
		var out struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: unmarshal %v (%s)", tc.name, err, raw)
		}
		if out.Error.Code != tc.code {
			t.Fatalf("%s: code = %d, want %d (%s)", tc.name, out.Error.Code, tc.code, raw)
		}
	}
}

// notification（无 id / id null）不应答。
func TestNotificationNoResponse(t *testing.T) {
	s, _, _ := newTestServer(false)
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
	} {
		if raw := s.Handle(context.Background(), []byte(body), testIdentity()); raw != nil {
			t.Fatalf("notification produced a response: %s", raw)
		}
	}
}

// ping 返回空对象结果。
func TestPing(t *testing.T) {
	s, _, _ := newTestServer(false)
	raw := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":9,"method":"ping"}`), testIdentity())
	var out struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
		t.Fatalf("ping response = %s err=%v", raw, err)
	}
}

// PAT scope 不足：files:read PAT 调写工具 → -32003；读工具正常；未限定
// scope 的 PAT 与 JWT 不受限。
func TestPATScopeEnforcement(t *testing.T) {
	s, ff, _ := newTestServer(false)
	readOnly := Identity{UserID: uuid.New(), IsPAT: true, Scopes: []string{"files:read"}}
	// 写工具被拒。
	out := call(t, s, readOnly, "df_create_folder", `{"name":"x"}`)
	if code := nestedErrorCode(out); code != CodeMissingScope {
		t.Fatalf("read-only PAT write: error.code = %v, want %d (%v)", nestedErrorCode(out), CodeMissingScope, out)
	}
	// 未实际执行。
	if len(ff.created) != 0 {
		t.Fatalf("scope-denied call must not execute, created = %v", ff.created)
	}
	// 读工具走工具执行（结果 isError，因为 fake 返回 notFound，但不是 -32003）。
	out = call(t, s, readOnly, "df_get_file", `{"file_id":"`+uuid.New().String()+`"}`)
	if nestedErrorCode(out) == CodeMissingScope {
		t.Fatalf("read tool must not be scope-denied: %v", out)
	}
	// 未限定 scope 的 PAT 可写。
	unscoped := Identity{UserID: ff.root.OwnerID, IsPAT: true}
	out = call(t, s, unscoped, "df_create_folder", `{"name":"ok"}`)
	if out["error"] != nil {
		t.Fatalf("unscoped PAT write failed: %v", out)
	}
	if len(ff.created) != 1 || ff.created[0] != "ok" {
		t.Fatalf("created = %v", ff.created)
	}
	// JWT 可写。
	jwtID := Identity{UserID: ff.root.OwnerID}
	out = call(t, s, jwtID, "df_create_folder", `{"name":"jwt"}`)
	if out["error"] != nil {
		t.Fatalf("jwt write failed: %v", out)
	}
}

// nestedErrorCode 从响应 map 提取 error.code（无错误返回 0）。
func nestedErrorCode(out map[string]any) int {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return 0
	}
	switch code := e["code"].(type) {
	case float64:
		return int(code)
	default:
		return -1
	}
}

// 未注入 search：tools/list 标注「当前部署未启用」，调用返回明确结构化错误。
func TestSearchUnavailable(t *testing.T) {
	s, _, _ := newTestServer(false)
	// tools/list 标注。
	raw := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), testIdentity())
	var listing struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	found := false
	for _, tool := range listing.Result.Tools {
		if tool.Name == "df_search_files" {
			found = true
			if !strings.Contains(tool.Description, "当前部署未启用") {
				t.Fatalf("df_search_files description = %q, want unavailable marker", tool.Description)
			}
		}
	}
	if !found {
		t.Fatal("df_search_files missing from tools/list")
	}
	// 调用返回 isError + 明确错误信息。
	out := call(t, s, testIdentity(), "df_search_files", `{"query":"合同"}`)
	if out["error"] != nil {
		t.Fatalf("unavailable tool must be a tool-level error, got rpc error: %v", out)
	}
	result, _ := out["result"].(map[string]any)
	if result == nil || result["isError"] != true {
		t.Fatalf("result.isError missing: %v", out)
	}
	text := toolText(t, result)
	if !strings.Contains(text, "not available") {
		t.Fatalf("error text = %q, want capability-not-available message", text)
	}
}

// 注入 search：df_search_files 正常返回。
func TestSearchAvailable(t *testing.T) {
	s, _, fs := newTestServer(true)
	out := call(t, s, testIdentity(), "df_search_files", `{"query":"合同"}`)
	result, _ := out["result"].(map[string]any)
	if result == nil || result["isError"] == true {
		t.Fatalf("search call failed: %v", out)
	}
	if len(fs.queries) != 1 || fs.queries[0] != "合同" {
		t.Fatalf("queries = %v", fs.queries)
	}
}

func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// Identity.Allows 语义矩阵（与 RequireScope 对齐）。
func TestIdentityAllows(t *testing.T) {
	cases := []struct {
		id   Identity
		want bool
	}{
		{Identity{UserID: uuid.New()}, true},                                               // JWT
		{Identity{UserID: uuid.New(), IsPAT: true}, true},                                  // 未限定 scope 的 PAT
		{Identity{UserID: uuid.New(), IsPAT: true, Scopes: []string{"files:read"}}, false}, // 只读 PAT 调写 scope
		{Identity{UserID: uuid.New(), IsPAT: true, Scopes: []string{"files:write"}}, true},
	}
	for i, tc := range cases {
		if got := tc.id.Allows(ScopeFilesWrite); got != tc.want {
			t.Fatalf("case %d: Allows(files:write) = %v, want %v", i, got, tc.want)
		}
	}
}

// 接口约束：确保测试替身满足依赖接口。
var (
	_ FileStore     = (*fakeFiles)(nil)
	_ SearchService = (*fakeSearch)(nil)
)
