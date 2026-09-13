package webpkg

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
)

// fakeSource 是 FileSource 的最小内存实现（模式同 http 包 previewFakeSource）。
type fakeSource struct {
	owner   uuid.UUID
	file    files.File
	version files.FileVersion
	blob    files.ObjectBlob
	views   int64
}

func (f *fakeSource) GetFileByID(id uuid.UUID) (files.File, error) {
	if id != f.file.ID || f.file.DeletedAt != nil {
		return files.File{}, files.ErrNotFound
	}
	return f.file, nil
}

func (f *fakeSource) CurrentVersion(owner, fileID uuid.UUID) (files.FileVersion, files.ObjectBlob, error) {
	if _, err := f.GetFileByID(fileID); err != nil || owner != f.owner {
		return files.FileVersion{}, files.ObjectBlob{}, files.ErrNoVersion
	}
	return f.version, f.blob, nil
}

type env struct {
	svc    *Service
	source *fakeSource
	repo   *MemoryRepo
	store  *upload.LocalStorage
}

func newEnv(t *testing.T, zipBytes []byte, mime, name string) *env {
	t.Helper()
	storage, err := upload.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Put("objects/site", bytes.NewReader(zipBytes)); err != nil {
		t.Fatal(err)
	}
	owner := uuid.New()
	source := &fakeSource{
		owner:   owner,
		file:    files.File{ID: uuid.New(), Name: name, OwnerID: owner, Type: "file"},
		version: files.FileVersion{Version: 1},
		blob:    files.ObjectBlob{StorageKey: "objects/site", Size: int64(len(zipBytes)), MimeType: mime, Status: files.BlobStatusAvailable},
	}
	repo := NewMemoryRepo()
	svc := NewService(repo, source, storage, DefaultLimits())
	return &env{svc: svc, source: source, repo: repo, store: storage}
}

func validZip(t *testing.T) []byte {
	t.Helper()
	return buildZip(t, []zipSpec{
		{name: "index.html", data: "<h1>hello</h1>"},
		{name: "assets/logo.svg", data: "<svg/>"},
	})
}

func TestExtractForFileReady(t *testing.T) {
	e := newEnv(t, validZip(t), "application/zip", "site.zip")
	pkg, err := e.svc.ExtractForFile(e.source.file.ID)
	if err != nil {
		t.Fatalf("ExtractForFile: %v", err)
	}
	if pkg.Status != StatusReady || pkg.EntryCount != 2 {
		t.Fatalf("pkg = %+v", pkg)
	}
	if !URLSafePublicID(pkg.PublicID) {
		t.Fatalf("public id = %q", pkg.PublicID)
	}
	// ready 包可解析内容。
	rc, ct, ok := e.svc.Resolve(pkg.PublicID, "index.html")
	if !ok {
		t.Fatal("resolve index.html failed")
	}
	defer rc.Close()
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "<h1>hello</h1>" {
		t.Fatalf("body = %q", data)
	}
	// ReadyPackage 联动入口。
	if pid, ok := e.svc.ReadyPackage(e.source.file.ID); !ok || pid != pkg.PublicID {
		t.Fatalf("ReadyPackage = %q, %v", pid, ok)
	}
}

// TestExtractForFileBlockedKeepsRow：安全校验失败置 blocked（含原因），
// 不影响文件本身（blob/文件行不变），且 ReadyPackage/Resolve 均不可用。
func TestExtractForFileBlockedKeepsRow(t *testing.T) {
	bad := buildZip(t, []zipSpec{{name: "about.html", data: "x"}})
	e := newEnv(t, bad, "application/zip", "site.zip")
	pkg, err := e.svc.ExtractForFile(e.source.file.ID)
	if !errors.Is(err, ErrWebpkgInvalid) {
		t.Fatalf("err = %v, want ErrWebpkgInvalid", err)
	}
	if pkg.Status != StatusBlocked || pkg.Error == "" {
		t.Fatalf("pkg = %+v, want blocked with reason", pkg)
	}
	if _, ok := e.svc.ReadyPackage(e.source.file.ID); ok {
		t.Fatal("blocked package must not be ready")
	}
	if _, _, ok := e.svc.Resolve(pkg.PublicID, "about.html"); ok {
		t.Fatal("blocked package must not resolve")
	}
	// 文件本身可用性不受影响。
	if _, _, err := e.source.CurrentVersion(e.source.owner, e.source.file.ID); err != nil {
		t.Fatalf("file availability affected: %v", err)
	}
}

// TestExtractForFileIdempotentRebuild：重复执行幂等重建（public_id 稳定，
// 旧 key 被替换，内容更新）。
func TestExtractForFileIdempotentRebuild(t *testing.T) {
	e := newEnv(t, validZip(t), "application/zip", "site.zip")
	first, err := e.svc.ExtractForFile(e.source.file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Put("objects/site", bytes.NewReader(buildZip(t, []zipSpec{
		{name: "index.html", data: "<h1>v2</h1>"},
		{name: "new.txt", data: "n"},
	}))); err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.ExtractForFile(e.source.file.ID)
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	if second.PublicID != first.PublicID {
		t.Fatalf("public id changed: %q -> %q", first.PublicID, second.PublicID)
	}
	rc, ct, ok := e.svc.Resolve(second.PublicID, "index.html")
	if !ok || ct != "text/html; charset=utf-8" {
		t.Fatalf("resolve after rebuild = %v, %q", ok, ct)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "<h1>v2</h1>" {
		t.Fatalf("body = %q, want v2", data)
	}
	// 旧条目已清理。
	if _, _, ok := e.svc.Resolve(second.PublicID, "assets/logo.svg"); ok {
		t.Fatal("stale entry still resolvable after rebuild")
	}
	// repo 中仍只有一行。
	got, err := e.repo.GetByFileID(e.source.file.ID)
	if err != nil || got.ID != first.ID {
		t.Fatalf("repo row = %+v, %v", got, err)
	}
}

func TestResolveGuards(t *testing.T) {
	e := newEnv(t, validZip(t), "application/zip", "site.zip")
	pkg, err := e.svc.ExtractForFile(e.source.file.ID)
	if err != nil {
		t.Fatal(err)
	}
	pid := pkg.PublicID

	// 穿越与非白名单。
	for _, rel := range []string{"../secret.txt", "a/../../x", "/etc/passwd", "app.exe", ".webpkg-manifest", "no-such.html"} {
		if _, _, ok := e.svc.Resolve(pid, rel); ok {
			t.Errorf("Resolve(%q) unexpectedly allowed", rel)
		}
	}
	// 非法 public_id。
	if _, _, ok := e.svc.Resolve("not-a-pid", "index.html"); ok {
		t.Error("invalid public id unexpectedly allowed")
	}
	// 文件软删除后不可解析。
	deleted := time.Now()
	e.source.file.DeletedAt = &deleted
	if _, _, ok := e.svc.Resolve(pid, "index.html"); ok {
		t.Error("deleted file unexpectedly resolvable")
	}
	e.source.file.DeletedAt = nil
	// blob 不可用后不可解析。
	e.source.blob.Status = files.BlobStatusQuarantined
	if _, _, ok := e.svc.Resolve(pid, "index.html"); ok {
		t.Error("quarantined blob unexpectedly resolvable")
	}
	e.source.blob.Status = files.BlobStatusAvailable
	// 非 ready 状态不可解析。
	_ = e.repo.SetResult(pkg.ID, StatusFailed, "boom", 0, 0)
	if _, _, ok := e.svc.Resolve(pid, "index.html"); ok {
		t.Error("failed package unexpectedly resolvable")
	}
}

// TestExtractForFileBlobUnavailable：当前版本对象非 available 时拒绝解包。
func TestExtractForFileBlobUnavailable(t *testing.T) {
	e := newEnv(t, validZip(t), "application/zip", "site.zip")
	e.source.blob.Status = files.BlobStatusScanning
	if _, err := e.svc.ExtractForFile(e.source.file.ID); !errors.Is(err, ErrBlobUnavailable) {
		t.Fatalf("err = %v, want ErrBlobUnavailable", err)
	}
}

// TestAutoExtractGating：仅 zip 候选（mime 或 .zip 名）且开关开启时触发。
func TestAutoExtractGating(t *testing.T) {
	// application/zip：触发并 ready。
	e := newEnv(t, validZip(t), "application/zip", "site")
	e.svc.AutoExtract(e.source.file.ID)
	if pid, ok := e.svc.ReadyPackage(e.source.file.ID); !ok || !URLSafePublicID(pid) {
		t.Fatal("auto extract did not produce ready package")
	}
	// octet-stream + .zip 名：触发。
	e2 := newEnv(t, validZip(t), "application/octet-stream", "site.ZIP")
	e2.svc.AutoExtract(e2.source.file.ID)
	if _, ok := e2.svc.ReadyPackage(e2.source.file.ID); !ok {
		t.Fatal(".zip name candidate not auto extracted")
	}
	// 非 zip 候选：不触发。
	e3 := newEnv(t, validZip(t), "application/octet-stream", "photo.png")
	e3.svc.AutoExtract(e3.source.file.ID)
	if _, ok := e3.svc.ReadyPackage(e3.source.file.ID); ok {
		t.Fatal("non-zip candidate unexpectedly extracted")
	}
	// 开关关闭：不触发。
	e4 := newEnv(t, validZip(t), "application/zip", "site.zip")
	e4.svc.SetEnabled(false)
	e4.svc.AutoExtract(e4.source.file.ID)
	if _, ok := e4.svc.ReadyPackage(e4.source.file.ID); ok {
		t.Fatal("auto extract ran while disabled")
	}
	// 文件不存在：静默。
	e5 := newEnv(t, validZip(t), "application/zip", "site.zip")
	e5.svc.AutoExtract(uuid.New())
}
