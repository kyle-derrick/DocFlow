package http

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
)

// zipEnv 复用 fakeShareFiles（sharetree_test）作为 zipper 的内存树：
// owner 的 项目/{readme.txt, data.bin, deleted.txt, docs/notes.md, empty/}。
type zipEnv struct {
	h         *Handler
	router    *gin.Engine
	ff        *fakeShareFiles
	repo      *share.MemoryStore
	shares    *share.Service
	owner     uuid.UUID
	rootID    uuid.UUID
	token     string
	shareID   uuid.UUID
	jwtSecret string
}

func newZipEnv(t *testing.T) *zipEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	shareOwner = uuid.New()
	ff := newFakeShareFiles()
	rootID := uuid.New()
	ff.files[rootID] = files.File{ID: rootID, Name: "项目", OwnerID: shareOwner, Type: "folder"}
	_ = ff.add(rootID, "readme.txt", "file", "text/plain", "hello zip")
	_ = ff.add(rootID, "data.bin", "file", "application/octet-stream", "\x00\x01\x02")
	deletedID := ff.add(rootID, "deleted.txt", "file", "text/plain", "gone")
	df := ff.files[deletedID]
	now := time.Now()
	df.DeletedAt = &now
	ff.files[deletedID] = df
	docs := ff.add(rootID, "docs", "folder", "", "")
	_ = ff.add(docs, "notes.md", "file", "text/markdown", "# n")
	_ = ff.add(rootID, "empty", "folder", "", "")

	h := NewHandler(nil, nil, nil, nil, nil, nil, newMemStorage(), false, "", time.Hour)
	h.zipper = ff
	for fileID, blob := range ff.blobs {
		if content, ok := ff.contents[fileID]; ok {
			_ = h.storage.Put(blob.StorageKey, strings.NewReader(content))
		}
	}
	router := gin.New()
	secret := "jwt-zip-test-secret-0123456789abcdef"
	h.Register(router, secret, 1_000_000, 1_000_000, 1_000_000)
	return &zipEnv{h: h, router: router, ff: ff, owner: shareOwner, rootID: rootID, jwtSecret: secret}
}

func (e *zipEnv) get(path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.RemoteAddr = "192.0.2.40:4444"
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// openZipResponse 打开响应体 zip 并返回 名称→内容 与目录集合。
func openZipResponse(t *testing.T, w *httptest.ResponseRecorder) (map[string]string, map[string]bool) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("open zip: %v (body %d bytes)", err, w.Body.Len())
	}
	contents := map[string]string{}
	dirs := map[string]bool{}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			dirs[f.Name] = true
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		contents[f.Name] = string(data)
	}
	return contents, dirs
}

// ffChild 查 fakeShareFiles 的直接子项（该 fake 无 child 方法）。
func ffChild(ff *fakeShareFiles, parent uuid.UUID, name string) (files.File, bool) {
	id, ok := ff.byName[parent][strings.ToLower(name)]
	if !ok {
		return files.File{}, false
	}
	return ff.files[id], true
}

// ---------- 认证侧 ----------

func TestDownloadFolderZipStructureAndContent(t *testing.T) {
	env := newZipEnv(t)
	w := env.get("/api/v1/files/"+env.rootID.String()+"/download.zip", testJWTFor(env.jwtSecret, env.owner))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".zip") {
		t.Fatalf("content-disposition = %q", cd)
	}
	contents, dirs := openZipResponse(t, w)
	for name, want := range map[string]string{
		"readme.txt":    "hello zip",
		"data.bin":      "\x00\x01\x02",
		"docs/notes.md": "# n",
	} {
		if got, ok := contents[name]; !ok || got != want {
			t.Fatalf("entry %q = %q (found=%v), want %q", name, got, ok, want)
		}
	}
	for _, dir := range []string{"docs/", "empty/"} {
		if !dirs[dir] {
			t.Fatalf("dir entry %q missing (dirs=%v contents=%v)", dir, dirs, contents)
		}
	}
	if _, ok := contents["deleted.txt"]; ok {
		t.Fatal("soft-deleted file must be excluded")
	}
	if _, ok := dirs["项目/"]; ok {
		t.Fatal("root folder itself must not be an entry (paths relative to root)")
	}
}

func TestDownloadFolderZipEmptyFolder(t *testing.T) {
	env := newZipEnv(t)
	empty, _ := ffChild(env.ff, env.rootID, "empty")
	w := env.get("/api/v1/files/"+empty.ID.String()+"/download.zip", testJWTFor(env.jwtSecret, env.owner))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("empty folder zip must be valid: %v", err)
	}
	if len(zr.File) != 0 {
		t.Fatalf("entries = %d, want 0", len(zr.File))
	}
}

func TestDownloadFolderZipRequiresFolderAndPermission(t *testing.T) {
	env := newZipEnv(t)
	// 文件 id → 400。
	fileID, _ := ffChild(env.ff, env.rootID, "readme.txt")
	if w := env.get("/api/v1/files/"+fileID.ID.String()+"/download.zip", testJWTFor(env.jwtSecret, env.owner)); w.Code != http.StatusBadRequest {
		t.Fatalf("file target: %d", w.Code)
	}
	// 他人目录（个人空间非 owner）→ 404（不泄露存在性）。
	if w := env.get("/api/v1/files/"+env.rootID.String()+"/download.zip", testJWTFor(env.jwtSecret, uuid.New())); w.Code != http.StatusNotFound {
		t.Fatalf("other user: %d", w.Code)
	}
	// 未认证 → 401。
	if w := env.get("/api/v1/files/"+env.rootID.String()+"/download.zip", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
}

func TestDownloadFolderZipEntryLimit413(t *testing.T) {
	env := newZipEnv(t)
	folder := uuid.New()
	env.ff.files[folder] = files.File{ID: folder, Name: "big", ParentID: &env.rootID, OwnerID: env.owner, Type: "folder"}
	for i := 0; i <= zipDownloadMaxEntries; i++ {
		_ = env.ff.add(folder, fmt.Sprintf("f%04d.txt", i), "file", "text/plain", "x")
	}
	// 根索引补充 big 目录（fake add 只维护目标父目录索引）。
	env.ff.byName[env.rootID][strings.ToLower("big")] = folder
	w := env.get("/api/v1/files/"+folder.String()+"/download.zip", testJWTFor(env.jwtSecret, env.owner))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
}

func TestDownloadFolderZipSizeLimit413(t *testing.T) {
	env := newZipEnv(t)
	// 单文件 blob.size 超 2GB：预遍历即拒绝（无需真实内容）。
	id := uuid.New()
	pid := env.rootID
	env.ff.files[id] = files.File{ID: id, Name: "huge.bin", ParentID: &pid, OwnerID: env.owner, Type: "file"}
	env.ff.versions[id] = files.FileVersion{ID: uuid.New(), FileID: id, Version: 1, Size: zipDownloadMaxTotalSize + 1}
	env.ff.blobs[id] = files.ObjectBlob{ID: uuid.New(), StorageKey: "objects/huge", Size: zipDownloadMaxTotalSize + 1, MimeType: "application/octet-stream", Status: files.BlobStatusAvailable}
	env.ff.byName[env.rootID]["huge.bin"] = id
	w := env.get("/api/v1/files/"+env.rootID.String()+"/download.zip", testJWTFor(env.jwtSecret, env.owner))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", w.Code)
	}
}

// ---------- 公开分享侧 ----------

func newZipShareEnv(t *testing.T, permission string, maxDownloads *int) *zipEnv {
	t.Helper()
	env := newZipEnv(t)
	repo := share.NewMemoryStore()
	svc := share.NewService(repo, env.ff)
	svc.SetTreeSource(env.ff)
	sh, token, err := svc.CreatePublic(env.owner, env.rootID, permission, 0, maxDownloads, share.ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	env.h.shares = svc
	env.repo = repo
	env.shares = svc
	env.token = token
	env.shareID = sh.ID
	return env
}

func TestPublicShareDownloadZipConsumesOnce(t *testing.T) {
	env := newZipShareEnv(t, share.PermissionDownload, nil)
	w := env.get("/api/v1/public/shares/"+env.token+"/download.zip", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	contents, dirs := openZipResponse(t, w)
	if contents["readme.txt"] != "hello zip" || !dirs["docs/"] {
		t.Fatalf("zip content mismatch: %v %v", contents, dirs)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Fatalf("content-disposition = %q", cd)
	}
	// 计数消耗一次（分享与根目录文件）。
	sh, err := env.repo.Get(env.shareID)
	if err != nil || sh.DownloadCount != 1 {
		t.Fatalf("download_count = %d (err=%v), want 1", sh.DownloadCount, err)
	}
	if env.ff.dl[env.rootID] != 1 {
		t.Fatalf("file download_count = %d, want 1", env.ff.dl[env.rootID])
	}
}

func TestPublicShareDownloadZipLimitGone(t *testing.T) {
	max := 1
	env := newZipShareEnv(t, share.PermissionDownload, &max)
	if w := env.get("/api/v1/public/shares/"+env.token+"/download.zip", ""); w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	// 达上限 → 410。
	if w := env.get("/api/v1/public/shares/"+env.token+"/download.zip", ""); w.Code != http.StatusGone {
		t.Fatalf("second: %d body %s", w.Code, w.Body.String())
	}
	// 撤销 → 410。
	env2 := newZipShareEnv(t, share.PermissionDownload, nil)
	if _, err := env2.shares.Revoke(env2.owner, env2.shareID); err != nil {
		t.Fatal(err)
	}
	if w := env2.get("/api/v1/public/shares/"+env2.token+"/download.zip", ""); w.Code != http.StatusGone {
		t.Fatalf("revoked: %d", w.Code)
	}
}

func TestPublicShareDownloadZipViewOnlyForbidden(t *testing.T) {
	env := newZipShareEnv(t, share.PermissionView, nil)
	w := env.get("/api/v1/public/shares/"+env.token+"/download.zip", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("view-only share: %d body %s", w.Code, w.Body.String())
	}
	// 未消耗计数。
	if sh, err := env.repo.Get(env.shareID); err != nil || sh.DownloadCount != 0 {
		t.Fatalf("download_count = %d, want 0", sh.DownloadCount)
	}
}

func TestPublicShareDownloadZipFileShare400(t *testing.T) {
	env := newZipEnv(t)
	repo := share.NewMemoryStore()
	svc := share.NewService(repo, env.ff)
	fileID, _ := ffChild(env.ff, env.rootID, "readme.txt")
	_, token, err := svc.CreatePublic(env.owner, fileID.ID, share.PermissionDownload, 0, nil, share.ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	env.h.shares = svc
	w := env.get("/api/v1/public/shares/"+token+"/download.zip", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("file share: %d body %s", w.Code, w.Body.String())
	}
}
