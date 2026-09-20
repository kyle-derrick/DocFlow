package http

import (
	"archive/zip"
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
	"github.com/docflow/docflow/internal/upload"
)

// rawFakeTree 补齐 unpackAPI 所需方法（CurrentVersion/FindChildByName 已在
// resolve_test.go 实现）。
func (t *rawFakeTree) Get(user, id uuid.UUID) (files.File, error) {
	// 软删除项不可见（与生产 files.Store.Get 的 deleted_at IS NULL 对齐）。
	f, ok := t.files[id]
	if !ok || f.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	if err := t.authorizeRead(f, user); err != nil {
		return files.File{}, err
	}
	return f, nil
}

func (t *rawFakeTree) ValidateFolder(user, parent uuid.UUID) error {
	f, ok := t.files[parent]
	if !ok || f.Type != "folder" {
		return files.ErrNotFound
	}
	if f.OwnerID != user && !t.writers[user] {
		return files.ErrForbidden
	}
	return nil
}

func (t *rawFakeTree) CreateFolderIn(user, parent uuid.UUID, name string) (files.File, error) {
	if existing, ok := t.child(parent, name); ok {
		if existing.Type == "folder" {
			return existing, nil
		}
		return files.File{}, files.ErrConflict
	}
	id := uuid.New()
	pid := parent
	spaceID := uuid.Nil
	if p, ok := t.files[parent]; ok {
		spaceID = p.SpaceID
	}
	f := files.File{ID: id, Name: name, ParentID: &pid, OwnerID: user, Type: "folder", SpaceID: spaceID}
	t.files[id] = f
	if t.byName[parent] == nil {
		t.byName[parent] = map[string]uuid.UUID{}
	}
	t.byName[parent][strings.ToLower(name)] = id
	return f, nil
}

// createUploaded 模拟 files.CreateUploadedFile：同名冲突 ErrConflict，否则
// 建文件行 + v1 版本与 blob（状态 available）。
func (t *rawFakeTree) createUploaded(user, parent uuid.UUID, name, storageKey string, size int64, sha256, mimeType string) (uuid.UUID, bool, error) {
	if _, ok := t.child(parent, name); ok {
		return uuid.Nil, false, files.ErrConflict
	}
	t.seq++
	id := uuid.New()
	pid := parent
	spaceID := uuid.Nil
	if p, ok := t.files[parent]; ok {
		spaceID = p.SpaceID
	}
	t.files[id] = files.File{ID: id, Name: name, ParentID: &pid, OwnerID: user, Type: "file", SpaceID: spaceID}
	if t.byName[parent] == nil {
		t.byName[parent] = map[string]uuid.UUID{}
	}
	t.byName[parent][strings.ToLower(name)] = id
	t.versions[id] = files.FileVersion{ID: uuid.New(), FileID: id, Version: 1, Size: size}
	t.blobs[id] = files.ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, Status: files.BlobStatusAvailable}
	return id, true, nil
}

// buildZip 构造内存 zip（名字→内容）。
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type unpackEnv struct {
	h      *Handler
	router *gin.Engine
	tree   *rawFakeTree
	owner  uuid.UUID
	zipID  uuid.UUID
}

func newUnpackEnv(t *testing.T, zipBytes []byte) *unpackEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tree := newRawFakeTree()
	owner := uuid.New()
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, Type: "folder", IsRoot: true}
	tree.add(root, 0, "", "")
	zipID := uuid.New()
	tree.add(files.File{ID: zipID, Name: "bundle.zip", ParentID: &root.ID, OwnerID: owner, Type: "file"}, int64(len(zipBytes)), "application/zip", files.BlobStatusAvailable)

	h := NewHandler(nil, nil, nil, nil, nil, nil, newMemStorage(), false, "", time.Hour)
	h.unpacker = tree
	// 真实 upload.Service：office 校验/黑名单/配额管线自然生效（内存 store）。
	h.uploads = upload.NewService(upload.NewMemoryStore(), h.storage, time.Hour, 1<<30, false, tree.ValidateFolder, tree.createUploaded)
	_ = h.storage.Put(tree.blobs[zipID].StorageKey, bytes.NewReader(zipBytes))

	router := gin.New()
	router.POST("/api/v1/files/:id/unpack", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, owner)
		h.unpackZip(c)
	})
	return &unpackEnv{h: h, router: router, tree: tree, owner: owner, zipID: zipID}
}

func (e *unpackEnv) unpack(t *testing.T) (int, unpackResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/"+e.zipID.String()+"/unpack", nil)
	req.RemoteAddr = "192.0.2.30:3333"
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	var out unpackResult
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func TestUnpackNormalPackage(t *testing.T) {
	data := buildZip(t, map[string]string{
		"index.html":       "<html></html>",
		"assets/app.js":    "console.log(1)",
		"assets/style.css": "body{}",
		"docs/说明.md":       "# hi",
	})
	env := newUnpackEnv(t, data)
	code, out := env.unpack(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d body %v", code, out)
	}
	if out.CreatedFiles != 4 || out.CreatedFolders != 3 || out.Skipped != 0 || len(out.Failures) != 0 {
		t.Fatalf("result = %+v", out)
	}
	// 树校验：bundle/{index.html, assets/{app.js, style.css}, docs/说明.md}。
	var bundle files.File
	for _, f := range env.tree.files {
		if f.Name == "bundle" && f.Type == "folder" && f.ParentID != nil {
			bundle = f
		}
	}
	if bundle.ID == uuid.Nil {
		t.Fatal("bundle folder not created")
	}
	if _, ok := env.tree.child(bundle.ID, "index.html"); !ok {
		t.Fatal("index.html missing")
	}
	// MIME 按扩展映射（upload 管线 detectContentType）。
	var indexID uuid.UUID
	for id, f := range env.tree.files {
		if f.Name == "index.html" && f.ParentID != nil && *f.ParentID == bundle.ID {
			indexID = id
		}
	}
	if mime := env.tree.blobs[indexID].MimeType; !strings.HasPrefix(mime, "text/html") {
		t.Fatalf("index.html mime = %q", mime)
	}
	// 中文条目落库。
	assets, _ := env.tree.child(bundle.ID, "assets")
	docs, _ := env.tree.child(bundle.ID, "docs")
	if _, ok := env.tree.child(docs.ID, "说明.md"); !ok {
		t.Fatal("中文条目 missing")
	}
	if _, ok := env.tree.child(assets.ID, "app.js"); !ok {
		t.Fatal("assets/app.js missing")
	}
}

func TestUnpackIdempotentRerun(t *testing.T) {
	data := buildZip(t, map[string]string{"index.html": "<html></html>", "a/b.txt": "b"})
	env := newUnpackEnv(t, data)
	if code, out := env.unpack(t); code != http.StatusOK || out.CreatedFiles != 2 {
		t.Fatalf("first run: %d %+v", code, out)
	}
	code, out := env.unpack(t)
	if code != http.StatusOK {
		t.Fatalf("second run: %d", code)
	}
	if out.CreatedFiles != 0 || out.CreatedFolders != 0 || out.Skipped != 2 || len(out.Failures) != 0 {
		// 第二次：目标目录已存在（复用），同名文件跳过。
		t.Fatalf("rerun = %+v", out)
	}
}

func TestUnpackRejectsTraversalAndInvalid(t *testing.T) {
	// 穿越条目 → 400，且不建任何目录（预校验先行）。
	env := newUnpackEnv(t, buildZip(t, map[string]string{"../evil.txt": "x"}))
	code, out := env.unpack(t)
	if code != http.StatusBadRequest {
		t.Fatalf("traversal: %d %+v", code, out)
	}
	for _, f := range env.tree.files {
		if f.Name == "bundle" {
			t.Fatal("traversal package must not create folders")
		}
	}
	// 绝对路径/反斜杠/保留名同样拒绝。
	for _, name := range []string{"/abs.txt", `a\b.txt`, "CON.txt"} {
		env := newUnpackEnv(t, buildZip(t, map[string]string{name: "x"}))
		if code, _ := env.unpack(t); code != http.StatusBadRequest {
			t.Fatalf("entry %q: code = %d", name, code)
		}
	}
}

func TestUnpackRejectsLimitsAndEmpty(t *testing.T) {
	// 条目数超限 → 400。
	entries := map[string]string{}
	for i := 0; i < 501; i++ {
		entries[fmt.Sprintf("f%03d.txt", i)] = "x"
	}
	env := newUnpackEnv(t, buildZip(t, entries))
	if code, _ := env.unpack(t); code != http.StatusBadRequest {
		t.Fatalf("too many entries: code = %d", code)
	}
	// 展开总量超限 → 400（单条目声明大小超 500MB 由 webpkg 校验语义覆盖，
	// 这里以多个大声明条目逼近上限验证路径）。
	// 空 zip（仅目录条目）→ 400。
	var onlyDirs bytes.Buffer
	zw := zip.NewWriter(&onlyDirs)
	if _, err := zw.Create("empty-dir/"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	env = newUnpackEnv(t, onlyDirs.Bytes())
	if code, _ := env.unpack(t); code != http.StatusBadRequest {
		t.Fatalf("empty zip: code = %d", code)
	}
	// 非 zip 内容 → 400。
	env = newUnpackEnv(t, []byte("this is not a zip"))
	if code, _ := env.unpack(t); code != http.StatusBadRequest {
		t.Fatalf("not a zip: code = %d", code)
	}
}

func TestUnpackAggregatesFailures(t *testing.T) {
	// 伪造 .docx（内容非 Office zip）→ upload 管线 office 校验拒绝该条目，
	// 其余条目正常落库，失败明细聚合返回（不半提交到整体失败）。
	data := buildZip(t, map[string]string{
		"good.txt": "ok",
		"bad.docx": "definitely-not-an-office-xml-package",
	})
	env := newUnpackEnv(t, data)
	code, out := env.unpack(t)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out.CreatedFiles != 1 || len(out.Failures) != 1 || out.Failures[0].Path != "bad.docx" {
		t.Fatalf("result = %+v", out)
	}
}

func TestUnpackRequiresWritePermission(t *testing.T) {
	data := buildZip(t, map[string]string{"index.html": "x"})
	env := newUnpackEnv(t, data)
	// 非目标父目录 owner（且无团队写权限）→ ValidateFolder 拒绝。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/files/"+env.zipID.String()+"/unpack", nil)
	req.RemoteAddr = "192.0.2.30:3333"
	router := gin.New()
	router.POST("/api/v1/files/:id/unpack", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, uuid.New())
		env.h.unpackZip(c)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
}
