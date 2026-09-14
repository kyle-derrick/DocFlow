package files

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// memVersionsRepo 是 versionsRepo 的内存实现（模式参照 memTrashRepo），
// 用于验证版本追加/裁剪/回滚的引用计数与保留策略语义。
type memVersionsRepo struct {
	files    map[uuid.UUID]File
	versions map[uuid.UUID]FileVersion
	blobs    map[uuid.UUID]ObjectBlob
}

func newMemVersionsRepo() *memVersionsRepo {
	return &memVersionsRepo{files: make(map[uuid.UUID]File), versions: make(map[uuid.UUID]FileVersion), blobs: make(map[uuid.UUID]ObjectBlob)}
}

func (m *memVersionsRepo) GetFileForUpdate(id uuid.UUID) (File, error) {
	f, ok := m.files[id]
	if !ok || f.Type != "file" || f.DeletedAt != nil {
		return File{}, ErrNotFound
	}
	return f, nil
}

func (m *memVersionsRepo) GetBlobBySHA(sha256 string) (ObjectBlob, error) {
	for _, b := range m.blobs {
		if b.SHA256 == sha256 {
			return b, nil
		}
	}
	return ObjectBlob{}, ErrNotFound
}

func (m *memVersionsRepo) IncrementBlobRef(id uuid.UUID) error {
	b, ok := m.blobs[id]
	if !ok {
		return ErrNotFound
	}
	b.RefCount++
	m.blobs[id] = b
	return nil
}

func (m *memVersionsRepo) ResurrectBlob(id uuid.UUID) (bool, error) {
	b, ok := m.blobs[id]
	if !ok || b.Status != BlobStatusDeleting || b.RefCount != 0 {
		return false, nil
	}
	b.Status = BlobStatusAvailable
	b.RefCount = 1
	m.blobs[id] = b
	return true, nil
}

func (m *memVersionsRepo) CreateBlob(b ObjectBlob) error {
	m.blobs[b.ID] = b
	return nil
}

func (m *memVersionsRepo) NextVersionNumber(fileID uuid.UUID) (int, error) {
	next := 1
	for _, v := range m.versions {
		if v.FileID == fileID && v.Version >= next {
			next = v.Version + 1
		}
	}
	return next, nil
}

func (m *memVersionsRepo) CreateVersion(v FileVersion) error {
	// (file_id, version) 唯一约束的内存等价物。
	for _, e := range m.versions {
		if e.FileID == v.FileID && e.Version == v.Version {
			return errors.New("unique constraint: file_versions(file_id, version)")
		}
	}
	m.versions[v.ID] = v
	return nil
}

func (m *memVersionsRepo) SetCurrentVersion(fileID, versionID uuid.UUID) error {
	f, ok := m.files[fileID]
	if !ok {
		return ErrNotFound
	}
	f.CurrentVersionID = &versionID
	m.files[fileID] = f
	return nil
}

func (m *memVersionsRepo) GetVersion(id uuid.UUID) (FileVersion, error) {
	v, ok := m.versions[id]
	if !ok {
		return FileVersion{}, ErrNotFound
	}
	return v, nil
}

func (m *memVersionsRepo) ListVersionsDesc(fileID uuid.UUID) ([]FileVersion, error) {
	var out []FileVersion
	for _, v := range m.versions {
		if v.FileID == fileID {
			out = append(out, v)
		}
	}
	// 版本号倒序（数量小，插入排序足够）。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Version > out[j-1].Version; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (m *memVersionsRepo) DeleteVersion(id uuid.UUID) error {
	delete(m.versions, id)
	return nil
}

func (m *memVersionsRepo) DecrementBlobRef(id uuid.UUID) error {
	b, ok := m.blobs[id]
	if !ok {
		return ErrNotFound
	}
	if b.RefCount > 0 {
		b.RefCount--
	}
	m.blobs[id] = b
	return nil
}

func (m *memVersionsRepo) MarkBlobDeletingIfZero(id uuid.UUID) (bool, error) {
	b, ok := m.blobs[id]
	if !ok || b.RefCount != 0 {
		return false, nil
	}
	b.Status = BlobStatusDeleting
	m.blobs[id] = b
	return true, nil
}

// seedFile 建一个有 v1（指向 sha 对应 blob）的文件并设为 current。
func (m *memVersionsRepo) seedFile(owner uuid.UUID, sha string) (File, FileVersion, ObjectBlob) {
	blob := ObjectBlob{ID: uuid.New(), SHA256: sha, StorageKey: "objects/" + sha, Size: 3, MimeType: "text/plain", RefCount: 1, Status: BlobStatusAvailable}
	m.blobs[blob.ID] = blob
	f := File{ID: uuid.New(), Name: "doc.txt", OwnerID: owner, Type: "file", ScopeType: "personal"}
	m.files[f.ID] = f
	v := FileVersion{ID: uuid.New(), FileID: f.ID, Version: 1, ObjectBlobID: blob.ID, ContentSHA256: sha, Size: 3, UserID: owner}
	m.versions[v.ID] = v
	m.SetCurrentVersion(f.ID, v.ID)
	return f, v, blob
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	shaE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	shaF = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	shaG = "gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg"
	shaH = "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh"
)

func TestAddVersionCreatesNewBlobAndIncrementsVersion(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f, v1, blob1 := repo.seedFile(owner, shaA)

	v2, newBlob, err := addVersionLogic(repo, f.ID, "objects/new", shaB, 4, "text/plain", owner)
	if err != nil {
		t.Fatal(err)
	}
	if !newBlob {
		t.Fatal("distinct sha must create a new blob")
	}
	if v2.Version != 2 {
		t.Fatalf("version = %d, want 2", v2.Version)
	}
	got := repo.files[f.ID]
	if got.CurrentVersionID == nil || *got.CurrentVersionID != v2.ID {
		t.Fatalf("current_version_id = %v, want new version %s", got.CurrentVersionID, v2.ID)
	}
	// 旧 blob/版本不受影响（版本不可变）。
	if repo.blobs[blob1.ID].RefCount != 1 {
		t.Fatalf("old blob ref_count changed: %+v", repo.blobs[blob1.ID])
	}
	if repo.versions[v1.ID].Version != 1 {
		t.Fatal("existing version must stay immutable")
	}
	nb := repo.blobs[v2.ObjectBlobID]
	if nb.RefCount != 1 || nb.Status != BlobStatusAvailable || nb.SHA256 != shaB {
		t.Fatalf("new blob = %+v", nb)
	}
	if nb.ID == blob1.ID {
		t.Fatal("new version must not reuse distinct-sha blob")
	}
}

func TestAddVersionReusesAvailableBlobAndIncrementsRef(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f, _, blob1 := repo.seedFile(owner, shaA)

	// 同文件、同内容再次上传：复用 blob，ref_count 1→2。
	v2, newBlob, err := addVersionLogic(repo, f.ID, "objects/ignored", shaA, 3, "text/plain", owner)
	if err != nil {
		t.Fatal(err)
	}
	if newBlob {
		t.Fatal("same-sha upload must reuse the available blob")
	}
	if v2.ObjectBlobID != blob1.ID {
		t.Fatalf("reused blob = %s, want %s", v2.ObjectBlobID, blob1.ID)
	}
	if got := repo.blobs[blob1.ID].RefCount; got != 2 {
		t.Fatalf("ref_count = %d, want 2", got)
	}
	if len(repo.blobs) != 1 {
		t.Fatalf("blob rows = %d, want 1（去重）", len(repo.blobs))
	}

	// 跨文件复用：另一文件上传相同内容。
	f2 := File{ID: uuid.New(), Name: "copy.txt", OwnerID: owner, Type: "file", ScopeType: "personal"}
	repo.files[f2.ID] = f2
	v3, newBlob, err := addVersionLogic(repo, f2.ID, "objects/ignored2", shaA, 3, "text/plain", owner)
	if err != nil {
		t.Fatal(err)
	}
	if newBlob || v3.ObjectBlobID != blob1.ID {
		t.Fatalf("cross-file reuse failed: newBlob=%v blob=%s", newBlob, v3.ObjectBlobID)
	}
	if got := repo.blobs[blob1.ID].RefCount; got != 3 {
		t.Fatalf("ref_count = %d, want 3", got)
	}
}

func TestAddVersionDoesNotReuseNonAvailableBlob(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f, _, blob1 := repo.seedFile(owner, shaA)
	// 同 sha 但隔离中的 blob：不可复用，且 sha 唯一约束阻止新建 → 上传失败。
	quarantined := repo.blobs[blob1.ID]
	quarantined.Status = BlobStatusQuarantined
	repo.blobs[blob1.ID] = quarantined

	if _, _, err := addVersionLogic(repo, f.ID, "objects/fresh", shaA, 3, "text/plain", owner); err != ErrBlobUnavailable {
		t.Fatalf("quarantined blob: err = %v, want ErrBlobUnavailable", err)
	}
	// deleting 且 ref_count=0（裁剪遗留）：复活复用同一行。
	f2, _, blobB := repo.seedFile(owner, shaB)
	if _, _, err := addVersionLogic(repo, f2.ID, "objects/c", shaC, 3, "text/plain", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pruneVersionsLogic(repo, f2.ID, 1, time.Time{}); err != nil { // 裁掉 v1(shaB)，current=v2 受保护
		t.Fatal(err)
	}
	if b := repo.blobs[blobB.ID]; b.Status != BlobStatusDeleting || b.RefCount != 0 {
		t.Fatalf("precondition failed: blob = %+v, want deleting/0", b)
	}
	v2, newBlob, err := addVersionLogic(repo, f2.ID, "ignored", shaB, 3, "text/plain", owner)
	if err != nil {
		t.Fatal(err)
	}
	if newBlob {
		t.Fatal("deleting blob with zero refs must be resurrected, not duplicated")
	}
	if v2.ObjectBlobID != blobB.ID {
		t.Fatalf("resurrect must reuse the same row: %s != %s", v2.ObjectBlobID, blobB.ID)
	}
	b := repo.blobs[blobB.ID]
	if b.Status != BlobStatusAvailable || b.RefCount != 1 {
		t.Fatalf("resurrected blob = %+v", b)
	}
	if len(repo.blobs) != 3 { // shaA + shaB(复活) + shaC
		t.Fatalf("blob rows = %d, want 3", len(repo.blobs))
	}
}

func TestAddVersionVersionNumbersIncrementAcrossAdds(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f, _, _ := repo.seedFile(owner, shaA)
	for i, sha := range []string{shaB, shaC, shaD} {
		v, _, err := addVersionLogic(repo, f.ID, "objects/"+sha, sha, 3, "text/plain", owner)
		if err != nil {
			t.Fatal(err)
		}
		if want := i + 2; v.Version != want {
			t.Fatalf("add #%d: version = %d, want %d", i+1, v.Version, want)
		}
	}
	versions, _ := repo.ListVersionsDesc(f.ID)
	if len(versions) != 4 || versions[0].Version != 4 || versions[3].Version != 1 {
		t.Fatalf("versions = %+v", versions)
	}
}

func TestAddVersionRejectsFolderAndDeletedFile(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	folder := File{ID: uuid.New(), Name: "dir", OwnerID: owner, Type: "folder", ScopeType: "personal"}
	repo.files[folder.ID] = folder
	if _, _, err := addVersionLogic(repo, folder.ID, "k", shaB, 1, "text/plain", owner); err != ErrNotFound {
		t.Fatalf("folder target: err = %v, want ErrNotFound", err)
	}
	f, _, _ := repo.seedFile(owner, shaA)
	deleted := repo.files[f.ID]
	now := deleted.CreatedAt
	deleted.DeletedAt = &now
	repo.files[f.ID] = deleted
	if _, _, err := addVersionLogic(repo, f.ID, "k", shaB, 1, "text/plain", owner); err != ErrNotFound {
		t.Fatalf("deleted target: err = %v, want ErrNotFound", err)
	}
}

func TestPruneVersionsKeepsLatestAndProtectsCurrent(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f, v1, blob1 := repo.seedFile(owner, shaA)
	// 再加 6 个版本（v2..v7），每个独立 blob。
	for _, sha := range []string{shaB, shaC, shaD, shaE, shaF, shaG} {
		if _, _, err := addVersionLogic(repo, f.ID, "objects/"+sha, sha, 3, "text/plain", owner); err != nil {
			t.Fatal(err)
		}
	}
	versions, _ := repo.ListVersionsDesc(f.ID)
	if len(versions) != 7 {
		t.Fatalf("seed versions = %d, want 7", len(versions))
	}

	pruned, err := pruneVersionsLogic(repo, f.ID, 5, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	after, _ := repo.ListVersionsDesc(f.ID)
	if len(after) != 5 || after[0].Version != 7 || after[4].Version != 3 {
		t.Fatalf("kept versions = %+v, want v7..v3", after)
	}
	// v1/v2 的 blob 归零 → 标 deleting（物理删除延后由 janitor 处理，此处只标状态）。
	if b := repo.blobs[blob1.ID]; b.RefCount != 0 || b.Status != BlobStatusDeleting {
		t.Fatalf("pruned v1 blob = %+v, want ref_count=0 status=deleting", b)
	}
	if _, err := repo.GetVersion(v1.ID); err != ErrNotFound {
		t.Fatal("pruned version row must be removed")
	}

	// current 保护：再加 2 个版本（v8/v9，current 随之指向 v9）后回滚 current 到 v3，
	// v3 落在保留窗口外，但 current 指向的版本裁剪时必须保留。
	for _, sha := range []string{shaH, shaA} {
		if _, _, err := addVersionLogic(repo, f.ID, "objects/x"+sha, sha, 3, "text/plain", owner); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := setCurrentVersionLogic(repo, f.ID, after[4].ID); err != nil {
		t.Fatal(err)
	}
	// 现在 7 个版本（v3..v9），keep=5 → 保留窗口 v9..v5；窗口外 v4 被裁，
	// v3 是 current 受保护 → 最终 6 个版本，pruned=1。
	pruned, err = pruneVersionsLogic(repo, f.ID, 5, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	final, _ := repo.ListVersionsDesc(f.ID)
	currentID := repo.files[f.ID].CurrentVersionID
	if currentID == nil || *currentID != after[4].ID {
		t.Fatalf("current = %v, want v3", currentID)
	}
	found := false
	for _, v := range final {
		if v.ID == *currentID {
			found = true
		}
	}
	if !found {
		t.Fatalf("current-pointed version was pruned: %+v", final)
	}
	if len(final) != 6 {
		t.Fatalf("final versions = %d, want 6 (keep 5 + current 受保护)", len(final))
	}
	if pruned != 1 {
		t.Fatalf("second prune = %d, want 1", pruned)
	}
	// v9 与被裁的 v1 内容相同（shaA）：其 blob 行在首次裁剪后处于 deleting/0，
	// 本次 AddVersion 应复活该行（ref=1/available）而非另起新行。
	if b := repo.blobs[blob1.ID]; b.RefCount != 1 || b.Status != BlobStatusAvailable {
		t.Fatalf("shaA blob after re-upload = %+v, want ref_count=1 available（复活）", b)
	}
}

func TestPruneVersionsSharedBlobOnlyDecrements(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f, _, blob1 := repo.seedFile(owner, shaA)
	// v2 复用 v1 的 blob（同内容覆盖上传）。
	if _, _, err := addVersionLogic(repo, f.ID, "ignored", shaA, 3, "text/plain", owner); err != nil {
		t.Fatal(err)
	}
	if got := repo.blobs[blob1.ID].RefCount; got != 2 {
		t.Fatalf("ref_count = %d, want 2", got)
	}
	// keep=1：裁掉 v1，blob 仍被 v2 引用 → 只递减、绝不标记 deleting。
	pruned, err := pruneVersionsLogic(repo, f.ID, 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	b := repo.blobs[blob1.ID]
	if b.RefCount != 1 || b.Status != BlobStatusAvailable {
		t.Fatalf("shared blob = %+v, want ref_count=1 available", b)
	}
}

func TestSetCurrentVersionLogicRejectsForeignVersion(t *testing.T) {
	repo := newMemVersionsRepo()
	owner := uuid.New()
	f1, v1, _ := repo.seedFile(owner, shaA)
	f2 := File{ID: uuid.New(), Name: "other.txt", OwnerID: owner, Type: "file", ScopeType: "personal"}
	repo.files[f2.ID] = f2

	if _, err := setCurrentVersionLogic(repo, f2.ID, v1.ID); err != ErrNotFileVersion {
		t.Fatalf("foreign version: err = %v, want ErrNotFileVersion", err)
	}
	if _, err := setCurrentVersionLogic(repo, f1.ID, uuid.New()); err != ErrNotFound {
		t.Fatalf("missing version: err = %v, want ErrNotFound", err)
	}
	if _, err := setCurrentVersionLogic(repo, f1.ID, v1.ID); err != nil {
		t.Fatal(err)
	}
	if got := repo.files[f1.ID].CurrentVersionID; got == nil || *got != v1.ID {
		t.Fatalf("current = %v, want %s", got, v1.ID)
	}
}

// TestAuthorizeFileWriteMatrix 覆盖版本写入（追加/回滚）授权矩阵：
// 个人文件仅 owner；团队文件 owner/editor 可写，viewer 与非成员 403；
// 未注入 TeamWriter 时团队文件一律拒绝。
func TestAuthorizeFileWriteMatrix(t *testing.T) {
	creator, other := uuid.New(), uuid.New()
	teamA := uuid.New()

	personalFile := File{ID: uuid.New(), Name: "own.txt", OwnerID: creator, Type: "file", ScopeType: "personal"}
	teamFile := File{ID: uuid.New(), Name: "team.txt", OwnerID: creator, Type: "file", ScopeType: "team", TeamID: &teamA}

	editor := uuid.New() // teamA editor
	viewer := uuid.New() // teamA viewer（只读成员）

	writer := fakeTeamWriter(map[uuid.UUID][]uuid.UUID{
		creator: {teamA},
		editor:  {teamA},
		// viewer/other 无写权限。
	})

	tests := []struct {
		name    string
		user    uuid.UUID
		file    File
		writer  TeamWriter
		wantErr error
	}{
		{"personal file by owner", creator, personalFile, writer, nil},
		{"personal file by other user", other, personalFile, writer, ErrNotFound},
		{"team file by creator (owner)", creator, teamFile, writer, nil},
		{"team file by editor", editor, teamFile, writer, nil},
		{"team file by viewer", viewer, teamFile, writer, ErrForbidden},
		{"team file by non-member", other, teamFile, writer, ErrForbidden},
		{"team file without writer injected", editor, teamFile, nil, ErrForbidden},
	}
	for _, tc := range tests {
		if err := authorizeFileWrite(tc.file, tc.user, tc.writer); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}
