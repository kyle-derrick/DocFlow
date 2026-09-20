package files

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeSpaceWriter 按 (user, space) 集合模拟 space 包的 CanWrite 查询。
func fakeSpaceWriter(writable map[uuid.UUID][]uuid.UUID) SpaceWriter {
	return func(user, spaceID uuid.UUID) (bool, error) {
		for _, s := range writable[user] {
			if s == spaceID {
				return true, nil
			}
		}
		return false, nil
	}
}

// fakeSpaceReader 按 (user, space) 集合模拟 space 包的在册成员查询（任意角色均可读）。
func fakeSpaceReader(members map[uuid.UUID][]uuid.UUID) SpaceReader {
	return func(user, spaceID uuid.UUID) (bool, error) {
		for _, s := range members[user] {
			if s == spaceID {
				return true, nil
			}
		}
		return false, nil
	}
}

// fakeSpaceDeleter 按 (user, space) 集合模拟 space 包的 CanDelete 查询。
func fakeSpaceDeleter(deletable map[uuid.UUID][]uuid.UUID) SpaceDeleter {
	return func(user, spaceID uuid.UUID) (bool, error) {
		for _, s := range deletable[user] {
			if s == spaceID {
				return true, nil
			}
		}
		return false, nil
	}
}

// fakeACL 按字符串键模拟路径级 ACL 求值（key = fileID|userID|perm → allowed）。
type fakeACL struct {
	results map[string]bool
}

func (f fakeACL) resolve(fileOrFolderID, spaceID, user uuid.UUID, perm string) (bool, bool, error) {
	key := fileOrFolderID.String() + "|" + user.String() + "|" + perm
	v, ok := f.results[key]
	return v, ok, nil
}

// memFolderRepo authorizeParentFolder 的最小内存实现。
type memFolderRepo struct {
	folders map[uuid.UUID]File
}

func (m *memFolderRepo) getFolder(id uuid.UUID) (File, error) {
	f, ok := m.folders[id]
	if !ok {
		return File{}, ErrNotFound
	}
	return f, nil
}

// TestAuthorizeParentFolderMatrix 覆盖 authorizeParentFolder（上传/建目录
// 的父目录授权）：空间目录按成员写权限（guest 与非成员 403），未注入
// writer fail closed。
func TestAuthorizeParentFolderMatrix(t *testing.T) {
	owner, editor, viewer, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	spaceA, spaceB := uuid.New(), uuid.New()

	personal := File{ID: uuid.New(), Name: "root", OwnerID: owner, Type: "folder", SpaceID: spaceA, IsRoot: true}
	spaceFolderB := File{ID: uuid.New(), Name: "shared", OwnerID: owner, Type: "folder", SpaceID: spaceB}

	repo := &memFolderRepo{folders: map[uuid.UUID]File{
		personal.ID: personal, spaceFolderB.ID: spaceFolderB,
	}}
	writer := fakeSpaceWriter(map[uuid.UUID][]uuid.UUID{
		owner:  {spaceA, spaceB}, // spaceA owner
		editor: {spaceA},
	})

	tests := []struct {
		name    string
		user    uuid.UUID
		parent  uuid.UUID
		wantErr error
	}{
		{"space root by space owner", owner, personal.ID, nil},
		{"space folder by editor of same space", editor, personal.ID, nil},
		{"space folder by editor of another space", editor, spaceFolderB.ID, ErrForbidden},
		{"space folder by viewer", viewer, personal.ID, ErrForbidden},
		{"space folder by non-member", other, personal.ID, ErrForbidden},
	}
	for _, tc := range tests {
		_, err := authorizeParentFolder(repo, tc.user, tc.parent, writer, nil)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestAuthorizeParentFolderWithoutSpaceWriter(t *testing.T) {
	owner := uuid.New()
	spaceID := uuid.New()
	root := File{ID: uuid.New(), Name: "root", OwnerID: owner, Type: "folder", SpaceID: spaceID}
	repo := &memFolderRepo{folders: map[uuid.UUID]File{root.ID: root}}
	if _, err := authorizeParentFolder(repo, owner, root.ID, nil, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("space folder without writer: err = %v, want ErrForbidden", err)
	}
}

// TestAuthorizeSpaceDeleteMatrix 覆盖删除入口（DELETE /files/:id、DELETE /trash/:id）
// 的授权矩阵：文件行 owner（上传者）短路；空间文件按 CanDelete（角色矩阵
// 含 delete），文件行 owner 之外无短路。
func TestAuthorizeSpaceDeleteMatrix(t *testing.T) {
	creator, spaceOwner, editor, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	spaceA := uuid.New()

	spaceFile := File{ID: uuid.New(), Name: "shared.txt", OwnerID: creator, Type: "file", SpaceID: spaceA}

	deleter := fakeSpaceDeleter(map[uuid.UUID][]uuid.UUID{
		spaceOwner: {spaceA}, // 空间 owner/admin/member*/具备 delete 的角色：CanDelete=true
		// creator（文件行 owner）经 owner 短路放行；editor：delete=false；other：非成员。
	})

	tests := []struct {
		name    string
		user    uuid.UUID
		file    File
		deleter SpaceDeleter
		wantErr error
	}{
		{"file by row owner", creator, spaceFile, deleter, nil},
		{"file by space member with delete", spaceOwner, spaceFile, deleter, nil},
		{"file by editor (no delete)", editor, spaceFile, deleter, ErrForbidden},
		{"file by non-member", other, spaceFile, deleter, ErrForbidden},
		{"file without deleter injected", spaceOwner, spaceFile, nil, ErrForbidden},
	}
	for _, tc := range tests {
		err := authorizeSpaceDelete(tc.file, tc.user, tc.deleter, nil)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

// TestAuthorizeFileAccessMatrix 覆盖 GET/下载/预览统一入口 authorizeFileAccess
// 的读授权矩阵：文件行 owner 短路；空间文件任意在册成员可读；非成员/跨空间
// 404 不泄露存在性。
func TestAuthorizeFileAccessMatrix(t *testing.T) {
	creator, other := uuid.New(), uuid.New()
	spaceA, spaceB := uuid.New(), uuid.New()

	sharedFile := File{ID: uuid.New(), Name: "shared.txt", OwnerID: creator, Type: "file", SpaceID: spaceA}
	orphan := File{ID: uuid.New(), Name: "orphan.txt", OwnerID: creator, Type: "file"}

	editor := uuid.New()  // spaceA 编辑（可读可写）
	guest := uuid.New()   // spaceA guest（只读成员，同样可读）
	memberB := uuid.New() // spaceB 成员（跨空间）

	reader := fakeSpaceReader(map[uuid.UUID][]uuid.UUID{
		editor:  {spaceA},
		guest:   {spaceA},
		memberB: {spaceB},
	})

	tests := []struct {
		name    string
		user    uuid.UUID
		file    File
		reader  SpaceReader
		wantErr error
	}{
		{"space file by creator (row owner)", creator, sharedFile, reader, nil},
		{"space file by editor member", editor, sharedFile, reader, nil},
		{"space file by guest member (read allowed)", guest, sharedFile, reader, nil},
		{"space file by member of another space", memberB, sharedFile, reader, ErrNotFound},
		{"space file by non-member", other, sharedFile, reader, ErrNotFound},
		{"space file without reader injected", editor, sharedFile, nil, ErrForbidden},
		{"orphan (no space) by row owner", creator, orphan, reader, nil},
		{"orphan (no space) by member", editor, orphan, reader, ErrNotFound},
	}
	for _, tc := range tests {
		err := authorizeFileAccess(tc.file, tc.user, tc.reader, nil)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

// TestAuthorizeWithACL 覆盖 ACL 接线：matched 时 ACL 结果优先于空间角色
// （allow 覆盖 guest 角色 / deny 覆盖空间 owner 的 CanDelete；行 owner 恒短路）。
func TestAuthorizeWithACL(t *testing.T) {
	owner, uploader, viewer := uuid.New(), uuid.New(), uuid.New()
	spaceID := uuid.New()
	root := File{ID: uuid.New(), Name: "root", OwnerID: owner, Type: "folder", SpaceID: spaceID, IsRoot: true}
	spaceFile := File{ID: uuid.New(), Name: "shared.txt", OwnerID: uploader, Type: "file", SpaceID: spaceID}

	// ACL：viewer 在 root 上 allow write；空间 owner（非行 owner）在 spaceFile 上 deny delete。
	acl := fakeACL{results: map[string]bool{
		root.ID.String() + "|" + viewer.String() + "|write":      true,
		spaceFile.ID.String() + "|" + owner.String() + "|delete": false,
	}}
	repo := &memFolderRepo{folders: map[uuid.UUID]File{root.ID: root}}

	writer := fakeSpaceWriter(map[uuid.UUID][]uuid.UUID{owner: {spaceID}})
	deleter := fakeSpaceDeleter(map[uuid.UUID][]uuid.UUID{owner: {spaceID}})
	reader := fakeSpaceReader(map[uuid.UUID][]uuid.UUID{owner: {spaceID}, viewer: {spaceID}})

	// viewer 非 writer 命中，但 ACL allow write 放行。
	if _, err := authorizeParentFolder(repo, viewer, root.ID, writer, acl.resolve); err != nil {
		t.Fatalf("acl allow write for viewer: %v", err)
	}
	// 空间 owner（非行 owner，CanDelete=true）在 spaceFile 上被 ACL deny delete 覆盖。
	if err := authorizeSpaceDelete(spaceFile, owner, deleter, acl.resolve); !errors.Is(err, ErrForbidden) {
		t.Fatalf("acl deny delete for space owner: err = %v, want ErrForbidden", err)
	}
	// 行 owner 恒短路：ACL deny 不影响 uploader 自身删除。
	if err := authorizeSpaceDelete(spaceFile, uploader, deleter, acl.resolve); err != nil {
		t.Fatalf("row owner delete must short-circuit: %v", err)
	}
	// read 无 ACL 条目：回退成员判定（owner 放行）。
	if err := authorizeFileAccess(spaceFile, owner, reader, acl.resolve); err != nil {
		t.Fatalf("owner read without acl entry: %v", err)
	}
}
