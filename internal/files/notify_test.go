package files

import (
	"testing"

	"github.com/google/uuid"
)

// dispatchVersionAdded 的过滤语义（AddVersion 事务提交后的 file.updated 接线点）：
// 仅团队文件（scope_type=team 且有 team_id）且写入者非文件行 owner 时回调；
// 个人文件、owner 自身写入、nil 回调均不触发。
func TestDispatchVersionAddedFilters(t *testing.T) {
	owner, editor := uuid.New(), uuid.New()
	teamID := uuid.New()
	version := FileVersion{ID: uuid.New(), FileID: uuid.New(), Version: 2}

	var gotFile File
	var gotActor uuid.UUID
	var gotVersion FileVersion
	calls := 0
	cb := func(f File, a uuid.UUID, v FileVersion) {
		calls++
		gotFile, gotActor, gotVersion = f, a, v
	}

	// 团队文件 + editor（≠owner）写入：回调，参数透传。
	teamFile := File{ID: version.FileID, OwnerID: owner, TeamID: &teamID, ScopeType: "team", Type: "file"}
	dispatchVersionAdded(cb, teamFile, editor, version)
	if calls != 1 || gotFile.ID != teamFile.ID || gotActor != editor || gotVersion.ID != version.ID {
		t.Fatalf("team file by non-owner: calls=%d got=%+v/%s/%+v", calls, gotFile, gotActor, gotVersion)
	}

	// 团队文件 + owner 自身写入：不回调。
	dispatchVersionAdded(cb, teamFile, owner, version)
	if calls != 1 {
		t.Fatalf("owner self-write must not notify, calls = %d", calls)
	}

	// 个人文件：不回调。
	personal := File{ID: uuid.New(), OwnerID: owner, ScopeType: "personal", Type: "file"}
	dispatchVersionAdded(cb, personal, editor, version)
	if calls != 1 {
		t.Fatalf("personal file must not notify, calls = %d", calls)
	}

	// scope_type=team 但 team_id 缺失：不回调。
	malformed := File{ID: uuid.New(), OwnerID: owner, ScopeType: "team", Type: "file"}
	dispatchVersionAdded(cb, malformed, editor, version)
	if calls != 1 {
		t.Fatalf("team scope without team_id must not notify, calls = %d", calls)
	}

	// 回调未注入（SetNotifyDispatcher 未调用）：不 panic。
	dispatchVersionAdded(nil, teamFile, editor, version)
}

// SetNotifyDispatcher 注入后可经版本钩子取回（幂等；nil 不覆盖）。
func TestSetNotifyDispatcherIdempotent(t *testing.T) {
	s := NewStore(nil)
	if s.versionNotify != nil {
		t.Fatal("default dispatcher must be nil")
	}
	var called bool
	fn := func(File, uuid.UUID, FileVersion) { called = true }
	s.SetNotifyDispatcher(fn)
	s.SetNotifyDispatcher(nil) // nil 不覆盖
	if s.versionNotify == nil {
		t.Fatal("dispatcher must be set and survive nil injection")
	}
	s.versionNotify(File{}, uuid.Nil, FileVersion{})
	if !called {
		t.Fatal("injected dispatcher must be invoked")
	}
}
