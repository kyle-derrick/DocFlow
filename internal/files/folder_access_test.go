package files

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// memFolderRepo 是 folderAccessor 的内存实现，用于验证团队/个人目录写权限矩阵。
type memFolderRepo struct {
	folders map[uuid.UUID]File
}

func (m *memFolderRepo) getFolder(id uuid.UUID) (File, error) {
	f, ok := m.folders[id]
	// 与 Store.getFolder 一致：仅返回未删除的目录。
	if !ok || f.Type != "folder" {
		return File{}, ErrNotFound
	}
	return f, nil
}

func (m *memFolderRepo) add(f File) uuid.UUID {
	m.folders[f.ID] = f
	return f.ID
}

// fakeTeamWriter 按 (user, team) 集合模拟 team 包的 CanWrite 查询。
func fakeTeamWriter(writable map[uuid.UUID][]uuid.UUID) TeamWriter {
	return func(user, teamID uuid.UUID) (bool, error) {
		for _, t := range writable[user] {
			if t == teamID {
				return true, nil
			}
		}
		return false, nil
	}
}

func TestAuthorizeParentFolderMatrix(t *testing.T) {
	owner, other := uuid.New(), uuid.New()
	teamA, teamB := uuid.New(), uuid.New()

	personal := File{ID: uuid.New(), Name: "personal", OwnerID: owner, Type: "folder", ScopeType: "personal"}
	teamRootA := File{ID: uuid.New(), Name: "root", OwnerID: owner, Type: "folder", ScopeType: "team", TeamID: &teamA, IsRoot: true}
	teamFolderB := File{ID: uuid.New(), Name: "shared", OwnerID: owner, Type: "folder", ScopeType: "team", TeamID: &teamB}
	notFolder := File{ID: uuid.New(), Name: "a.txt", OwnerID: owner, Type: "file", ScopeType: "personal"}

	editor := uuid.New() // teamA editor（可写）
	viewer := uuid.New() // teamA viewer（只读）

	repo := &memFolderRepo{folders: map[uuid.UUID]File{
		personal.ID: personal, teamRootA.ID: teamRootA, teamFolderB.ID: teamFolderB, notFolder.ID: notFolder,
	}}
	writer := fakeTeamWriter(map[uuid.UUID][]uuid.UUID{
		owner:  {teamA}, // teamA owner
		editor: {teamA},
		// viewer 无写权限；other 无任何团队。
	})

	tests := []struct {
		name    string
		user    uuid.UUID
		folder  uuid.UUID
		wantErr error
	}{
		{"personal folder by owner", owner, personal.ID, nil},
		{"personal folder by other user", other, personal.ID, ErrNotFound},
		{"team root by team owner", owner, teamRootA.ID, nil},
		{"team folder by editor of same team", editor, teamRootA.ID, nil},
		{"team folder by editor of another team", editor, teamFolderB.ID, ErrForbidden}, // editor 只在 teamA，不在 teamB
		{"team folder by viewer", viewer, teamRootA.ID, ErrForbidden},
		{"team folder by non-member", other, teamRootA.ID, ErrForbidden},
		{"missing folder", owner, uuid.New(), ErrNotFound},
		{"not a folder", owner, notFolder.ID, ErrNotFound},
	}
	for _, tc := range tests {
		_, err := authorizeParentFolder(repo, tc.user, tc.folder, writer)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestAuthorizeParentFolderWithoutTeamWriter(t *testing.T) {
	teamID := uuid.New()
	owner := uuid.New()
	root := File{ID: uuid.New(), Name: "root", OwnerID: owner, Type: "folder", ScopeType: "team", TeamID: &teamID}
	repo := &memFolderRepo{folders: map[uuid.UUID]File{root.ID: root}}
	// 未注入团队权限源：团队目录一律拒绝（安全默认）。
	if _, err := authorizeParentFolder(repo, owner, root.ID, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("team folder without writer: err = %v, want ErrForbidden", err)
	}
	// 个人目录不受影响。
	personal := File{ID: uuid.New(), Name: "p", OwnerID: owner, Type: "folder", ScopeType: "personal"}
	repo.add(personal)
	if _, err := authorizeParentFolder(repo, owner, personal.ID, nil); err != nil {
		t.Fatalf("personal folder without writer: %v", err)
	}
}

// fakeTeamReader 按 (user, team) 集合模拟 team 包的在册成员查询（任意角色均可读）。
func fakeTeamReader(members map[uuid.UUID][]uuid.UUID) TeamReader {
	return func(user, teamID uuid.UUID) (bool, error) {
		for _, t := range members[user] {
			if t == teamID {
				return true, nil
			}
		}
		return false, nil
	}
}

// TestAuthorizeFileAccessMatrix 覆盖 GET/下载/预览统一入口 authorizeFileAccess 的读授权矩阵：
// 个人文件仅 owner；团队文件任意在册成员（owner/editor/viewer）可读；非成员/跨团队 404 不泄露存在性。
func TestAuthorizeFileAccessMatrix(t *testing.T) {
	creator, other := uuid.New(), uuid.New()
	teamA, teamB := uuid.New(), uuid.New()

	personalFile := File{ID: uuid.New(), Name: "own.txt", OwnerID: creator, Type: "file", ScopeType: "personal"}
	teamFile := File{ID: uuid.New(), Name: "team.txt", OwnerID: creator, Type: "file", ScopeType: "team", TeamID: &teamA}
	// scope_type='team' 但 team_id 缺失：按个人作用域处理。
	orphanTeamScope := File{ID: uuid.New(), Name: "orphan.txt", OwnerID: creator, Type: "file", ScopeType: "team"}

	editor := uuid.New()  // teamA editor（可读可写）
	viewer := uuid.New()  // teamA viewer（只读成员，同样可读）
	memberB := uuid.New() // teamB 成员（跨团队）

	reader := fakeTeamReader(map[uuid.UUID][]uuid.UUID{
		editor:  {teamA},
		viewer:  {teamA},
		memberB: {teamB},
	})

	tests := []struct {
		name    string
		user    uuid.UUID
		file    File
		reader  TeamReader
		wantErr error
	}{
		{"personal file by owner", creator, personalFile, reader, nil},
		{"personal file by other user", other, personalFile, reader, ErrNotFound},
		{"team file by creator (owner)", creator, teamFile, reader, nil},
		{"team file by editor member", editor, teamFile, reader, nil},
		{"team file by viewer member (read allowed)", viewer, teamFile, reader, nil},
		{"team file by member of another team", memberB, teamFile, reader, ErrNotFound},
		{"team file by non-member", other, teamFile, reader, ErrNotFound},
		{"team file without reader injected", editor, teamFile, nil, ErrForbidden},
		{"team scope without team_id treated as personal", editor, orphanTeamScope, reader, ErrNotFound},
	}
	for _, tc := range tests {
		err := authorizeFileAccess(tc.file, tc.user, tc.reader)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}
