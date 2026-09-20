package tagging

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// fakeFileSource 按 owner 模拟 files.Store 的读授权（Get 即 authorizeFileAccess）。
type fakeFileSource struct {
	ownerFiles map[uuid.UUID][]uuid.UUID // user → 可读文件 ID 列表
}

func (f fakeFileSource) Get(user, id uuid.UUID) (files.File, error) {
	for _, fid := range f.ownerFiles[user] {
		if fid == id {
			return files.File{ID: id, OwnerID: user, Type: "file"}, nil
		}
	}
	return files.File{}, files.ErrNotFound
}

func newTestService(userFiles map[uuid.UUID][]uuid.UUID) *Service {
	return NewService(NewMemoryRepo(), fakeFileSource{ownerFiles: userFiles})
}

func TestNormalizeTagName(t *testing.T) {
	long := ""
	for i := 0; i < 65; i++ {
		long += "标"
	}
	exact := long[:len(long)-3] // 64 rune
	tests := []struct {
		input, want string
		valid       bool
	}{
		{"  cafe\u0301  ", "café", true}, // NFC 组合归一 + 去首尾空白
		{"", "", false},                  // 空
		{"   ", "", false},               // 仅空白
		{"a\nb", "", false},              // 控制字符
		{"a\x00b", "", false},            // NUL
		{"a/b 允许", "a/b 允许", true},       // 标签名允许路径字符（与文件名规则不同）
		{long, "", false},                // 65 rune 拒绝
		{exact, exact, true},             // 64 rune 边界通过
	}
	for _, tc := range tests {
		got, err := NormalizeTagName(tc.input)
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("NormalizeTagName(%q) = %q, %v (valid=%v)", tc.input, got, err, tc.valid)
		}
	}
}

// TestTagCRUDAndOwnership 覆盖标签 CRUD 与归属校验：
// 每用户内 (user_id, name) 唯一；他人标签按不存在处理（删除/使用均 404 语义）。
func TestTagCRUDAndOwnership(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	svc := newTestService(map[uuid.UUID][]uuid.UUID{})

	a1, err := svc.CreateTag(alice, "重要")
	if err != nil {
		t.Fatal(err)
	}
	// 同名冲突（同一用户）。
	if _, err := svc.CreateTag(alice, "重要"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create: err = %v, want ErrConflict", err)
	}
	// 不同用户可同名。
	b1, err := svc.CreateTag(bob, "重要")
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID == b1.ID {
		t.Fatal("tags of different users must be distinct rows")
	}
	// 列表只含自己的标签。
	aTags, err := svc.ListTags(alice)
	if err != nil || len(aTags) != 1 || aTags[0].ID != a1.ID {
		t.Fatalf("alice tags = %+v, err = %v", aTags, err)
	}
	// bob 删除 alice 的标签：按不存在处理。
	if err := svc.DeleteTag(bob, a1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete: err = %v, want ErrNotFound", err)
	}
	// GetOwnedTag 同样拒绝他人标签。
	if _, err := svc.GetOwnedTag(bob, a1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user get: err = %v, want ErrNotFound", err)
	}
	// alice 删除自己的标签成功。
	if err := svc.DeleteTag(alice, a1.ID); err != nil {
		t.Fatal(err)
	}
	if tags, _ := svc.ListTags(alice); len(tags) != 0 {
		t.Fatalf("alice tags after delete = %d, want 0", len(tags))
	}
}

// TestFileTagLifecycle 覆盖文件打/去标签：
// 只能把自己的标签打给有权读的文件；重复打幂等；解除幂等；级联清理。
func TestFileTagLifecycle(t *testing.T) {
	alice, bob := uuid.New(), uuid.New()
	fileA := uuid.New() // alice 可读
	fileB := uuid.New() // bob 可读
	svc := newTestService(map[uuid.UUID][]uuid.UUID{
		alice: {fileA},
		bob:   {fileB},
	})

	tag, err := svc.CreateTag(alice, "合同")
	if err != nil {
		t.Fatal(err)
	}
	bobTag, err := svc.CreateTag(bob, "bob-only")
	if err != nil {
		t.Fatal(err)
	}

	// 正常打标 + 幂等重打。
	if _, err := svc.AddFileTag(alice, fileA, tag.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddFileTag(alice, fileA, tag.ID); err != nil {
		t.Fatalf("idempotent re-tag: %v", err)
	}
	// 无读权限的文件拒绝（files.Get 失败）。
	if _, err := svc.AddFileTag(alice, fileB, tag.ID); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("tag foreign file: err = %v, want files.ErrNotFound", err)
	}
	// 他人标签拒绝。
	if _, err := svc.AddFileTag(alice, fileA, bobTag.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("use foreign tag: err = %v, want ErrNotFound", err)
	}
	// 列出文件标签：仅自己且已挂载的。
	attached, err := svc.ListFileTags(alice, fileA)
	if err != nil || len(attached) != 1 || attached[0].ID != tag.ID {
		t.Fatalf("file tags = %+v, err = %v", attached, err)
	}
	// bob 读同一文件（无权限）→ 列表失败。
	if _, err := svc.ListFileTags(bob, fileA); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("list tags on foreign file: err = %v, want files.ErrNotFound", err)
	}
	// 解除关联（幂等）。
	if err := svc.RemoveFileTag(alice, fileA, tag.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveFileTag(alice, fileA, tag.ID); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	if attached, _ := svc.ListFileTags(alice, fileA); len(attached) != 0 {
		t.Fatalf("attached after remove = %d, want 0", len(attached))
	}

	// 删除标签时关联级联清理：重新打标后删标签，文件标签列表为空。
	if _, err := svc.AddFileTag(alice, fileA, tag.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteTag(alice, tag.ID); err != nil {
		t.Fatal(err)
	}
	if attached, _ := svc.ListFileTags(alice, fileA); len(attached) != 0 {
		t.Fatalf("attached after tag delete = %d, want 0 (cascade)", len(attached))
	}
}
