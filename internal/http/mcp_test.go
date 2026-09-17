package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/mcp"
	"github.com/docflow/docflow/internal/search"
	"github.com/docflow/docflow/internal/share"
	"github.com/docflow/docflow/internal/team"
	"github.com/docflow/docflow/internal/upload"
)

// ---------- rawFakeTree 补齐 mcp.FileStore / share.FileSource ----------

// mcpFindRoot 返回 owner 的个人根目录（不存在则按 EnsureRoot 语义创建）。
func (t *rawFakeTree) mcpFindRoot(owner uuid.UUID) (files.File, bool) {
	for _, f := range t.files {
		if f.IsRoot && f.OwnerID == owner && f.TeamID == nil {
			return f, true
		}
	}
	return files.File{}, false
}

func (t *rawFakeTree) EnsureRoot(owner uuid.UUID) (files.File, error) {
	if root, ok := t.mcpFindRoot(owner); ok {
		return root, nil
	}
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, Type: "folder", IsRoot: true, ScopeType: "personal"}
	t.files[root.ID] = root
	return root, nil
}

func (t *rawFakeTree) children(parent uuid.UUID) []files.File {
	var out []files.File
	for _, f := range t.files {
		if f.ParentID != nil && *f.ParentID == parent && f.DeletedAt == nil {
			out = append(out, f)
		}
	}
	return out
}

func (t *rawFakeTree) List(owner uuid.UUID, parent *uuid.UUID, limit int, sort files.SortOptions) ([]files.File, error) {
	if parent == nil {
		return nil, fmt.Errorf("root listing unsupported in fake")
	}
	p, ok := t.files[*parent]
	if !ok || p.DeletedAt != nil {
		return nil, files.ErrNotFound
	}
	if p.OwnerID != owner {
		return nil, files.ErrNotFound
	}
	out := t.children(*parent)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (t *rawFakeTree) TeamRoot(teamID uuid.UUID) (files.File, error) {
	for _, f := range t.files {
		if f.IsRoot && f.TeamID != nil && *f.TeamID == teamID && f.DeletedAt == nil {
			return f, nil
		}
	}
	return files.File{}, files.ErrNotFound
}

func (t *rawFakeTree) ListTeam(teamID, parent uuid.UUID, limit int, f files.TeamListFilter) ([]files.File, error) {
	out := make([]files.File, 0)
	for _, item := range t.children(parent) {
		if item.TeamID != nil && *item.TeamID == teamID {
			out = append(out, item)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (t *rawFakeTree) GetTeamFolder(teamID, id uuid.UUID) (files.File, error) {
	f, ok := t.files[id]
	if !ok || f.DeletedAt != nil || f.Type != "folder" || f.TeamID == nil || *f.TeamID != teamID {
		return files.File{}, files.ErrNotFound
	}
	return f, nil
}

// reindex 把 f 的名称索引从旧名换到新名（父目录不变）。
func (t *rawFakeTree) reindex(f files.File, oldName string) {
	if f.ParentID != nil {
		if bucket := t.byName[*f.ParentID]; bucket != nil {
			delete(bucket, strings.ToLower(oldName))
			bucket[strings.ToLower(f.Name)] = f.ID
		}
	}
}

func (t *rawFakeTree) Rename(user, id uuid.UUID, name string) (files.File, error) {
	f, ok := t.files[id]
	if !ok || f.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	if f.IsRoot {
		return files.File{}, files.ErrRoot
	}
	if err := t.authorizeWrite(f, user); err != nil {
		return files.File{}, err
	}
	old := f.Name
	f.Name = name
	f.UpdatedAt = time.Now()
	t.files[id] = f
	t.reindex(f, old)
	return f, nil
}

func (t *rawFakeTree) Copy(user, id, parent uuid.UUID, name string) (files.File, error) {
	src, ok := t.files[id]
	if !ok || src.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	if src.Type != "file" {
		return files.File{}, files.ErrFolderCopy
	}
	if err := t.authorizeRead(src, user); err != nil {
		return files.File{}, err
	}
	p, ok := t.files[parent]
	if !ok || p.DeletedAt != nil || p.Type != "folder" {
		return files.File{}, files.ErrNotFound
	}
	if p.OwnerID != user && !t.writers[user] {
		return files.File{}, files.ErrForbidden
	}
	if name == "" {
		name = src.Name + " copy"
	}
	if _, exists := t.child(parent, name); exists {
		return files.File{}, files.ErrConflict
	}
	t.seq++
	newID := uuid.New()
	copied := files.File{ID: newID, Name: name, ParentID: &parent, OwnerID: user, Type: "file", ScopeType: p.ScopeType, TeamID: p.TeamID}
	t.files[newID] = copied
	t.byName[parent][strings.ToLower(name)] = newID
	v := t.versions[id]
	blob := t.blobs[id]
	t.versions[newID] = files.FileVersion{ID: uuid.New(), FileID: newID, Version: 1, Size: blob.Size}
	t.blobs[newID] = blob
	_ = v
	return copied, nil
}

func (t *rawFakeTree) Delete(user, id uuid.UUID) error {
	f, ok := t.files[id]
	if !ok || f.DeletedAt != nil {
		return files.ErrNotFound
	}
	if f.IsRoot {
		return files.ErrRoot
	}
	if err := t.authorizeWrite(f, user); err != nil {
		return err
	}
	now := time.Now()
	f.DeletedAt = &now
	t.files[id] = f
	if f.ParentID != nil {
		delete(t.byName[*f.ParentID], strings.ToLower(f.Name))
	}
	return nil
}

func (t *rawFakeTree) Restore(user, id uuid.UUID) (files.File, error) {
	f, ok := t.files[id]
	if !ok || f.DeletedAt == nil {
		return files.File{}, files.ErrNotFound
	}
	if err := t.authorizeWrite(f, user); err != nil {
		return files.File{}, err
	}
	f.DeletedAt = nil
	t.files[id] = f
	if f.ParentID != nil {
		if t.byName[*f.ParentID] == nil {
			t.byName[*f.ParentID] = map[string]uuid.UUID{}
		}
		t.byName[*f.ParentID][strings.ToLower(f.Name)] = f.ID
	}
	return f, nil
}

func (t *rawFakeTree) ListTrashScope(user uuid.UUID, scope string, teamID *uuid.UUID, limit int) ([]files.File, error) {
	out := make([]files.File, 0)
	for _, f := range t.files {
		if f.DeletedAt == nil || f.IsRoot {
			continue
		}
		if scope == "team" {
			if teamID == nil || f.TeamID == nil || *f.TeamID != *teamID {
				continue
			}
		} else if f.OwnerID != user || f.TeamID != nil {
			continue
		}
		out = append(out, f)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (t *rawFakeTree) ListVersions(user, fileID uuid.UUID) ([]files.VersionDetail, error) {
	f, ok := t.files[fileID]
	if !ok || f.DeletedAt != nil {
		return nil, files.ErrNotFound
	}
	if err := t.authorizeRead(f, user); err != nil {
		return nil, err
	}
	v, ok := t.versions[fileID]
	if !ok {
		return nil, files.ErrNoVersion
	}
	blob := t.blobs[fileID]
	return []files.VersionDetail{{
		ID: v.ID, Version: v.Version, Size: blob.Size, SHA256: blob.SHA256,
		MimeType: blob.MimeType, Status: blob.Status, UserID: v.UserID, CreatedAt: v.CreatedAt,
	}}, nil
}

func (t *rawFakeTree) SetCurrentVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, error) {
	f, ok := t.files[fileID]
	if !ok || f.DeletedAt != nil {
		return files.File{}, files.FileVersion{}, files.ErrNotFound
	}
	if err := t.authorizeWrite(f, user); err != nil {
		return files.File{}, files.FileVersion{}, err
	}
	v, ok := t.versions[fileID]
	if !ok || v.ID != versionID {
		return files.File{}, files.FileVersion{}, files.ErrNotFileVersion
	}
	return f, v, nil
}

func (t *rawFakeTree) ReadVersion(user, fileID, versionID uuid.UUID) (files.File, files.FileVersion, files.ObjectBlob, error) {
	f, ok := t.files[fileID]
	if !ok || f.DeletedAt != nil {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	if err := t.authorizeRead(f, user); err != nil {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, err
	}
	v, ok := t.versions[fileID]
	if !ok || v.ID != versionID {
		return files.File{}, files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	return f, v, t.blobs[fileID], nil
}

func (t *rawFakeTree) BatchMove(user uuid.UUID, ids []uuid.UUID, target uuid.UUID) ([]files.BatchItemResult, error) {
	targetFolder, ok := t.files[target]
	if !ok || targetFolder.DeletedAt != nil || targetFolder.Type != "folder" {
		return nil, files.ErrNotFound
	}
	out := make([]files.BatchItemResult, 0, len(ids))
	for _, id := range ids {
		f, ok := t.files[id]
		if !ok || f.DeletedAt != nil {
			out = append(out, files.BatchItemResult{ID: id, OK: false, ErrorCode: files.BatchCodeNotFound})
			continue
		}
		if err := t.authorizeWrite(f, user); err != nil {
			out = append(out, files.BatchItemResult{ID: id, OK: false, ErrorCode: files.BatchCodeForbidden})
			continue
		}
		if id == target {
			out = append(out, files.BatchItemResult{ID: id, OK: false, ErrorCode: files.BatchCodeInvalidTarget})
			continue
		}
		old := f.Name
		f.ParentID = &target
		t.files[id] = f
		t.reindex(f, old)
		out = append(out, files.BatchItemResult{ID: id, OK: true})
	}
	return out, nil
}

func (t *rawFakeTree) IncrementDownloadCount(user, fileID uuid.UUID) error {
	_, err := t.Get(user, fileID)
	return err
}

func (t *rawFakeTree) IncrementViewCount(user, fileID uuid.UUID) error {
	_, err := t.Get(user, fileID)
	return err
}

// ValidateReplaceTarget / ReplaceFileVersion 为 upload.Service「覆盖为新
// 版本」管线的 fake 注入点（对应生产 files.Store 同名方法）。
func (t *rawFakeTree) ValidateReplaceTarget(user, target uuid.UUID) (files.File, error) {
	f, ok := t.files[target]
	if !ok || f.DeletedAt != nil || f.Type != "file" {
		return files.File{}, files.ErrNotFound
	}
	if err := t.authorizeWrite(f, user); err != nil {
		return files.File{}, err
	}
	return f, nil
}

func (t *rawFakeTree) ReplaceFileVersion(user, fileID uuid.UUID, storageKey, sha256 string, size int64, mimeType string) (bool, error) {
	t.seq++
	prev := t.versions[fileID]
	t.versions[fileID] = files.FileVersion{ID: uuid.New(), FileID: fileID, Version: prev.Version + 1, ObjectBlobID: uuid.New(), ContentSHA256: sha256, Size: size, UserID: user, CreatedAt: time.Now()}
	t.blobs[fileID] = files.ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: files.BlobStatusAvailable}
	return true, nil
}

// 编译期断言：fake 满足 MCP 依赖接口。
var (
	_ mcp.FileStore    = (*rawFakeTree)(nil)
	_ share.FileSource = (*rawFakeTree)(nil)
)

// ---------- PAT 测试替身（auth.TokenStore / auth.Credentials） ----------

type mcpFakeTokenStore struct {
	tokens  []auth.APIToken
	touched []uuid.UUID
}

func (s *mcpFakeTokenStore) Create(t auth.APIToken) error {
	s.tokens = append(s.tokens, t)
	return nil
}
func (s *mcpFakeTokenStore) List(owner uuid.UUID) ([]auth.APIToken, error) { return nil, nil }
func (s *mcpFakeTokenStore) Revoke(owner, id uuid.UUID, now time.Time) (bool, error) {
	return false, nil
}
func (s *mcpFakeTokenStore) Update(owner, id uuid.UUID, name *string, scopes *[]string) (auth.APIToken, error) {
	return auth.APIToken{}, nil
}
func (s *mcpFakeTokenStore) FindActiveByPrefix(prefix string, now time.Time) (auth.PATLookup, bool, error) {
	for _, t := range s.tokens {
		if t.Prefix == prefix && t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(now)) {
			return auth.PATLookup{ID: t.ID, UserID: t.UserID, TokenHash: t.TokenHash, Scopes: t.Scopes}, true, nil
		}
	}
	return auth.PATLookup{}, false, nil
}
func (s *mcpFakeTokenStore) TouchLastUsed(id uuid.UUID)                 { s.touched = append(s.touched, id) }
func (s *mcpFakeTokenStore) DeleteExpired(now time.Time) (int64, error) { return 0, nil }

type mcpFakeCreds struct{ users map[uuid.UUID]auth.User }

func (c *mcpFakeCreds) FindActiveByEmail(email string) (auth.User, error) {
	return auth.User{}, auth.ErrUserNotFound
}
func (c *mcpFakeCreds) GetByID(id uuid.UUID) (auth.User, error) {
	if u, ok := c.users[id]; ok {
		return u, nil
	}
	return auth.User{}, auth.ErrUserNotFound
}
func (c *mcpFakeCreds) UpdatePasswordHash(id uuid.UUID, hash string) error { return nil }

// ---------- 测试环境 ----------

const mcpTestSecret = "jwt-mcp-test-secret-0123456789"

type mcpTestEnv struct {
	h        *Handler
	router   *gin.Engine
	tree     *rawFakeTree
	owner    uuid.UUID
	authSvc  *auth.Service
	tokens   *mcpFakeTokenStore
	storage  *memStorage
	teamSvc  *team.Service
	searchOn *fakeMCPSearch
}

// fakeMCPSearch 为 searchService 的内存实现。
type fakeMCPSearch struct{ queries []string }

func (f *fakeMCPSearch) Query(user uuid.UUID, opts search.QueryOptions) ([]search.Result, error) {
	f.queries = append(f.queries, opts.Q)
	return []search.Result{{ID: uuid.New(), Name: opts.Q + ".md", Type: "file", UpdatedAt: time.Now()}}, nil
}

func newMCPTestEnv(t *testing.T, withSearch bool) *mcpTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tree := newRawFakeTree()
	owner := uuid.New()
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, Type: "folder", IsRoot: true, ScopeType: "personal"}
	tree.add(root, 0, "", "")

	storage := newMemStorage()
	h := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", time.Hour)
	// 真实 upload.Service：office 校验/黑名单/配额管线自然生效（内存 store）；
	// 覆盖为新版本经 SetVersionTarget 注入 fake 的 validate/replace。
	uploadSvc := upload.NewService(upload.NewMemoryStore(), storage, time.Hour, 1<<30, false, tree.ValidateFolder, tree.createUploaded)
	uploadSvc.SetVersionTarget(tree.ValidateReplaceTarget, tree.ReplaceFileVersion)
	teamSvc := team.NewService(team.NewMemoryStore())
	h.mcpDeps.Files = tree
	h.mcpDeps.Uploads = uploadSvc
	h.mcpDeps.Shares = share.NewService(share.NewMemoryStore(), tree)
	h.mcpDeps.Teams = teamSvc

	// 真实 auth.Service + 内存 PAT 存储：PAT 全链路（创建/校验/scope）。
	tokens := &mcpFakeTokenStore{}
	svc := auth.NewService(nil, mcpTestSecret, time.Minute, time.Hour)
	svc.SetTokenStore(tokens)
	svc.SetCredentials(&mcpFakeCreds{users: map[uuid.UUID]auth.User{owner: {ID: owner, Username: "alice", Email: "a@e.com", Status: auth.StatusActive}}})
	h.auth = svc

	var fs *fakeMCPSearch
	if withSearch {
		fs = &fakeMCPSearch{}
		h.search = fs
	}

	router := gin.New()
	h.Register(router, mcpTestSecret, 1_000_000, 1_000_000, 1_000_000)
	return &mcpTestEnv{h: h, router: router, tree: tree, owner: owner, authSvc: svc, tokens: tokens, storage: storage, teamSvc: teamSvc, searchOn: fs}
}

// callMCP 以给定 Authorization 头发起一次 /mcp JSON-RPC 调用。
func (e *mcpTestEnv) callMCP(t *testing.T, authHeader, method string, params map[string]any) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	var out map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// callTool 发起一次 tools/call 并取回 result 与 content text。
func (e *mcpTestEnv) callTool(t *testing.T, authHeader, name string, args map[string]any) map[string]any {
	t.Helper()
	code, out := e.callMCP(t, authHeader, "tools/call", map[string]any{"name": name, "arguments": args})
	if code != http.StatusOK {
		t.Fatalf("tools/call %s: HTTP %d body %v", name, code, out)
	}
	if e := rpcError(t, out); e != 0 {
		t.Fatalf("tools/call %s: unexpected rpc error %d: %v", name, e, out)
	}
	return out
}

func rpcError(t *testing.T, out map[string]any) int {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		return 0
	}
	code, _ := e["code"].(float64)
	return int(code)
}

// toolPayload 解析 result.content[0].text 为 map。
func toolPayload(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("missing result: %v", out)
	}
	if isError, _ := result["isError"].(bool); isError {
		t.Fatalf("tool error: %v", result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("missing content: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool payload not JSON: %v (%s)", err, text)
	}
	return payload
}

// toolErrorText 取 isError 结果的文本（无错误返回空串）。
func toolErrorText(t *testing.T, out map[string]any) string {
	t.Helper()
	result, _ := out["result"].(map[string]any)
	if result == nil {
		return ""
	}
	if isError, _ := result["isError"].(bool); !isError {
		return ""
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func (e *mcpTestEnv) jwt() string { return "Bearer " + testJWTFor(mcpTestSecret, e.owner) }

// newPAT 创建带 scopes 的 PAT（scopes 为 nil 表示未限定）。
func (e *mcpTestEnv) newPAT(t *testing.T, scopes []string) string {
	t.Helper()
	_, plaintext, err := e.authSvc.NewPersonalAccessTokenWithScopes(e.owner, "mcp-test", 0, scopes)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + plaintext
}

// ---------- 测试用例 ----------

// initialize：协议版本 / capabilities / serverInfo（HTTP + JSON-RPC 信封）。
func TestMCPInitialize(t *testing.T) {
	env := newMCPTestEnv(t, false)
	code, out := env.callMCP(t, env.jwt(), "initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if code != http.StatusOK {
		t.Fatalf("status = %d body %v", code, out)
	}
	if out["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v", out["jsonrpc"])
	}
	result, _ := out["result"].(map[string]any)
	if result == nil || result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("result = %v", result)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities.tools missing: %v", caps)
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "docflow" {
		t.Fatalf("serverInfo = %v", info)
	}
}

// tools/list：≥20 个 df_ 工具。
func TestMCPToolsList(t *testing.T) {
	env := newMCPTestEnv(t, false)
	_, out := env.callMCP(t, env.jwt(), "tools/list", nil)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) < 20 {
		t.Fatalf("tools = %d, want >= 20", len(tools))
	}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if name, _ := tool["name"].(string); !strings.HasPrefix(name, "df_") {
			t.Fatalf("tool %v missing df_ prefix", name)
		}
	}
}

// PAT 鉴权失败：缺失/伪造凭证 → 401 + JSON-RPC -32001。
func TestMCPAuthFailures(t *testing.T) {
	env := newMCPTestEnv(t, false)
	cases := []string{"", "Bearer dfpat_invalidinvalidinvalidinvalidinvalidin", "Bearer not-a-jwt"}
	for _, authHeader := range cases {
		code, out := env.callMCP(t, authHeader, "tools/list", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("auth %q: status = %d, want 401", authHeader, code)
		}
		if got := rpcError(t, out); got != mcp.CodeUnauthorized {
			t.Fatalf("auth %q: rpc error = %d, want %d (%v)", authHeader, got, mcp.CodeUnauthorized, out)
		}
	}
}

// PAT scope 不足：files:read PAT 写 → -32003；读工具正常；未限定 PAT 可写。
func TestMCPPATScopes(t *testing.T) {
	env := newMCPTestEnv(t, false)
	readOnly := env.newPAT(t, []string{"files:read"})
	unscoped := env.newPAT(t, nil)

	// 只读 PAT 调写工具 → -32003，且未建目录。
	_, out := env.callMCP(t, readOnly, "tools/call", map[string]any{"name": "df_create_folder", "arguments": map[string]any{"name": "x"}})
	if got := rpcError(t, out); got != mcp.CodeMissingScope {
		t.Fatalf("rpc error = %d, want %d (%v)", got, mcp.CodeMissingScope, out)
	}
	for _, f := range env.tree.files {
		if f.Name == "x" {
			t.Fatal("scope-denied call must not create anything")
		}
	}
	// 只读 PAT 调读工具正常。
	_, out = env.callMCP(t, readOnly, "tools/call", map[string]any{"name": "df_list_files", "arguments": map[string]any{}})
	if got := rpcError(t, out); got != 0 {
		t.Fatalf("read tool denied: %v", out)
	}
	// 未限定 scope 的 PAT 可写。
	_, out = env.callMCP(t, unscoped, "tools/call", map[string]any{"name": "df_create_folder", "arguments": map[string]any{"name": "patdir"}})
	if got := rpcError(t, out); got != 0 {
		t.Fatalf("unscoped PAT write denied: %v", out)
	}
}

// 全链路（httptest）：建目录 → 写文件（上传管线）→ 读回 → 路径定位 → 删除 → 不可见。
func TestMCPCreateWriteReadResolveDeleteChain(t *testing.T) {
	env := newMCPTestEnv(t, false)
	authHeader := env.jwt()

	// 1. 建目录。
	folder := toolPayload(t, env.callTool(t, authHeader, "df_create_folder", map[string]any{"name": "AI 工作区"}))
	folderID, _ := folder["id"].(string)
	if folderID == "" || folder["type"] != "folder" {
		t.Fatalf("df_create_folder result = %v", folder)
	}

	// 2. 写文件（走 UploadBytes 管线：内存存储 + blob + 版本落库）。
	created := toolPayload(t, env.callTool(t, authHeader, "df_write_file", map[string]any{
		"name": "note.md", "parent_id": folderID, "content_text": "# hello DocFlow",
	}))
	fileID, _ := created["id"].(string)
	if fileID == "" || created["name"] != "note.md" {
		t.Fatalf("df_write_file result = %v", created)
	}
	if cv, _ := created["current_version"].(map[string]any); cv == nil {
		t.Fatalf("df_write_file missing current_version: %v", created)
	}

	// 3. 读回（文本直读，mime 由扩展名推断为 text/markdown）。
	read := toolPayload(t, env.callTool(t, authHeader, "df_read_file", map[string]any{"file_id": fileID}))
	if read["content"] != "# hello DocFlow" || read["encoding"] != "text" {
		t.Fatalf("df_read_file result = %v", read)
	}
	if mime, _ := read["mime_type"].(string); !strings.HasPrefix(mime, "text/markdown") {
		t.Fatalf("mime_type = %v", read["mime_type"])
	}

	// 4. 路径定位（个人空间根下相对路径）。
	resolved := toolPayload(t, env.callTool(t, authHeader, "df_resolve_path", map[string]any{"scope": "personal", "path": "AI 工作区/note.md"}))
	if resolved["file_id"] != fileID || resolved["canonical_path"] != "AI 工作区/note.md" {
		t.Fatalf("df_resolve_path result = %v", resolved)
	}

	// 5. 删除（软删除）→ 再取元数据应报 not found。
	deleted := toolPayload(t, env.callTool(t, authHeader, "df_delete_file", map[string]any{"file_id": fileID}))
	if deleted["deleted"] != true {
		t.Fatalf("df_delete_file result = %v", deleted)
	}
	_, out := env.callMCP(t, authHeader, "tools/call", map[string]any{"name": "df_get_file", "arguments": map[string]any{"file_id": fileID}})
	if text := toolErrorText(t, out); !strings.Contains(text, "not found") {
		t.Fatalf("df_get_file after delete = %q, want not-found error (%v)", text, out)
	}
}

// 覆盖为新版本 + base64 读取 + 版本列表/回滚。
func TestMCPVersionOps(t *testing.T) {
	env := newMCPTestEnv(t, false)
	authHeader := env.jwt()
	created := toolPayload(t, env.callTool(t, authHeader, "df_write_file", map[string]any{"name": "log.txt", "content_text": "v1"}))
	fileID, _ := created["id"].(string)

	// 覆盖为新版本（StartReplace 管线，fake ReplaceFileVersion 落 v2）。
	overwritten := toolPayload(t, env.callTool(t, authHeader, "df_write_version", map[string]any{"file_id": fileID, "content_text": "v2-content"}))
	if cv, _ := overwritten["current_version"].(map[string]any); cv == nil {
		t.Fatalf("df_write_version missing current_version: %v", overwritten)
	}
	// 读回新内容。
	read := toolPayload(t, env.callTool(t, authHeader, "df_read_file", map[string]any{"file_id": fileID}))
	if read["content"] != "v2-content" {
		t.Fatalf("after overwrite content = %v", read["content"])
	}
	// base64 读取。
	b64 := toolPayload(t, env.callTool(t, authHeader, "df_read_file", map[string]any{"file_id": fileID, "as": "base64"}))
	if b64["encoding"] != "base64" || b64["content"] == "" {
		t.Fatalf("df_read_file base64 = %v", b64)
	}

	// 版本列表 → 回滚到该版本（fake 单版本语义，版本行即当前 v2）。
	versions := toolPayload(t, env.callTool(t, authHeader, "df_list_versions", map[string]any{"file_id": fileID}))
	list, _ := versions["versions"].([]any)
	if len(list) == 0 {
		t.Fatalf("df_list_versions = %v", versions)
	}
	first, _ := list[0].(map[string]any)
	versionID, _ := first["id"].(string)
	restored := toolPayload(t, env.callTool(t, authHeader, "df_restore_version", map[string]any{"file_id": fileID, "version_id": versionID}))
	if restored["id"] != fileID {
		t.Fatalf("df_restore_version = %v", restored)
	}
}

// 团队列表 + 分享创建/撤销。
func TestMCPSharesAndTeams(t *testing.T) {
	env := newMCPTestEnv(t, false)
	authHeader := env.jwt()

	// df_list_teams：新建团队后可见（team.Service + MemoryStore）。
	if _, _, err := env.teamSvc.CreateTeam(env.owner, "平台组", ""); err != nil {
		t.Fatal(err)
	}
	teams := toolPayload(t, env.callTool(t, authHeader, "df_list_teams", map[string]any{}))
	if list, _ := teams["teams"].([]any); len(list) != 1 {
		t.Fatalf("df_list_teams = %v", teams)
	}

	// df_create_share（公开，带密码）→ df_list_shares → df_revoke_share。
	file := toolPayload(t, env.callTool(t, authHeader, "df_write_file", map[string]any{"name": "share.md", "content_text": "s"}))
	fileID, _ := file["id"].(string)
	shared := toolPayload(t, env.callTool(t, authHeader, "df_create_share", map[string]any{
		"file_id": fileID, "permission": "view", "expires_in_days": 7, "password": "pass1234",
	}))
	token, _ := shared["token"].(string)
	if token == "" || shared["has_password"] != true {
		t.Fatalf("df_create_share = %v", shared)
	}
	if !strings.HasPrefix(shared["share_url"].(string), "/api/v1/public/shares/") {
		t.Fatalf("share_url = %v", shared["share_url"])
	}
	listing := toolPayload(t, env.callTool(t, authHeader, "df_list_shares", map[string]any{}))
	if shares, _ := listing["shares"].([]any); len(shares) != 1 {
		t.Fatalf("df_list_shares = %v", listing)
	}
	shareID, _ := shared["share_id"].(string)
	revoked := toolPayload(t, env.callTool(t, authHeader, "df_revoke_share", map[string]any{"share_id": shareID}))
	if revoked["revoked"] != true {
		t.Fatalf("df_revoke_share = %v", revoked)
	}
}

// JSON-RPC 错误码与 GET 405。
func TestMCPHTTPErrors(t *testing.T) {
	env := newMCPTestEnv(t, false)
	authHeader := env.jwt()

	// GET /mcp → 405。
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", authHeader)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d, want 405", w.Code)
	}

	// parse error → -32700。
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0"`))
	req.Header.Set("Authorization", authHeader)
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if got := rpcError(t, out); got != mcp.CodeParseError {
		t.Fatalf("parse error code = %d (%v)", got, out)
	}

	// method not found → -32601。
	_, out = env.callMCP(t, authHeader, "prompts/list", nil)
	if got := rpcError(t, out); got != mcp.CodeMethodNotFound {
		t.Fatalf("method not found code = %d (%v)", got, out)
	}

	// invalid params（file_id 非 UUID）→ -32602。
	_, out = env.callMCP(t, authHeader, "tools/call", map[string]any{"name": "df_get_file", "arguments": map[string]any{"file_id": "zzz"}})
	if got := rpcError(t, out); got != mcp.CodeInvalidParams {
		t.Fatalf("invalid params code = %d (%v)", got, out)
	}

	// notification → 202 无 body。
	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", authHeader)
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification = %d %q, want 202 empty", w.Code, w.Body.String())
	}
}

// 未启用 search：df_search_files 返回明确错误（isError + not available）。
func TestMCPSearchDisabled(t *testing.T) {
	env := newMCPTestEnv(t, false)
	_, out := env.callMCP(t, env.jwt(), "tools/call", map[string]any{"name": "df_search_files", "arguments": map[string]any{"query": "合同"}})
	if got := rpcError(t, out); got != 0 {
		t.Fatalf("unexpected rpc error %d: %v", got, out)
	}
	if text := toolErrorText(t, out); !strings.Contains(text, "not available") {
		t.Fatalf("search disabled error = %q (%v)", text, out)
	}
}

// 启用 search：df_search_files 正常返回。
func TestMCPSearchEnabled(t *testing.T) {
	env := newMCPTestEnv(t, true)
	payload := toolPayload(t, env.callTool(t, env.jwt(), "df_search_files", map[string]any{"query": "合同"}))
	if results, _ := payload["results"].([]any); len(results) != 1 {
		t.Fatalf("df_search_files = %v", payload)
	}
	if len(env.searchOn.queries) != 1 || env.searchOn.queries[0] != "合同" {
		t.Fatalf("queries = %v", env.searchOn.queries)
	}
}

// 大文件读上限：超 2MB 返回明确错误并提示 download 链接。
func TestMCPReadFileTooLarge(t *testing.T) {
	env := newMCPTestEnv(t, false)
	authHeader := env.jwt()
	// 直接在 fake 树里放一个 3MB blob（内容用零填充，不真正写满存储：
	// ReadSection 读前先做大小检查，不会触达存储）。
	root, _ := env.tree.mcpFindRoot(env.owner)
	env.tree.add(files.File{ID: uuid.New(), Name: "big.txt", ParentID: &root.ID, OwnerID: env.owner, Type: "file", ScopeType: "personal"}, 3<<20, "text/plain", files.BlobStatusAvailable)
	var bigID uuid.UUID
	for _, f := range env.tree.files {
		if f.Name == "big.txt" {
			bigID = f.ID
		}
	}
	_, out := env.callMCP(t, authHeader, "tools/call", map[string]any{"name": "df_read_file", "arguments": map[string]any{"file_id": bigID.String()}})
	text := toolErrorText(t, out)
	if !strings.Contains(text, "download") {
		t.Fatalf("oversized read error = %q, want download hint (%v)", text, out)
	}
}

// 二进制内容 text 模式 → 提示改用 base64。
func TestMCPReadBinaryHint(t *testing.T) {
	env := newMCPTestEnv(t, false)
	authHeader := env.jwt()
	root, _ := env.tree.mcpFindRoot(env.owner)
	binID := uuid.New()
	env.tree.add(files.File{ID: binID, Name: "image.png", ParentID: &root.ID, OwnerID: env.owner, Type: "file", ScopeType: "personal"}, 8, "image/png", files.BlobStatusAvailable)
	env.tree.contents[binID] = "PNGDATA "
	_ = env.storage.Put(env.tree.blobs[binID].StorageKey, strings.NewReader("PNGDATA "))
	_, out := env.callMCP(t, authHeader, "tools/call", map[string]any{"name": "df_read_file", "arguments": map[string]any{"file_id": binID.String()}})
	if text := toolErrorText(t, out); !strings.Contains(text, "as=base64") {
		t.Fatalf("binary hint = %q (%v)", text, out)
	}
	// base64 模式成功。
	payload := toolPayload(t, env.callTool(t, authHeader, "df_read_file", map[string]any{"file_id": binID.String(), "as": "base64"}))
	if payload["encoding"] != "base64" || payload["content"] == "" {
		t.Fatalf("base64 read = %v", payload)
	}
}
