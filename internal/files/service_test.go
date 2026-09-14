package files

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		input, want string
		valid       bool
	}{
		{"  cafe\u0301  ", "café", true}, {"", "", false}, {"a/b", "", false}, {"a\\b", "", false}, {"a\n", "", false}, {"report. ", "", false}, {"CON.txt", "", false}, {"normal.txt", "normal.txt", true},
		// unicode.Cf 格式字符拒绝：RLO（U+202E，视觉欺骗）、零宽空格（U+200B）、
		// 零宽连接符（U+200D）、BOM（U+FEFF）。
		{"\u202Eevil.txt", "", false}, {"a\u200Bb.txt", "", false}, {"join\u200Ded.txt", "", false}, {"\uFEFFreadme.txt", "", false},
		// 非常规但合法的 Unicode（组合类等）不受影响。
		{"café 中文.txt", "café 中文.txt", true},
	}
	for _, test := range tests {
		got, err := NormalizeName(test.input)
		if (err == nil) != test.valid || got != test.want {
			t.Errorf("NormalizeName(%q) = %q, %v", test.input, got, err)
		}
	}
}
func TestNormalizeNameRuneLimit(t *testing.T) {
	name := ""
	for i := 0; i < 256; i++ {
		name += "界"
	}
	if _, err := NormalizeName(name); err == nil {
		t.Fatal("expected 256 rune name to be rejected")
	}
}

// ---- CreateUploadedFile 的 blob 内容去重（resolveUploadBlobLogic） ----

// flakyResurrectRepo 模拟「复活时行恰好已被 janitor 删除」（ResurrectBlob 落空）。
type flakyResurrectRepo struct{ *memVersionsRepo }

func (f flakyResurrectRepo) ResurrectBlob(id uuid.UUID) (bool, error) {
	delete(f.blobs, id)
	return false, nil
}

func TestResolveUploadBlobReusesAvailable(t *testing.T) {
	repo := newMemVersionsRepo()
	blob := ObjectBlob{ID: uuid.New(), SHA256: "dup", StorageKey: "objects/old", RefCount: 1, Status: BlobStatusAvailable}
	repo.blobs[blob.ID] = blob

	got, newBlob, err := resolveUploadBlobLogic(repo, "objects/new-upload", "dup", 10, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	if newBlob {
		t.Fatal("available blob must be reused (newBlob=false)")
	}
	if got.ID != blob.ID || got.StorageKey != "objects/old" {
		t.Fatalf("blob = %+v, want reuse of existing row", got)
	}
	if b := repo.blobs[blob.ID]; b.RefCount != 2 {
		t.Fatalf("ref_count = %d, want 2", b.RefCount)
	}
}

func TestResolveUploadBlobResurrectsDeleting(t *testing.T) {
	repo := newMemVersionsRepo()
	blob := ObjectBlob{ID: uuid.New(), SHA256: "zombie", StorageKey: "objects/old", RefCount: 0, Status: BlobStatusDeleting}
	repo.blobs[blob.ID] = blob

	got, newBlob, err := resolveUploadBlobLogic(repo, "objects/new-upload", "zombie", 10, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	if newBlob || got.ID != blob.ID {
		t.Fatalf("blob = %+v newBlob=%v, want resurrected reuse", got, newBlob)
	}
	if b := repo.blobs[blob.ID]; b.Status != BlobStatusAvailable || b.RefCount != 1 {
		t.Fatalf("blob = %+v, want available ref_count=1", b)
	}
}

func TestResolveUploadBlobCreatesWhenResurrectLost(t *testing.T) {
	repo := newMemVersionsRepo()
	blob := ObjectBlob{ID: uuid.New(), SHA256: "gone", StorageKey: "objects/old", RefCount: 0, Status: BlobStatusDeleting}
	repo.blobs[blob.ID] = blob

	got, newBlob, err := resolveUploadBlobLogic(flakyResurrectRepo{repo}, "objects/new-upload", "gone", 10, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	if !newBlob || got.ID == blob.ID || got.StorageKey != "objects/new-upload" || got.RefCount != 1 || got.Status != BlobStatusAvailable {
		t.Fatalf("blob = %+v newBlob=%v, want fresh row on storage key of this upload", got, newBlob)
	}
}

func TestResolveUploadBlobCreatesWhenMissing(t *testing.T) {
	repo := newMemVersionsRepo()
	got, newBlob, err := resolveUploadBlobLogic(repo, "objects/fresh", "fresh", 10, "application/zip")
	if err != nil {
		t.Fatal(err)
	}
	if !newBlob || got.SHA256 != "fresh" || got.RefCount != 1 || got.Status != BlobStatusAvailable {
		t.Fatalf("blob = %+v newBlob=%v, want new available row", got, newBlob)
	}
	if len(repo.blobs) != 1 {
		t.Fatalf("blobs = %d, want 1", len(repo.blobs))
	}
}

func TestResolveUploadBlobRejectsQuarantinedAndFailed(t *testing.T) {
	for _, status := range []string{BlobStatusQuarantined, BlobStatusFailed, BlobStatusScanning} {
		repo := newMemVersionsRepo()
		repo.blobs[uuid.New()] = ObjectBlob{ID: uuid.New(), SHA256: "bad", StorageKey: "objects/bad", RefCount: 1, Status: status}
		if _, _, err := resolveUploadBlobLogic(repo, "objects/new", "bad", 10, "application/zip"); !errors.Is(err, ErrBlobUnavailable) {
			t.Fatalf("status %s: err = %v, want ErrBlobUnavailable", status, err)
		}
	}
}
