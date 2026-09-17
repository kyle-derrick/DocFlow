package share

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// fakeFiles 是 FileSource 的内存实现（不依赖 PostgreSQL）。
type fakeFiles struct {
	files     map[uuid.UUID]files.File
	versions  map[uuid.UUID]files.FileVersion
	blobs     map[uuid.UUID]files.ObjectBlob
	downloads map[uuid.UUID]int64
	views     map[uuid.UUID]int64
	seq       int
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{files: map[uuid.UUID]files.File{}, versions: map[uuid.UUID]files.FileVersion{}, blobs: map[uuid.UUID]files.ObjectBlob{}, downloads: map[uuid.UUID]int64{}, views: map[uuid.UUID]int64{}}
}

func (f *fakeFiles) addFile(owner uuid.UUID, name, fileType, blobStatus string) uuid.UUID {
	f.seq++
	blobID, fileID := uuid.New(), uuid.New()
	f.blobs[blobID] = files.ObjectBlob{ID: blobID, SHA256: fmt.Sprintf("%064d", f.seq), StorageKey: "objects/" + fileID.String(), Size: 42, MimeType: "text/plain", Status: blobStatus}
	f.versions[fileID] = files.FileVersion{ID: uuid.New(), FileID: fileID, Version: 1, ObjectBlobID: blobID, Size: 42}
	f.files[fileID] = files.File{ID: fileID, Name: name, OwnerID: owner, Type: fileType}
	return fileID
}

func (f *fakeFiles) Get(owner, id uuid.UUID) (files.File, error) {
	fl, ok := f.files[id]
	if !ok || fl.OwnerID != owner || fl.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return fl, nil
}

func (f *fakeFiles) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.Get(owner, fileID); err != nil {
		return files.FileVersion{}, files.ObjectBlob{}, err
	}
	v, ok := f.versions[fileID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
	}
	b, ok := f.blobs[v.ObjectBlobID]
	if !ok {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
	}
	return v, b, nil
}

func (f *fakeFiles) IncrementDownloadCount(owner, fileID uuid.UUID) error {
	if _, err := f.Get(owner, fileID); err != nil {
		return err
	}
	f.downloads[fileID]++
	return nil
}

func (f *fakeFiles) IncrementViewCount(owner, fileID uuid.UUID) error {
	if _, err := f.Get(owner, fileID); err != nil {
		return err
	}
	f.views[fileID]++
	return nil
}

// setDeleted 标记/清除文件软删除状态（map 中的结构体不能直接赋值字段）。
func (f *fakeFiles) setDeleted(fileID uuid.UUID, at *time.Time) {
	fl := f.files[fileID]
	fl.DeletedAt = at
	f.files[fileID] = fl
}

// newTestService 构造内存版服务；返回的 now 指针可写入以推进测试时钟。
func newTestService() (*Service, *MemoryStore, *fakeFiles, uuid.UUID, uuid.UUID, *time.Time) {
	repo := NewMemoryStore()
	ff := newFakeFiles()
	svc := NewService(repo, ff)
	owner := uuid.New()
	fileID := ff.addFile(owner, "report.txt", "file", files.BlobStatusAvailable)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	return svc, repo, ff, owner, fileID, &now
}

func TestNewTokenFormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 256; i++ {
		token, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 43 {
			t.Fatalf("token %q length = %d, want 43 (32 bytes URL-safe base64)", token, len(token))
		}
		for _, ch := range token {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				t.Fatalf("token %q contains non URL-safe character %q", token, ch)
			}
		}
		if seen[token] {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = true
	}
}

func TestHashTokenKnownVector(t *testing.T) {
	if got, want := HashToken("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Fatalf("HashToken(abc) = %q, want %q", got, want)
	}
	if got := len(HashToken("whatever")); got != 64 {
		t.Fatalf("hash length = %d, want 64", got)
	}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if HashToken(token) == token {
		t.Fatal("hash must differ from plaintext token")
	}
}

func TestCreateStoresOnlyHashAndSetsPublicFlag(t *testing.T) {
	svc, repo, _, owner, fileID, _ := newTestService()
	max := 5
	sh, token, err := svc.Create(owner, fileID, PermissionDownload, time.Hour, &max)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("plaintext token must be returned once")
	}
	stored, err := repo.GetByTokenHash(HashToken(token))
	if err != nil {
		t.Fatalf("share must be resolvable by token hash: %v", err)
	}
	if stored.ID != sh.ID {
		t.Fatalf("resolved share id = %v, want %v", stored.ID, sh.ID)
	}
	if stored.TokenHash == token || stored.TokenHash != HashToken(token) {
		t.Fatal("database must only store the SHA-256 hash of the token")
	}
	if stored.Permission != PermissionDownload {
		t.Fatalf("permission = %q, want %q", stored.Permission, PermissionDownload)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(stored.CreatedAt.Add(time.Hour)) {
		t.Fatalf("expires_at = %v, want created_at + 1h", stored.ExpiresAt)
	}
	if stored.MaxDownloads == nil || *stored.MaxDownloads != 5 {
		t.Fatalf("max_downloads = %v, want 5", stored.MaxDownloads)
	}
	if stored.DownloadCount != 0 {
		t.Fatalf("download_count = %d, want 0", stored.DownloadCount)
	}
	if !repo.IsPublic(fileID) {
		t.Fatal("files.is_public must be set after creating a share")
	}
}

// 默认有效期热读取：Create/CreatePrivate 收到 expiresIn==0 时采用注入的
// 默认小时数；未注入或返回非正值（0，模拟读失败回退）时维持「永久」。
// 显式指定有效期不受默认值影响。
func TestCreateUsesDefaultExpiryProvider(t *testing.T) {
	svc, _, ff, owner, fileID, _ := newTestService()

	// 未注入：永久（既有行为）。
	sh, _, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sh.ExpiresAt != nil {
		t.Fatalf("no provider: expires_at = %v, want nil (永久)", sh.ExpiresAt)
	}

	hours := 48
	svc.SetDefaultExpiryProvider(func() int { return hours })

	// expiresIn==0：采用默认 48h。
	sh, _, err = svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sh.ExpiresAt == nil || !sh.ExpiresAt.Equal(sh.CreatedAt.Add(48*time.Hour)) {
		t.Fatalf("default expiry: expires_at = %v, want created_at + 48h", sh.ExpiresAt)
	}

	// 显式有效期优先于默认值。
	sh, _, err = svc.Create(owner, fileID, PermissionView, 2*time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sh.ExpiresAt == nil || !sh.ExpiresAt.Equal(sh.CreatedAt.Add(2*time.Hour)) {
		t.Fatalf("explicit expiry: expires_at = %v, want created_at + 2h", sh.ExpiresAt)
	}

	// provider 返回 0（读失败回退）：永久。
	hours = 0
	sh, _, err = svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sh.ExpiresAt != nil {
		t.Fatalf("provider zero: expires_at = %v, want nil (永久)", sh.ExpiresAt)
	}

	// 私有分享同语义。
	hours = 24
	svc.SetDefaultExpiryProvider(func() int { return hours })
	priv, err := svc.CreatePrivate(owner, ff.addFile(owner, "p.txt", "file", files.BlobStatusAvailable), PermissionView, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if priv.ExpiresAt == nil || !priv.ExpiresAt.Equal(priv.CreatedAt.Add(24*time.Hour)) {
		t.Fatalf("private default expiry: expires_at = %v, want created_at + 24h", priv.ExpiresAt)
	}
}

func TestCreateValidations(t *testing.T) {
	svc, _, ff, owner, fileID, now := newTestService()
	folderID := ff.addFile(owner, "docs", "folder", files.BlobStatusAvailable)
	noVersionID := ff.addFile(owner, "empty.txt", "file", files.BlobStatusAvailable)
	delete(ff.versions, noVersionID)
	quarantinedID := ff.addFile(owner, "bad.txt", "file", files.BlobStatusQuarantined)
	deletedID := ff.addFile(owner, "gone.txt", "file", files.BlobStatusAvailable)
	deleted := *now
	ff.setDeleted(deletedID, &deleted)

	negative := -1
	tests := []struct {
		name        string
		owner       uuid.UUID
		file        uuid.UUID
		permission  string
		expiresIn   time.Duration
		maxDownload *int
		wantErr     error
	}{
		{"other owner file", uuid.New(), fileID, PermissionView, 0, nil, ErrFileNotFound},
		{"deleted file", owner, deletedID, PermissionView, 0, nil, ErrFileNotFound},
		// 目录分享自 v1.1 起允许作为公开分享根（CreatePublic）；
		// CreatePrivate 仍限文件（见 private_test.go）。
		{"folder", owner, folderID, PermissionView, 0, nil, nil},
		{"no current version", owner, noVersionID, PermissionView, 0, nil, ErrFileNotShareable},
		{"blob not available", owner, quarantinedID, PermissionView, 0, nil, ErrFileNotShareable},
		{"invalid permission", owner, fileID, "rw", 0, nil, ErrInvalidPermission},
		{"negative expiry", owner, fileID, PermissionView, -time.Second, nil, ErrInvalidExpiry},
		{"negative max downloads", owner, fileID, PermissionView, 0, &negative, ErrInvalidMaxDownloads},
	}
	for _, tc := range tests {
		if _, _, err := svc.Create(tc.owner, tc.file, tc.permission, tc.expiresIn, tc.maxDownload); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestResolveLifecycle(t *testing.T) {
	svc, _, ff, owner, fileID, now := newTestService()

	// 未知 token → 404 语义。
	if _, err := svc.Resolve("no-such-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: err = %v, want ErrNotFound", err)
	}

	// 有效分享 → 返回 share+file+blob。
	sh, token, err := svc.Create(owner, fileID, PermissionView, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Resolve(token)
	if err != nil {
		t.Fatal(err)
	}
	if r.Share.ID != sh.ID || r.File.Name != "report.txt" || r.Blob.Status != files.BlobStatusAvailable || r.Version.Version != 1 {
		t.Fatalf("unexpected resolved result: %+v", r)
	}

	// 过期 → 失效。
	*now = now.Add(2 * time.Hour)
	if _, err := svc.Resolve(token); !errors.Is(err, ErrGone) {
		t.Fatalf("expired share: err = %v, want ErrGone", err)
	}
	*now = now.Add(-2 * time.Hour)

	// 撤销 → 失效。
	sh2, token2, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Revoke(owner, sh2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(token2); !errors.Is(err, ErrGone) {
		t.Fatalf("revoked share: err = %v, want ErrGone", err)
	}

	// 达到下载上限 → 失效。
	one := 1
	_, token3, err := svc.Create(owner, fileID, PermissionDownload, 0, &one)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForDownload(token3); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(token3); !errors.Is(err, ErrGone) {
		t.Fatalf("exhausted share: err = %v, want ErrGone", err)
	}

	// 文件删除后 → 失效（repo 中分享仍存在）。
	_, token4, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := *now
	ff.setDeleted(fileID, &deleted)
	if _, err := svc.Resolve(token4); !errors.Is(err, ErrGone) {
		t.Fatalf("share of deleted file: err = %v, want ErrGone", err)
	}
	ff.setDeleted(fileID, nil)
}

func TestResolveForDownloadPermissionAndCounts(t *testing.T) {
	// view 权限：元数据可解析，但下载被拒且不计数。
	svc, repo, ff, owner, fileID, _ := newTestService()
	_, viewToken, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForDownload(viewToken); !errors.Is(err, ErrDownloadForbidden) {
		t.Fatalf("view share download: err = %v, want ErrDownloadForbidden", err)
	}
	if got := shareByHash(t, repo, HashToken(viewToken)).DownloadCount; got != 0 {
		t.Fatalf("view share download_count = %d, want 0", got)
	}
	if ff.downloads[fileID] != 0 {
		t.Fatalf("files.download_count = %d, want 0", ff.downloads[fileID])
	}

	// download 权限：成功并双计数。
	_, dlToken, err := svc.Create(owner, fileID, PermissionDownload, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForDownload(dlToken); err != nil {
		t.Fatal(err)
	}
	if got := shareByHash(t, repo, HashToken(dlToken)).DownloadCount; got != 1 {
		t.Fatalf("share download_count = %d, want 1", got)
	}
	if ff.downloads[fileID] != 1 {
		t.Fatalf("files.download_count = %d, want 1", ff.downloads[fileID])
	}

	// max_downloads=1：第二次下载超限。
	one := 1
	_, limited, err := svc.Create(owner, fileID, PermissionDownload, 0, &one)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForDownload(limited); err != nil {
		t.Fatal(err)
	}
	// 第二次下载超限：Resolve 阶段即判定失效（ErrGone），
	// 并发窗口内由 ConsumeDownload 判定时返回 ErrDownloadLimit，两者均映射 410。
	if _, err := svc.ResolveForDownload(limited); !errors.Is(err, ErrDownloadLimit) && !errors.Is(err, ErrGone) {
		t.Fatalf("second download: err = %v, want ErrDownloadLimit or ErrGone", err)
	}

	// 对象变为不可用：元数据仍可解析，下载拒绝。
	_, unavailable, err := svc.Create(owner, fileID, PermissionDownload, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	blobID := ff.versions[fileID].ObjectBlobID
	blob := ff.blobs[blobID]
	blob.Status = files.BlobStatusQuarantined
	ff.blobs[blobID] = blob
	if _, err := svc.Resolve(unavailable); err != nil {
		t.Fatalf("metadata must still resolve: %v", err)
	}
	if _, err := svc.ResolveForDownload(unavailable); !errors.Is(err, ErrFileNotAvailable) {
		t.Fatalf("unavailable blob download: err = %v, want ErrFileNotAvailable", err)
	}
}

// 下载计数补偿：ResolveForDownload 已消耗计数但内容读取失败时，
// DecrementDownload 回退分享 download_count（下限 0，不越减）。
func TestDecrementDownloadCompensation(t *testing.T) {
	svc, repo, ff, owner, fileID, _ := newTestService()
	_, token, err := svc.Create(owner, fileID, PermissionDownload, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.ResolveForDownload(token)
	if err != nil {
		t.Fatal(err)
	}
	if got := shareByHash(t, repo, HashToken(token)).DownloadCount; got != 1 {
		t.Fatalf("share download_count after consume = %d, want 1", got)
	}
	// 模拟存储读取失败后的补偿。
	if err := svc.DecrementDownload(r); err != nil {
		t.Fatalf("DecrementDownload: %v", err)
	}
	if got := shareByHash(t, repo, HashToken(token)).DownloadCount; got != 0 {
		t.Fatalf("share download_count after compensation = %d, want 0", got)
	}
	// 下限 0：对计数为 0 的分享再次补偿不产生负数。
	if err := svc.DecrementDownload(r); err != nil {
		t.Fatalf("DecrementDownload at zero: %v", err)
	}
	if got := shareByHash(t, repo, HashToken(token)).DownloadCount; got != 0 {
		t.Fatalf("share download_count after double compensation = %d, want 0", got)
	}
	// 不存在的分享静默成功。
	if err := svc.DecrementDownload(Resolved{Share: Share{ID: uuid.New()}}); err != nil {
		t.Fatalf("DecrementDownload missing share: %v", err)
	}
	// 补偿后分享仍可正常下载（计数窗口未被破坏）。
	if _, err := svc.ResolveForDownload(token); err != nil {
		t.Fatalf("download after compensation: %v", err)
	}
	if ff.downloads[fileID] != 2 {
		t.Fatalf("files.download_count = %d, want 2", ff.downloads[fileID])
	}
}

func TestResolveForPreviewPermissionAndCounts(t *testing.T) {
	svc, repo, ff, owner, fileID, _ := newTestService()

	// view 与 download 权限均允许预览，且不消耗分享 download_count。
	for _, perm := range []string{PermissionView, PermissionDownload} {
		_, token, err := svc.Create(owner, fileID, perm, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := svc.ResolveForPreview(token)
		if err != nil {
			t.Fatalf("%s share preview: %v", perm, err)
		}
		// 确定可预览后由调用方递增 view_count（与 HTTP 层行为一致）。
		if err := svc.IncrementPreviewView(r); err != nil {
			t.Fatalf("%s increment view: %v", perm, err)
		}
		if got := shareByHash(t, repo, HashToken(token)).DownloadCount; got != 0 {
			t.Fatalf("%s share download_count = %d, want 0 (preview must not consume)", perm, got)
		}
	}
	if ff.views[fileID] != 2 {
		t.Fatalf("files.view_count = %d, want 2", ff.views[fileID])
	}
	if ff.downloads[fileID] != 0 {
		t.Fatalf("files.download_count = %d, want 0", ff.downloads[fileID])
	}

	// 预置后续用例的分享（blob 尚可用时创建；之后统一置为不可用）。
	_, unavailable, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked, revokedToken, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 对象不可用：元数据可解析，预览拒绝。
	blobID := ff.versions[fileID].ObjectBlobID
	blob := ff.blobs[blobID]
	blob.Status = files.BlobStatusQuarantined
	ff.blobs[blobID] = blob
	if _, err := svc.Resolve(unavailable); err != nil {
		t.Fatalf("metadata must still resolve: %v", err)
	}
	if _, err := svc.ResolveForPreview(unavailable); !errors.Is(err, ErrFileNotAvailable) {
		t.Fatalf("unavailable blob preview: err = %v, want ErrFileNotAvailable", err)
	}

	// 失效分享（撤销）→ ErrGone，且不递增计数。
	if _, err := svc.Revoke(owner, revoked.ID); err != nil {
		t.Fatal(err)
	}
	before := ff.views[fileID]
	if _, err := svc.ResolveForPreview(revokedToken); !errors.Is(err, ErrGone) {
		t.Fatalf("revoked share preview: err = %v, want ErrGone", err)
	}
	if ff.views[fileID] != before {
		t.Fatal("revoked share preview must not increment view_count")
	}
}

func TestRevokeOwnerIsolationAndPublicFlag(t *testing.T) {
	svc, repo, _, owner, fileID, _ := newTestService()
	sh1, _, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	sh2, _, err := svc.Create(owner, fileID, PermissionDownload, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 非 owner 撤销 → 404 语义。
	if _, err := svc.Revoke(uuid.New(), sh1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke by non-owner: err = %v, want ErrNotFound", err)
	}
	// 撤销一个，仍有另一个有效分享 → is_public 保持 true。
	revoked, err := svc.Revoke(owner, sh1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoked_at must be set")
	}
	if !repo.IsPublic(fileID) {
		t.Fatal("is_public must stay true while another active share exists")
	}
	// 再撤销最后一个 → is_public 清除；重复撤销幂等。
	if _, err := svc.Revoke(owner, sh2.ID); err != nil {
		t.Fatal(err)
	}
	if repo.IsPublic(fileID) {
		t.Fatal("is_public must be cleared when no active share remains")
	}
	if _, err := svc.Revoke(owner, sh2.ID); err != nil {
		t.Fatalf("revoke must be idempotent: %v", err)
	}
	// 撤销不存在的分享。
	if _, err := svc.Revoke(owner, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing share: err = %v, want ErrNotFound", err)
	}
}

func TestListOwnerScopedAndOrdered(t *testing.T) {
	svc, _, _, owner, fileID, now := newTestService()
	other := uuid.New()
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		sh, _, err := svc.Create(owner, fileID, PermissionView, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sh.ID)
		// 推进时钟保证 created_at 严格递增，可确定排序。
		*now = now.Add(time.Minute)
	}
	if _, _, err := svc.Create(other, fileID, PermissionView, 0, nil); err == nil {
		t.Fatal("other owner must not be able to share this file")
	}
	out, err := svc.List(owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("list length = %d, want 3", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i-1].CreatedAt.Before(out[i].CreatedAt) {
			t.Fatal("list must be ordered by created_at DESC")
		}
	}
	if out[0].ID != ids[2] || out[2].ID != ids[0] {
		t.Fatal("list order does not match created_at DESC")
	}
	if limited, _ := svc.List(owner, 2); len(limited) != 2 {
		t.Fatalf("limit not applied: %d", len(limited))
	}
}

func shareByHash(t *testing.T, repo *MemoryStore, hash string) Share {
	t.Helper()
	sh, err := repo.GetByTokenHash(hash)
	if err != nil {
		t.Fatalf("share lookup by hash: %v", err)
	}
	return sh
}

// ---------- 团队文件 CanShare 分享门控（设计 6.5.5） ----------

// addTeamFile 向 fakeFiles 添加团队作用域文件（OwnerID=上传者本人，
// fakeFiles.Get 的 owner 判定即可通过，CanShare 门控由 teamSharer 决定）。
func (f *fakeFiles) addTeamFile(owner uuid.UUID, teamID uuid.UUID, name string) uuid.UUID {
	f.seq++
	blobID, fileID := uuid.New(), uuid.New()
	f.blobs[blobID] = files.ObjectBlob{ID: blobID, SHA256: fmt.Sprintf("%064d", f.seq), StorageKey: "objects/" + fileID.String(), Size: 42, MimeType: "text/plain", Status: files.BlobStatusAvailable}
	f.versions[fileID] = files.FileVersion{ID: uuid.New(), FileID: fileID, Version: 1, ObjectBlobID: blobID, Size: 42}
	f.files[fileID] = files.File{ID: fileID, Name: name, OwnerID: owner, Type: "file", ScopeType: "team", TeamID: &teamID}
	return fileID
}

// TestCreateShareTeamCanShareGate 覆盖团队文件分享门控：
// CanShare=true（owner/editor/含 share 权限的自定义角色）可创建公开与私有分享；
// CanShare=false（viewer/deny share）拒绝；门控未接线时 fail closed；个人文件不受影响。
func TestCreateShareTeamCanShareGate(t *testing.T) {
	teamID := uuid.New()
	editor := uuid.New() // CanShare=true
	viewer := uuid.New() // CanShare=false

	svc, _, ff, owner, fileID, _ := newTestService()
	svc.SetTeamSharer(func(user, team uuid.UUID) (bool, error) {
		return user == editor, nil
	})
	editorTeamFile := ff.addTeamFile(editor, teamID, "editor.txt")
	viewerTeamFile := ff.addTeamFile(viewer, teamID, "viewer.txt")

	// CanShare=true：公开与私有分享均可创建。
	if _, _, err := svc.CreatePublic(editor, editorTeamFile, PermissionView, 0, nil, ShareOptions{}); err != nil {
		t.Fatalf("editor create public share: %v", err)
	}
	if _, err := svc.CreatePrivate(editor, editorTeamFile, PermissionView, 0, nil, nil, nil); err != nil {
		t.Fatalf("editor create private share: %v", err)
	}
	// CanShare=false：拒绝（ErrForbidden）。
	if _, _, err := svc.CreatePublic(viewer, viewerTeamFile, PermissionView, 0, nil, ShareOptions{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer create public share: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.CreatePrivate(viewer, viewerTeamFile, PermissionView, 0, nil, nil, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer create private share: err = %v, want ErrForbidden", err)
	}
	// 个人文件不受门控影响（owner 可分享）。
	if _, _, err := svc.CreatePublic(owner, fileID, PermissionView, 0, nil, ShareOptions{}); err != nil {
		t.Fatalf("personal file share: %v", err)
	}
}

// TestCreateShareTeamGateFailClosed 门控未接线（teamSharer=nil）时团队文件分享一律拒绝。
func TestCreateShareTeamGateFailClosed(t *testing.T) {
	teamID := uuid.New()
	svc, _, ff, _, _, _ := newTestService()
	member := uuid.New()
	teamFile := ff.addTeamFile(member, teamID, "member.txt")
	if _, _, err := svc.CreatePublic(member, teamFile, PermissionView, 0, nil, ShareOptions{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("team share without gate: err = %v, want ErrForbidden", err)
	}
}
