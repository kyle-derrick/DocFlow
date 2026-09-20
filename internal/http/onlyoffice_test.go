package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/onlyoffice"
	"github.com/docflow/docflow/internal/upload"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const onlyOfficeTestSecret = "onlyoffice-test-secret-0123456789abcdef"

// ooFileStore 是 onlyoffice.FileStore 的最小内存实现（供端点级测试）。
type ooFileStore struct {
	file    files.File
	version files.FileVersion
	blob    files.ObjectBlob
}

func (s *ooFileStore) Get(user, id uuid.UUID) (files.File, error) {
	if id == s.file.ID && s.file.OwnerID == user {
		return s.file, nil
	}
	return files.File{}, files.ErrNotFound
}

func (s *ooFileStore) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := s.Get(owner, fileID); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	return s.version, s.blob, nil
}

func (s *ooFileStore) GetFileByID(id uuid.UUID) (files.File, error) {
	if id == s.file.ID {
		return s.file, nil
	}
	return files.File{}, files.ErrNotFound
}

func (s *ooFileStore) GetVersionBlob(fileID, versionID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if fileID == s.file.ID && versionID == s.version.ID {
		return s.version, s.blob, nil
	}
	return files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
}

func (s *ooFileStore) AddVersion(files.File, string, string, int64, string, uuid.UUID) (files.FileVersion, bool, error) {
	return files.FileVersion{}, false, errors.New("not used in http tests")
}

// ooMemStorage 是 upload.Storage 的最小内存实现。
type ooMemStorage struct{ data map[string][]byte }

func newOOMemStorage() *ooMemStorage { return &ooMemStorage{data: map[string][]byte{}} }

func (s *ooMemStorage) Put(key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.data[key] = b
	return nil
}

func (s *ooMemStorage) Append(key string, r io.Reader) (int64, error) { return 0, nil }

type ooReadCloser struct {
	*bytes.Reader
}

func (ooReadCloser) Close() error { return nil }

func (s *ooMemStorage) Read(key string) (io.ReadCloser, error) {
	b, ok := s.data[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return ooReadCloser{bytes.NewReader(b)}, nil
}

func (s *ooMemStorage) Delete(key string) error { return nil }

func newOnlyOfficeTestHandler(store onlyoffice.FileStore, storage upload.Storage) *Handler {
	h := NewHandler(nil, nil, nil, nil, nil, nil, storage, false, "", 0)
	svc := onlyoffice.New(onlyoffice.Config{
		ServerURL:    "http://onlyoffice:80",
		DownloadBase: "http://backend:8080",
		JWTSecret:    onlyOfficeTestSecret,
	}, store, storage, func(uuid.UUID) (string, error) { return "alice", nil }, nil)
	h.SetOnlyOffice(svc, 1_000_000)
	return h
}

// 未启用（未注入服务）时三个 onlyoffice 端点均不注册（404）；
// config 探测端点恒注册：禁用时 enabled=false 且不暴露 server_url。
func TestOnlyOfficeRoutesDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiSecret = "0123456789abcdef0123456789abcdef"
	handler := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	router := gin.New()
	handler.Register(router, apiSecret, 120, 10, 60)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/onlyoffice/session"},
		{http.MethodGet, "/api/v1/onlyoffice/download/" + uuid.NewString()},
		{http.MethodPost, "/api/v1/onlyoffice/callback"},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(route.method, route.path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s disabled: status = %d, want 404", route.method, route.path, w.Code)
		}
	}

	// config：无 Bearer → 401（认证组）；合法 Bearer → 200 {enabled:false, server_url:null}。
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/onlyoffice/config", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("config without bearer: status = %d, want 401", w.Code)
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": uuid.NewString(), "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(apiSecret))
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onlyoffice/config", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("config disabled: status = %d, body = %s", w.Code, w.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("config body: %v", err)
	}
	if cfg["enabled"] != false || cfg["server_url"] != nil {
		t.Fatalf("config disabled: enabled = %v, server_url = %v", cfg["enabled"], cfg["server_url"])
	}
}

// config 探测端点启用态：返回 enabled=true 与 DocumentServer 基地址。
// 未配置 PublicURL（回退态）：server_url 返回内网 ONLYOFFICE_SERVER_URL。
func TestOnlyOfficeConfigEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiSecret = "0123456789abcdef0123456789abcdef"
	handler := newOnlyOfficeTestHandler(&ooFileStore{}, newOOMemStorage())
	router := gin.New()
	handler.Register(router, apiSecret, 120, 10, 60)

	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": uuid.NewString(), "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(apiSecret))
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onlyoffice/config", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("config enabled: status = %d, body = %s", w.Code, w.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("config body: %v", err)
	}
	if cfg["enabled"] != true || cfg["server_url"] != "http://onlyoffice:80" {
		t.Fatalf("config enabled = %v, server_url = %v", cfg["enabled"], cfg["server_url"])
	}
}

// config 探测端点 public 优先态：配置 PublicURL 时 server_url 返回浏览器可达
// 地址而非内网 ONLYOFFICE_SERVER_URL（SSRF 校验基准不受影响——仍以 ServerURL
// 为准，见 onlyoffice 包 TestURLAllowedOrigins）。
func TestOnlyOfficeConfigPublicURLPrecedence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiSecret = "0123456789abcdef0123456789abcdef"
	h := NewHandler(nil, nil, nil, nil, nil, nil, newOOMemStorage(), false, "", 0)
	svc := onlyoffice.New(onlyoffice.Config{
		ServerURL: "http://onlyoffice:80",
		// 末尾斜杠应被归一化去掉。
		PublicURL: "https://example.com/onlyoffice/",
		JWTSecret: onlyOfficeTestSecret,
	}, &ooFileStore{}, newOOMemStorage(), func(uuid.UUID) (string, error) { return "alice", nil }, nil)
	h.SetOnlyOffice(svc, 1_000_000)
	router := gin.New()
	h.Register(router, apiSecret, 120, 10, 60)

	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": uuid.NewString(), "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(apiSecret))
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onlyoffice/config", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("config public: status = %d, body = %s", w.Code, w.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("config body: %v", err)
	}
	if cfg["enabled"] != true || cfg["server_url"] != "https://example.com/onlyoffice" {
		t.Fatalf("config public = %v, server_url = %v", cfg["enabled"], cfg["server_url"])
	}
}

// 启用后 session（Bearer）与 callback（无 Bearer）端点行为。
func TestOnlyOfficeSessionAndCallbackEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const apiSecret = "0123456789abcdef0123456789abcdef"
	owner := uuid.New()
	store := &ooFileStore{
		file:    files.File{ID: uuid.New(), Name: "PRD.docx", OwnerID: owner, Type: "file"},
		version: files.FileVersion{ID: uuid.New(), Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/x/1", Size: 11, MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Status: files.BlobStatusAvailable},
	}
	store.version.FileID = store.file.ID
	storage := newOOMemStorage()
	storage.data["objects/x/1"] = []byte("hello docx!")
	handler := newOnlyOfficeTestHandler(store, storage)
	router := gin.New()
	handler.Register(router, apiSecret, 120, 10, 60)

	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": owner.String(), "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(apiSecret))
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}

	// session：200，返回含 token 的 DocsAPI 配置，document.url 指向签名下载。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/onlyoffice/session", strings.NewReader(`{"file_id":"`+store.file.ID.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session: status = %d, body = %s", w.Code, w.Body.String())
	}
	var config map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &config); err != nil {
		t.Fatalf("session body: %v", err)
	}
	if config["token"] == nil || config["documentType"] != "word" {
		t.Fatalf("session config = %v", config)
	}
	document := config["document"].(map[string]any)
	// key 为 DS 8.x 白名单字符集格式：<fileID 32hex>-<versionID 32hex>（无冒号）。
	wantKey := strings.ReplaceAll(store.file.ID.String(), "-", "") + "-" + strings.ReplaceAll(store.version.ID.String(), "-", "")
	if document["key"] != wantKey {
		t.Fatalf("document.key = %v, want %s", document["key"], wantKey)
	}
	docURL := document["url"].(string)
	if !strings.HasPrefix(docURL, "http://backend:8080/api/v1/onlyoffice/download/"+store.file.ID.String()+"/PRD.docx?") {
		t.Fatalf("document.url = %q", docURL)
	}
	// 未接线写授权器（SetWriteAuthorizer）：session 保守降级只读会话。
	editorConfig := config["editorConfig"].(map[string]any)
	if editorConfig["mode"] != "view" {
		t.Fatalf("mode without write authorizer = %v, want view (fail closed)", editorConfig["mode"])
	}
	if perms := document["permissions"].(map[string]any); perms["edit"] != false {
		t.Fatalf("permissions.edit without write authorizer = %v, want false", perms["edit"])
	}

	// 无 Bearer 访问 session → 401。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/onlyoffice/session", strings.NewReader(`{"file_id":"`+store.file.ID.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("session without bearer: status = %d, want 401", w.Code)
	}

	// download：从 document.url 取 token/v，无 Bearer 成功回源（attachment）。
	u := docURL[strings.Index(docURL, "/api/v1/onlyoffice/download/"):]
	req = httptest.NewRequest(http.MethodGet, u, nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download: status = %d, body = %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello docx!" {
		t.Fatalf("download body = %q", w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	// 篡改 token → 401。
	req = httptest.NewRequest(http.MethodGet, u+"X", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered download: status = %d, want 401", w.Code)
	}

	// callback：无 token → 200 {"error":1}；合法 status=4 → 200 {"error":0}。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/onlyoffice/callback",
		strings.NewReader(`{"key":"`+store.file.ID.String()+`:`+store.version.ID.String()+`","status":4}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"error":1`) {
		t.Fatalf("callback without token: status = %d body = %s", w.Code, w.Body.String())
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"key": store.file.ID.String() + ":" + store.version.ID.String(), "status": 4,
	}).SignedString([]byte(onlyOfficeTestSecret))
	if err != nil {
		t.Fatalf("sign callback token: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/v1/onlyoffice/callback",
		strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"error":0`) {
		t.Fatalf("valid callback: status = %d body = %s", w.Code, w.Body.String())
	}
}

// onlyofficeError 映射矩阵。
func TestOnlyOfficeErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid token", onlyoffice.ErrInvalidToken, http.StatusUnauthorized},
		{"invalid key", onlyoffice.ErrInvalidKey, http.StatusBadRequest},
		{"invalid target", files.ErrInvalidTarget, http.StatusBadRequest},
		{"url not allowed", onlyoffice.ErrURLNotAllowed, http.StatusForbidden},
		{"blob unavailable", files.ErrBlobUnavailable, http.StatusForbidden},
		{"forbidden", files.ErrForbidden, http.StatusForbidden},
		{"not found", files.ErrNotFound, http.StatusNotFound},
		{"no version", files.ErrNoVersion, http.StatusConflict},
		{"too large", onlyoffice.ErrDownloadTooLarge, http.StatusRequestEntityTooLarge},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		if !onlyofficeError(c, tc.err) {
			t.Fatalf("%s: expected error handled", tc.name)
		}
		if w.Code != tc.want {
			t.Fatalf("%s: status = %d, want %d", tc.name, w.Code, tc.want)
		}
		if w.Body.Len() == 0 {
			t.Fatalf("%s: body must not be empty", tc.name)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if onlyofficeError(c, nil) {
		t.Fatal("nil error must not be handled")
	}
}
