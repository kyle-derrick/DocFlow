package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/contenturl"
	"github.com/docflow/docflow/internal/files"
)

// rawFakeTree 是 resolveAPI/unpackAPI 的内存实现：维护父子树与版本/blob，
// 读授权模拟「owner 或团队成员（members）」，写授权模拟「owner 或 editor
// （writers）」——退团场景删除 members[user] 即可即时生效。
type rawFakeTree struct {
	files    map[uuid.UUID]files.File
	byName   map[uuid.UUID]map[string]uuid.UUID
	versions map[uuid.UUID]files.FileVersion // fileID → 当前版本
	blobs    map[uuid.UUID]files.ObjectBlob  // fileID → 当前版本 blob
	contents map[uuid.UUID]string            // fileID → 存储内容（长度与 blob.Size 一致）
	members  map[uuid.UUID]bool              // actor → 可读团队资源
	writers  map[uuid.UUID]bool              // actor → 可写
	seq      int
}

func newRawFakeTree() *rawFakeTree {
	return &rawFakeTree{
		files: map[uuid.UUID]files.File{}, byName: map[uuid.UUID]map[string]uuid.UUID{},
		versions: map[uuid.UUID]files.FileVersion{}, blobs: map[uuid.UUID]files.ObjectBlob{},
		contents: map[uuid.UUID]string{}, members: map[uuid.UUID]bool{}, writers: map[uuid.UUID]bool{},
	}
}

func (t *rawFakeTree) add(f files.File, size int64, mime, blobStatus string) {
	t.seq++
	t.files[f.ID] = f
	if f.ParentID != nil {
		if t.byName[*f.ParentID] == nil {
			t.byName[*f.ParentID] = map[string]uuid.UUID{}
		}
		t.byName[*f.ParentID][strings.ToLower(f.Name)] = f.ID
	}
	versionID := uuid.New()
	t.versions[f.ID] = files.FileVersion{ID: versionID, FileID: f.ID, Version: 1, Size: size}
	t.blobs[f.ID] = files.ObjectBlob{ID: uuid.New(), SHA256: fmt.Sprintf("%064d", t.seq), StorageKey: "objects/" + f.ID.String(), Size: size, MimeType: mime, Status: blobStatus}
	if f.Type == "file" {
		// 内容长度与 blob.Size 严格一致（DataFromReader 按 Content-Length 截断）。
		content := []byte(fmt.Sprintf("c%06d-of-%s", t.seq, f.Name))
		if int64(len(content)) > size {
			content = content[:size]
		}
		for int64(len(content)) < size {
			content = append(content, '.')
		}
		t.contents[f.ID] = string(content)
	}
}

func (t *rawFakeTree) remove(id uuid.UUID) {
	f := t.files[id]
	if f.ParentID != nil {
		delete(t.byName[*f.ParentID], strings.ToLower(f.Name))
	}
	delete(t.files, id)
}

func (t *rawFakeTree) child(parent uuid.UUID, name string) (files.File, bool) {
	id, ok := t.byName[parent][strings.ToLower(name)]
	if !ok {
		return files.File{}, false
	}
	return t.files[id], true
}

// authorizeRead 模拟 authorizeFileAccess：owner 短路；团队资源须成员；
// 个人资源他人 ErrNotFound（不泄露存在性）。
func (t *rawFakeTree) authorizeRead(f files.File, user uuid.UUID) error {
	if f.OwnerID == user {
		return nil
	}
	if f.ScopeType == "team" {
		if !t.members[user] {
			return files.ErrNotFound
		}
		return nil
	}
	return files.ErrNotFound
}

func (t *rawFakeTree) authorizeWrite(f files.File, user uuid.UUID) error {
	if f.OwnerID == user {
		return nil
	}
	if !t.writers[user] {
		return files.ErrForbidden
	}
	return nil
}

func (t *rawFakeTree) resolve(actor uuid.UUID, root files.File, path string, write bool) (files.File, []files.File, error) {
	segments, err := files.SplitPathSegments(path)
	if err != nil {
		return files.File{}, nil, err
	}
	cur := root
	chain := []files.File{root}
	for i, seg := range segments {
		next, ok := t.child(cur.ID, seg)
		if !ok {
			return files.File{}, nil, files.ErrNotFound
		}
		if i < len(segments)-1 && next.Type != "folder" {
			return files.File{}, nil, files.ErrNotFound
		}
		chain = append(chain, next)
		cur = next
	}
	if write {
		return cur, chain, t.authorizeWrite(cur, actor)
	}
	return cur, chain, t.authorizeRead(cur, actor)
}

func (t *rawFakeTree) ResolveReadablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (files.File, []files.File, error) {
	root, err := t.namespaceRoot(actor, nsType, scopeID)
	if err != nil {
		return files.File{}, nil, err
	}
	return t.resolve(actor, root, path, false)
}

func (t *rawFakeTree) ResolveWritablePath(actor uuid.UUID, nsType string, scopeID uuid.UUID, path string) (files.File, []files.File, error) {
	root, err := t.namespaceRoot(actor, nsType, scopeID)
	if err != nil {
		return files.File{}, nil, err
	}
	return t.resolve(actor, root, path, true)
}

func (t *rawFakeTree) namespaceRoot(actor uuid.UUID, nsType string, scopeID uuid.UUID) (files.File, error) {
	switch nsType {
	case files.NamespacePersonal:
		if scopeID != actor {
			return files.File{}, files.ErrNotFound
		}
		for _, f := range t.files {
			if f.IsRoot && f.OwnerID == actor && f.TeamID == nil {
				return f, nil
			}
		}
		return files.File{}, files.ErrNotFound
	case files.NamespaceTeam:
		if !t.members[actor] {
			return files.File{}, files.ErrNotFound
		}
		for _, f := range t.files {
			if f.IsRoot && f.TeamID != nil && *f.TeamID == scopeID {
				return f, nil
			}
		}
		return files.File{}, files.ErrNotFound
	default:
		return files.File{}, files.ErrInvalidNamespace
	}
}

func (t *rawFakeTree) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	f, ok := t.files[fileID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	if err := t.authorizeRead(f, owner); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	v, ok := t.versions[fileID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
	}
	return v, t.blobs[fileID], nil
}

func (t *rawFakeTree) FindChildByName(parent uuid.UUID, name string) (files.File, error) {
	if f, ok := t.child(parent, name); ok {
		return f, nil
	}
	return files.File{}, files.ErrNotFound
}

// ---------- 测试环境 ----------

const testRawSecret = "raw-test-secret-0123456789abcdef"

type rawTestEnv struct {
	h        *Handler
	router   *gin.Engine
	tree     *rawFakeTree
	signer   *contenturl.Signer
	personal uuid.UUID // 个人空间 owner
	teamID   uuid.UUID
	root     files.File // 个人根
	teamRoot files.File
}

func newRawTestEnv(t *testing.T) *rawTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tree := newRawFakeTree()
	owner := uuid.New()
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, Type: "folder", IsRoot: true, ScopeType: "personal"}
	tree.add(root, 0, "", "")
	docs := files.File{ID: uuid.New(), Name: "docs", ParentID: &root.ID, OwnerID: owner, Type: "folder", ScopeType: "personal"}
	tree.add(docs, 0, "", "")
	tree.add(files.File{ID: uuid.New(), Name: "index.html", ParentID: &docs.ID, OwnerID: owner, Type: "file", ScopeType: "personal"}, 21, "text/html", files.BlobStatusAvailable)
	tree.add(files.File{ID: uuid.New(), Name: "app.js", ParentID: &docs.ID, OwnerID: owner, Type: "file", ScopeType: "personal"}, 9, "text/javascript", files.BlobStatusAvailable)
	tree.add(files.File{ID: uuid.New(), Name: "data.bin", ParentID: &docs.ID, OwnerID: owner, Type: "file", ScopeType: "personal"}, 4, "application/octet-stream", files.BlobStatusAvailable)
	tree.add(files.File{ID: uuid.New(), Name: "quarantined.txt", ParentID: &root.ID, OwnerID: owner, Type: "file", ScopeType: "personal"}, 4, "text/plain", files.BlobStatusQuarantined)

	teamID := uuid.New()
	teamRoot := files.File{ID: uuid.New(), Name: "团队根", OwnerID: uuid.New(), Type: "folder", IsRoot: true, ScopeType: "team", TeamID: &teamID}
	tree.add(teamRoot, 0, "", "")
	tree.add(files.File{ID: uuid.New(), Name: "report.pdf", ParentID: &teamRoot.ID, OwnerID: teamRoot.OwnerID, Type: "file", ScopeType: "team", TeamID: &teamID}, 11, "application/pdf", files.BlobStatusAvailable)

	h := NewHandler(nil, nil, nil, nil, nil, nil, newMemStorage(), false, "", time.Hour)
	h.resolver = tree
	signer := contenturl.NewSigner(testRawSecret, contenturl.DefaultTTL)
	h.contentSigner = signer
	// 存储内容：与 blob key 对应且长度与 blob.Size 一致。
	for fileID, blob := range tree.blobs {
		if content, ok := tree.contents[fileID]; ok {
			_ = h.storage.Put(blob.StorageKey, strings.NewReader(content))
		}
	}
	router := gin.New()
	h.Register(router, "jwt-test-secret-0123456789abcdef", 1_000_000, 1_000_000, 1_000_000)
	return &rawTestEnv{h: h, router: router, tree: tree, signer: signer, personal: owner, teamID: teamID, root: root, teamRoot: teamRoot}
}

func testJWTFor(secret string, user uuid.UUID) string {
	claims := jwt.RegisteredClaims{Subject: user.String(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}
	token, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	return token
}

func (e *rawTestEnv) get(path, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.RemoteAddr = "192.0.2.10:1111"
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *rawTestEnv) resolveJSON(t *testing.T, nsType string, scope uuid.UUID, path, query string) (int, map[string]any) {
	t.Helper()
	url := fmt.Sprintf("/api/v1/resolve/%s/%s/%s", nsType, scope, path)
	if query != "" {
		url += "?" + query
	}
	w := e.get(url, testJWTFor("jwt-test-secret-0123456789abcdef", e.personal))
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

func (e *rawTestEnv) signRaw(t *testing.T, user uuid.UUID, nsType string, scope uuid.UUID) string {
	t.Helper()
	token, err := e.signer.Sign(contenturl.Claims{Purpose: contenturl.PurposeRaw, UserID: user.String(), NSType: nsType, NSScope: scope.String()})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// ---------- resolve API ----------

func TestResolvePathAPI(t *testing.T) {
	env := newRawTestEnv(t)
	code, out := env.resolveJSON(t, files.NamespacePersonal, env.personal, "docs/app.js", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d body %s", code, mustJSON(t, out))
	}
	if out["type"] != "file" || out["name"] != "app.js" || out["canonical_path"] != "docs/app.js" {
		t.Fatalf("out = %v", out)
	}
	if mime := out["mime_type"]; mime != "text/javascript" {
		t.Fatalf("mime = %v", mime)
	}
	rawURL, _ := out["raw_url"].(string)
	if !strings.HasPrefix(rawURL, "/raw/auth/") || !strings.Contains(rawURL, "/personal/"+env.personal.String()+"/docs/app.js") {
		t.Fatalf("raw_url = %q", rawURL)
	}
	if _, ok := out["grant_expires_at"].(string); !ok {
		t.Fatalf("grant_expires_at missing: %v", out)
	}
	// 目录解析：raw_url/view_url 以尾斜杠结尾（index.html 语义）。
	code, out = env.resolveJSON(t, files.NamespacePersonal, env.personal, "docs", "")
	if code != http.StatusOK || out["type"] != "folder" {
		t.Fatalf("folder resolve: %d %v", code, out)
	}
	if raw, _ := out["raw_url"].(string); !strings.HasSuffix(raw, "/docs/") {
		t.Fatalf("folder raw_url = %q, want trailing slash", raw)
	}
	// origin_content=1 且配置基地址 → 绝对 raw_url。
	env.h.SetContentPublicBaseURL("https://content.example.com")
	code, out = env.resolveJSON(t, files.NamespacePersonal, env.personal, "docs/app.js", "origin_content=1")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if raw, _ := out["raw_url"].(string); !strings.HasPrefix(raw, "https://content.example.com/raw/auth/") {
		t.Fatalf("absolute raw_url = %q", raw)
	}
	// 未带 origin_content 时保持相对。
	_, out = env.resolveJSON(t, files.NamespacePersonal, env.personal, "docs/app.js", "")
	if raw, _ := out["raw_url"].(string); !strings.HasPrefix(raw, "/raw/auth/") {
		t.Fatalf("relative raw_url = %q", raw)
	}
}

func TestResolvePathAuthAndErrors(t *testing.T) {
	env := newRawTestEnv(t)
	// 未登录 401。
	if w := env.get("/api/v1/resolve/personal/"+env.personal.String()+"/docs", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", w.Code)
	}
	// 他人 scope → 404（personal 要求 scopeID==actor）。
	other := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/resolve/personal/"+env.personal.String()+"/docs/app.js", nil)
	req.Header.Set("Authorization", "Bearer "+testJWTFor("jwt-test-secret-0123456789abcdef", other))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("other actor: %d", w.Code)
	}
	// 不存在 → 404；非法命名空间 → 400；穿越段 → 400。
	if code, _ := env.resolveJSON(t, files.NamespacePersonal, env.personal, "missing/x.txt", ""); code != http.StatusNotFound {
		t.Fatalf("missing: %d", code)
	}
	if code, _ := env.resolveJSON(t, "bogus", env.personal, "x", ""); code != http.StatusBadRequest {
		t.Fatalf("bogus ns: %d", code)
	}
	if code, _ := env.resolveJSON(t, files.NamespacePersonal, env.personal, "../etc/passwd", ""); code != http.StatusBadRequest {
		t.Fatalf("traversal: %d", code)
	}
	// mode=edit：owner 可写。
	editCode, _ := env.resolveJSON(t, files.NamespacePersonal, env.personal, "docs/app.js", "mode=edit")
	if editCode != http.StatusOK {
		t.Fatalf("owner edit mode: %d", editCode)
	}
	// 非法 scope（非 UUID）→ 400。
	if w := env.get("/api/v1/resolve/personal/not-a-uuid/x", testJWTFor("jwt-test-secret-0123456789abcdef", env.personal)); w.Code != http.StatusBadRequest {
		t.Fatalf("bad scope id: %d", w.Code)
	}
}

// ---------- /raw/auth 矩阵 ----------

func (e *rawTestEnv) rawAuth(t *testing.T, grant, nsType string, scope uuid.UUID, path string) *httptest.ResponseRecorder {
	t.Helper()
	return e.get(fmt.Sprintf("/raw/auth/%s/%s/%s/%s", grant, nsType, scope, path), "")
}

func TestRawAuthServesFile(t *testing.T) {
	env := newRawTestEnv(t)
	grant := env.signRaw(t, env.personal, files.NamespacePersonal, env.personal)
	w := env.rawAuth(t, grant, files.NamespacePersonal, env.personal, "docs/app.js")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	want := env.tree.contents[lookupFileID(env, "app.js")]
	if body := w.Body.String(); body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("content-type = %q", ct)
	}
	for header, want := range map[string]string{
		"Content-Security-Policy": "sandbox allow-scripts",
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "private, no-store",
		"Referrer-Policy":         "no-referrer",
		"Accept-Ranges":           "bytes",
	} {
		if got := w.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline; filename=") {
		t.Fatalf("content-disposition = %q", cd)
	}
}

func lookupFileID(e *rawTestEnv, name string) uuid.UUID {
	for _, f := range e.tree.files {
		if f.Name == name {
			return f.ID
		}
	}
	return uuid.Nil
}

func TestRawAuthGrantMatrix(t *testing.T) {
	env := newRawTestEnv(t)
	valid := env.signRaw(t, env.personal, files.NamespacePersonal, env.personal)
	// 篡改 grant。
	tampered := "x" + valid[1:]
	if w := env.rawAuth(t, tampered, files.NamespacePersonal, env.personal, "docs/app.js"); w.Code != http.StatusNotFound {
		t.Fatalf("tampered grant: %d", w.Code)
	}
	// 过期 grant（签发时显式指定过去时间）。
	expired, err := env.signer.Sign(contenturl.Claims{Purpose: contenturl.PurposeRaw, UserID: env.personal.String(), NSType: files.NamespacePersonal, NSScope: env.personal.String(), Exp: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if w := env.rawAuth(t, expired, files.NamespacePersonal, env.personal, "docs/app.js"); w.Code != http.StatusNotFound {
		t.Fatalf("expired grant: %d", w.Code)
	}
	// 跨命名空间 / 跨 scope：grant 与 URL 不一致 → 404。
	if w := env.rawAuth(t, valid, "team", env.teamID, "report.pdf"); w.Code != http.StatusNotFound {
		t.Fatalf("cross ns: %d", w.Code)
	}
	if w := env.rawAuth(t, valid, files.NamespacePersonal, uuid.New(), "docs/app.js"); w.Code != http.StatusNotFound {
		t.Fatalf("cross scope: %d", w.Code)
	}
	// JWT access token 冒充 grant → 404。
	jwtGrant := testJWTFor("jwt-test-secret-0123456789abcdef", env.personal)
	if w := env.rawAuth(t, jwtGrant, files.NamespacePersonal, env.personal, "docs/app.js"); w.Code != http.StatusNotFound {
		t.Fatalf("jwt as grant: %d", w.Code)
	}
	// raw-share grant 冒充 raw grant → 404（purpose 隔离）。
	shareGrant, _ := env.signer.Sign(contenturl.Claims{Purpose: contenturl.PurposeRawShare, ShareID: uuid.New().String(), UserID: env.personal.String(), NSType: files.NamespacePersonal, NSScope: env.personal.String()})
	if w := env.rawAuth(t, shareGrant, files.NamespacePersonal, env.personal, "docs/app.js"); w.Code != http.StatusNotFound {
		t.Fatalf("raw-share as raw: %d", w.Code)
	}
	// 路径不存在 → 404；穿越段 → 404（raw 域不泄露细节）。
	if w := env.rawAuth(t, valid, files.NamespacePersonal, env.personal, "nope/x.js"); w.Code != http.StatusNotFound {
		t.Fatalf("missing path: %d", w.Code)
	}
	if w := env.rawAuth(t, valid, files.NamespacePersonal, env.personal, "../app.js"); w.Code != http.StatusNotFound {
		t.Fatalf("traversal: %d", w.Code)
	}
}

// TestRawAuthACLChangeImmediately404 模拟退团后立即 404（集成式：ACL 变更
// 生效后同 grant 请求被拒）。
func TestRawAuthACLChangeImmediately404(t *testing.T) {
	env := newRawTestEnv(t)
	// viewer 是团队成员可读团队文件。
	viewer := uuid.New()
	env.tree.members[viewer] = true
	grant := env.signRaw(t, viewer, files.NamespaceTeam, env.teamID)
	if w := env.rawAuth(t, grant, files.NamespaceTeam, env.teamID, "report.pdf"); w.Code != http.StatusOK {
		t.Fatalf("member read: %d", w.Code)
	}
	// 退团（ACL/成员表变更）→ 同一 grant 立即 404。
	delete(env.tree.members, viewer)
	if w := env.rawAuth(t, grant, files.NamespaceTeam, env.teamID, "report.pdf"); w.Code != http.StatusNotFound {
		t.Fatalf("after leaving team: %d", w.Code)
	}
}

func TestRawAuthFolderIndexAndRedirect(t *testing.T) {
	env := newRawTestEnv(t)
	grant := env.signRaw(t, env.personal, files.NamespacePersonal, env.personal)
	// 目录无尾斜杠 → 308 加斜杠（保留查询串）。
	req := httptest.NewRequest(http.MethodGet, "/raw/auth/"+grant+"/personal/"+env.personal.String()+"/docs?x=1", nil)
	req.RemoteAddr = "192.0.2.10:1111"
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasSuffix(loc, "/docs/?x=1") {
		t.Fatalf("location = %q", loc)
	}
	// 目录尾斜杠 → index.html。
	w = env.rawAuth(t, grant, files.NamespacePersonal, env.personal, "docs/")
	if w.Code != http.StatusOK {
		t.Fatalf("index: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	// 无 index.html 的目录（根目录）→ 404。
	w = env.rawAuth(t, grant, files.NamespacePersonal, env.personal, "/")
	if w.Code != http.StatusNotFound {
		t.Fatalf("root without index: %d", w.Code)
	}
}

func TestRawAuthWhitelistRangeHEAD(t *testing.T) {
	env := newRawTestEnv(t)
	grant := env.signRaw(t, env.personal, files.NamespacePersonal, env.personal)
	// 未知扩展 → 415。
	if w := env.rawAuth(t, grant, files.NamespacePersonal, env.personal, "docs/data.bin"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unknown ext: %d", w.Code)
	}
	// blob 非 available（quarantined）→ 404。
	if w := env.rawAuth(t, grant, files.NamespacePersonal, env.personal, "quarantined.txt"); w.Code != http.StatusNotFound {
		t.Fatalf("quarantined: %d", w.Code)
	}
	// 单区间 Range → 206。
	req := httptest.NewRequest(http.MethodGet, "/raw/auth/"+grant+"/personal/"+env.personal.String()+"/docs/app.js", nil)
	req.Header.Set("Range", "bytes=0-3")
	req.RemoteAddr = "192.0.2.10:1111"
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || len(w.Body.String()) != 4 {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	if cr := w.Header().Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-3/") {
		t.Fatalf("content-range = %q", cr)
	}
	// 不可满足区间 → 416 + Content-Range */size。
	req.Header.Set("Range", "bytes=9999-")
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable: %d", w.Code)
	}
	// HEAD：状态与安全头一致（httptest.ResponseRecorder 会记录 body 写入，
	// 真实 net/http 服务端对 HEAD 丢弃 body——此处仅断言元数据）。
	req = httptest.NewRequest(http.MethodHead, "/raw/auth/"+grant+"/personal/"+env.personal.String()+"/docs/app.js", nil)
	req.RemoteAddr = "192.0.2.10:1111"
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: %d", w.Code)
	}
	if w.Header().Get("Content-Type") == "" || w.Header().Get("Content-Security-Policy") != rawCSP {
		t.Fatalf("HEAD headers missing: %v", w.Header())
	}
}

func mustJSON(t *testing.T, v map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(v)
	return string(raw)
}

// rawCSP 引用断言（防止常量被误改）。
var _ = auth.UserIDContextKey
