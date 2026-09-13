package upload

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/google/uuid"
)

// fakeTargetFile 模拟 files.Store 侧的目标文件（AddVersion+Prune 的注入等价物），
// 用于验证 targeting 会话的完整流转与注入契约。
type fakeTargetFile struct {
	name    string
	parent  uuid.UUID
	owner   uuid.UUID
	team    *uuid.UUID
	writers map[uuid.UUID]bool // 团队成员写权限（editor/owner）
	deleted bool
	// 版本内容 sha 列表（下标+1 即版本号）。
	versions []string
	blobs    map[string]*fakeBlob
	current  int
}

type fakeBlob struct {
	ref    int64
	status string
}

// fakeVersionStore 是 SetVersionTarget 两个钩子的内存实现（keep 即保留策略）。
type fakeVersionStore struct {
	targets      map[uuid.UUID]*fakeTargetFile
	keep         int
	replaceCalls int
	createCalls  int // createFile 钩子调用计数（targeting 会话必须为 0）
}

func newFakeVersionStore(keep int) *fakeVersionStore {
	return &fakeVersionStore{targets: make(map[uuid.UUID]*fakeTargetFile), keep: keep}
}

func (s *fakeVersionStore) addFile(f *fakeTargetFile) uuid.UUID {
	id := uuid.New()
	f.blobs = make(map[string]*fakeBlob)
	f.versions = nil
	s.targets[id] = f
	return id
}

func (f *fakeTargetFile) seedVersion(sha string) {
	f.versions = append(f.versions, sha)
	if b, ok := f.blobs[sha]; ok {
		b.ref++
	} else {
		f.blobs[sha] = &fakeBlob{ref: 1, status: files.BlobStatusAvailable}
	}
	f.current = len(f.versions)
}

// canWrite 模拟 authorizeFileWrite：个人文件 owner；团队文件成员写权限。
func (f *fakeTargetFile) canWrite(user uuid.UUID) error {
	if f.owner == user {
		return nil
	}
	if f.team == nil {
		return files.ErrNotFound
	}
	if !f.writers[user] {
		return files.ErrForbidden
	}
	return nil
}

// validate 模拟 files.Store.ValidateReplaceTarget。
func (s *fakeVersionStore) validate(user, fileID uuid.UUID) (files.File, error) {
	f, ok := s.targets[fileID]
	if !ok || f.deleted {
		return files.File{}, files.ErrNotFound
	}
	if err := f.canWrite(user); err != nil {
		return files.File{}, err
	}
	return files.File{ID: fileID, Name: f.name, ParentID: &f.parent, OwnerID: f.owner, Type: "file", ScopeType: "personal"}, nil
}

// replace 模拟 files.Store.ReplaceFileVersion：AddVersion（去重复用/新建）+ PruneVersions(keep)。
func (s *fakeVersionStore) replace(user, fileID uuid.UUID, storageKey, sha string, size int64, mimeType string) (bool, error) {
	f, ok := s.targets[fileID]
	if !ok || f.deleted {
		return false, files.ErrNotFound
	}
	if err := f.canWrite(user); err != nil {
		return false, err
	}
	s.replaceCalls++
	newBlob := true
	if b, ok := f.blobs[sha]; ok {
		switch {
		case b.status == files.BlobStatusAvailable:
			b.ref++
			newBlob = false
		case b.status == files.BlobStatusDeleting && b.ref == 0:
			b.status, b.ref, newBlob = files.BlobStatusAvailable, 1, false
		default:
			return false, files.ErrBlobUnavailable
		}
	} else {
		f.blobs[sha] = &fakeBlob{ref: 1, status: files.BlobStatusAvailable}
	}
	f.versions = append(f.versions, sha)
	f.current = len(f.versions)
	// PruneVersions(keep)：保留最新 keep 个；current 指向的版本受保护。
	for len(f.versions) > s.keep {
		oldest := 1 // 版本号从 1 开始
		if oldest == f.current {
			break
		}
		sha0 := f.versions[0]
		f.versions = f.versions[1:]
		b := f.blobs[sha0]
		if b.ref > 0 {
			b.ref--
		}
		if b.ref == 0 {
			b.status = files.BlobStatusDeleting
		}
	}
	return newBlob, nil
}

func shaOf(content string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(content))) }

func newReplaceService(t *testing.T, store *fakeVersionStore) (*Service, *MemoryStore, *LocalStorage) {
	t.Helper()
	memStore := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(memStore, storage, time.Hour, 1<<20, false, nil, func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, error) {
		store.createCalls++
		return uuid.New(), nil
	})
	svc.SetVersionTarget(store.validate, store.replace)
	return svc, memStore, storage
}

// TestStartReplaceValidatesTarget 覆盖会话创建时的目标校验矩阵。
func TestStartReplaceValidatesTarget(t *testing.T) {
	user, other := uuid.New(), uuid.New()
	parent := uuid.New()

	// 未注入钩子：版本覆盖能力不可用。
	bare := NewService(NewMemoryStore(), mustLocal(t), time.Hour, 1<<20, false, nil, nil)
	if _, err := bare.StartReplace(user, uuid.New(), 3, ""); !errors.Is(err, ErrTargetUnavailable) {
		t.Fatalf("err = %v, want ErrTargetUnavailable", err)
	}

	store := newFakeVersionStore(5)
	personal := store.addFile(&fakeTargetFile{name: "mine.txt", parent: parent, owner: user})
	teamID := uuid.New()
	viewer := uuid.New()
	teamFile := store.addFile(&fakeTargetFile{name: "team.txt", parent: parent, owner: uuid.New(), team: &teamID, writers: map[uuid.UUID]bool{user: true}})

	svc, _, _ := newReplaceService(t, store)
	if _, err := svc.StartReplace(user, uuid.New(), 3, ""); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("missing target: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.StartReplace(other, personal, 3, ""); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("personal file by other: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.StartReplace(viewer, teamFile, 3, ""); !errors.Is(err, files.ErrForbidden) {
		t.Fatalf("team file by viewer: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.StartReplace(user, teamFile, 3, ""); err != nil {
		t.Fatalf("team file by editor: %v", err)
	}
}

func mustLocal(t *testing.T) *LocalStorage {
	t.Helper()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

// TestTargetedUploadSessionFlow 验证 targeting 会话流转（Start→Append→Complete）：
// 沿用现有文件名/父目录、不创建新 File、版本+1、按 keep 裁剪生效。
func TestTargetedUploadSessionFlow(t *testing.T) {
	user := uuid.New()
	parent := uuid.New()
	store := newFakeVersionStore(3)
	target := store.addFile(&fakeTargetFile{name: "report.txt", parent: parent, owner: user})
	store.targets[target].seedVersion(shaOf("v1"))
	store.targets[target].seedVersion(shaOf("v2"))
	store.targets[target].seedVersion(shaOf("v3"))
	svc, memStore, _ := newReplaceService(t, store)

	v, err := svc.StartReplace(user, target, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	// 名称/父目录沿用目标文件现有值。
	if v.Name != "report.txt" {
		t.Fatalf("session name = %q, want existing file name", v.Name)
	}
	if v.ParentID != parent {
		t.Fatalf("session parent = %v, want %v", v.ParentID, parent)
	}
	if v.TargetFileID == nil || *v.TargetFileID != target {
		t.Fatalf("target_file_id = %v", v.TargetFileID)
	}
	if v.Status != StatusUploading {
		t.Fatalf("status = %s", v.Status)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	done, err := svc.Complete(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusAvailable {
		t.Fatalf("status = %s, want available", done.Status)
	}
	if got, _ := memStore.Get(v.ID); got.TargetFileID == nil || *got.TargetFileID != target {
		t.Fatalf("persisted target_file_id = %v", got.TargetFileID)
	}
	// file 单一：不创建新 File（createFile 钩子零调用）。
	if store.createCalls != 0 {
		t.Fatalf("createFile called %d times, want 0", store.createCalls)
	}
	f := store.targets[target]
	if store.replaceCalls != 1 {
		t.Fatalf("replace called %d times", store.replaceCalls)
	}
	// 版本+1（4 个）后被裁剪到 keep=3：v1 被裁、blob 归零置 deleting；
	// 版本号不重排，current 仍指向第 4 版（新内容）。
	if len(f.versions) != 3 {
		t.Fatalf("versions = %v, want 3 (4-1 pruned)", f.versions)
	}
	if f.versions[0] != shaOf("v2") || f.versions[2] != shaOf("abc") {
		t.Fatalf("kept versions = %v", f.versions)
	}
	if f.current != 4 {
		t.Fatalf("current version = %d, want 4 (版本号不随裁剪重排)", f.current)
	}
	if b := f.blobs[shaOf("v1")]; b == nil || b.ref != 0 || b.status != files.BlobStatusDeleting {
		t.Fatalf("pruned v1 blob = %+v, want ref=0 deleting", f.blobs[shaOf("v1")])
	}
	if b := f.blobs[shaOf("abc")]; b == nil || b.ref != 1 || b.status != files.BlobStatusAvailable {
		t.Fatalf("new version blob = %+v", f.blobs[shaOf("abc")])
	}
}

// TestTargetedUploadVersionNumbersIncrement 连续两次覆盖：版本号 4、5 递增。
func TestTargetedUploadVersionNumbersIncrement(t *testing.T) {
	user := uuid.New()
	store := newFakeVersionStore(5)
	target := store.addFile(&fakeTargetFile{name: "log.txt", parent: uuid.New(), owner: user})
	store.targets[target].seedVersion(shaOf("init"))
	svc, _, _ := newReplaceService(t, store)

	for i, content := range []string{"one", "two"} {
		v, err := svc.StartReplace(user, target, int64(len(content)), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Append(v.ID, 0, bytes.NewBufferString(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Complete(v.ID); err != nil {
			t.Fatal(err)
		}
		f := store.targets[target]
		if want := 2 + i; f.current != want || len(f.versions) != want {
			t.Fatalf("after upload #%d: current = %d, versions = %d, want %d", i+1, f.current, len(f.versions), want)
		}
	}
}

// TestTargetedUploadReuseDeletesRedundantObject 同内容覆盖命中去重：
// 复用既有 blob（ref+1），本次上传的物理对象冗余应被清理。
func TestTargetedUploadReuseDeletesRedundantObject(t *testing.T) {
	user := uuid.New()
	store := newFakeVersionStore(5)
	target := store.addFile(&fakeTargetFile{name: "same.txt", parent: uuid.New(), owner: user})
	store.targets[target].seedVersion(shaOf("abc"))
	svc, _, storage := newReplaceService(t, store)

	v, err := svc.StartReplace(user, target, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	done, err := svc.Complete(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusAvailable {
		t.Fatalf("status = %s", done.Status)
	}
	// 复用既有 blob：不应为新内容保留第二个物理对象。
	if r, err := storage.Read(done.StorageKey); err == nil {
		r.Close()
		t.Fatalf("redundant object %s must be deleted on dedupe hit", done.StorageKey)
	}
	f := store.targets[target]
	if b := f.blobs[shaOf("abc")]; b.ref != 2 || b.status != files.BlobStatusAvailable {
		t.Fatalf("blob = %+v, want ref=2 available", b)
	}
	if len(f.versions) != 2 {
		t.Fatalf("versions = %v", f.versions)
	}
}

// TestTargetedUploadCompleteFailureIsTerminal replace 失败（如会话期间文件被软删除）
// → 会话进入 failed 终态。
func TestTargetedUploadCompleteFailureIsTerminal(t *testing.T) {
	user := uuid.New()
	store := newFakeVersionStore(5)
	target := store.addFile(&fakeTargetFile{name: "gone.txt", parent: uuid.New(), owner: user})
	store.targets[target].seedVersion(shaOf("x"))
	svc, memStore, _ := newReplaceService(t, store)

	v, err := svc.StartReplace(user, target, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	// 会话期间目标被移入回收站。
	store.targets[target].deleted = true
	if _, err := svc.Complete(v.ID); err == nil {
		t.Fatal("expected complete failure after target deleted")
	}
	if got, _ := memStore.Get(v.ID); got.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
}
