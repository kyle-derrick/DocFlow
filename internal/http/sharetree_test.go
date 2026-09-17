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

	"github.com/docflow/docflow/internal/contenturl"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/share"
)

// fakeShareFiles 同时实现 share.FileSource 与 share.TreeSource（内存树，
// 复用 resolve_test 的 rawFakeTree 结构语义）。
type fakeShareFiles struct {
	files    map[uuid.UUID]files.File
	byName   map[uuid.UUID]map[string]uuid.UUID
	versions map[uuid.UUID]files.FileVersion
	blobs    map[uuid.UUID]files.ObjectBlob
	contents map[uuid.UUID]string
	dl       map[uuid.UUID]int64
	views    map[uuid.UUID]int64
	seq      int
}

func newFakeShareFiles() *fakeShareFiles {
	return &fakeShareFiles{
		files: map[uuid.UUID]files.File{}, byName: map[uuid.UUID]map[string]uuid.UUID{},
		versions: map[uuid.UUID]files.FileVersion{}, blobs: map[uuid.UUID]files.ObjectBlob{},
		contents: map[uuid.UUID]string{}, dl: map[uuid.UUID]int64{}, views: map[uuid.UUID]int64{},
	}
}

func (f *fakeShareFiles) add(parent uuid.UUID, name, fileType, mime, content string) uuid.UUID {
	f.seq++
	id := uuid.New()
	parentID := parent
	f.files[id] = files.File{ID: id, Name: name, ParentID: &parentID, OwnerID: shareOwner, Type: fileType}
	if f.byName[parent] == nil {
		f.byName[parent] = map[string]uuid.UUID{}
	}
	f.byName[parent][strings.ToLower(name)] = id
	if fileType == "file" {
		f.versions[id] = files.FileVersion{ID: uuid.New(), FileID: id, Version: 1, Size: int64(len(content))}
		f.blobs[id] = files.ObjectBlob{ID: uuid.New(), SHA256: fmt.Sprintf("%064d", f.seq), StorageKey: "objects/" + id.String(), Size: int64(len(content)), MimeType: mime, Status: files.BlobStatusAvailable}
		f.contents[id] = content
	}
	return id
}

var shareOwner uuid.UUID

func (f *fakeShareFiles) Get(owner, id uuid.UUID) (files.File, error) {
	fl, ok := f.files[id]
	if !ok || fl.OwnerID != owner || fl.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return fl, nil
}

func (f *fakeShareFiles) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.Get(owner, fileID); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	v, ok := f.versions[fileID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
	}
	return v, f.blobs[fileID], nil
}

func (f *fakeShareFiles) IncrementDownloadCount(owner, fileID uuid.UUID) error {
	f.dl[fileID]++
	return nil
}
func (f *fakeShareFiles) IncrementViewCount(owner, fileID uuid.UUID) error {
	f.views[fileID]++
	return nil
}

func (f *fakeShareFiles) ResolveSubpath(base files.File, path string) (files.File, []files.File, error) {
	if base.Type != "folder" {
		return files.File{}, nil, files.ErrInvalidTarget
	}
	segments, err := files.SplitPathSegments(path)
	if err != nil {
		return files.File{}, nil, err
	}
	cur := base
	chain := []files.File{base}
	for i, seg := range segments {
		id, ok := f.byName[cur.ID][strings.ToLower(seg)]
		if !ok {
			return files.File{}, nil, files.ErrNotFound
		}
		next := f.files[id]
		if next.DeletedAt != nil {
			return files.File{}, nil, files.ErrNotFound
		}
		if i < len(segments)-1 && next.Type != "folder" {
			return files.File{}, nil, files.ErrNotFound
		}
		chain = append(chain, next)
		cur = next
	}
	return cur, chain, nil
}

func (f *fakeShareFiles) ListFolderChildren(folderID uuid.UUID, limit int) ([]files.File, error) {
	var out []files.File
	for _, id := range f.byName[folderID] {
		if fl, ok := f.files[id]; ok && fl.DeletedAt == nil {
			out = append(out, fl)
		}
	}
	return out, nil
}

type shareTreeEnv struct {
	h        *Handler
	router   *gin.Engine
	shares   *share.Service
	ff       *fakeShareFiles
	repo     *share.MemoryStore
	signer   *contenturl.Signer
	owner    uuid.UUID
	rootID   uuid.UUID
	token    string
	shareID  uuid.UUID
	password string
}

// newShareTreeEnv 建目录分享环境：owner 的 /站点/{index.html, assets/app.js}。
// password 非空时创建密码保护的分享。
func newShareTreeEnv(t *testing.T, password string) *shareTreeEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	shareOwner = uuid.New()
	ff := newFakeShareFiles()
	rootID := uuid.New()
	ff.files[rootID] = files.File{ID: rootID, Name: "站点", OwnerID: shareOwner, Type: "folder"}
	_ = ff.add(rootID, "index.html", "file", "text/html", "<html>index</html>")
	assets := ff.add(rootID, "assets", "folder", "", "")
	_ = ff.add(assets, "app.js", "file", "text/javascript", "console.log(1)")
	_ = ff.add(rootID, "bad.exe", "file", "application/octet-stream", "MZ")

	repo := share.NewMemoryStore()
	svc := share.NewService(repo, ff)
	svc.SetTreeSource(ff)
	var token string
	var shareID uuid.UUID
	var err error
	if password == "" {
		var sh share.Share
		sh, token, err = svc.CreatePublic(shareOwner, rootID, share.PermissionView, 0, nil, share.ShareOptions{})
		shareID = sh.ID
	} else {
		var sh share.Share
		sh, token, err = svc.CreatePublic(shareOwner, rootID, share.PermissionView, 0, nil, share.ShareOptions{Password: password})
		shareID = sh.ID
	}
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(nil, nil, nil, svc, nil, nil, newMemStorage(), false, "", time.Hour)
	signer := contenturl.NewSigner(testRawSecret, contenturl.DefaultTTL)
	h.contentSigner = signer
	for fileID, blob := range ff.blobs {
		_ = h.storage.Put(blob.StorageKey, strings.NewReader(ff.contents[fileID]))
	}
	router := gin.New()
	h.Register(router, "jwt-test-secret-0123456789abcdef", 1_000_000, 1_000_000, 1_000_000)
	return &shareTreeEnv{h: h, router: router, shares: svc, ff: ff, repo: repo, signer: signer, owner: shareOwner, rootID: rootID, token: token, shareID: shareID, password: password}
}

func (e *shareTreeEnv) get(path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	req.RemoteAddr = "192.0.2.20:2222"
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *shareTreeEnv) treeGrant(t *testing.T) string {
	t.Helper()
	w := e.get("/api/v1/public/shares/" + e.token + "/tree/")
	if w.Code != http.StatusOK {
		t.Fatalf("tree: %d %s", w.Code, w.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	rawBase, _ := out["raw_base"].(string)
	parts := strings.Split(strings.TrimPrefix(rawBase, "/raw/share/"+e.token+"/"), "/")
	return parts[0]
}

func TestPublicShareTreeListing(t *testing.T) {
	env := newShareTreeEnv(t, "")
	w := env.get("/api/v1/public/shares/" + env.token + "/tree/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["type"] != "folder" || out["name"] != "站点" {
		t.Fatalf("out = %v", out)
	}
	rawBase, _ := out["raw_base"].(string)
	if !strings.HasPrefix(rawBase, "/raw/share/"+env.token+"/") {
		t.Fatalf("raw_base = %q", rawBase)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %v", out["entries"])
	}
	// 子目录清单 + 文件元数据。
	w = env.get("/api/v1/public/shares/" + env.token + "/tree/assets")
	if w.Code != http.StatusOK {
		t.Fatalf("sub: %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	entries, _ = out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("sub entries = %v", out["entries"])
	}
	first, _ := entries[0].(map[string]any)
	if first["path"] != "assets/app.js" || first["type"] != "file" || first["mime_type"] != "text/javascript" {
		t.Fatalf("entry = %v", first)
	}
	w = env.get("/api/v1/public/shares/" + env.token + "/tree/assets/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("file: %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["type"] != "file" || out["size"].(float64) != float64(len("console.log(1)")) {
		t.Fatalf("file meta = %v", out)
	}
	// 越根/不存在 → 404。
	if w := env.get("/api/v1/public/shares/" + env.token + "/tree/不存在.html"); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", w.Code)
	}
	if w := env.get("/api/v1/public/shares/" + env.token + "/tree/../x"); w.Code != http.StatusNotFound {
		t.Fatalf("traversal: %d", w.Code)
	}
}

func TestRawShareServesSubresources(t *testing.T) {
	env := newShareTreeEnv(t, "")
	grant := env.treeGrant(t)
	// 文件：内容 + 安全头。
	w := env.get(fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, grant))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "console.log(1)" {
		t.Fatalf("body = %q", body)
	}
	if w.Header().Get("Content-Security-Policy") != rawCSP || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("headers = %v", w.Header())
	}
	// 目录无尾斜杠 → 308 加斜杠（子目录路径；裸根无斜杠由 gin 路由器先行
	// 301 补斜杠，语义一致）；尾斜杠 → index.html。
	rootPath := fmt.Sprintf("/raw/share/%s/%s", env.token, grant)
	if w := env.get(rootPath + "/assets"); w.Code != http.StatusPermanentRedirect {
		t.Fatalf("no slash: %d", w.Code)
	}
	w = env.get(rootPath + "/")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if body := w.Body.String(); body != "<html>index</html>" {
		t.Fatalf("index body = %q", body)
	}
	// 未知扩展 → 415。
	if w := env.get(fmt.Sprintf("/raw/share/%s/%s/bad.exe", env.token, grant)); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad ext: %d", w.Code)
	}
	// Range。
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, grant), nil)
	req.Header.Set("Range", "bytes=0-6")
	req.RemoteAddr = "192.0.2.20:2222"
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "console" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	// 子资源 raw 不消耗分享下载计数。
	if sh, gerr := env.repo.Get(env.shareID); gerr != nil || sh.DownloadCount != 0 {
		t.Fatalf("raw subresource must not consume download count: %+v err=%v", sh, gerr)
	}
}

func TestRawShareGrantAndShareState(t *testing.T) {
	env := newShareTreeEnv(t, "")
	// 过期 grant → 404。
	expired, _ := env.signer.Sign(contenturl.Claims{Purpose: contenturl.PurposeRawShare, ShareID: env.shareID.String(), Exp: time.Now().Add(-time.Minute).Unix()})
	if w := env.get(fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, expired)); w.Code != http.StatusNotFound {
		t.Fatalf("expired grant: %d", w.Code)
	}
	// purpose=raw 的 grant 冒充 → 404。
	rawGrant, _ := env.signer.Sign(contenturl.Claims{Purpose: contenturl.PurposeRaw, UserID: env.owner.String()})
	if w := env.get(fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, rawGrant)); w.Code != http.StatusNotFound {
		t.Fatalf("raw as raw-share: %d", w.Code)
	}
	// grant 绑定其他分享（token/grant 不匹配）→ 404。
	otherShare := uuid.New()
	otherGrant, _ := env.signer.Sign(contenturl.Claims{Purpose: contenturl.PurposeRawShare, ShareID: otherShare.String()})
	if w := env.get(fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, otherGrant)); w.Code != http.StatusNotFound {
		t.Fatalf("cross share: %d", w.Code)
	}
	// 撤销 → tree 与 raw 均 410。
	grant := env.treeGrant(t)
	if _, err := env.shares.Revoke(env.owner, env.shareID); err != nil {
		t.Fatal(err)
	}
	if w := env.get("/api/v1/public/shares/" + env.token + "/tree/"); w.Code != http.StatusGone {
		t.Fatalf("tree revoked: %d", w.Code)
	}
	if w := env.get(fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, grant)); w.Code != http.StatusGone {
		t.Fatalf("raw revoked: %d", w.Code)
	}
}

func TestRawShareBlobUnavailable(t *testing.T) {
	env := newShareTreeEnv(t, "")
	grant := env.treeGrant(t)
	// 当前版本 blob 转 quarantined → raw 404（tree 元数据仍可见但 size 归零语义）。
	var appID uuid.UUID
	for id, f := range env.ff.files {
		if f.Name == "app.js" {
			appID = id
		}
	}
	blob := env.ff.blobs[appID]
	blob.Status = files.BlobStatusQuarantined
	env.ff.blobs[appID] = blob
	if w := env.get(fmt.Sprintf("/raw/share/%s/%s/assets/app.js", env.token, grant)); w.Code != http.StatusNotFound {
		t.Fatalf("unavailable blob: %d", w.Code)
	}
}

func TestShareTreePasswordFlow(t *testing.T) {
	env := newShareTreeEnv(t, "pass-1234")
	// 未携带会话 cookie：tree 401 PASSWORD_REQUIRED（且不签发 grant）。
	w := env.get("/api/v1/public/shares/" + env.token + "/tree/")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "PASSWORD_REQUIRED") {
		t.Fatalf("no session: %d %s", w.Code, w.Body.String())
	}
	// verify → 会话 cookie → tree OK。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/public/shares/"+env.token+"/verify", bytes.NewReader([]byte(`{"password":"pass-1234"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.20:2222"
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("verify must set session cookie")
	}
	if w := env.get("/api/v1/public/shares/"+env.token+"/tree/", cookies...); w.Code != http.StatusOK {
		t.Fatalf("tree with session: %d %s", w.Code, w.Body.String())
	}
}
