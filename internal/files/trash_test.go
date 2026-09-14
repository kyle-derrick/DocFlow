package files

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// memTrashRepo 是 trashRepo 的内存实现，用于在无数据库环境下
// 验证恢复冲突与彻底删除引用计数语义。
type memTrashRepo struct {
	files    map[uuid.UUID]File
	versions map[uuid.UUID]FileVersion // key: version id
	blobs    map[uuid.UUID]ObjectBlob
	// webpkgs 模拟 web_packages 行（file_id → public_id）。
	webpkgs map[uuid.UUID]string
	// calls 记录写操作调用顺序，供断言 FK 删除顺序（Clear→DeleteVersions→DeleteFiles）。
	calls []string
}

func newMemTrashRepo() *memTrashRepo {
	return &memTrashRepo{files: make(map[uuid.UUID]File), versions: make(map[uuid.UUID]FileVersion), blobs: make(map[uuid.UUID]ObjectBlob), webpkgs: make(map[uuid.UUID]string)}
}

func (m *memTrashRepo) GetAny(id uuid.UUID) (File, error) {
	f, ok := m.files[id]
	if !ok {
		return File{}, ErrNotFound
	}
	return f, nil
}

func (m *memTrashRepo) ParentAlive(parent uuid.UUID) (bool, error) {
	f, ok := m.files[parent]
	return ok && f.DeletedAt == nil, nil
}

func (m *memTrashRepo) HasActiveSibling(parent *uuid.UUID, name string, exclude uuid.UUID) (bool, error) {
	for id, f := range m.files {
		if id == exclude || f.DeletedAt != nil {
			continue
		}
		if (parent == nil && f.ParentID == nil) || (parent != nil && f.ParentID != nil && *f.ParentID == *parent) {
			if equalFoldName(f.Name, name) {
				return true, nil
			}
		}
	}
	return false, nil
}

func equalFoldName(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func (m *memTrashRepo) Undelete(id uuid.UUID) error {
	f, ok := m.files[id]
	if !ok {
		return ErrNotFound
	}
	f.DeletedAt = nil
	m.files[id] = f
	return nil
}

func (m *memTrashRepo) Descendants(root uuid.UUID) ([]uuid.UUID, error) {
	children := make(map[uuid.UUID][]uuid.UUID)
	for id, f := range m.files {
		if f.ParentID != nil {
			children[*f.ParentID] = append(children[*f.ParentID], id)
		}
	}
	all := []uuid.UUID{root}
	seen := map[uuid.UUID]bool{root: true}
	for i := 0; i < len(all); i++ {
		for _, child := range children[all[i]] {
			if !seen[child] {
				seen[child] = true
				all = append(all, child)
			}
		}
	}
	return all, nil
}

func (m *memTrashRepo) FilesByIDs(ids []uuid.UUID) ([]File, error) {
	var out []File
	for _, id := range ids {
		if f, ok := m.files[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memTrashRepo) BlobRefsForFiles(fileIDs []uuid.UUID) ([]BlobRef, error) {
	counts := make(map[uuid.UUID]int64)
	for _, v := range m.versions {
		for _, fid := range fileIDs {
			if v.FileID == fid {
				counts[v.ObjectBlobID]++
			}
		}
	}
	var out []BlobRef
	for id, n := range counts {
		out = append(out, BlobRef{BlobID: id, Count: n})
	}
	return out, nil
}

func (m *memTrashRepo) WebpkgPublicIDs(fileIDs []uuid.UUID) ([]string, error) {
	want := make(map[uuid.UUID]bool, len(fileIDs))
	for _, id := range fileIDs {
		want[id] = true
	}
	var out []string
	for fid, pid := range m.webpkgs {
		if want[fid] {
			out = append(out, pid)
		}
	}
	return out, nil
}

func (m *memTrashRepo) DeleteVersions(fileIDs []uuid.UUID) error {
	m.calls = append(m.calls, "delete_versions")
	for id, v := range m.versions {
		for _, fid := range fileIDs {
			if v.FileID == fid {
				delete(m.versions, id)
			}
		}
	}
	return nil
}

func (m *memTrashRepo) ClearCurrentVersions(fileIDs []uuid.UUID) error {
	m.calls = append(m.calls, "clear_current_versions")
	for _, fid := range fileIDs {
		if f, ok := m.files[fid]; ok {
			f.CurrentVersionID = nil
			m.files[fid] = f
		}
	}
	return nil
}

func (m *memTrashRepo) DeleteFiles(ids []uuid.UUID) error {
	m.calls = append(m.calls, "delete_files")
	for _, id := range ids {
		delete(m.files, id)
	}
	return nil
}

func (m *memTrashRepo) CountBlobRefs(blobID uuid.UUID) (int64, error) {
	var count int64
	for _, v := range m.versions {
		if v.ObjectBlobID == blobID {
			count++
		}
	}
	return count, nil
}

func (m *memTrashRepo) GetBlob(id uuid.UUID) (ObjectBlob, error) {
	b, ok := m.blobs[id]
	if !ok {
		return ObjectBlob{}, ErrNotFound
	}
	return b, nil
}

func (m *memTrashRepo) DecrementBlobBy(id uuid.UUID, n int64) error {
	b, ok := m.blobs[id]
	if !ok {
		return ErrNotFound
	}
	b.RefCount -= n
	if b.RefCount < 0 {
		b.RefCount = 0
	}
	m.blobs[id] = b
	return nil
}

func (m *memTrashRepo) ZeroBlobAndMarkDeleting(id uuid.UUID) error {
	b, ok := m.blobs[id]
	if !ok {
		return ErrNotFound
	}
	b.RefCount = 0
	b.Status = BlobStatusDeleting
	m.blobs[id] = b
	return nil
}

// DeleteBlobRechecked 行锁复核的内存等价物：按当前（而非快照）状态复核
// status='deleting' 且 ref_count=0，通过后删物理对象并删行。
func (m *memTrashRepo) DeleteBlobRechecked(id uuid.UUID, deleteObject func(string) error) (bool, error) {
	b, ok := m.blobs[id]
	if !ok || b.Status != BlobStatusDeleting || b.RefCount != 0 {
		return false, nil
	}
	if err := deleteObject(b.StorageKey); err != nil {
		return false, err
	}
	delete(m.blobs, id)
	return true, nil
}

// addFile 添加文件行；deleted 表示软删除时间（nil 为未删除）。
func (m *memTrashRepo) addFile(owner uuid.UUID, parent *uuid.UUID, name, kind string, isRoot bool, deleted *time.Time) File {
	f := File{ID: uuid.New(), Name: name, ParentID: parent, OwnerID: owner, Type: kind, IsRoot: isRoot, ScopeType: "personal", DeletedAt: deleted}
	m.files[f.ID] = f
	return f
}

// addVersion 为文件添加指向 blob 的版本，并可选设为当前版本。
func (m *memTrashRepo) addVersion(fileID, blobID uuid.UUID, set bool) FileVersion {
	v := FileVersion{ID: uuid.New(), FileID: fileID, Version: 1, ObjectBlobID: blobID, UserID: uuid.New()}
	m.versions[v.ID] = v
	if set {
		f := m.files[fileID]
		f.CurrentVersionID = &v.ID
		m.files[fileID] = f
	}
	return v
}

func ptrID(id uuid.UUID) *uuid.UUID { return &id }

// allowAll 直通授权回调：逻辑层单测只关注恢复/删除语义（授权由 Store 层
// authorizeFileWrite/authorizeTeamDelete 回调注入，另见 folder_access_test）。
func allowAll(File) error { return nil }

// ---- restoreLogic ----

func TestRestoreRejectsWhenParentDeleted(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	now := time.Now()
	parent := repo.addFile(owner, nil, "parent", "folder", false, &now)
	child := repo.addFile(owner, ptrID(parent.ID), "doc.txt", "file", false, &now)

	_, err := restoreLogic(repo, child.ID, allowAll)
	if err != ErrParentDeleted {
		t.Fatalf("expected ErrParentDeleted, got %v", err)
	}
	// 不静默改名/移动：文件保持原名原父且仍处于软删除状态。
	got, _ := repo.GetAny(child.ID)
	if got.DeletedAt == nil || got.Name != "doc.txt" || got.ParentID == nil || *got.ParentID != parent.ID {
		t.Fatalf("file was mutated during failed restore: %+v", got)
	}
}

func TestRestoreRejectsNameConflict(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()
	deleted := repo.addFile(owner, ptrID(root.ID), "a.txt", "file", false, &now)
	repo.addFile(owner, ptrID(root.ID), "A.txt", "file", false, nil) // 同名活跃文件（大小写不敏感）

	_, err := restoreLogic(repo, deleted.ID, allowAll)
	if err != ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	got, _ := repo.GetAny(deleted.ID)
	if got.DeletedAt == nil || got.Name != "a.txt" {
		t.Fatalf("file was renamed or restored on conflict: %+v", got)
	}
}

func TestRestoreSuccess(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()
	deleted := repo.addFile(owner, ptrID(root.ID), "b.txt", "file", false, &now)

	f, err := restoreLogic(repo, deleted.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if f.DeletedAt != nil {
		t.Fatal("restored file must have deleted_at cleared")
	}
}

func TestRestoreOnlyDeletedAndNeverRoot(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	active := repo.addFile(owner, nil, "active", "file", false, nil)
	if _, err := restoreLogic(repo, active.ID, allowAll); err != ErrNotDeleted {
		t.Fatalf("restore active file: expected ErrNotDeleted, got %v", err)
	}
	now := time.Now()
	root := repo.addFile(owner, nil, "root", "folder", true, &now)
	if _, err := restoreLogic(repo, root.ID, allowAll); err != ErrRoot {
		t.Fatalf("restore deleted root: expected ErrRoot, got %v", err)
	}
	if _, err := restoreLogic(repo, uuid.New(), allowAll); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ---- purgeLogic ----

func TestPurgeRejectsActiveAndRoot(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	active := repo.addFile(owner, nil, "active", "file", false, nil)
	if _, _, _, err := purgeLogic(repo, active.ID, allowAll); err != ErrNotDeleted {
		t.Fatalf("purge active file: expected ErrNotDeleted, got %v", err)
	}
	now := time.Now()
	root := repo.addFile(owner, nil, "root", "folder", true, &now)
	if _, _, _, err := purgeLogic(repo, root.ID, allowAll); err != ErrRoot {
		t.Fatalf("purge root: expected ErrRoot, got %v", err)
	}
}

func TestPurgeSharedBlobOnlyDecrementsRefCount(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()

	// 同一 blob 被两个文件引用（ref_count=2）。
	blob := ObjectBlob{ID: uuid.New(), SHA256: "a", StorageKey: "objects/shared", RefCount: 2, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob
	f1 := repo.addFile(owner, ptrID(root.ID), "one.txt", "file", false, &now)
	f2 := repo.addFile(owner, ptrID(root.ID), "two.txt", "file", false, nil)
	repo.addVersion(f1.ID, blob.ID, true)
	repo.addVersion(f2.ID, blob.ID, true)

	purged, deleting, _, err := purgeLogic(repo, f1.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0].ID != f1.ID {
		t.Fatalf("purged files = %+v", purged)
	}
	if len(deleting) != 0 {
		t.Fatalf("shared blob must not be scheduled for deletion: %+v", deleting)
	}
	got, _ := repo.GetBlob(blob.ID)
	if got.RefCount != 1 || got.Status != BlobStatusAvailable {
		t.Fatalf("shared blob after purge = %+v, want ref_count=1 status=available", got)
	}
	// 仍被引用的文件 f2 与其版本不受影响。
	if _, err := repo.GetAny(f2.ID); err != nil {
		t.Fatal("referencing file must survive purge of sibling")
	}
	if n, _ := repo.CountBlobRefs(blob.ID); n != 1 {
		t.Fatalf("remaining refs = %d, want 1", n)
	}
}

func TestPurgeLastReferenceMarksBlobDeleting(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()

	blob := ObjectBlob{ID: uuid.New(), SHA256: "b", StorageKey: "objects/last", RefCount: 1, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob
	f := repo.addFile(owner, ptrID(root.ID), "only.txt", "file", false, &now)
	repo.addVersion(f.ID, blob.ID, true)

	purged, deleting, _, err := purgeLogic(repo, f.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 {
		t.Fatalf("purged = %+v", purged)
	}
	if len(deleting) != 1 || deleting[0].ID != blob.ID || deleting[0].RefCount != 0 || deleting[0].Status != BlobStatusDeleting {
		t.Fatalf("deleting = %+v", deleting)
	}
	if _, err := repo.GetAny(f.ID); err != ErrNotFound {
		t.Fatal("file row must be hard-deleted")
	}
	if n, _ := repo.CountBlobRefs(blob.ID); n != 0 {
		t.Fatalf("versions must be removed, got %d refs", n)
	}
}

func TestPurgeMultipleVersionsSameBlobZeroesRefCount(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()

	// 同一文件的 2 个版本都指向同一 blob（ref_count=2）。
	blob := ObjectBlob{ID: uuid.New(), SHA256: "g", StorageKey: "objects/multi", RefCount: 2, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob
	f := repo.addFile(owner, ptrID(root.ID), "v.txt", "file", false, &now)
	repo.addVersion(f.ID, blob.ID, true)
	repo.addVersion(f.ID, blob.ID, false)

	purged, deleting, _, err := purgeLogic(repo, f.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || len(deleting) != 1 {
		t.Fatalf("purged=%d deleting=%d", len(purged), len(deleting))
	}
	got, _ := repo.GetBlob(blob.ID)
	if got.RefCount != 0 || got.Status != BlobStatusDeleting {
		t.Fatalf("blob = %+v, want ref_count=0 status=deleting (multi-version decrement must fully drain)", got)
	}
}

func TestPurgeRemovesAllDescendants(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()

	dir := repo.addFile(owner, ptrID(root.ID), "dir", "folder", false, &now)
	sub := repo.addFile(owner, ptrID(dir.ID), "sub", "folder", false, nil) // 未单独软删除，随父目录一并处理
	file := repo.addFile(owner, ptrID(sub.ID), "deep.txt", "file", false, nil)
	blob := ObjectBlob{ID: uuid.New(), SHA256: "c", StorageKey: "objects/deep", RefCount: 1, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob
	repo.addVersion(file.ID, blob.ID, true)

	purged, deleting, _, err := purgeLogic(repo, dir.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 3 {
		t.Fatalf("expected 3 purged rows (dir/sub/file), got %d", len(purged))
	}
	for _, f := range purged {
		if _, err := repo.GetAny(f.ID); err != ErrNotFound {
			t.Fatalf("descendant %s still exists", f.ID)
		}
	}
	if len(deleting) != 1 || deleting[0].ID != blob.ID {
		t.Fatalf("deleting = %+v", deleting)
	}
}

func TestPurgeBlobsDeletesOnlyUnreferenced(t *testing.T) {
	repo := newMemTrashRepo()

	zero := ObjectBlob{ID: uuid.New(), SHA256: "d", StorageKey: "objects/zero", RefCount: 0, Status: BlobStatusDeleting}
	stillRef := ObjectBlob{ID: uuid.New(), SHA256: "e", StorageKey: "objects/ref", RefCount: 1, Status: BlobStatusDeleting}
	available := ObjectBlob{ID: uuid.New(), SHA256: "f", StorageKey: "objects/avail", RefCount: 0, Status: BlobStatusAvailable}
	repo.blobs[zero.ID] = zero
	repo.blobs[stillRef.ID] = stillRef
	repo.blobs[available.ID] = available

	var deletedKeys []string
	if err := purgeBlobsLogic(repo, []ObjectBlob{zero, stillRef, available}, func(key string) error {
		deletedKeys = append(deletedKeys, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(deletedKeys) != 1 || deletedKeys[0] != zero.StorageKey {
		t.Fatalf("physical deletes = %v, want only %q", deletedKeys, zero.StorageKey)
	}
	if _, err := repo.GetBlob(zero.ID); err != ErrNotFound {
		t.Fatal("unreferenced blob row must be removed")
	}
	if _, err := repo.GetBlob(stillRef.ID); err != nil {
		t.Fatal("referenced blob row must survive")
	}
	if _, err := repo.GetBlob(available.ID); err != nil {
		t.Fatal("available blob row must survive")
	}
}

// TestPurgeFKOrder：current_version_id 外键必须先于 file_versions 清除
// （顺序错误时 PostgreSQL 拒绝删除版本行）。
func TestPurgeFKOrder(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()
	blob := ObjectBlob{ID: uuid.New(), SHA256: "o", StorageKey: "objects/order", RefCount: 1, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob
	f := repo.addFile(owner, ptrID(root.ID), "ordered.txt", "file", false, &now)
	repo.addVersion(f.ID, blob.ID, true)

	if _, _, _, err := purgeLogic(repo, f.ID, allowAll); err != nil {
		t.Fatal(err)
	}
	want := []string{"clear_current_versions", "delete_versions", "delete_files"}
	if len(repo.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", repo.calls, want)
	}
	for i, c := range want {
		if repo.calls[i] != c {
			t.Fatalf("call order = %v, want %v (clear current_version_id before deleting versions)", repo.calls, want)
		}
	}
}

// TestPurgeBlobsRecheckSkipsResurrected：快照为 deleting/ref=0，复核前行已被
// AddVersion 复活（available/ref=1）→ 不删物理对象、保留行、不报错。
func TestPurgeBlobsRecheckSkipsResurrected(t *testing.T) {
	repo := newMemTrashRepo()
	snapshot := ObjectBlob{ID: uuid.New(), SHA256: "r", StorageKey: "objects/raced", RefCount: 0, Status: BlobStatusDeleting}
	repo.blobs[snapshot.ID] = snapshot
	// 复核前竞速：行已复活为 available/ref=1。
	repo.blobs[snapshot.ID] = ObjectBlob{ID: snapshot.ID, SHA256: "r", StorageKey: "objects/raced", RefCount: 1, Status: BlobStatusAvailable}

	var deletedKeys []string
	if err := purgeBlobsLogic(repo, []ObjectBlob{snapshot}, func(key string) error {
		deletedKeys = append(deletedKeys, key)
		return nil
	}); err != nil {
		t.Fatalf("resurrected blob must be skipped without error: %v", err)
	}
	if len(deletedKeys) != 0 {
		t.Fatalf("physical deletes = %v, want none (blob resurrected between snapshot and recheck)", deletedKeys)
	}
	got, err := repo.GetBlob(snapshot.ID)
	if err != nil || got.Status != BlobStatusAvailable || got.RefCount != 1 {
		t.Fatalf("blob = %+v, %v; want resurrected row preserved", got, err)
	}
}

// TestPurgeReturnsWebpkgPrefixes：purge 返回被删文件关联网页包的对象前缀，
// 供事务提交后经 SetWebpkgCleaner 清理（未挂包的文件无前缀）。
func TestPurgeReturnsWebpkgPrefixes(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	root := repo.addFile(owner, nil, "root", "folder", true, nil)
	now := time.Now()
	blob := ObjectBlob{ID: uuid.New(), SHA256: "w", StorageKey: "objects/webpkg", RefCount: 1, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob

	plain := repo.addFile(owner, ptrID(root.ID), "plain.txt", "file", false, &now)
	packaged := repo.addFile(owner, ptrID(root.ID), "site.zip", "file", false, &now)
	repo.addVersion(plain.ID, blob.ID, true)
	repo.addVersion(packaged.ID, blob.ID, true)
	repo.webpkgs[packaged.ID] = "pub123"

	_, _, prefixes, err := purgeLogic(repo, packaged.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 1 || prefixes[0] != "webpkg/pub123" {
		t.Fatalf("webpkg prefixes = %v, want [webpkg/pub123]", prefixes)
	}

	_, _, prefixes, err = purgeLogic(repo, plain.ID, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 0 {
		t.Fatalf("webpkg prefixes = %v, want none for plain file", prefixes)
	}
}
