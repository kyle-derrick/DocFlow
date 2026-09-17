package http

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
)

// buildXmindZip 构造含 2 sheet、嵌套 children 与 notes 的 .xmind 内容。
func buildXmindZip(t *testing.T) []byte {
	t.Helper()
	content := `[
  {"title":"工作计划","rootTopic":{"title":"季度目标","notes":{"plain":{"content":"根备注"}},"children":{"attached":[
    {"title":"研发","children":{"attached":[{"title":"后端重构"},{"title":"前端优化"}]}},
    {"title":"市场"}
  ]}}},
  {"title":"会议","rootTopic":{"title":"周会","children":{"attached":[{"title":"待办"}]}}}
]`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("content.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type convertEnv struct {
	h      *Handler
	router *gin.Engine
	tree   *rawFakeTree
	owner  uuid.UUID
	root   files.File
}

// newConvertEnv 建 owner 根目录 + 指定源文件（name/content/blob 状态），
// 并装配真实 upload.Service（office 校验/配额管线经内存 store 生效）。
func newConvertEnv(t *testing.T, name string, content []byte, blobStatus string) *convertEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tree := newRawFakeTree()
	owner := uuid.New()
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, Type: "folder", IsRoot: true, ScopeType: "personal"}
	tree.add(root, 0, "", "")
	srcID := uuid.New()
	tree.add(files.File{ID: srcID, Name: name, ParentID: &root.ID, OwnerID: owner, Type: "file", ScopeType: "personal"}, int64(len(content)), "application/octet-stream", blobStatus)
	tree.contents[srcID] = string(content)

	h := NewHandler(nil, nil, nil, nil, nil, nil, newMemStorage(), false, "", time.Hour)
	h.unpacker = tree
	h.uploads = upload.NewService(upload.NewMemoryStore(), h.storage, time.Hour, 1<<30, false, tree.ValidateFolder, tree.createUploaded)
	_ = h.storage.Put(tree.blobs[srcID].StorageKey, bytes.NewReader(content))

	router := gin.New()
	router.POST("/api/v1/files/:id/convert-markdown", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, owner)
		h.convertToMarkdown(c)
	})
	return &convertEnv{h: h, router: router, tree: tree, owner: owner, root: root}
}

func (e *convertEnv) convert(t *testing.T, id uuid.UUID, actor uuid.UUID) (int, map[string]any) {
	t.Helper()
	router := e.router
	if actor != e.owner {
		router = gin.New()
		router.POST("/api/v1/files/:id/convert-markdown", func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			e.h.convertToMarkdown(c)
		})
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/"+id.String()+"/convert-markdown", nil)
	req.RemoteAddr = "192.0.2.50:5555"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func (e *convertEnv) sourceID(t *testing.T, name string) uuid.UUID {
	t.Helper()
	for id, f := range e.tree.files {
		if f.Name == name {
			return id
		}
	}
	t.Fatalf("source %s not found", name)
	return uuid.Nil
}

// readConverted 经对象存储读回转换产物内容（走上传管线落库后的 blob key）。
func (e *convertEnv) readConverted(t *testing.T, fileID uuid.UUID) string {
	t.Helper()
	blob, ok := e.tree.blobs[fileID]
	if !ok {
		t.Fatalf("converted file %s has no blob", fileID)
	}
	rc, err := e.h.storage.Read(blob.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return string(data)
}

func TestConvertMarkdownCreatesFile(t *testing.T) {
	env := newConvertEnv(t, "plan.xmind", buildXmindZip(t), files.BlobStatusAvailable)
	src := env.sourceID(t, "plan.xmind")
	code, out := env.convert(t, src, env.owner)
	if code != http.StatusCreated {
		t.Fatalf("status = %d body %v", code, out)
	}
	if out["name"] != "plan.md" {
		t.Fatalf("name = %v", out["name"])
	}
	fileIDText, _ := out["file_id"].(string)
	if fileIDText == "" {
		t.Fatalf("file_id missing: %v", out)
	}
	// 读回 markdown 断言结构。
	md := env.readConverted(t, uuid.MustParse(fileIDText))
	for _, want := range []string{
		"# 工作计划", "- 季度目标", "  > 根备注", "  - 研发", "    - 后端重构", "    - 前端优化", "  - 市场",
		"# 会议", "- 周会", "  - 待办",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	// 新文件落在源文件同目录（root 下）。
	created := env.tree.files[uuid.MustParse(fileIDText)]
	if created.ParentID == nil || *created.ParentID != env.root.ID || created.Type != "file" {
		t.Fatalf("created file = %+v", created)
	}
}

func TestConvertMarkdownNameConflictSuffix(t *testing.T) {
	env := newConvertEnv(t, "plan.xmind", buildXmindZip(t), files.BlobStatusAvailable)
	src := env.sourceID(t, "plan.xmind")
	if code, _ := env.convert(t, src, env.owner); code != http.StatusCreated {
		t.Fatal("first convert failed")
	}
	// 第二次：plan.md 已占 → plan-converted-1.md。
	code, out := env.convert(t, src, env.owner)
	if code != http.StatusCreated {
		t.Fatalf("second: %d %v", code, out)
	}
	if out["name"] != "plan-converted-1.md" {
		t.Fatalf("name = %v", out["name"])
	}
	// 第三次：plan-converted-2.md。
	_, out = env.convert(t, src, env.owner)
	if out["name"] != "plan-converted-2.md" {
		t.Fatalf("third name = %v", out["name"])
	}
}

func TestConvertMarkdownRejectsInvalidSources(t *testing.T) {
	// 非 xmind 源 → 422。
	env := newConvertEnv(t, "notes.txt", []byte("plain text"), files.BlobStatusAvailable)
	code, out := env.convert(t, env.sourceID(t, "notes.txt"), env.owner)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("non-xmind: %d %v", code, out)
	}
	// 坏 zip（.xmind 扩展但内容非法）→ 422 带原因。
	env = newConvertEnv(t, "bad.xmind", []byte("not a zip at all"), files.BlobStatusAvailable)
	code, out = env.convert(t, env.sourceID(t, "bad.xmind"), env.owner)
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "invalid xmind") {
		t.Fatalf("bad zip: %d %v", code, out)
	}
	// 老版 content.xml → 422 且错误信息明确。
	var legacy bytes.Buffer
	zw := zip.NewWriter(&legacy)
	if _, err := zw.Create("content.xml"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	env = newConvertEnv(t, "old.xmind", legacy.Bytes(), files.BlobStatusAvailable)
	code, out = env.convert(t, env.sourceID(t, "old.xmind"), env.owner)
	if code != http.StatusUnprocessableEntity || !strings.Contains(out["error"].(string), "content.xml") {
		t.Fatalf("legacy: %d %v", code, out)
	}
}

func TestConvertMarkdownUnavailableAndForbidden(t *testing.T) {
	// blob 非 available → 403。
	env := newConvertEnv(t, "plan.xmind", buildXmindZip(t), files.BlobStatusQuarantined)
	code, _ := env.convert(t, env.sourceID(t, "plan.xmind"), env.owner)
	if code != http.StatusForbidden {
		t.Fatalf("quarantined: %d", code)
	}
	// 他人（个人空间非 owner）→ 404 不泄露存在性。
	env = newConvertEnv(t, "plan.xmind", buildXmindZip(t), files.BlobStatusAvailable)
	code, _ = env.convert(t, env.sourceID(t, "plan.xmind"), uuid.New())
	if code != http.StatusNotFound {
		t.Fatalf("other user: %d", code)
	}
}
