package files

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSortClauseWhitelist 覆盖排序白名单：合法键生成对应列与方向
// （附 id 稳定次序），非法键/方向一律拒绝（HTTP 层 400）。
func TestSortClauseWhitelist(t *testing.T) {
	tests := []struct {
		sort, order string
		want        string
		ok          bool
	}{
		{"name", "asc", "lower(name) ASC, id ASC", true},
		{"name", "desc", "lower(name) DESC, id DESC", true},
		{"name", "", "lower(name) ASC, id ASC", true},
		{"updated_at", "desc", "updated_at DESC, id DESC", true},
		{"size", "asc", "(SELECT fv.size FROM file_versions fv WHERE fv.id = files.current_version_id) ASC, id ASC", true},
		// 非法：未知键、未知方向、注入尝试。
		{"owner_id", "asc", "", false},
		{"", "asc", "", false},
		{"name; DROP TABLE files", "", "", false},
		{"name", "up", "", false},
		{"name", "asc; --", "", false},
	}
	for _, tc := range tests {
		got, ok := SortClause(tc.sort, tc.order)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("SortClause(%q, %q) = %q, %v; want %q, %v", tc.sort, tc.order, got, ok, tc.want, tc.ok)
		}
	}
}

// TestBatchErrorCode 覆盖批量单项错误码映射（per-item results 的 error_code）。
func TestBatchErrorCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{ErrNotFound, BatchCodeNotFound},
		{ErrForbidden, BatchCodeForbidden},
		{ErrRoot, BatchCodeRoot},
		{ErrConflict, BatchCodeNameConflict},
		{ErrMoveTarget, BatchCodeInvalidTarget},
		{ErrParentDeleted, BatchCodeParentDeleted},
		{ErrNotDeleted, BatchCodeNotDeleted},
		{errors.New("boom"), BatchCodeInternal},
		{nil, ""},
	}
	for _, tc := range tests {
		if got := batchErrorCode(tc.err); got != tc.want {
			t.Errorf("batchErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestMoveItemDecision 移动单项纯逻辑判定：根目录不可移动；目标为自身拒绝。
// （部分成功语义的其余分支——名称冲突/后代环/归属校验依赖数据访问，见
// BatchMove 集成路径与 openapi 契约描述。）
func TestMoveItemDecision(t *testing.T) {
	id := uuid.New()
	root := File{ID: id, IsRoot: true, Type: "folder"}
	normal := File{ID: id, Type: "file"}
	target := File{ID: uuid.New(), Type: "folder"}

	if err := moveItemDecision(root, target); !errors.Is(err, ErrRoot) {
		t.Errorf("root move: err = %v, want ErrRoot", err)
	}
	if err := moveItemDecision(normal, File{ID: id}); !errors.Is(err, ErrMoveTarget) {
		t.Errorf("self target: err = %v, want ErrMoveTarget", err)
	}
	if err := moveItemDecision(normal, target); err != nil {
		t.Errorf("normal move: err = %v, want nil", err)
	}
}

// TestAuthorizeBatchWriteMatrix 覆盖批量写授权矩阵：
// 个人文件仅 owner（非 owner 归一 NOT_FOUND）；团队文件要求写权限
// （无授权器/无权限 FORBIDDEN）。
func TestAuthorizeBatchWriteMatrix(t *testing.T) {
	owner, other := uuid.New(), uuid.New()
	teamA := uuid.New()
	personal := File{ID: uuid.New(), OwnerID: owner, Type: "file", ScopeType: "personal"}
	teamFile := File{ID: uuid.New(), OwnerID: owner, Type: "file", ScopeType: "team", TeamID: &teamA}

	editor := uuid.New()
	writer := fakeTeamWriter(map[uuid.UUID][]uuid.UUID{
		editor: {teamA},
		// other / viewer 不在可写集合。
	})

	tests := []struct {
		name    string
		store   *Store
		user    uuid.UUID
		file    File
		wantErr error
	}{
		{"personal by owner", &Store{}, owner, personal, nil},
		{"personal by other", &Store{}, other, personal, ErrNotFound},
		{"team file by editor", &Store{teamWriter: writer}, editor, teamFile, nil},
		{"team file by non-writer", &Store{teamWriter: writer}, other, teamFile, ErrForbidden},
		{"team file without writer", &Store{}, editor, teamFile, ErrForbidden},
	}
	for _, tc := range tests {
		err := tc.store.authorizeBatchWrite(tc.file, tc.user)
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
}
