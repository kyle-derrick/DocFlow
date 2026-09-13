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
}

func newMemTrashRepo() *memTrashRepo {
	return &memTrashRepo{files: make(map[uuid.UUID]File), versions: make(map[uuid.UUID]FileVersion), blobs: make(map[uuid.UUID]ObjectBlob)}
}

func (m *memTrashRepo) GetAny(owner, id uuid.UUID) (File, error) {
	f, ok := m.files[id]
	if !ok || f.OwnerID != owner {
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

func (m *memTrashRepo) DeleteVersions(fileIDs []uuid.UUID) error {
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
	for _, fid := range fileIDs {
		if f, ok := m.files[fid]; ok {
			f.CurrentVersionID = nil
			m.files[fid] = f
		}
	}
	return nil
}

func (m *memTrashRepo) DeleteFiles(ids []uuid.UUID) error {
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

func (m *memTrashRepo) DeleteBlobRowIfUnreferenced(id uuid.UUID) (bool, error) {
	b, ok := m.blobs[id]
	if !ok || b.RefCount != 0 || b.Status != BlobStatusDeleting {
		return false, nil
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

// ---- restoreLogic ----

func TestRestoreRejectsWhenParentDeleted(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	now := time.Now()
	parent := repo.addFile(owner, nil, "parent", "folder", false, &now)
	child := repo.addFile(owner, ptrID(parent.ID), "doc.txt", "file", false, &now)

	_, err := restoreLogic(repo, owner, child.ID)
	if err != ErrParentDeleted {
		t.Fatalf("expected ErrParentDeleted, got %v", err)
	}
	// 不静默改名/移动：文件保持原名原父且仍处于软删除状态。
	got, _ := repo.GetAny(owner, child.ID)
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

	_, err := restoreLogic(repo, owner, deleted.ID)
	if err != ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
	got, _ := repo.GetAny(owner, deleted.ID)
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

	f, err := restoreLogic(repo, owner, deleted.ID)
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
	if _, err := restoreLogic(repo, owner, active.ID); err != ErrNotDeleted {
		t.Fatalf("restore active file: expected ErrNotDeleted, got %v", err)
	}
	now := time.Now()
	root := repo.addFile(owner, nil, "root", "folder", true, &now)
	if _, err := restoreLogic(repo, owner, root.ID); err != ErrRoot {
		t.Fatalf("restore deleted root: expected ErrRoot, got %v", err)
	}
	if _, err := restoreLogic(repo, owner, uuid.New()); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ---- purgeLogic ----

func TestPurgeRejectsActiveAndRoot(t *testing.T) {
	repo := newMemTrashRepo()
	owner := uuid.New()
	active := repo.addFile(owner, nil, "active", "file", false, nil)
	if _, _, err := purgeLogic(repo, owner, active.ID); err != ErrNotDeleted {
		t.Fatalf("purge active file: expected ErrNotDeleted, got %v", err)
	}
	now := time.Now()
	root := repo.addFile(owner, nil, "root", "folder", true, &now)
	if _, _, err := purgeLogic(repo, owner, root.ID); err != ErrRoot {
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

	purged, deleting, err := purgeLogic(repo, owner, f1.ID)
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
	if _, err := repo.GetAny(owner, f2.ID); err != nil {
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

	purged, deleting, err := purgeLogic(repo, owner, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 {
		t.Fatalf("purged = %+v", purged)
	}
	if len(deleting) != 1 || deleting[0].ID != blob.ID || deleting[0].RefCount != 0 || deleting[0].Status != BlobStatusDeleting {
		t.Fatalf("deleting = %+v", deleting)
	}
	if _, err := repo.GetAny(owner, f.ID); err != ErrNotFound {
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

	purged, deleting, err := purgeLogic(repo, owner, f.ID)
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

	purged, deleting, err := purgeLogic(repo, owner, dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 3 {
		t.Fatalf("expected 3 purged rows (dir/sub/file), got %d", len(purged))
	}
	for _, f := range purged {
		if _, err := repo.GetAny(owner, f.ID); err != ErrNotFound {
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
