// Package http —— ai_platform_tools_test.go：内置平台文件工具执行桥单测
// （executePlatformTool 的 list/mkdir/write/read 往返、拒绝路径、审计）与
// /ai/chat use_files 端到端（工具声明注入 + 工具循环落库 + SSE 完成）。
package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/ai"
	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/space"
	"github.com/docflow/docflow/internal/upload"
)

// CurrentBlobs 补齐 rawFakeTree 的 platformFileStore 能力（批量当前版本
// blob 元数据；resolve/mcp 测试文件未实现，此处补齐供内置工具使用）。
func (t *rawFakeTree) CurrentBlobs(ids []uuid.UUID) map[uuid.UUID]files.ObjectBlob {
	out := make(map[uuid.UUID]files.ObjectBlob, len(ids))
	for _, id := range ids {
		if b, ok := t.blobs[id]; ok {
			out[id] = b
		}
	}
	return out
}

// 编译期断言：rawFakeTree 满足内置平台工具的文件源接口。
var _ platformFileStore = (*rawFakeTree)(nil)

// newPlatformToolEnv 组装执行器测试环境：真 space.Service（内存 store，
// owner 预置默认空间）+ rawFakeTree（根目录挂默认空间下）+ 真
// upload.Service（office 校验/黑名单/配额管线自然生效）+ 内存存储。
func newPlatformToolEnv(t *testing.T) (*platformToolCtx, *rawFakeTree, *memStorage, *memAuditRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tree := newRawFakeTree()
	owner := uuid.New()
	spaceSvc := space.NewService(space.NewMemoryStore())
	defaultSpace, _, err := spaceSvc.EnsureDefaultSpace(owner, "alice")
	if err != nil {
		t.Fatal(err)
	}
	tree.members[owner] = true
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, SpaceID: defaultSpace.ID, Type: "folder", IsRoot: true}
	tree.add(root, 0, "", "")
	storage := newMemStorage()
	h := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", time.Hour)
	uploadSvc := upload.NewService(upload.NewMemoryStore(), storage, time.Hour, 1<<30, false, tree.ValidateFolder, tree.createUploaded)
	uploadSvc.SetVersionTarget(tree.ValidateReplaceTarget, tree.ReplaceFileVersion)
	h.uploads = uploadSvc
	h.spaces = spaceSvc
	h.mcpDeps.Files = tree // 文件源注入通道（生产为 NewHandler 的 h.files）
	rec := &memAuditRecorder{}
	h.SetAuditRecorder(rec)
	ptc := h.newPlatformToolCtx(nil, owner, uuid.Nil)
	if ptc == nil {
		t.Fatal("执行上下文构造失败（依赖装配不完整）")
	}
	return ptc, tree, storage, rec
}

// callTool 执行一个工具并解析结果 JSON 的 ok 标志。
func callTool(t *testing.T, ptc *platformToolCtx, name, args string) (bool, string) {
	t.Helper()
	out, err := ptc.executePlatformTool(name, json.RawMessage(args))
	if err != nil {
		return false, err.Error()
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("工具 %s 结果不是 JSON: %v（%s）", name, jerr, out)
	}
	return res.OK, res.Error
}

// TestExecutePlatformToolRoundtrip mkdir→write→read→覆盖→list_dir 往返：
// 写入经真实 upload 管线落库（新文件 UploadBytes / 覆盖 StartReplace 版本链），
// 读回内容一致，覆盖后版本号递增，审计含 ai.tool。
func TestExecutePlatformToolRoundtrip(t *testing.T) {
	ptc, tree, storage, rec := newPlatformToolEnv(t)

	// 1. df_mkdir 多级建目录。
	if ok, msg := callTool(t, ptc, "df_mkdir", `{"path":"项目/文档"}`); !ok {
		t.Fatalf("df_mkdir 失败: %s", msg)
	}
	// 2. df_write_file 新建文件（经 upload 管线落库）。
	ok, msg := callTool(t, ptc, "df_write_file", `{"path":"项目/文档/笔记.md","content":"# 标题\n正文内容"}`)
	if !ok {
		t.Fatalf("df_write_file 新建失败: %s", msg)
	}
	var fileID uuid.UUID
	for id, f := range tree.files {
		if f.Name == "笔记.md" {
			fileID = id
		}
	}
	if fileID == uuid.Nil {
		t.Fatal("文件未落库到文件树")
	}
	// 内容应写入对象存储（blob key 指向物理内容）。
	blob := tree.blobs[fileID]
	if blob.Status != files.BlobStatusAvailable {
		t.Fatalf("blob 状态 = %s", blob.Status)
	}
	raw, err := storage.Read(blob.StorageKey)
	if err != nil {
		t.Fatalf("读取落库内容失败: %v", err)
	}
	body, _ := io.ReadAll(raw)
	raw.Close()
	if string(body) != "# 标题\n正文内容" {
		t.Fatalf("落库内容 = %q", body)
	}
	// 3. df_read_file 读回。
	ok, msg = callTool(t, ptc, "df_read_file", `{"path":"项目/文档/笔记.md"}`)
	if !ok {
		t.Fatalf("df_read_file 失败: %s", msg)
	}
	out, _ := ptc.executePlatformTool("df_read_file", json.RawMessage(`{"path":"项目/文档/笔记.md"}`))
	if !strings.Contains(out, "# 标题\\n正文内容") {
		t.Fatalf("读回内容缺失: %s", out)
	}
	// 4. df_write_file 覆盖（版本链：版本号 1→2）。
	if ok, msg := callTool(t, ptc, "df_write_file", `{"path":"项目/文档/笔记.md","content":"改写后的内容"}`); !ok {
		t.Fatalf("df_write_file 覆盖失败: %s", msg)
	}
	if v := tree.versions[fileID]; v.Version != 2 {
		t.Fatalf("覆盖后版本 = %d, want 2（版本链保护）", v.Version)
	}
	// 5. df_list_dir 列目录（含子目录与大小）。
	if ok, msg := callTool(t, ptc, "df_list_dir", `{"path":"项目"}`); !ok {
		t.Fatalf("df_list_dir 失败: %s", msg)
	}
	out, _ = ptc.executePlatformTool("df_list_dir", json.RawMessage(`{"path":"项目"}`))
	if !strings.Contains(out, `"name":"文档"`) || !strings.Contains(out, `"type":"folder"`) {
		t.Fatalf("list_dir 结果异常: %s", out)
	}
	// 根目录列举（path 省略）。
	if ok, msg := callTool(t, ptc, "df_list_dir", `{}`); !ok {
		t.Fatalf("df_list_dir 根目录失败: %s", msg)
	}
	// 6. 审计：df_write_file 的成功记录（metadata 含工具名+参数摘要）。
	var writeAudit *audit.Entry = nil
	for i := range rec.entries {
		if rec.entries[i].Action == "ai.tool" && strings.Contains(rec.entries[i].Metadata, `"tool":"df_write_file"`) {
			writeAudit = &rec.entries[i]
		}
	}
	if writeAudit == nil {
		t.Fatal("缺少 df_write_file 的 ai.tool 审计记录")
	}
	if writeAudit.Status != "success" || !strings.Contains(writeAudit.Metadata, "content_bytes=") {
		t.Fatalf("ai.tool 审计 = %+v meta=%s", *writeAudit, writeAudit.Metadata)
	}
}

// TestExecutePlatformToolRejections 拒绝路径：二进制扩展名、超限内容、
// 父目录不存在、读不存在文件、缺必填参数、未知工具。
func TestExecutePlatformToolRejections(t *testing.T) {
	ptc, _, _, rec := newPlatformToolEnv(t)

	// 扩展名白名单：exe 拒绝、无扩展名拒绝。
	if ok, msg := callTool(t, ptc, "df_write_file", `{"path":"evil.exe","content":"x"}`); ok || !strings.Contains(msg, "白名单") {
		t.Fatalf("exe 应被白名单拒绝: ok=%v msg=%s", ok, msg)
	}
	if ok, _ := callTool(t, ptc, "df_write_file", `{"path":"noext","content":"x"}`); ok {
		t.Fatal("无扩展名应被拒绝")
	}
	// 内容超 2MB。
	big := strings.Repeat("a", ai.PlatformToolWriteMaxBytes+1)
	if ok, msg := callTool(t, ptc, "df_write_file", fmt.Sprintf(`{"path":"big.md","content":%q}`, big)); ok || !strings.Contains(msg, "上限") {
		t.Fatalf("超限内容应被拒绝: ok=%v msg=%s", ok, msg)
	}
	// 父目录不存在。
	if ok, msg := callTool(t, ptc, "df_write_file", `{"path":"不存在的目录/a.md","content":"x"}`); ok || !strings.Contains(msg, "父目录不存在") {
		t.Fatalf("父目录缺失应被拒绝: ok=%v msg=%s", ok, msg)
	}
	// 读不存在的文件。
	if ok, msg := callTool(t, ptc, "df_read_file", `{"path":"不存在.md"}`); ok || !strings.Contains(msg, "不存在") {
		t.Fatalf("读不存在文件应报错: ok=%v msg=%s", ok, msg)
	}
	// 必填参数缺失。
	if ok, _ := callTool(t, ptc, "df_read_file", `{}`); ok {
		t.Fatal("df_read_file 缺 path 应报错")
	}
	if ok, _ := callTool(t, ptc, "df_search", `{}`); ok {
		t.Fatal("df_search 缺 query 应报错")
	}
	// 未知工具返回 Go 错误（chat 层转为「工具调用失败」）。
	if _, err := ptc.executePlatformTool("df_chmod", json.RawMessage(`{}`)); err == nil {
		t.Fatal("未知工具应返回错误")
	}
	// 失败也写审计。
	var failed int
	for _, e := range rec.entries {
		if e.Action == "ai.tool" && e.Status == "failure" {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("失败执行应写 ai.tool failure 审计")
	}
}

// TestExecutePlatformToolSearch df_search：未装配检索服务时报错不中断。
func TestExecutePlatformToolSearch(t *testing.T) {
	ptc, _, _, _ := newPlatformToolEnv(t)
	if ok, _ := callTool(t, ptc, "df_search", `{"query":"笔记"}`); ok {
		t.Fatal("未装配 search 时 df_search 应报 ok:false")
	}
	ptc.svc.search = &fakeMCPSearch{}
	if ok, msg := callTool(t, ptc, "df_search", `{"query":"笔记"}`); !ok {
		t.Fatalf("df_search 失败: %s", msg)
	}
	out, _ := ptc.executePlatformTool("df_search", json.RawMessage(`{"query":"笔记"}`))
	if !strings.Contains(out, `"snippet"`) && !strings.Contains(out, `"name"`) {
		t.Fatalf("df_search 结果异常: %s", out)
	}
}

// ---------- /ai/chat use_files 端到端 ----------

// newUseFilesChatEnv 组装 aiChat 测试路由：fake openai 兼容上游（第 1 轮
// tool_calls df_write_file、第 2 轮文本）+ 平台工具依赖（同
// newPlatformToolEnv）。返回 router、tree、上游请求体捕获。
func newUseFilesChatEnv(t *testing.T, toolRound bool) (*gin.Engine, *rawFakeTree, *[]map[string]any) {
	t.Helper()
	tree := newRawFakeTree()
	owner := uuid.New()
	spaceSvc := space.NewService(space.NewMemoryStore())
	defaultSpace, _, err := spaceSvc.EnsureDefaultSpace(owner, "alice")
	if err != nil {
		t.Fatal(err)
	}
	tree.members[owner] = true
	tree.add(files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, SpaceID: defaultSpace.ID, Type: "folder", IsRoot: true}, 0, "", "")
	storage := newMemStorage()
	h := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", time.Hour)
	uploadSvc := upload.NewService(upload.NewMemoryStore(), storage, time.Hour, 1<<30, false, tree.ValidateFolder, tree.createUploaded)
	uploadSvc.SetVersionTarget(tree.ValidateReplaceTarget, tree.ReplaceFileVersion)
	h.uploads = uploadSvc
	h.spaces = spaceSvc
	h.mcpDeps.Files = tree
	h.aiLimiter = newDynamicRateLimiter()
	h.settings = &fakeSettingsService{}

	var mu sync.Mutex
	bodies := &[]map[string]any{}
	rounds := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sse := func(payload string) { _, _ = w.Write([]byte("data: " + payload + "\n\n")) }
		if toolRound && rounds == 1 {
			// 并行工具调用：先建目录再写文件（一轮两个 tool_calls）。
			sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_f0","function":{"name":"df_mkdir","arguments":"{\"path\":\"ai\"}"}}]}}]}`)
			sse(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_f1","function":{"name":"df_write_file","arguments":"{\"path\":\"ai/草稿.md\",\"content\":\"AI 写入的内容\"}"}}]}}]}`)
			sse(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
			sse(`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":9}}`)
			sse(`[DONE]`)
			return
		}
		sse(`{"choices":[{"delta":{"content":"文件已写入。"}}]}`)
		sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		sse(`{"choices":[],"usage":{"prompt_tokens":6,"completion_tokens":6}}`)
		sse(`[DONE]`)
	}))
	t.Cleanup(up.Close)
	h.aiSvc = ai.NewService(func() (settings.AIConfig, error) {
		return settings.AIConfig{Providers: []settings.AIProvider{{ID: "o1", Name: "OpenAI", Kind: settings.AIKindOpenAICompatible, BaseURL: up.URL, Model: "gpt-test", Enabled: true}}, DefaultProvider: "o1", Temperature: 0.3, MaxTokens: 128, PerUserPerMin: 100}, nil
	})
	r := gin.New()
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, owner)
			handle(c)
		}
	}
	r.POST("/api/v1/ai/chat", withUser(h.aiChat))
	return r, tree, bodies
}

// toolsNames 提取上游请求体的 tools 函数名列表。
func toolsNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := json.Marshal(body["tools"])
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Function.Name)
	}
	return out
}

// TestAIChatUseFilesToolsInjected use_files 缺省（默认 true，用户有空间）：
// 上游请求 tools 含全部 df_*；use_files=false 时不注入。
func TestAIChatUseFilesToolsInjected(t *testing.T) {
	r, _, bodies := newUseFilesChatEnv(t, false)
	w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"看下我的文件"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if len(*bodies) == 0 {
		t.Fatal("上游未收到请求")
	}
	got := toolsNames(t, (*bodies)[0])
	want := map[string]bool{"df_list_dir": false, "df_read_file": false, "df_write_file": false, "df_mkdir": false, "df_search": false}
	for _, n := range got {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Fatalf("上游 tools 缺少 %s（实际 %v）", n, got)
		}
	}
	// 显式关闭：上游请求不含 tools。
	r2, _, bodiesOff := newUseFilesChatEnv(t, false)
	w2 := postAI(r2, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"看下我的文件"}],"use_files":false}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("off status = %d body=%s", w2.Code, w2.Body.String())
	}
	if tools := toolsNames(t, (*bodiesOff)[0]); len(tools) != 0 {
		t.Fatalf("use_files=false 不应注入工具: %v", tools)
	}
}

// TestAIChatUseFilesToolLoop 全链路：模型请求 df_write_file → 执行器经
// upload 管线落库 → 工具结果回喂 → 最终文本经 SSE 完成（tool 事件可见）。
func TestAIChatUseFilesToolLoop(t *testing.T) {
	r, tree, _ := newUseFilesChatEnv(t, true)
	w := postAI(r, "/api/v1/ai/chat", `{"messages":[{"role":"user","content":"帮我写个草稿"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// SSE tool 事件（执行前下发，tool 字段按名透传）。
	if !strings.Contains(body, "event: tool") || !strings.Contains(body, `"tool":"df_write_file"`) || !strings.Contains(body, `"server":"docflow"`) {
		t.Fatalf("tool 事件缺失: %s", body)
	}
	events, joined := sseDeltas(body)
	has := func(name string) bool {
		for _, e := range events {
			if e == name {
				return true
			}
		}
		return false
	}
	if !has("meta") || !has("delta") || !has("done") || !has("tool") {
		t.Fatalf("events = %v", events)
	}
	if joined != "文件已写入。" {
		t.Fatalf("delta joined = %q", joined)
	}
	// 工具真实落库：目录与文件均在树中。
	var draft uuid.UUID
	for id, f := range tree.files {
		if f.Name == "草稿.md" {
			draft = id
		}
	}
	if draft == uuid.Nil {
		t.Fatal("df_write_file 未落库（草稿.md 不存在）")
	}
	if tree.versions[draft].Version != 1 {
		t.Fatalf("版本 = %d, want 1", tree.versions[draft].Version)
	}
	found := false
	for _, f := range tree.files {
		if f.Name == "ai" && f.Type == "folder" {
			found = true
		}
	}
	if !found {
		t.Fatal("df_mkdir 未落库（目录 ai 不存在）")
	}
}
