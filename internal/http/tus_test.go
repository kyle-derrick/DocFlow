package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// memStorage 内存假存储，满足 upload.Storage，测试不落盘。
type memStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStorage() *memStorage { return &memStorage{objects: make(map[string][]byte)} }

func (m *memStorage) Put(key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return nil
}
func (m *memStorage) Append(key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append(m.objects[key], b...)
	return int64(len(b)), nil
}
func (m *memStorage) Read(key string) (io.ReadCloser, error) {
	m.mu.Lock()
	b, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		return nil, upload.ErrInvalidKey
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memStorage) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

const tusTestMaxSize = 64

// tusTestEnv 用内存 store + 假存储构建 tus 测试路由：
// 中间件直接注入 user_id，绕开 JWT；不依赖 PostgreSQL。
// 测试环境无 files.Store，创建会话时一律显式携带 parent_id，避免默认根目录路径。
type tusTestEnv struct {
	router *gin.Engine
	svc    *upload.Service
	store  *upload.MemoryStore
	user   uuid.UUID
	parent uuid.UUID
}

func newTusTestEnv(t *testing.T) *tusTestEnv {
	t.Helper()
	store := upload.NewMemoryStore()
	svc := upload.NewService(store, newMemStorage(), time.Hour, tusTestMaxSize, false, nil, nil)
	return newTusEnvFor(t, svc, store, uuid.New())
}

// newTusEnvFor 为指定用户构建路由；多环境共享同一 service 可验证 owner 隔离。
func newTusEnvFor(t *testing.T, svc *upload.Service, store *upload.MemoryStore, user uuid.UUID) *tusTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, svc, newMemStorage(), false, "", 0)
	router := gin.New()
	api := router.Group("/api/v1")
	api.Use(func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, user)
		c.Next()
	})
	registerTUSRoutes(api.Group("/tus"), h)
	api.GET("/uploads/:id", h.getUpload)
	return &tusTestEnv{router: router, svc: svc, store: store, user: user, parent: uuid.New()}
}

// tusMeta 组装 Upload-Metadata 头，自动附加 parent_id。
func (e *tusTestEnv) tusMeta(pairs ...string) string {
	return strings.Join(append(pairs, "parent_id "+b64(e.parent.String())), ",")
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func tusCreate(r http.Handler, length string, meta string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tus/files", nil)
	req.Header.Set("Upload-Length", length)
	if meta != "" {
		req.Header.Set("Upload-Metadata", meta)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func tusPatch(r http.Handler, id string, offset int64, body, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/tus/files/"+id, strings.NewReader(body))
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func tusHead(r http.Handler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodHead, "/api/v1/tus/files/"+id, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// mustCreateTUS 创建一个合法会话并断言 201，返回会话 id。
func mustCreateTUS(t *testing.T, env *tusTestEnv, size int64, meta string) string {
	t.Helper()
	w := tusCreate(env.router, strconv.FormatInt(size, 10), meta)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, body = %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/api/v1/tus/files/") {
		t.Fatalf("create: Location = %q", loc)
	}
	return strings.TrimPrefix(loc, "/api/v1/tus/files/")
}

func waitUploadStatus(t *testing.T, store *upload.MemoryStore, id uuid.UUID, want upload.Status) upload.UploadSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, want %s", v.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- metadata 解析（纯函数） ---

func TestParseTusMetadata(t *testing.T) {
	meta, err := parseTusMetadata("filename " + b64("hello world.txt") + ",filetype " + b64("text/plain") + ",is_confirmed")
	if err != nil {
		t.Fatalf("valid header rejected: %v", err)
	}
	if meta["filename"] != "hello world.txt" || meta["filetype"] != "text/plain" || meta["is_confirmed"] != "" {
		t.Fatalf("unexpected metadata: %#v", meta)
	}
	empty, err := parseTusMetadata("")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty header should yield empty map, got %#v, %v", empty, err)
	}
	for _, bad := range []string{
		"filename " + b64("a.txt") + ",filetype !!!not-base64!!!", // 非法 base64 字符
		"filename a.txt",             // value 未编码
		"a b c",                      // 多余字段
		"filename " + b64("x") + ",", // 末尾空对
	} {
		if _, err := parseTusMetadata(bad); err == nil {
			t.Fatalf("invalid header %q accepted", bad)
		}
	}
}

// --- 创建流程（POST） ---

func TestTusCreateHeaders(t *testing.T) {
	env := newTusTestEnv(t)
	sum := sha256.Sum256([]byte("hello"))
	meta := env.tusMeta(
		"filename "+b64("a.txt"),
		"filetype "+b64("text/plain"),
		"sha256 "+b64(strings.ToUpper(hex.EncodeToString(sum[:]))),
	)
	w := tusCreate(env.router, "5", meta)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Fatalf("Tus-Resumable = %q", got)
	}
	if got := w.Header().Get("Upload-Offset"); got != "0" {
		t.Fatalf("Upload-Offset = %q", got)
	}
	if _, err := http.ParseTime(w.Header().Get("Upload-Expires")); err != nil {
		t.Fatalf("Upload-Expires not RFC 7231 date: %q (%v)", w.Header().Get("Upload-Expires"), err)
	}
	id := strings.TrimPrefix(w.Header().Get("Location"), "/api/v1/tus/files/")
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("Location id invalid: %v", err)
	}
	v, err := env.store.Get(uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "a.txt" || v.Size != 5 || v.ParentID != env.parent {
		t.Fatalf("unexpected session: %#v", v)
	}
	if v.ExpectedSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("ExpectedSHA256 = %q, want lowercase hex", v.ExpectedSHA256)
	}
	if v.Metadata != meta {
		t.Fatalf("Metadata = %q, want raw header", v.Metadata)
	}
	if v.TusID != v.ID.String() {
		t.Fatalf("TusID = %q, want id string", v.TusID)
	}
}

func TestTusCreateErrors(t *testing.T) {
	env := newTusTestEnv(t)
	cases := []struct {
		name   string
		length string
		meta   string
		want   int
	}{
		{"missing upload-length", "", env.tusMeta("filename " + b64("a.txt")), http.StatusBadRequest},
		{"invalid upload-length", "abc", env.tusMeta("filename " + b64("a.txt")), http.StatusBadRequest},
		{"negative upload-length", "-1", env.tusMeta("filename " + b64("a.txt")), http.StatusBadRequest},
		{"exceeds max size", strconv.Itoa(tusTestMaxSize + 1), env.tusMeta("filename " + b64("a.txt")), http.StatusRequestEntityTooLarge},
		{"missing filename", "3", env.tusMeta("filetype " + b64("text/plain")), http.StatusBadRequest},
		{"no metadata at all", "3", "", http.StatusBadRequest},
		{"invalid base64 value", "3", env.tusMeta("filename ***"), http.StatusBadRequest},
		{"invalid parent id", "3", "filename " + b64("a.txt") + ",parent_id " + b64("not-a-uuid"), http.StatusBadRequest},
		{"invalid sha256", "3", env.tusMeta("filename "+b64("a.txt"), "sha256 "+b64("zz")), http.StatusBadRequest},
		{"malformed metadata", "3", "a b c", http.StatusBadRequest},
	}
	for _, tc := range cases {
		if w := tusCreate(env.router, tc.length, tc.meta); w.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.name, w.Code, tc.want, w.Body.String())
		}
	}
	// 合法请求仍可创建。
	if w := tusCreate(env.router, "3", env.tusMeta("filename "+b64("a.txt"))); w.Code != http.StatusCreated {
		t.Fatalf("valid create: status = %d", w.Code)
	}
}

// --- HEAD ---

func TestTusHeadOffsetAndMetadataEcho(t *testing.T) {
	env := newTusTestEnv(t)
	meta := env.tusMeta("filename "+b64("a.txt"), "filetype "+b64("text/plain"))
	id := mustCreateTUS(t, env, 5, meta)
	if w := tusPatch(env.router, id, 0, "hel", "application/offset+octet-stream"); w.Code != http.StatusNoContent {
		t.Fatalf("patch: status = %d", w.Code)
	}
	w := tusHead(env.router, id)
	if w.Code != http.StatusOK {
		t.Fatalf("head: status = %d", w.Code)
	}
	if got := w.Header().Get("Upload-Offset"); got != "3" {
		t.Fatalf("Upload-Offset = %q, want 3", got)
	}
	if got := w.Header().Get("Upload-Length"); got != "5" {
		t.Fatalf("Upload-Length = %q, want 5", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("Upload-Metadata"); got != meta {
		t.Fatalf("Upload-Metadata = %q, want echo %q", got, meta)
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Fatalf("Tus-Resumable = %q", got)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD response must not carry body, got %q", w.Body.String())
	}
	// 经自定义 API 创建的会话（无 tus metadata）不回显该头。
	v2, err := env.svc.Start(env.user, env.parent, "c.txt", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if w := tusHead(env.router, v2.ID.String()); w.Code != http.StatusOK || w.Header().Get("Upload-Metadata") != "" {
		t.Fatalf("no-metadata session: status = %d, header = %q", w.Code, w.Header().Get("Upload-Metadata"))
	}
}

func TestTusHeadNotFoundAndOwnerIsolation(t *testing.T) {
	env := newTusTestEnv(t)
	if w := tusHead(env.router, uuid.New().String()); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status = %d, want 404", w.Code)
	}
	if w := tusHead(env.router, "not-a-uuid"); w.Code != http.StatusNotFound {
		t.Fatalf("malformed id: status = %d, want 404", w.Code)
	}
	id := mustCreateTUS(t, env, 3, env.tusMeta("filename "+b64("a.txt")))
	// 另一用户访问同一资源：404，不泄露存在性。
	other := newTusEnvFor(t, env.svc, env.store, uuid.New())
	if w := tusHead(other.router, id); w.Code != http.StatusNotFound {
		t.Fatalf("other owner HEAD: status = %d, want 404", w.Code)
	}
	if w := tusPatch(other.router, id, 0, "abc", "application/offset+octet-stream"); w.Code != http.StatusNotFound {
		t.Fatalf("other owner PATCH: status = %d, want 404", w.Code)
	}
}

func TestTusHeadExpired(t *testing.T) {
	env := newTusTestEnv(t)
	id := mustCreateTUS(t, env, 3, env.tusMeta("filename "+b64("a.txt")))
	v, _ := env.store.Get(uuid.MustParse(id))
	v.ExpiresAt = time.Now().Add(-time.Minute)
	if err := env.store.Update(v); err != nil {
		t.Fatal(err)
	}
	if w := tusHead(env.router, id); w.Code != http.StatusGone {
		t.Fatalf("expired HEAD: status = %d, want 410", w.Code)
	}
}

// --- PATCH ---

func TestTusPatchFlow(t *testing.T) {
	env := newTusTestEnv(t)
	id := mustCreateTUS(t, env, 5, env.tusMeta("filename "+b64("a.txt")))
	w := tusPatch(env.router, id, 0, "hel", "application/offset+octet-stream")
	if w.Code != http.StatusNoContent {
		t.Fatalf("append: status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Upload-Offset"); got != "3" {
		t.Fatalf("Upload-Offset = %q, want 3", got)
	}
	if _, err := http.ParseTime(w.Header().Get("Upload-Expires")); err != nil {
		t.Fatalf("Upload-Expires invalid: %v", err)
	}
	if got := w.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Fatalf("Tus-Resumable = %q", got)
	}
	// 错误 offset：409。
	if w := tusPatch(env.router, id, 0, "lo", "application/offset+octet-stream"); w.Code != http.StatusConflict {
		t.Fatalf("stale offset: status = %d, want 409", w.Code)
	}
	// 错误 Content-Type：415。
	if w := tusPatch(env.router, id, 3, "lo", "application/octet-stream"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type: status = %d, want 415", w.Code)
	}
	// 越界：body 超过剩余容量 → 400 且 offset 不变。
	if w := tusPatch(env.router, id, 3, "lo!!", "application/offset+octet-stream"); w.Code != http.StatusBadRequest {
		t.Fatalf("exceeds remaining: status = %d, want 400", w.Code)
	}
	v, _ := env.store.Get(uuid.MustParse(id))
	if v.Offset != 3 {
		t.Fatalf("offset = %d, want unchanged 3", v.Offset)
	}
	// 非法 Upload-Offset 头：400。
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/tus/files/"+id, strings.NewReader("lo"))
	req.Header.Set("Upload-Offset", "x")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid offset header: status = %d, want 400", rec.Code)
	}
	// 继续补完剩余部分。
	if w := tusPatch(env.router, id, 3, "lo", "application/offset+octet-stream"); w.Code != http.StatusNoContent {
		t.Fatalf("final chunk: status = %d", w.Code)
	}
}

func TestTusPatchExpired(t *testing.T) {
	env := newTusTestEnv(t)
	id := mustCreateTUS(t, env, 3, env.tusMeta("filename "+b64("a.txt")))
	v, _ := env.store.Get(uuid.MustParse(id))
	v.ExpiresAt = time.Now().Add(-time.Minute)
	if err := env.store.Update(v); err != nil {
		t.Fatal(err)
	}
	w := tusPatch(env.router, id, 0, "abc", "application/offset+octet-stream")
	if w.Code != http.StatusGone {
		t.Fatalf("expired PATCH: status = %d, want 410", w.Code)
	}
}

// --- 自动完成 ---

func TestTusAutoComplete(t *testing.T) {
	env := newTusTestEnv(t)
	data := "hello tus"
	sum := sha256.Sum256([]byte(data))
	meta := env.tusMeta("filename "+b64("a.txt"), "sha256 "+b64(hex.EncodeToString(sum[:])))
	id := mustCreateTUS(t, env, int64(len(data)), meta)
	if w := tusPatch(env.router, id, 0, data, "application/offset+octet-stream"); w.Code != http.StatusNoContent {
		t.Fatalf("patch full: status = %d, body = %s", w.Code, w.Body.String())
	}
	uid := uuid.MustParse(id)
	v := waitUploadStatus(t, env.store, uid, upload.StatusAvailable)
	if v.CompletedAt == nil {
		t.Fatal("CompletedAt not set")
	}
	if v.StorageKey != fmt.Sprintf("objects/%s/%s", v.UserID, v.ID) {
		t.Fatalf("StorageKey = %q", v.StorageKey)
	}
	// 完成状态可通过现有 GET /api/v1/uploads/:id 查询。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/uploads/"+id, nil)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"available"`) {
		t.Fatalf("get upload: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTusPatchTerminalGone(t *testing.T) {
	env := newTusTestEnv(t)
	id := mustCreateTUS(t, env, 3, env.tusMeta("filename "+b64("a.txt")))
	if w := tusPatch(env.router, id, 0, "abc", "application/offset+octet-stream"); w.Code != http.StatusNoContent {
		t.Fatalf("patch: status = %d", w.Code)
	}
	waitUploadStatus(t, env.store, uuid.MustParse(id), upload.StatusAvailable)
	if w := tusPatch(env.router, id, 3, "", "application/offset+octet-stream"); w.Code != http.StatusGone {
		t.Fatalf("terminal PATCH: status = %d, want 410", w.Code)
	}
	// HEAD 仍可查询终态会话的 offset/length。
	if w := tusHead(env.router, id); w.Code != http.StatusOK || w.Header().Get("Upload-Offset") != "3" {
		t.Fatalf("terminal HEAD: status = %d, offset = %q", w.Code, w.Header().Get("Upload-Offset"))
	}
}

// --- OPTIONS 与路由注册 ---

func TestTusOptions(t *testing.T) {
	env := newTusTestEnv(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/tus/files", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d", w.Code)
	}
	for header, want := range map[string]string{
		"Tus-Resumable": "1.0.0",
		"Tus-Version":   "1.0.0",
		"Tus-Extension": "creation,expiration",
		"Tus-Max-Size":  strconv.Itoa(tusTestMaxSize),
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// 完整 Register 冒烟：tus 路由组与现有 /api/v1 树不冲突（冲突会 panic）。
func TestRegisterTusRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := upload.NewService(upload.NewMemoryStore(), newMemStorage(), time.Hour, tusTestMaxSize, false, nil, nil)
	handler := NewHandler(nil, nil, nil, nil, nil, svc, nil, false, "", 0)
	router := gin.New()
	handler.Register(router, "0123456789abcdef0123456789abcdef", 120, 10, 60)
	for _, route := range []struct {
		method, path string
	}{
		{http.MethodOptions, "/api/v1/tus/files"},
		{http.MethodPost, "/api/v1/tus/files"},
		{http.MethodHead, "/api/v1/tus/files/:id"},
		{http.MethodPatch, "/api/v1/tus/files/:id"},
		// 原 /uploads 自定义 API 必须保留。
		{http.MethodPost, "/api/v1/uploads"},
		{http.MethodPatch, "/api/v1/uploads/:id"},
		{http.MethodPost, "/api/v1/uploads/:id/complete"},
		{http.MethodGet, "/api/v1/uploads/:id"},
	} {
		found := false
		for _, r := range router.Routes() {
			if r.Method == route.method && r.Path == route.path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("route %s %s not registered", route.method, route.path)
		}
	}
}
