package upload

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testService(t *testing.T, size int64) (*Service, *MemoryStore, *LocalStorage, uuid.UUID) {
	t.Helper()
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user, parent := uuid.New(), uuid.New()
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, nil)
	v, err := svc.Start(user, parent, "a.txt", size, "")
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, storage, v.ID
}

func TestAppendTooLargeDoesNotWrite(t *testing.T) {
	svc, store, storage, id := testService(t, 3)
	_, err := svc.Append(id, 0, bytes.NewBufferString("abcd"))
	if !errors.Is(err, ErrSize) {
		t.Fatalf("expected ErrSize, got %v", err)
	}
	v, _ := store.Get(id)
	if v.Offset != 0 {
		t.Fatalf("offset changed to %d", v.Offset)
	}
	if r, err := storage.Read(v.StorageKey); err == nil {
		r.Close()
		t.Fatal("temporary object was polluted")
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	if _, err := svc.Append(id, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	first, err := svc.Complete(id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Complete(id)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Status != StatusAvailable {
		t.Fatalf("unexpected repeated completion: %#v", second)
	}
	if got, _ := store.Get(id); got.Status != StatusAvailable {
		t.Fatal("completion was not persisted")
	}
}

// countingScanner 第一次扫描拒绝，之后放行；
// 用于验证 quarantined 是终态：重复 complete 不会重新扫描。
type countingScanner struct{ calls int }

func (s *countingScanner) Scan(io.Reader) error {
	s.calls++
	if s.calls == 1 {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestCompleteQuarantinedIsTerminal(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user, parent := uuid.New(), uuid.New()
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, nil)
	scanner := &countingScanner{}
	svc.SetScanner(scanner)
	v, err := svc.Start(user, parent, "a.txt", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(v.ID); !errors.Is(err, ErrRejected) {
		t.Fatalf("first complete: expected ErrRejected, got %v", err)
	}
	if got, _ := store.Get(v.ID); got.Status != StatusQuarantined {
		t.Fatalf("status = %s, want quarantined", got.Status)
	}
	// 重复调用：直接返回当前状态，不重新处理。
	second, err := svc.Complete(v.ID)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("second complete: expected ErrRejected, got %v", err)
	}
	if second.Status != StatusQuarantined {
		t.Fatalf("second complete status = %s, want quarantined", second.Status)
	}
	if scanner.calls != 1 {
		t.Fatalf("scanner ran %d times, quarantined session must not be reprocessed", scanner.calls)
	}
}

func TestCompleteFailedIsTerminal(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	v0, _ := store.Get(id)
	// 篡改期望校验和，使第一次 complete 校验失败进入 failed 终态。
	v0.ExpectedSHA256 = strings.Repeat("0", 64)
	if err := store.Update(v0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(id, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(id); !errors.Is(err, ErrChecksum) {
		t.Fatalf("first complete: expected ErrChecksum, got %v", err)
	}
	if got, _ := store.Get(id); got.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	// 重复调用：返回 failed 终态（ErrFailed），不重新读存储/校验。
	second, err := svc.Complete(id)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("second complete: expected ErrFailed, got %v", err)
	}
	if second.Status != StatusFailed {
		t.Fatalf("second complete status = %s, want failed", second.Status)
	}
}
