package upload

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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

// TestMaxSizeProviderHotOverride 单文件大小上限热读取：SetMaxSizeProvider
// 注入后 Start 建会话按热值校验（更小即拒、更大即放行），MaxSize()（tus
// Tus-Max-Size 头数据源）同步取热值；provider 返回非正值或未注入时回退
// 构造值。
func TestMaxSizeProviderHotOverride(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	user, parent := uuid.New(), uuid.New()
	svc := NewService(store, storage, time.Hour, 100, false, nil, nil)

	// 未注入：按构造值校验。
	if _, err := svc.Start(user, parent, "a.txt", 200, ""); !errors.Is(err, ErrSize) {
		t.Fatalf("constructed max: err = %v, want ErrSize", err)
	}
	if svc.MaxSize() != 100 {
		t.Fatalf("MaxSize = %d, want 100", svc.MaxSize())
	}

	limit := int64(50)
	svc.SetMaxSizeProvider(func() int64 { return limit })

	// 热值 50 < 构造值 100：更小的请求被拒。
	if _, err := svc.Start(user, parent, "a.txt", 80, ""); !errors.Is(err, ErrSize) {
		t.Fatalf("hot smaller max: err = %v, want ErrSize", err)
	}
	if _, err := svc.Start(user, parent, "a.txt", 50, ""); err != nil {
		t.Fatalf("hot max boundary: %v", err)
	}
	// MaxSize 与热值保持一致（tus 头不至于宣告旧上限）。
	if svc.MaxSize() != 50 {
		t.Fatalf("MaxSize = %d, want hot 50", svc.MaxSize())
	}

	// 热值调大（模拟运行时放宽）：超过构造值的请求放行。
	limit = 500
	if _, err := svc.Start(user, parent, "b.txt", 400, ""); err != nil {
		t.Fatalf("hot larger max: %v", err)
	}

	// provider 返回非正值（读失败回退）：回落构造值。
	limit = 0
	if _, err := svc.Start(user, parent, "c.txt", 200, ""); !errors.Is(err, ErrSize) {
		t.Fatalf("fallback max: err = %v, want ErrSize", err)
	}
	if _, err := svc.Start(user, parent, "c.txt", 100, ""); err != nil {
		t.Fatalf("fallback boundary: %v", err)
	}
}

// TestAppendTooLargeRejectedAndSelfHealing 流式化后的超限语义：请求体超过
// 会话剩余容量时报 ErrSize、offset 不变；已流入存储的字节（至多 remaining）
// 由同 offset 重试覆写（AppendAt 绝对定位），不产生数据损坏。
func TestAppendTooLargeRejectedAndSelfHealing(t *testing.T) {
	svc, store, storage, id := testService(t, 3)
	_, err := svc.Append(id, 0, bytes.NewBufferString("abcd"))
	if !errors.Is(err, ErrSize) {
		t.Fatalf("expected ErrSize, got %v", err)
	}
	v, _ := store.Get(id)
	if v.Offset != 0 {
		t.Fatalf("offset changed to %d", v.Offset)
	}
	// 同 offset 重试：覆写超限时残留的 3 字节，流转不受影响。
	if _, err := svc.Append(id, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	done, err := svc.Complete(id)
	if err != nil || done.Status != StatusAvailable {
		t.Fatalf("complete after retry: %v %s", err, done.Status)
	}
	r, err := storage.Read(done.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "abc" {
		t.Fatalf("final object = %q, want %q (残留未被覆写)", data, "abc")
	}
}

// TestAppendDeclaredLengthRejected 声明 Content-Length 超限（容量/单请求上限）
// 时在写入前直接拒绝，存储零写入。
func TestAppendDeclaredLengthRejected(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	if _, err := svc.AppendWithLength(id, 0, 4, bytes.NewBufferString("abcd")); !errors.Is(err, ErrSize) {
		t.Fatalf("expected ErrSize, got %v", err)
	}
	svc.SetPatchMaxBytes(2)
	if _, err := svc.AppendWithLength(id, 0, 3, bytes.NewBufferString("abc")); !errors.Is(err, ErrPatchTooLarge) {
		t.Fatalf("expected ErrPatchTooLarge, got %v", err)
	}
	if v, _ := store.Get(id); v.Offset != 0 {
		t.Fatalf("offset = %d, want 0", v.Offset)
	}
}

// TestAppendPatchMaxUnknownLengthChunked 未知长度（分块）请求体超过单请求
// 上限：按 ErrPatchTooLarge 拒绝（写入被截断在上限处，offset 回退）。
func TestAppendPatchMaxUnknownLengthChunked(t *testing.T) {
	svc, store, _, id := testService(t, 10)
	svc.SetPatchMaxBytes(4)
	_, err := svc.Append(id, 0, bytes.NewBufferString("abcdef")) // declared=-1
	if !errors.Is(err, ErrPatchTooLarge) {
		t.Fatalf("expected ErrPatchTooLarge, got %v", err)
	}
	if v, _ := store.Get(id); v.Offset != 0 {
		t.Fatalf("offset = %d, want 0（预占已回退）", v.Offset)
	}
	// 上限内的分块正常写入。
	if _, err := svc.Append(id, 0, bytes.NewBufferString("abcd")); err != nil {
		t.Fatal(err)
	}
	if v, _ := store.Get(id); v.Offset != 4 {
		t.Fatalf("offset = %d, want 4", v.Offset)
	}
}

// TestAppendConcurrentSameOffsetOnlyOneWins 并发 PATCH 同一 offset（tus 客户端
// 重试竞态）：per-session 互斥 + CAS 双保险下仅一方成功，落库 offset 与存储
// 内容一致、无交错损坏。
func TestAppendConcurrentSameOffsetOnlyOneWins(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	const workers = 16
	var wg sync.WaitGroup
	var successes atomic.Int32
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := svc.Append(id, 0, bytes.NewBufferString("abc")); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successes = %d, want 1", got)
	}
	v, _ := store.Get(id)
	if v.Offset != 3 {
		t.Fatalf("offset = %d, want 3", v.Offset)
	}
	if done, err := svc.Complete(id); err != nil || done.Status != StatusAvailable {
		t.Fatalf("complete: %v %s", err, done.Status)
	}
}

// TestCompleteConcurrentRunsPipelineOnce 并发 Complete（tus 自动补完与手动
// complete 竞态）：CAS 状态迁移下流水线仅执行一次，全部调用幂等返回 available。
func TestCompleteConcurrentRunsPipelineOnce(t *testing.T) {
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var createCalls atomic.Int32
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, bool, error) {
		createCalls.Add(1)
		return uuid.New(), true, nil
	})
	user, parent := uuid.New(), uuid.New()
	v, err := svc.Start(user, parent, "a.txt", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	var wg sync.WaitGroup
	results := make([]UploadSession, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.Complete(v.ID)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("complete[%d] err = %v（应幂等成功）", i, errs[i])
		}
		if results[i].Status != StatusAvailable {
			t.Fatalf("complete[%d] status = %s, want available", i, results[i].Status)
		}
	}
	if got := createCalls.Load(); got != 1 {
		t.Fatalf("createFile called %d times, want 1", got)
	}
	if got, _ := store.Get(v.ID); got.Status != StatusAvailable {
		t.Fatalf("persisted status = %s", got.Status)
	}
}

// TestCompleteConcurrentAppendDuringComplete Complete 进行中并发 Append：
// 状态已离开 uploading，Append 直接 ErrOffset，不会污染终态流转。
func TestCompleteConcurrentAppendDuringComplete(t *testing.T) {
	svc, store, _, id := testService(t, 3)
	if _, err := svc.Append(id, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	done, err := svc.Complete(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(id, 3, bytes.NewBufferString("x")); !errors.Is(err, ErrOffset) {
		t.Fatalf("append after complete: err = %v, want ErrOffset", err)
	}
	if got, _ := store.Get(id); got.Status != StatusAvailable || got.Offset != done.Offset {
		t.Fatalf("state mutated after completion: %#v", got)
	}
}

// TestMemoryStoreCAS 直接验证 store 层 CAS 语义（AdvanceOffset / Mark*）。
func TestMemoryStoreCAS(t *testing.T) {
	store := NewMemoryStore()
	user, parent := uuid.New(), uuid.New()
	v := UploadSession{ID: uuid.New(), UserID: user, ParentID: parent, TusID: uuid.New().String(), Name: "a", Size: 3, Status: StatusUploading, StorageKey: "tmp/x"}
	if err := store.Save(v); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.AdvanceOffset(v.ID, 1, 1, now); !errors.Is(err, ErrOffset) {
		t.Fatalf("AdvanceOffset from wrong offset: %v, want ErrOffset", err)
	}
	if err := store.AdvanceOffset(v.ID, 0, 3, now); err != nil {
		t.Fatal(err)
	}
	// offset 达到 size 但状态先置 verifying 后又回 uploading 的场景：
	if ok, _ := store.MarkVerifying(v.ID, 3); !ok {
		t.Fatal("MarkVerifying should win at uploading+size")
	}
	if ok, _ := store.MarkVerifying(v.ID, 3); ok {
		t.Fatal("second MarkVerifying must lose")
	}
	if ok, _ := store.MarkAvailable(v.ID, "objects/u/x", now); ok {
		t.Fatal("MarkAvailable from verifying must lose")
	}
	if ok, _ := store.MarkScanning(v.ID); !ok {
		t.Fatal("MarkScanning should win from verifying")
	}
	if ok, _ := store.MarkAvailable(v.ID, "objects/u/x", now); !ok {
		t.Fatal("MarkAvailable should win from scanning")
	}
	got, _ := store.Get(v.ID)
	if got.Status != StatusAvailable || got.CompletedAt == nil || got.StorageKey != "objects/u/x" {
		t.Fatalf("final state = %#v", got)
	}
	// uploading 期间 offset 未达 size：MarkVerifying 必须失败。
	v2 := UploadSession{ID: uuid.New(), UserID: user, ParentID: parent, TusID: uuid.New().String(), Name: "b", Size: 9, Status: StatusUploading, StorageKey: "tmp/y"}
	_ = store.Save(v2)
	if ok, _ := store.MarkVerifying(v2.ID, 9); ok {
		t.Fatal("MarkVerifying must fail while offset != size")
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

// TestCompleteCreateFileDedupeDeletesRedundantObject createFile 命中内容去重
// （newBlob=false，同 sha256 复用既有 blob）：本次上传的 finalKey 物理对象
// 被清理（与 replace 分支同策略）。
func TestCompleteCreateFileDedupeDeletesRedundantObject(t *testing.T) {
	store := NewMemoryStore()
	storage := mustLocal(t)
	svc := NewService(store, storage, time.Hour, 1<<20, false, nil, func(uuid.UUID, uuid.UUID, string, string, int64, string, string) (uuid.UUID, bool, error) {
		return uuid.New(), false, nil // 模拟 resolveUploadBlob 命中去重复用
	})
	user, parent := uuid.New(), uuid.New()
	v, err := svc.Start(user, parent, "a.txt", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	done, err := svc.Complete(v.ID)
	if err != nil || done.Status != StatusAvailable {
		t.Fatalf("complete: %v %s", err, done.Status)
	}
	if r, err := storage.Read(done.StorageKey); err == nil {
		r.Close()
		t.Fatalf("redundant object %s must be deleted on dedupe hit", done.StorageKey)
	}
}

// TestLocalStorageAppendAtOverwrite LocalStorage.AppendAt 按 offset 绝对定位
// 覆写：同 offset 重试覆盖残留（自愈），不需要 truncate。
func TestLocalStorageAppendAtOverwrite(t *testing.T) {
	storage := mustLocal(t)
	if n, err := storage.AppendAt("tmp/a", 0, strings.NewReader("abc")); err != nil || n != 3 {
		t.Fatalf("append@0: n=%d err=%v", n, err)
	}
	if n, err := storage.AppendAt("tmp/a", 2, strings.NewReader("XY")); err != nil || n != 2 {
		t.Fatalf("append@2: n=%d err=%v", n, err)
	}
	r, err := storage.Read("tmp/a")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "abXY" {
		t.Fatalf("content = %q, want %q", data, "abXY")
	}
}

// TestLocalStorageReadRange LocalStorage.ReadRange（RangeReader）与
// upload.ReadSection 走原生分支。
func TestLocalStorageReadRange(t *testing.T) {
	storage := mustLocal(t)
	if err := storage.Put("objects/u/1", strings.NewReader("hello world")); err != nil {
		t.Fatal(err)
	}
	r, err := storage.ReadRange("objects/u/1", 6, 5)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "world" {
		t.Fatalf("ReadRange = %q, want %q", data, "world")
	}
	// ReadSection 命中 RangeReader 实现时直接透传。
	sr, err := ReadSection(storage, "objects/u/1", 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	sdata, _ := io.ReadAll(sr)
	sr.Close()
	if string(sdata) != "hello" {
		t.Fatalf("ReadSection = %q, want %q", sdata, "hello")
	}
}

// legacyStorage 仅实现 Storage 四方法（不实现 RangeReader/OffsetAppender），
// 模拟 onlyoffice 等尚未实现区间读取的测试替身，验证 ReadSection 的 Seek 退化路径。
type legacyStorage struct{ inner *LocalStorage }

func (s *legacyStorage) Put(key string, r io.Reader) error { return s.inner.Put(key, r) }
func (s *legacyStorage) Append(key string, r io.Reader) (int64, error) {
	return s.inner.Append(key, r)
}
func (s *legacyStorage) Read(key string) (io.ReadCloser, error) { return s.inner.Read(key) }
func (s *legacyStorage) Delete(key string) error                { return s.inner.Delete(key) }

func TestReadSectionFallsBackToSeek(t *testing.T) {
	inner := mustLocal(t)
	if err := inner.Put("objects/u/2", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	var s Storage = &legacyStorage{inner: inner}
	r, err := ReadSection(s, "objects/u/2", 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "3456" {
		t.Fatalf("fallback ReadSection = %q, want 3456", data)
	}
}
