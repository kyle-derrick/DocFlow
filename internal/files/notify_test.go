package files

import (
	"testing"

	"github.com/google/uuid"
)

// dispatchVersionAdded 的过滤语义（AddVersion 事务提交后的 file.updated 接线点）：
// 写入者非文件行 owner 时回调（统一空间模型：所有文件都在空间内，通知
// 空间成员由注入方处理）；owner 自身写入、nil 回调均不触发。
func TestDispatchVersionAddedFilters(t *testing.T) {
	owner, editor := uuid.New(), uuid.New()
	spaceID := uuid.New()
	version := FileVersion{ID: uuid.New(), FileID: uuid.New(), Version: 2}

	var gotFile File
	var gotActor uuid.UUID
	var gotVersion FileVersion
	calls := 0
	cb := func(f File, a uuid.UUID, v FileVersion) {
		calls++
		gotFile, gotActor, gotVersion = f, a, v
	}

	// 空间文件 + editor（≠owner）写入：回调，参数透传。
	spaceFile := File{ID: version.FileID, OwnerID: owner, SpaceID: spaceID, Type: "file"}
	dispatchVersionAdded(cb, spaceFile, editor, version)
	if calls != 1 || gotFile.ID != spaceFile.ID || gotActor != editor || gotVersion.ID != version.ID {
		t.Fatalf("space file by non-owner: calls=%d got=%+v/%s/%+v", calls, gotFile, gotActor, gotVersion)
	}

	// 空间文件 + owner 自身写入：不回调。
	dispatchVersionAdded(cb, spaceFile, owner, version)
	if calls != 1 {
		t.Fatalf("owner self-write must not notify, calls = %d", calls)
	}

	// 回调未注入（SetNotifyDispatcher 未调用）：不 panic。
	dispatchVersionAdded(nil, spaceFile, editor, version)
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
