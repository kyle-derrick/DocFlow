package upload

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/google/uuid"
)

// notifyCall 记录一次通知回调的参数。
type notifyCall struct {
	user     uuid.UUID
	event    string
	title    string
	body     string
	resource uuid.UUID
}

// fakeNotifyDispatcher 记录全部回调（并发安全：Complete 可能经任务队列并发补完）。
type fakeNotifyDispatcher struct {
	mu    sync.Mutex
	calls []notifyCall
}

func (f *fakeNotifyDispatcher) record(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, notifyCall{user: userID, event: eventType, title: title, body: body, resource: resourceID})
}

func (f *fakeNotifyDispatcher) snapshot() []notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notifyCall(nil), f.calls...)
}

// 完成路径（新文件创建）：通知属主 upload.completed（标题含文件名、资源为文件 ID）。
func TestCompleteNotifiesOwnerUploadCompleted(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user, parent := uuid.New(), uuid.New()
	fileID := uuid.New()
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil,
		func(_, _ uuid.UUID, _ string, _ string, _ int64, _ string, _ string) (uuid.UUID, bool, error) {
			return fileID, true, nil
		})
	dispatcher := &fakeNotifyDispatcher{}
	svc.SetNotifyDispatcher(dispatcher.record)
	v, err := svc.Start(user, parent, "report.pdf", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(v.ID); err != nil {
		t.Fatal(err)
	}
	calls := dispatcher.snapshot()
	if len(calls) != 1 {
		t.Fatalf("notify calls = %d, want 1: %+v", len(calls), calls)
	}
	c := calls[0]
	if c.user != user || c.event != "upload.completed" || c.resource != fileID {
		t.Fatalf("call = %+v, want user=%s upload.completed resource=%s", c, user, fileID)
	}
	if !strings.Contains(c.title, "report.pdf") {
		t.Fatalf("title must contain file name: %q", c.title)
	}
	// 重复 Complete（终态幂等）不重复通知。
	if _, err := svc.Complete(v.ID); err != nil {
		t.Fatal(err)
	}
	if calls := dispatcher.snapshot(); len(calls) != 1 {
		t.Fatalf("terminal idempotent complete must not re-notify, calls = %d", len(calls))
	}
}

// 覆盖为新版本路径：通知属主 upload.completed（资源为目标文件 ID）。
func TestCompleteReplaceNotifiesOwner(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user := uuid.New()
	target := uuid.New()
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, nil)
	svc.SetVersionTarget(
		func(_, id uuid.UUID) (files.File, error) {
			return files.File{ID: id, OwnerID: user, Type: "file", Name: "spec.docx"}, nil
		},
		func(_, _ uuid.UUID, _ string, _ string, _ int64, _ string) (bool, error) { return true, nil },
	)
	dispatcher := &fakeNotifyDispatcher{}
	svc.SetNotifyDispatcher(dispatcher.record)
	data := officeArchive(t, "[Content_Types].xml", "word/document.xml")
	v, err := svc.StartReplace(user, target, int64(len(data)), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(v.ID); err != nil {
		t.Fatal(err)
	}
	calls := dispatcher.snapshot()
	if len(calls) != 1 || calls[0].event != "upload.completed" || calls[0].resource != target {
		t.Fatalf("calls = %+v, want one upload.completed with resource %s", calls, target)
	}
	if !strings.Contains(calls[0].title, "spec.docx") {
		t.Fatalf("title must contain file name: %q", calls[0].title)
	}
}

// 隔离终态：通知属主 upload.quarantined（资源为空），重复 Complete 不重复通知。
func TestCompleteQuarantinedNotifiesOwner(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user, parent := uuid.New(), uuid.New()
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, nil)
	svc.SetScanner(&countingScanner{})
	dispatcher := &fakeNotifyDispatcher{}
	svc.SetNotifyDispatcher(dispatcher.record)
	v, err := svc.Start(user, parent, "evil.exe", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(v.ID); err != ErrRejected {
		t.Fatalf("complete = %v, want ErrRejected", err)
	}
	calls := dispatcher.snapshot()
	if len(calls) != 1 || calls[0].event != "upload.quarantined" || calls[0].user != user || calls[0].resource != uuid.Nil {
		t.Fatalf("calls = %+v, want one upload.quarantined for owner with nil resource", calls)
	}
	if !strings.Contains(calls[0].title, "evil.exe") {
		t.Fatalf("title must contain file name: %q", calls[0].title)
	}
	// 隔离为终态：重复 Complete 直接返回，不重复通知。
	if _, err := svc.Complete(v.ID); err != ErrRejected {
		t.Fatalf("repeat complete = %v, want ErrRejected", err)
	}
	if calls := dispatcher.snapshot(); len(calls) != 1 {
		t.Fatalf("terminal quarantine must not re-notify, calls = %d", len(calls))
	}
}

// 校验失败（failed 终态）不通知：仅完成与隔离两类事件。
func TestCompleteFailedDoesNotNotify(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	v0, _ := store.Get(id)
	v0.ExpectedSHA256 = strings.Repeat("0", 64)
	if err := store.Update(v0); err != nil {
		t.Fatal(err)
	}
	dispatcher := &fakeNotifyDispatcher{}
	svc.SetNotifyDispatcher(dispatcher.record)
	if _, err := svc.Append(id, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(id); err == nil {
		t.Fatal("expected checksum error")
	}
	if calls := dispatcher.snapshot(); len(calls) != 0 {
		t.Fatalf("failed terminal must not notify, calls = %+v", calls)
	}
}
