package onlyoffice

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ---- 内存 fake（无 DB/网络）----

const testJWTSecret = "0123456789abcdef0123456789abcdef"

const testServerURL = "http://onlyoffice:80"

// fakeFileStore 是 FileStore 的内存实现：种子数据 + AddVersion 计数。
type fakeFileStore struct {
	mu       sync.Mutex
	files    map[uuid.UUID]files.File
	versions map[uuid.UUID]files.FileVersion // versionID → 版本
	blobs    map[uuid.UUID]files.ObjectBlob  // blobID → blob
	addCalls int
	addErr   error
	// readers 额外读授权（模拟团队文件「任意在册成员可读」；owner 始终可读）。
	readers map[uuid.UUID]bool
}

func newFakeFileStore() *fakeFileStore {
	return &fakeFileStore{
		files:    make(map[uuid.UUID]files.File),
		versions: make(map[uuid.UUID]files.FileVersion),
		blobs:    make(map[uuid.UUID]files.ObjectBlob),
	}
}

// seedFile 建一个个人文件（owner）与一版 available 内容，返回文件与版本。
func (f *fakeFileStore) seedFile(owner uuid.UUID, name, content string) (files.File, files.FileVersion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	blob := files.ObjectBlob{
		ID: uuid.New(), SHA256: sumHex(content), StorageKey: "objects/" + owner.String() + "/" + uuid.NewString(),
		Size: int64(len(content)), MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		RefCount: 1, Status: files.BlobStatusAvailable,
	}
	file := files.File{ID: uuid.New(), Name: name, OwnerID: owner, Type: "file"}
	version := files.FileVersion{ID: uuid.New(), FileID: file.ID, Version: 1, ObjectBlobID: blob.ID, ContentSHA256: blob.SHA256, Size: blob.Size, UserID: owner}
	file.CurrentVersionID = &version.ID
	f.files[file.ID] = file
	f.versions[version.ID] = version
	f.blobs[blob.ID] = blob
	return file, version
}

func (f *fakeFileStore) Get(user, id uuid.UUID) (files.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[id]
	if !ok || file.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	if file.OwnerID != user && !f.readers[user] {
		// 与 authorizeFileAccess 同规则：无读授权的访问统一 404。
		return files.File{}, files.ErrNotFound
	}
	return file, nil
}

// allowReader 授予 user 读权限（模拟团队成员；测试辅助）。
func (f *fakeFileStore) allowReader(user uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readers == nil {
		f.readers = make(map[uuid.UUID]bool)
	}
	f.readers[user] = true
}

func (f *fakeFileStore) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	file, err := f.Get(owner, fileID)
	if err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	if file.CurrentVersionID == nil {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
	}
	return f.versionBlob(*file.CurrentVersionID)
}

func (f *fakeFileStore) GetFileByID(id uuid.UUID) (files.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[id]
	if !ok || file.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return file, nil
}

func (f *fakeFileStore) GetVersionBlob(fileID, versionID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.GetFileByID(fileID); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	return f.versionBlob(versionID)
}

func (f *fakeFileStore) versionBlob(versionID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	version, ok := f.versions[versionID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	blob, ok := f.blobs[version.ObjectBlobID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNotFound
	}
	return version, blob, nil
}

func (f *fakeFileStore) AddVersion(file files.File, storageKey, sha256 string, size int64, mimeType string, userID uuid.UUID) (files.FileVersion, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return files.FileVersion{}, false, f.addErr
	}
	f.addCalls++
	// 同 sha256 的 available blob 去重复用（模拟 addVersionLogic 语义）。
	for _, b := range f.blobs {
		if b.SHA256 == sha256 && b.Status == files.BlobStatusAvailable {
			b.RefCount++
			f.blobs[b.ID] = b
			version := files.FileVersion{ID: uuid.New(), Version: f.nextVersionLocked(file.ID), ObjectBlobID: b.ID, ContentSHA256: sha256, Size: size, UserID: userID}
			f.versions[version.ID] = version
			f.setCurrentLocked(file.ID, version.ID)
			return version, false, nil
		}
	}
	blob := files.ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: files.BlobStatusAvailable}
	f.blobs[blob.ID] = blob
	version := files.FileVersion{ID: uuid.New(), Version: f.nextVersionLocked(file.ID), ObjectBlobID: blob.ID, ContentSHA256: sha256, Size: size, UserID: userID}
	f.versions[version.ID] = version
	f.setCurrentLocked(file.ID, version.ID)
	return version, true, nil
}

func (f *fakeFileStore) nextVersionLocked(fileID uuid.UUID) int {
	next := 1
	for _, v := range f.versions {
		if v.FileID == fileID && v.Version >= next {
			next = v.Version + 1
		}
	}
	return next
}

func (f *fakeFileStore) setCurrentLocked(fileID, versionID uuid.UUID) {
	if file, ok := f.files[fileID]; ok {
		file.CurrentVersionID = &versionID
		f.files[fileID] = file
	}
}

func (f *fakeFileStore) addVersionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addCalls
}

// memStorage 是 upload.Storage 的内存实现。
type memStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStorage() *memStorage { return &memStorage{objects: make(map[string][]byte)} }

func (s *memStorage) Put(key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = b
	return nil
}

func (s *memStorage) Append(key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append(s.objects[key], b...)
	return int64(len(b)), nil
}

func (s *memStorage) Read(key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	if !ok {
		return nil, errors.New("object not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memStorage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *memStorage) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	return out
}

// fakeFetch 记录被请求的 URL，返回预置内容（无网络）。
type fakeFetch struct {
	mu          sync.Mutex
	urls        []string
	content     []byte
	contentType string
	err         error
}

func (f *fakeFetch) fetch(rawURL string) (io.ReadCloser, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, rawURL)
	if f.err != nil {
		return nil, "", f.err
	}
	if f.content == nil {
		f.content = []byte{}
	}
	return io.NopCloser(bytes.NewReader(f.content)), f.contentType, nil
}

// fakeRecorder 收集审计条目。
type fakeRecorder struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (r *fakeRecorder) Record(e audit.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *fakeRecorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.Action)
	}
	return out
}

func sumHex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// newTestService 构造带内存依赖的服务；now 为 nil 时用真实时钟。
// 默认注入「全放行」写授权器（模拟 main 生产接线）；需要验证 fail closed
// 或拒绝路径的测试可将 s.authorizeWrite 置 nil 或 SetWriteAuthorizer 覆盖。
func newTestService(store FileStore, st upload.Storage, fetcher *fakeFetch, recorder *fakeRecorder, now func() time.Time) *Service {
	s := New(Config{
		ServerURL:        testServerURL,
		DownloadBase:     "http://backend:8080",
		JWTSecret:        testJWTSecret,
		TokenTTL:         5 * time.Minute,
		DownloadMaxBytes: 1 << 20,
	}, store, st, func(uuid.UUID) (string, error) { return "alice", nil }, recorder)
	s.SetWriteAuthorizer(func(uuid.UUID, uuid.UUID) error { return nil })
	if fetcher != nil {
		s.fetch = fetcher.fetch
	}
	if now != nil {
		s.now = now
	}
	return s
}

// ---- 编辑配置与 token ----

// 编辑配置生成：key 绑定版本、URL 带签名 token、lang=zh，token 为
// document+editorConfig 整体签名的 HS256 JWT（exp=iat+5min）。
func TestEditConfigTokenGenerationAndExpiry(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "PRD.docx", "hello world")
	base := time.Now()
	s := newTestService(store, newMemStorage(), nil, &fakeRecorder{}, func() time.Time { return base })

	config, err := s.NewEditConfig(owner, file.ID)
	if err != nil {
		t.Fatalf("NewEditConfig: %v", err)
	}
	document := config["document"].(map[string]any)
	if got := document["key"]; got != documentKey(file.ID, version.ID) {
		t.Fatalf("document.key = %v, want %s", got, documentKey(file.ID, version.ID))
	}
	editor := config["editorConfig"].(map[string]any)
	if editor["lang"] != "zh" {
		t.Fatalf("lang = %v, want zh", editor["lang"])
	}
	if user := editor["user"].(map[string]any); user["name"] != "alice" || user["id"] != owner.String() {
		t.Fatalf("editorConfig.user = %v", user)
	}
	if !strings.HasPrefix(document["url"].(string), "http://backend:8080/api/v1/onlyoffice/download/"+file.ID.String()+"/PRD.docx?v=") {
		t.Fatalf("document.url = %v", document["url"])
	}

	// token 校验：payload 含 document+editorConfig，exp=iat+300s。
	token := config["token"].(string)
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
		return []byte(testJWTSecret), nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("config token invalid: %v", err)
	}
	if doc, ok := claims["document"].(map[string]any); !ok || doc["key"] != documentKey(file.ID, version.ID) {
		t.Fatalf("token.document = %v", claims["document"])
	}
	if _, ok := claims["editorConfig"].(map[string]any); !ok {
		t.Fatalf("token missing editorConfig")
	}
	iat, _ := claimNumber(claims["iat"])
	exp, _ := claimNumber(claims["exp"])
	if exp-iat != int64((5 * time.Minute).Seconds()) {
		t.Fatalf("token ttl = %d, want 300", exp-iat)
	}

	// 过期：时钟推进 6 分钟后解析失败。
	if _, err := jwt.ParseWithClaims(token, jwt.MapClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testJWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return base.Add(6 * time.Minute) })); err == nil {
		t.Fatal("expired config token must be rejected")
	}
}

// 无读权限（非 owner）与不可用 blob 的编辑配置生成被拒绝。
func TestEditConfigPermissionAndBlobUnavailable(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, _ := store.seedFile(owner, "a.docx", "x")
	s := newTestService(store, newMemStorage(), nil, &fakeRecorder{}, nil)
	if _, err := s.NewEditConfig(uuid.New(), file.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("non-owner: err = %v, want files.ErrNotFound", err)
	}
	// 不可用 blob（隔离态）拒绝。
	store.mu.Lock()
	for id, blob := range store.blobs {
		blob.Status = files.BlobStatusQuarantined
		store.blobs[id] = blob
	}
	store.mu.Unlock()
	if _, err := s.NewEditConfig(owner, file.ID); !errors.Is(err, files.ErrBlobUnavailable) {
		t.Fatalf("quarantined blob: err = %v, want files.ErrBlobUnavailable", err)
	}
}

// TestEditConfigRejectsUnsupportedFileType 验证 OnlyOffice 不支持的扩展
// （如 .dfrt 富文本、.drawio）在生成配置前被 ErrInvalidFileType 拦截——
// 此前会生成 fileType=dfrt 的配置，DocumentServer 报晦涩的
// "document.fileType parameter is invalid"；Office/pdf/txt/无扩展名放行。
func TestEditConfigRejectsUnsupportedFileType(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	dfrt, _ := store.seedFile(owner, "文档.dfrt", "{}")
	drawio, _ := store.seedFile(owner, "流程图.drawio", "<mxfile/>")
	bare, _ := store.seedFile(owner, "README", "hi")
	s := newTestService(store, newMemStorage(), nil, &fakeRecorder{}, nil)

	if _, err := s.NewEditConfig(owner, dfrt.ID); !errors.Is(err, ErrInvalidFileType) {
		t.Fatalf(".dfrt: err = %v, want ErrInvalidFileType", err)
	}
	if _, err := s.NewEditConfig(owner, drawio.ID); !errors.Is(err, ErrInvalidFileType) {
		t.Fatalf(".drawio: err = %v, want ErrInvalidFileType", err)
	}
	// 无扩展名（documentFileMeta 回退 txt 口径）与白名单类型正常生成。
	if _, err := s.NewEditConfig(owner, bare.ID); err != nil {
		t.Fatalf("无扩展名应回退 txt 放行: %v", err)
	}
	if _, err := s.NewShareViewConfig(files.File{ID: dfrt.ID, Name: "文档.dfrt", Type: "file"}, files.FileVersion{}, SessionOptions{}); !errors.Is(err, ErrInvalidFileType) {
		t.Fatalf("分享查看 .dfrt: err = %v, want ErrInvalidFileType", err)
	}
}

// 编辑配置按写权限降级：可写 mode=edit/permissions.edit=true；
// 只读（授权器拒绝）与未接线授权器（fail closed）均 mode=view/edit=false；
// 编辑配置 token 携带 aud=onlyoffice-config（用途隔离）。
func TestEditConfigWritePermissionDowngrade(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	viewer := uuid.New()
	file, _ := store.seedFile(owner, "a.docx", "x")
	s := newTestService(store, newMemStorage(), nil, &fakeRecorder{}, nil)
	s.SetWriteAuthorizer(func(user, fileID uuid.UUID) error {
		if user != owner {
			return files.ErrForbidden
		}
		return nil
	})

	assertMode := func(cfg map[string]any, wantMode string, wantEdit bool) {
		t.Helper()
		editor := cfg["editorConfig"].(map[string]any)
		if editor["mode"] != wantMode {
			t.Fatalf("mode = %v, want %v", editor["mode"], wantMode)
		}
		perms := cfg["document"].(map[string]any)["permissions"].(map[string]any)
		if perms["edit"] != wantEdit {
			t.Fatalf("permissions.edit = %v, want %v", perms["edit"], wantEdit)
		}
	}

	// viewer（有读权限、无写权限）→ 只读会话。
	store.allowReader(viewer)
	cfg, err := s.NewEditConfig(viewer, file.ID)
	if err != nil {
		t.Fatalf("viewer config: %v", err)
	}
	assertMode(cfg, "view", false)

	// owner 可写 → edit。
	cfg, err = s.NewEditConfig(owner, file.ID)
	if err != nil {
		t.Fatalf("owner config: %v", err)
	}
	assertMode(cfg, "edit", true)
	// 编辑配置 token 声明 aud=onlyoffice-config。
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(cfg["token"].(string), claims, func(*jwt.Token) (any, error) {
		return []byte(testJWTSecret), nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("config token invalid: %v", err)
	}
	if got := claimString(claims["aud"]); got != configAudience {
		t.Fatalf("config token aud = %q, want %q", got, configAudience)
	}

	// 未接线授权器 → 保守 view（fail closed）。
	bare := newTestService(store, newMemStorage(), nil, &fakeRecorder{}, nil)
	bare.authorizeWrite = nil
	cfg, err = bare.NewEditConfig(owner, file.ID)
	if err != nil {
		t.Fatalf("bare config: %v", err)
	}
	assertMode(cfg, "view", false)
}

// ---- 签名下载校验矩阵 ----

// 合法 token 放行；篡改/过期/用途不符/绑定参数不符/隔离 blob 拒绝。
func TestResolveDownloadTokenMatrix(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "content-v1")
	base := time.Now()
	s := newTestService(store, newMemStorage(), nil, &fakeRecorder{}, func() time.Time { return base })

	token, err := s.signDownloadToken(file.ID, version.ID)
	if err != nil {
		t.Fatalf("signDownloadToken: %v", err)
	}

	// 合法：返回文件/版本/blob。
	f, _, blob, err := s.ResolveDownload(file.ID, version.ID.String(), token)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if f.ID != file.ID || blob.SHA256 != sumHex("content-v1") {
		t.Fatalf("unexpected entities: file=%v blob=%v", f.ID, blob.SHA256)
	}

	// 篡改（追加字符改变签名段）。
	if _, _, _, err := s.ResolveDownload(file.ID, version.ID.String(), token+"Q"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered: err = %v, want ErrInvalidToken", err)
	}
	// 错误密钥。
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": file.ID.String(), "v": version.ID.String(), "aud": downloadAudience,
		"iat": base.Unix(), "exp": base.Add(5 * time.Minute).Unix(),
	}).SignedString([]byte("wrong-secret-wrong-secret-wrong!!"))
	if _, _, _, err := s.ResolveDownload(file.ID, version.ID.String(), forged); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong key: err = %v, want ErrInvalidToken", err)
	}
	// 过期：时钟推进至下载 token 默认有效期之后。
	s.now = func() time.Time { return base.Add(DefaultDownloadTokenTTL + time.Minute) }
	if _, _, _, err := s.ResolveDownload(file.ID, version.ID.String(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired: err = %v, want ErrInvalidToken", err)
	}
	s.now = func() time.Time { return base }
	// 用途不符（aud 非 onlyoffice-download）。
	other, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": file.ID.String(), "v": version.ID.String(), "aud": "something-else",
		"iat": base.Unix(), "exp": base.Add(5 * time.Minute).Unix(),
	}).SignedString([]byte(testJWTSecret))
	if _, _, _, err := s.ResolveDownload(file.ID, version.ID.String(), other); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong aud: err = %v, want ErrInvalidToken", err)
	}
	// 绑定参数不符：token 用在别的 file / 别的版本。
	otherFile, otherVersion := store.seedFile(uuid.New(), "b.docx", "y")
	if _, _, _, err := s.ResolveDownload(otherFile.ID, version.ID.String(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("sub mismatch: err = %v, want ErrInvalidToken", err)
	}
	if _, _, _, err := s.ResolveDownload(file.ID, otherVersion.ID.String(), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("v mismatch: err = %v, want ErrInvalidToken", err)
	}
	// 缺参。
	if _, _, _, err := s.ResolveDownload(file.ID, "", token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("missing v: err = %v, want ErrInvalidToken", err)
	}
	if _, _, _, err := s.ResolveDownload(file.ID, version.ID.String(), ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("missing token: err = %v, want ErrInvalidToken", err)
	}

	// 隔离 blob 拒绝下载。
	store.mu.Lock()
	for id, blob := range store.blobs {
		blob.Status = files.BlobStatusQuarantined
		store.blobs[id] = blob
	}
	store.mu.Unlock()
	if _, _, _, err := s.ResolveDownload(file.ID, version.ID.String(), token); !errors.Is(err, files.ErrBlobUnavailable) {
		t.Fatalf("quarantined: err = %v, want files.ErrBlobUnavailable", err)
	}
}

// signCallbackToken 以指定密钥对回调体签名（模拟 DocumentServer）。
func signCallbackToken(t *testing.T, secret string, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal callback body: %v", err)
	}
	claims := jwt.MapClaims{}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// 回调端点辅助：组装 body JSON（token 以指定密钥对除 token 外字段签名）。
func callbackBody(t *testing.T, secret string, fields map[string]any, withToken bool) []byte {
	t.Helper()
	body := make(map[string]any, len(fields))
	for k, v := range fields {
		body[k] = v
	}
	if withToken {
		body["token"] = signCallbackToken(t, secret, fields)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return raw
}

// 防 SSRF 同源判定矩阵：仅与 ONLYOFFICE_SERVER_URL 同 scheme/host/port 放行。
func TestURLAllowedOrigins(t *testing.T) {
	s := newTestService(nil, nil, nil, nil, nil)
	allowed := []string{
		"http://onlyoffice:80/cache/files/data.docx",
		"http://onlyoffice/cache/files/data.docx", // 默认端口归一化
	}
	for _, u := range allowed {
		if !s.urlAllowed(u) {
			t.Errorf("urlAllowed(%q) = false, want true", u)
		}
	}
	denied := []string{
		"http://evil.example.com/cache/files/data.docx", // 外部域名
		"http://onlyoffice.evil.com/x",                  // 前缀伪装
		"https://onlyoffice/x",                          // scheme 不同（端口归一化后 443 != 80）
		"http://onlyoffice:8080/x",                      // 端口不同
		"http://user:pass@onlyoffice:80/x",              // 带凭据
		"file:///etc/passwd",                            // 非 http(s)
		"",                                              // 空
		"onlyoffice:80/x",                               // 非绝对 URL
	}
	for _, u := range denied {
		if s.urlAllowed(u) {
			t.Errorf("urlAllowed(%q) = true, want false", u)
		}
	}
}

// TestPublicServerURLFor public URL 推导规则：未配置/本机回环（127.x、
// localhost）→ 按请求 scheme+host 拼 /onlyoffice；显式外网域名原样；
// host 为空回退 PublicServerURL()（PublicURL 优先，未配置回退 ServerURL）。
func TestPublicServerURLFor(t *testing.T) {
	newSvc := func(public string) *Service {
		return New(Config{ServerURL: testServerURL, PublicURL: public, JWTSecret: testJWTSecret}, nil, nil, nil, nil)
	}
	cases := []struct {
		name         string
		publicURL    string
		scheme, host string
		want         string
	}{
		{"未配置按 Host 推导 http", "", "http", "docflow.example.com", "http://docflow.example.com/onlyoffice"},
		{"未配置按 Host 推导 https", "", "https", "docflow.example.com:443", "https://docflow.example.com:443/onlyoffice"},
		{"显式外网域名原样", "https://office.example.com/onlyoffice", "http", "docflow.example.com", "https://office.example.com/onlyoffice"},
		{"显式 127.0.0.1 视为未配置", "http://127.0.0.1/onlyoffice", "https", "docflow.example.com", "https://docflow.example.com/onlyoffice"},
		{"显式 localhost 视为未配置", "http://localhost/onlyoffice", "http", "10.0.0.8:8080", "http://10.0.0.8:8080/onlyoffice"},
		{"非法 scheme 归一 http", "http://127.0.0.1/onlyoffice", "ftp", "h", "http://h/onlyoffice"},
		{"host 空回退显式 PublicURL", "https://office.example.com/onlyoffice", "http", "", "https://office.example.com/onlyoffice"},
		{"host 空且未配置回退 ServerURL", "", "http", "", testServerURL},
	}
	for _, tc := range cases {
		if got := newSvc(tc.publicURL).PublicServerURLFor(tc.scheme, tc.host); got != tc.want {
			t.Errorf("%s: PublicServerURLFor(%q, %q) = %q, want %q", tc.name, tc.scheme, tc.host, got, tc.want)
		}
	}
}
