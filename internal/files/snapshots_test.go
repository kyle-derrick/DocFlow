package files

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type snapshotVersionRepo struct {
	*memVersionsRepo
	protected map[uuid.UUID]bool
}

func (r *snapshotVersionRepo) SnapshotVersionRefs(id uuid.UUID) (int64, error) {
	if r.protected[id] {
		return 1, nil
	}
	return 0, nil
}
func TestSnapshotVersionProtection(t *testing.T) {
	repo := &snapshotVersionRepo{memVersionsRepo: newMemVersionsRepo(), protected: map[uuid.UUID]bool{}}
	user := uuid.New()
	f, v1, _ := repo.seedFile(user, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, _, err := addVersionLogic(repo, f.ID, "new", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 3, "text/plain", user)
	if err != nil {
		t.Fatal(err)
	}
	repo.protected[v1.ID] = true
	if _, err := deleteVersionLogic(repo, f.ID, v1.ID); !errors.Is(err, ErrSnapshotConflict) {
		t.Fatalf("delete protected version: %v", err)
	}
	count, err := pruneVersionsLogic(repo, f.ID, 1, time.Time{})
	if err != nil || count != 0 {
		t.Fatalf("prune protected version: %d %v", count, err)
	}
	if _, ok := repo.versions[v1.ID]; !ok {
		t.Fatal("snapshot version removed")
	}
}
func TestSnapshotDiffPaths(t *testing.T) {
	id := uuid.New()
	snapshot := snapshotMap([]DirectorySnapshotEntry{{RelativePath: "old.txt", NodeType: "file", SourceVersionID: &id}, {RelativePath: "same", NodeType: "folder"}})
	current := snapshotMap([]DirectorySnapshotEntry{{RelativePath: "new.txt", NodeType: "file"}, {RelativePath: "same", NodeType: "folder"}})
	if len(snapshot) != 2 || len(current) != 2 || snapshot["old.txt"].SourceVersionID == nil {
		t.Fatal("snapshot path map missing entries")
	}
}
