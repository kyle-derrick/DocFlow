package onlyoffice

import (
	"errors"
	"sync"
	"testing"

	"github.com/docflow/docflow/internal/files"
	"github.com/google/uuid"
)

// memoryCallbackStore 契约：首次 TryRecord 插入成功、同 (file,key,url) 重复
// 返回 inserted=false、不同 url/文件互不影响、Release 后同键可重新插入。
func TestMemoryCallbackStoreSemantics(t *testing.T) {
	m := newMemoryCallbackStore()
	fileID, otherFile := uuid.New(), uuid.New()
	key, url1, url2 := "k1", "http://onlyoffice:80/a", "http://onlyoffice:80/b"

	if inserted, err := m.TryRecord(fileID, key, url1, "2", "0"); err != nil || !inserted {
		t.Fatalf("first TryRecord = %v, %v; want true, nil", inserted, err)
	}
	if inserted, err := m.TryRecord(fileID, key, url1, "2", "0"); err != nil || inserted {
		t.Fatalf("duplicate TryRecord = %v, %v; want false, nil", inserted, err)
	}
	if inserted, err := m.TryRecord(fileID, key, url2, "2", "0"); err != nil || !inserted {
		t.Fatalf("different url TryRecord = %v, %v; want true, nil", inserted, err)
	}
	if inserted, err := m.TryRecord(otherFile, key, url1, "2", "0"); err != nil || !inserted {
		t.Fatalf("different file TryRecord = %v, %v; want true, nil", inserted, err)
	}
	if err := m.Release(fileID, key, url1); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if inserted, err := m.TryRecord(fileID, key, url1, "2", "0"); err != nil || !inserted {
		t.Fatalf("TryRecord after Release = %v, %v; want true, nil", inserted, err)
	}
	// Release 不存在的键为幂等操作。
	if err := m.Release(fileID, key, url1); err != nil {
		t.Fatalf("Release existing key again: %v", err)
	}
}

// fakeCallbackStore 记录 TryRecord/Release 调用序列（验证 Service 协作）。
type fakeCallbackStore struct {
	mu       sync.Mutex
	rows     map[string]struct{}
	tryCalls int
	released []string
}

func newFakeCallbackStore() *fakeCallbackStore {
	return &fakeCallbackStore{rows: make(map[string]struct{})}
}

func (f *fakeCallbackStore) TryRecord(fileID uuid.UUID, key, url, status, result string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tryCalls++
	idem := callbackRecordKey(fileID, key, url)
	if _, ok := f.rows[idem]; ok {
		return false, nil
	}
	f.rows[idem] = struct{}{}
	return true, nil
}

func (f *fakeCallbackStore) Release(fileID uuid.UUID, key, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, key)
	delete(f.rows, callbackRecordKey(fileID, key, url))
	return nil
}

// Service 与注入的 CallbackStore 协作：成功保存只 TryRecord 不 Release；
// 幂等命中不再建版本；处理失败回滚（Release）后重试可成功。
func TestCallbackStoreCooperation(t *testing.T) {
	store := newFakeFileStore()
	owner := uuid.New()
	file, version := store.seedFile(owner, "a.docx", "v1")
	key := documentKey(file.ID, version.ID)
	s := newTestService(store, newMemStorage(), &fakeFetch{content: []byte("v2")}, &fakeRecorder{}, nil)
	cbs := newFakeCallbackStore()
	s.SetCallbackStore(cbs)

	// 成功路径：TryRecord 一次、不 Release。
	body := callbackBody(t, testJWTSecret, map[string]any{"key": key, "status": 2, "url": sameOriginURL}, true)
	if err := s.HandleCallback(body, "", "", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	if cbs.tryCalls != 1 || len(cbs.released) != 0 {
		t.Fatalf("success path: tryCalls=%d released=%v, want 1, empty", cbs.tryCalls, cbs.released)
	}

	// 幂等命中：返回成功且不建版本、不再进入处理。
	if err := s.HandleCallback(body, "", "", ""); err != nil {
		t.Fatalf("duplicate callback: %v", err)
	}
	if got := store.addVersionCount(); got != 1 {
		t.Fatalf("AddVersion calls after duplicate = %d, want 1", got)
	}
	if cbs.tryCalls != 2 || len(cbs.released) != 0 {
		t.Fatalf("duplicate path: tryCalls=%d released=%v, want 2, empty", cbs.tryCalls, cbs.released)
	}

	// 失败路径（AddVersion 报错）：处理失败回滚幂等记录。
	failing := callbackBody(t, testJWTSecret, map[string]any{"key": key, "status": 2, "url": sameOriginURL + "?r=1"}, true)
	store.mu.Lock()
	store.addErr = files.ErrBlobUnavailable
	store.mu.Unlock()
	if err := s.HandleCallback(failing, "", "", ""); !errors.Is(err, files.ErrBlobUnavailable) {
		t.Fatalf("failing save: err = %v, want files.ErrBlobUnavailable", err)
	}
	if len(cbs.released) != 1 {
		t.Fatalf("released keys = %v, want exactly 1", cbs.released)
	}

	// 重试（同 key/url）：回滚后可重新抢占并成功。
	store.mu.Lock()
	store.addErr = nil
	store.mu.Unlock()
	if err := s.HandleCallback(failing, "", "", ""); err != nil {
		t.Fatalf("retry after release: %v", err)
	}
	if got := store.addVersionCount(); got != 2 {
		t.Fatalf("AddVersion calls after retry = %d, want 2", got)
	}
	if len(cbs.released) != 1 {
		t.Fatalf("released keys after retry = %v, want still 1", cbs.released)
	}
}
