package onlyoffice

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/google/uuid"
)

const sameOriginURL = "http://onlyoffice:80/cache/files/data.docx"

// 回调 JWT 校验矩阵：无 token / 错密钥 / 过期一律拒绝（{"error":1}）。
func TestCallbackJWTMatrix(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fields := map[string]any{"key": documentKey(file.ID, version.ID), "status": 2, "url": sameOriginURL}
	s := newTestService(store, newMemStorage(), &fakeFetch{}, &fakeRecorder{}, nil)

	// 无 token（body 与 Authorization 头均缺失）。
	if err := s.HandleCallback(callbackBody(t, testJWTSecret, fields, false), "", "127.0.0.1", "ds"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("no token: err = %v, want ErrInvalidToken", err)
	}
	// 错密钥。
	if err := s.HandleCallback(callbackBody(t, "wrong-secret-wrong-secret-wrong!!", fields, true), "", "127.0.0.1", "ds"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong key: err = %v, want ErrInvalidToken", err)
	}
	// 过期（exp 已过）。
	expired := signCallbackToken(t, testJWTSecret, map[string]any{
		"key": documentKey(file.ID, version.ID), "status": 4, "exp": time.Now().Add(-time.Minute).Unix(),
	})
	if err := s.HandleCallback([]byte(`{"token":"`+expired+`"}`), "", "127.0.0.1", "ds"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired: err = %v, want ErrInvalidToken", err)
	}
	// Authorization: Bearer 头（无 body.token）也可通过校验（status 4 空处理成功）。
	headerToken := signCallbackToken(t, testJWTSecret, map[string]any{
		"key": documentKey(file.ID, version.ID), "status": 4,
	})
	if err := s.HandleCallback([]byte(`{"key":"`+documentKey(file.ID, version.ID)+`","status":4}`), "Bearer "+headerToken, "127.0.0.1", "ds"); err != nil {
		t.Fatalf("bearer header: err = %v", err)
	}
	// 非 JSON body。
	if err := s.HandleCallback([]byte("not json"), "", "", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad body: err = %v, want ErrInvalidToken", err)
	}
}

// status=2/6 保存：下载 → 新版本（SHA-256、临时 key 转正）→ 审计；
// 同 (file_id, document.key, url) 的重复回调幂等，只建一版。
func TestCallbackSaveIdempotent(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	editor := uuid.New()
	fetcher := &fakeFetch{content: []byte("edited content v2"), contentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}
	recorder := &fakeRecorder{}
	storage := newMemStorage()
	s := newTestService(store, storage, fetcher, recorder, nil)

	key := documentKey(file.ID, version.ID)
	body := callbackBody(t, testJWTSecret, map[string]any{
		"key": key, "status": 2, "url": sameOriginURL, "users": []string{editor.String()},
	}, true)
	if err := s.HandleCallback(body, "", "10.0.0.9", "onlyoffice-ds"); err != nil {
		t.Fatalf("save callback: %v", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls = %d, want 1", got)
	}
	// 版本内容与哈希落库正确，归属编辑用户。
	current, blob, err := store.CurrentVersion(owner, file.ID)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if current.Version != 2 || current.UserID != editor {
		t.Fatalf("new version = {version:%d user:%v}, want {2, %v}", current.Version, current.UserID, editor)
	}
	if blob.SHA256 != sumHex("edited content v2") || blob.Size != int64(len("edited content v2")) {
		t.Fatalf("blob = {sha:%s size:%d}", blob.SHA256, blob.Size)
	}
	if got, _ := storage.Read(blob.StorageKey); got == nil {
		t.Fatal("final object missing in storage")
	}
	// 临时 key 已清理。
	for _, k := range storage.keys() {
		if strings.HasPrefix(k, "tmp/onlyoffice-") {
			t.Fatalf("temp key %q not cleaned", k)
		}
	}
	// 审计 onlyoffice.save 成功。
	saved := false
	for _, e := range recorder.entries {
		if e.Action == audit.ActionOnlyOfficeSave && e.Status == audit.StatusSuccess && e.ResourceID == file.ID.String() {
			saved = true
			if e.UserID == nil || *e.UserID != editor {
				t.Fatalf("audit user = %v, want editor", e.UserID)
			}
			if e.IP == nil || *e.IP != "10.0.0.9" {
				t.Fatalf("audit ip = %v", e.IP)
			}
		}
	}
	if !saved {
		t.Fatalf("audit actions = %v, want onlyoffice.save success", recorder.actions())
	}

	// 同 key 重复回调（DocumentServer 重试）：幂等记录冲突，返回成功且不重复建版本。
	if err := s.HandleCallback(body, "", "10.0.0.9", "onlyoffice-ds"); err != nil {
		t.Fatalf("duplicate callback: %v", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls after duplicate = %d, want 1", got)
	}

	// 同 key 换 url（DocumentServer 重试生成新缓存 url 场景）：幂等键含 url，
	// 各自抢占成功 → 各建一版。
	fetcher.content = []byte("edited content v3")
	bodyURL2 := callbackBody(t, testJWTSecret, map[string]any{
		"key": key, "status": 2, "url": sameOriginURL + "?retry=1", "users": []string{editor.String()},
	}, true)
	if err := s.HandleCallback(bodyURL2, "", "10.0.0.9", "onlyoffice-ds"); err != nil {
		t.Fatalf("callback with different url: %v", err)
	}
	if got := store.addVersionCount(); got != 2 {
		t.Fatalf("AddVersion calls after different url = %d, want 2", got)
	}

	// status=6（强制保存）新内容 → 再建一版。
	fetcher.content = []byte("forcesave content v4")
	body6 := callbackBody(t, testJWTSecret, map[string]any{
		"key": key, "status": 6, "url": sameOriginURL + "?t=2", "users": []string{editor.String()},
	}, true)
	if err := s.HandleCallback(body6, "", "10.0.0.9", "onlyoffice-ds"); err != nil {
		t.Fatalf("forcesave callback: %v", err)
	}
	if got := store.addVersionCount(); got != 3 {
		t.Fatalf("AddVersion calls after forcesave = %d, want 3", got)
	}
}

// status=4 仅清理：不下载、不建版本，写 onlyoffice.cleanup 审计。
func TestCallbackStatus4Cleanup(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fetcher := &fakeFetch{}
	recorder := &fakeRecorder{}
	s := newTestService(store, newMemStorage(), fetcher, recorder, nil)

	body := callbackBody(t, testJWTSecret, map[string]any{
		"key": documentKey(file.ID, version.ID), "status": 4,
	}, true)
	if err := s.HandleCallback(body, "", "10.0.0.9", "onlyoffice-ds"); err != nil {
		t.Fatalf("cleanup callback: %v", err)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
	if len(fetcher.urls) != 0 {
		t.Fatalf("fetch called on status 4: %v", fetcher.urls)
	}
	found := false
	for _, e := range recorder.entries {
		if e.Action == audit.ActionOnlyOfficeCleanup {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit actions = %v, want onlyoffice.cleanup", recorder.actions())
	}

	// 其他状态（1/3/7）空处理：成功且无副作用。
	for _, status := range []int{1, 3, 7} {
		body := callbackBody(t, testJWTSecret, map[string]any{
			"key": documentKey(file.ID, version.ID), "status": status,
		}, true)
		if err := s.HandleCallback(body, "", "", ""); err != nil {
			t.Fatalf("status %d: err = %v", status, err)
		}
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
}

// 回调 url 防 SSRF：外部域名/异端口拒绝且不发起下载；审计记录失败。
func TestCallbackSSRFBlocked(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fetcher := &fakeFetch{content: []byte("evil")}
	recorder := &fakeRecorder{}
	s := newTestService(store, newMemStorage(), fetcher, recorder, nil)

	for _, url := range []string{
		"http://evil.example.com/cache/files/data.docx",
		"http://onlyoffice:8080/x",
		"https://onlyoffice/x",
		"http://169.254.169.254/latest/meta-data",
	} {
		body := callbackBody(t, testJWTSecret, map[string]any{
			"key": documentKey(file.ID, version.ID), "status": 2, "url": url,
		}, true)
		err := s.HandleCallback(body, "", "10.0.0.9", "ds")
		if !errors.Is(err, ErrURLNotAllowed) {
			t.Fatalf("url %q: err = %v, want ErrURLNotAllowed", url, err)
		}
	}
	if len(fetcher.urls) != 0 {
		t.Fatalf("fetch must not be called, got %v", fetcher.urls)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
	found := false
	for _, e := range recorder.entries {
		if e.Action == audit.ActionOnlyOfficeSave && e.Status == audit.StatusFailure {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit actions = %v, want onlyoffice.save failure", recorder.actions())
	}
}

// blob 不可用（同 sha256 处于 quarantined，AddVersion 拒绝）时保存失败，
// 且冗余物理对象被清理。
func TestCallbackBlobUnavailableRejectsSave(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	content := "quarantined-duplicate"
	store.mu.Lock()
	quarantined := files.ObjectBlob{ID: uuid.New(), SHA256: sumHex(content), StorageKey: "objects/other", Size: int64(len(content)), RefCount: 0, Status: files.BlobStatusQuarantined}
	store.blobs[quarantined.ID] = quarantined
	store.mu.Unlock()
	store.addErr = files.ErrBlobUnavailable
	fetcher := &fakeFetch{content: []byte(content)}
	storage := newMemStorage()
	recorder := &fakeRecorder{}
	s := newTestService(store, storage, fetcher, recorder, nil)

	body := callbackBody(t, testJWTSecret, map[string]any{
		"key": documentKey(file.ID, version.ID), "status": 2, "url": sameOriginURL,
	}, true)
	err := s.HandleCallback(body, "", "10.0.0.9", "ds")
	if !errors.Is(err, files.ErrBlobUnavailable) {
		t.Fatalf("err = %v, want files.ErrBlobUnavailable", err)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
	// 冗余对象（tmp 与 final）均已清理。
	if keys := storage.keys(); len(keys) != 0 {
		t.Fatalf("orphan objects left: %v", keys)
	}
	// 失败后允许重试（处理失败已回滚幂等记录）：解除错误后同 key 回调成功。
	store.mu.Lock()
	store.addErr = nil
	store.mu.Unlock()
	if err := s.HandleCallback(body, "", "10.0.0.9", "ds"); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls after retry = %d, want 1", got)
	}
}

// 非法 document.key（解析不出 file_id）与不存在的文件拒绝保存；
// 空 key 在 claims 必填校验即拒绝（ErrInvalidToken）。
func TestCallbackInvalidKeyAndMissingFile(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	_, version := store.seedFile(owner, "a.docx", "v1")
	s := newTestService(store, newMemStorage(), &fakeFetch{}, &fakeRecorder{}, nil)

	// key 声明缺失（空串）：claims 必填校验拒绝。
	body := callbackBody(t, testJWTSecret, map[string]any{"key": "", "status": 2, "url": sameOriginURL}, true)
	if err := s.HandleCallback(body, "", "", ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("empty key: err = %v, want ErrInvalidToken", err)
	}
	// key 非法（解析不出 file_id）。
	body = callbackBody(t, testJWTSecret, map[string]any{"key": "not-a-uuid:xy", "status": 2, "url": sameOriginURL}, true)
	if err := s.HandleCallback(body, "", "", ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key: err = %v, want ErrInvalidKey", err)
	}
	missing := uuid.New()
	body = callbackBody(t, testJWTSecret, map[string]any{
		"key": documentKey(missing, version.ID), "status": 2, "url": sameOriginURL,
	}, true)
	if err := s.HandleCallback(body, "", "", ""); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("missing file: err = %v, want files.ErrNotFound", err)
	}
}

// 超过大小上限的下载被拒绝（临时对象清理）。
func TestCallbackDownloadTooLarge(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	s := newTestService(store, newMemStorage(), &fakeFetch{content: []byte("0123456789abcdef")}, &fakeRecorder{}, nil)
	s.cfg.DownloadMaxBytes = 4

	body := callbackBody(t, testJWTSecret, map[string]any{
		"key": documentKey(file.ID, version.ID), "status": 2, "url": sameOriginURL,
	}, true)
	if err := s.HandleCallback(body, "", "", ""); !errors.Is(err, ErrDownloadTooLarge) {
		t.Fatalf("err = %v, want ErrDownloadTooLarge", err)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
}

// 篡改 body 但保留他人签名 token：以 claims（签名覆盖）为准，body 的 url 不生效。
func TestCallbackClaimsAreTrustSource(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fetcher := &fakeFetch{content: []byte("ok")}
	s := newTestService(store, newMemStorage(), fetcher, &fakeRecorder{}, nil)

	// token 声明 status=4（仅清理）；body 伪造成 status=2 + 外部 url。
	token := signCallbackToken(t, testJWTSecret, map[string]any{
		"key": documentKey(file.ID, version.ID), "status": 4,
	})
	body := []byte(`{"key":"` + documentKey(file.ID, version.ID) + `","status":2,"url":"http://evil.example.com/x","token":"` + token + `"}`)
	if err := s.HandleCallback(body, "", "", ""); err != nil {
		t.Fatalf("claims trust source: %v", err)
	}
	if len(fetcher.urls) != 0 {
		t.Fatalf("forged body url must not be fetched, got %v", fetcher.urls)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
}

// 回调 token 用途隔离：编辑配置 token（aud=onlyoffice-config）与下载 token
// （aud=onlyoffice-download）打回调一律拒绝；无 aud + 完整 claims 的 DS token
// 通过并正常保存。
func TestCallbackTokenAudienceIsolation(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fetcher := &fakeFetch{content: []byte("ok")}
	recorder := &fakeRecorder{}
	s := newTestService(store, newMemStorage(), fetcher, recorder, nil)
	key := documentKey(file.ID, version.ID)
	saveBody := func(token string) []byte {
		return []byte(`{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `","token":"` + token + `"}`)
	}

	// 编辑配置 token 打回调：拒绝（{"error":1}）。
	config, err := s.NewEditConfig(owner, file.ID)
	if err != nil {
		t.Fatalf("NewEditConfig: %v", err)
	}
	if err := s.HandleCallback(saveBody(config["token"].(string)), "", "10.0.0.9", "ds"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("config token as callback: err = %v, want ErrInvalidToken", err)
	}

	// 下载 token 打回调：拒绝。
	downloadToken, err := s.signDownloadToken(file.ID, version.ID)
	if err != nil {
		t.Fatalf("signDownloadToken: %v", err)
	}
	if err := s.HandleCallback(saveBody(downloadToken), "", "10.0.0.9", "ds"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("download token as callback: err = %v, want ErrInvalidToken", err)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls after rejected tokens = %d, want 0", got)
	}
	if len(fetcher.urls) != 0 {
		t.Fatalf("fetch must not be called, got %v", fetcher.urls)
	}

	// 无 aud + 完整 claims（DS 实际形态，users[0] 有写权限）：通过并保存。
	dsToken := signCallbackToken(t, testJWTSecret, map[string]any{
		"key": key, "status": 2, "url": sameOriginURL, "users": []string{owner.String()},
	})
	if err := s.HandleCallback(saveBody(dsToken), "", "10.0.0.9", "ds"); err != nil {
		t.Fatalf("ds token without aud: %v", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls = %d, want 1", got)
	}
}

// 安全字段缺 claims 拒绝：status/key 缺失、保存类状态（2/6）缺 url 一律
// error:1，即使 body 携带同名字段也不回退；status=4 等非保存状态无需 url。
func TestCallbackRequiredClaims(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	fetcher := &fakeFetch{content: []byte("evil")}
	s := newTestService(store, newMemStorage(), fetcher, &fakeRecorder{}, nil)
	key := documentKey(file.ID, version.ID)

	cases := []struct {
		name   string
		claims map[string]any
		body   string
	}{
		{
			name:   "missing status claim",
			claims: map[string]any{"key": key, "url": sameOriginURL},
			body:   `{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `"}`,
		},
		{
			name:   "missing key claim",
			claims: map[string]any{"status": 2, "url": sameOriginURL},
			body:   `{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `"}`,
		},
		{
			name:   "save without url claim",
			claims: map[string]any{"key": key, "status": 2},
			body:   `{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `"}`,
		},
		{
			name:   "forcesave without url claim",
			claims: map[string]any{"key": key, "status": 6},
			body:   `{"key":"` + key + `","status":6,"url":"` + sameOriginURL + `"}`,
		},
	}
	for _, tc := range cases {
		token := signCallbackToken(t, testJWTSecret, tc.claims)
		body := []byte(tc.body + `,"token":"` + token + `"}`)
		if err := s.HandleCallback(body, "", "", ""); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: err = %v, want ErrInvalidToken", tc.name, err)
		}
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls = %d, want 0", got)
	}
	if len(fetcher.urls) != 0 {
		t.Fatalf("fetch must not be called, got %v", fetcher.urls)
	}

	// status=4（清理）协议上不携带 url：key+status claims 即可通过。
	token := signCallbackToken(t, testJWTSecret, map[string]any{"key": key, "status": 4})
	body := []byte(`{"key":"` + key + `","status":4,"token":"` + token + `"}`)
	if err := s.HandleCallback(body, "", "", ""); err != nil {
		t.Fatalf("cleanup without url claim: %v", err)
	}
}

// 保存写鉴权：actor 取验签 claims 的 users[0]（body users 不可伪造授权）；
// 无写权限、未接线授权器均 fail closed 拒绝且不发起下载。
func TestCallbackSaveWriteAuthorization(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	editor, viewer := uuid.New(), uuid.New()
	fetcher := &fakeFetch{content: []byte("v2")}
	recorder := &fakeRecorder{}
	s := newTestService(store, newMemStorage(), fetcher, recorder, nil)
	s.SetWriteAuthorizer(func(user, fileID uuid.UUID) error {
		if user != owner && user != editor {
			return files.ErrForbidden
		}
		return nil
	})
	key := documentKey(file.ID, version.ID)

	// claims users[0]=viewer（无写权限）：拒绝；body 伪造 owner 无效。
	token := signCallbackToken(t, testJWTSecret, map[string]any{
		"key": key, "status": 2, "url": sameOriginURL, "users": []string{viewer.String()},
	})
	body := []byte(`{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `","users":["` + owner.String() + `"],"token":"` + token + `"}`)
	if err := s.HandleCallback(body, "", "10.0.0.9", "ds"); !errors.Is(err, ErrSaveForbidden) {
		t.Fatalf("viewer save: err = %v, want ErrSaveForbidden", err)
	}
	if got := store.addVersionCount(); got != 0 {
		t.Fatalf("AddVersion calls after denied save = %d, want 0", got)
	}
	if len(fetcher.urls) != 0 {
		t.Fatalf("fetch must not be called on denied save, got %v", fetcher.urls)
	}
	denied := false
	for _, e := range recorder.entries {
		if e.Action == audit.ActionOnlyOfficeSave && e.Status == audit.StatusFailure {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("audit actions = %v, want onlyoffice.save failure", recorder.actions())
	}

	// claims users[0]=editor（有写权限）：保存成功且版本归属 editor。
	token = signCallbackToken(t, testJWTSecret, map[string]any{
		"key": key, "status": 2, "url": sameOriginURL, "users": []string{editor.String()},
	})
	body = []byte(`{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `","token":"` + token + `"}`)
	if err := s.HandleCallback(body, "", "10.0.0.9", "ds"); err != nil {
		t.Fatalf("editor save: %v", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls = %d, want 1", got)
	}
	current, _, err := store.CurrentVersion(owner, file.ID)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if current.UserID != editor {
		t.Fatalf("new version user = %v, want editor %v", current.UserID, editor)
	}

	// 未接线授权器（fail closed）：即使 owner 也拒绝。
	bare := newTestService(store, newMemStorage(), fetcher, &fakeRecorder{}, nil)
	bare.authorizeWrite = nil
	token = signCallbackToken(t, testJWTSecret, map[string]any{
		"key": key, "status": 2, "url": sameOriginURL + "?t=2", "users": []string{owner.String()},
	})
	body = []byte(`{"key":"` + key + `","status":2,"url":"` + sameOriginURL + `?t=2","token":"` + token + `"}`)
	if err := bare.HandleCallback(body, "", "", ""); !errors.Is(err, ErrSaveForbidden) {
		t.Fatalf("save without authorizer: err = %v, want ErrSaveForbidden", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls after fail-closed = %d, want 1", got)
	}
}
