package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/upload"
)

// --- 测试替身 ---

// fakeCompleter 记录 Complete 调用（channel 通知），可注入返回值。
type fakeCompleter struct {
	called  chan uuid.UUID
	session upload.UploadSession
	err     error
}

func newFakeCompleter(session upload.UploadSession, err error) *fakeCompleter {
	return &fakeCompleter{called: make(chan uuid.UUID, 4), session: session, err: err}
}

func (f *fakeCompleter) Complete(id uuid.UUID) (upload.UploadSession, error) {
	f.called <- id
	return f.session, f.err
}

// fakeExtractor 记录 AutoExtract 调用（channel 通知）。
type fakeExtractor struct {
	called chan uuid.UUID
}

func (f *fakeExtractor) AutoExtract(fileID uuid.UUID) { f.called <- fileID }

// fakeRecorder 记录审计条目。
type fakeRecorder struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (f *fakeRecorder) Record(e audit.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// counterValue 从默认 registry 读取指定指标中标签完全匹配系列的当前值
// （计数器为进程级全局，测试一律取前后差值断言；与 internal/upload 同款实现）。
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func waitCalled(t *testing.T, ch chan uuid.UUID) uuid.UUID {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not invoked within timeout")
		return uuid.Nil
	}
}

// --- InProcess 驱动 ---

// InProcess Enqueuer 在 goroutine 中触发处理函数并传入正确 ID，
// 计入 docflow_queue_enqueued_total{driver=inprocess} 与
// docflow_queue_processed_total{status=success}。
func TestInProcessDispatchesHandlers(t *testing.T) {
	sessionID, fileID := uuid.New(), uuid.New()
	completions := newFakeCompleter(upload.UploadSession{}, nil)
	extractor := &fakeExtractor{called: make(chan uuid.UUID, 4)}
	p := NewInProcess(
		CompleteUploadHandler(completions, nil),
		ExtractWebpkgHandler(extractor),
	)
	if p.Driver() != "inprocess" {
		t.Fatalf("Driver() = %q, want inprocess", p.Driver())
	}

	enqueuedBefore := counterValue(t, "docflow_queue_enqueued_total", map[string]string{"type": TaskTypeCompleteUpload, "driver": "inprocess"})
	okBefore := counterValue(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "success"})
	if err := p.EnqueueCompleteUpload(sessionID); err != nil {
		t.Fatalf("EnqueueCompleteUpload: %v", err)
	}
	if got := waitCalled(t, completions.called); got != sessionID {
		t.Fatalf("Complete id = %s, want %s", got, sessionID)
	}
	if d := counterValue(t, "docflow_queue_enqueued_total", map[string]string{"type": TaskTypeCompleteUpload, "driver": "inprocess"}) - enqueuedBefore; d != 1 {
		t.Fatalf("enqueued 差值 = %v, want 1", d)
	}
	waitMetric(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "success"}, okBefore, 1)

	extractOKBefore := counterValue(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeExtractWebpkg, "status": "success"})
	if err := p.EnqueueExtractWebpkg(fileID); err != nil {
		t.Fatalf("EnqueueExtractWebpkg: %v", err)
	}
	if got := waitCalled(t, extractor.called); got != fileID {
		t.Fatalf("AutoExtract id = %s, want %s", got, fileID)
	}
	waitMetric(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeExtractWebpkg, "status": "success"}, extractOKBefore, 1)
}

// waitMetric 轮询等待计数器差值达到 want（InProcess 在 goroutine 中计数，
// 断言点可能先于 goroutine 完成）。
func waitMetric(t *testing.T, name string, labels map[string]string, before, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if counterValue(t, name, labels)-before == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s%v 差值 != %v", name, labels, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 处理函数 panic 被 recover，进程不崩，计入 processed{status=failed}；
// 后续任务照常执行（哨兵证明队列未死）。
func TestInProcessRecoversPanic(t *testing.T) {
	boom := func(context.Context, uuid.UUID) error { panic("boom") }
	failedBefore := counterValue(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "failed"})
	p := NewInProcess(boom, boom)
	if err := p.EnqueueCompleteUpload(uuid.New()); err != nil {
		t.Fatalf("enqueue panicking task: %v", err)
	}
	waitMetric(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "failed"}, failedBefore, 1)
	done := make(chan struct{})
	p.completeUpload = func(context.Context, uuid.UUID) error { close(done); return nil }
	if err := p.EnqueueCompleteUpload(uuid.New()); err != nil {
		t.Fatalf("enqueue after panic: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("panic was not recovered (sentinel never ran)")
	}
}

// 处理函数返回错误：InProcess 不炸进程，计入 processed{status=failed}。
func TestInProcessHandlerErrorCountedFailed(t *testing.T) {
	cause := errors.New("transient")
	p := NewInProcess(func(context.Context, uuid.UUID) error { return cause }, nil)
	failedBefore := counterValue(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "failed"})
	if err := p.EnqueueCompleteUpload(uuid.New()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitMetric(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "failed"}, failedBefore, 1)
}

// Close 取消在途任务的执行 ctx 并等待其返回；等待在途任务（不感知 ctx）
// 时阻塞至任务结束，且 Close 幂等（二次调用立即返回已完成状态）。
func TestInProcessCloseWaitsForInFlightTasks(t *testing.T) {
	ctxSeen := make(chan context.Context, 1)
	release := make(chan struct{})
	p := NewInProcess(func(ctx context.Context, id uuid.UUID) error {
		ctxSeen <- ctx
		<-release
		return nil
	}, nil)
	if err := p.EnqueueCompleteUpload(uuid.New()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var taskCtx context.Context
	select {
	case taskCtx = <-ctxSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("task was not invoked within timeout")
	}

	closed := make(chan bool, 1)
	go func() { closed <- p.Close(2 * time.Second) }()
	// 在途任务未结束：Close 不得提前返回。
	select {
	case <-closed:
		t.Fatal("Close returned while task still running")
	case <-time.After(100 * time.Millisecond):
	}
	// Close 已取消在途任务的执行 ctx。
	select {
	case <-taskCtx.Done():
	default:
		t.Fatal("task ctx was not canceled by Close")
	}

	close(release)
	select {
	case ok := <-closed:
		if !ok {
			t.Fatal("Close = false, want true after tasks drained")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after task finished")
	}
	// 幂等：收尾完成后再次 Close 立即返回 true。
	if !p.Close(100 * time.Millisecond) {
		t.Fatal("second Close = false, want true")
	}
}

// Close 超时：任务卡死（既不结束也不响应 ctx）时按 timeout 返回 false，
// 不无限阻塞退出序列。
func TestInProcessCloseTimesOut(t *testing.T) {
	block := make(chan struct{})
	p := NewInProcess(func(context.Context, uuid.UUID) error {
		<-block
		return nil
	}, nil)
	defer close(block)
	if err := p.EnqueueCompleteUpload(uuid.New()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	start := time.Now()
	if p.Close(50 * time.Millisecond) {
		t.Fatal("Close = true, want false on timeout")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("Close returned early: %v", elapsed)
	}
}

// --- 载荷序列化 / asynq mux 派发（不连 Redis） ---

// 载荷 JSON 形状与 decodePayload 往返一致。
func TestPayloadRoundTrip(t *testing.T) {
	sessionID, fileID := uuid.New(), uuid.New()
	raw, err := marshalCompleteUploadPayload(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"session_id":%q}`, sessionID); string(raw) != want {
		t.Fatalf("complete-upload payload = %s, want %s", raw, want)
	}
	got, err := decodePayload(TaskTypeCompleteUpload, raw)
	if err != nil || got != sessionID {
		t.Fatalf("decode = (%s, %v), want (%s, nil)", got, err, sessionID)
	}

	raw, err = marshalExtractWebpkgPayload(fileID)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`{"file_id":%q}`, fileID); string(raw) != want {
		t.Fatalf("extract-webpkg payload = %s, want %s", raw, want)
	}
	got, err = decodePayload(TaskTypeExtractWebpkg, raw)
	if err != nil || got != fileID {
		t.Fatalf("decode = (%s, %v), want (%s, nil)", got, err, fileID)
	}

	// 非法载荷与未知类型均报错。
	if _, err := decodePayload(TaskTypeCompleteUpload, []byte("not-json")); err == nil {
		t.Fatal("invalid payload: want error")
	}
	if _, err := decodePayload("task:unknown", []byte("{}")); err == nil {
		t.Fatal("unknown task type: want error")
	}
}

// asynq 任务构造：typename 与载荷 JSON 正确。
func TestNewAsynqTaskTypenameAndPayload(t *testing.T) {
	sessionID := uuid.New()
	payload, err := marshalCompleteUploadPayload(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	task := asynq.NewTask(TaskTypeCompleteUpload, payload)
	if task.Type() != "task:complete-upload" {
		t.Fatalf("typename = %q", task.Type())
	}
	if string(task.Payload()) != fmt.Sprintf(`{"session_id":%q}`, sessionID) {
		t.Fatalf("payload = %s", task.Payload())
	}
}

// NewMux 注册的处理器可脱离 Redis 直接经 ProcessTask 派发（asynq mux 的
// 本地分发路径），两类任务均到达共用 TaskFunc 并计入 processed{success}。
func TestMuxDispatchesTasksLocally(t *testing.T) {
	sessionID, fileID := uuid.New(), uuid.New()
	completions := newFakeCompleter(upload.UploadSession{}, nil)
	extractor := &fakeExtractor{called: make(chan uuid.UUID, 4)}
	mux := NewMux(
		CompleteUploadHandler(completions, nil),
		ExtractWebpkgHandler(extractor),
	)
	payload, _ := marshalCompleteUploadPayload(sessionID)
	if err := mux.ProcessTask(context.Background(), asynq.NewTask(TaskTypeCompleteUpload, payload)); err != nil {
		t.Fatalf("ProcessTask complete-upload: %v", err)
	}
	if got := waitCalled(t, completions.called); got != sessionID {
		t.Fatalf("Complete id = %s, want %s", got, sessionID)
	}
	payload, _ = marshalExtractWebpkgPayload(fileID)
	if err := mux.ProcessTask(context.Background(), asynq.NewTask(TaskTypeExtractWebpkg, payload)); err != nil {
		t.Fatalf("ProcessTask extract-webpkg: %v", err)
	}
	if got := waitCalled(t, extractor.called); got != fileID {
		t.Fatalf("AutoExtract id = %s, want %s", got, fileID)
	}
	// 未知 typename 由 ServeMux 拒绝。
	if err := mux.ProcessTask(context.Background(), asynq.NewTask("task:unknown", []byte("{}"))); err == nil {
		t.Fatal("unknown typename: want error")
	}
}

// 载荷非法：mux 包装返回错误（含 SkipRetry 语义），计入 processed{failed}。
func TestMuxInvalidPayloadFails(t *testing.T) {
	mux := NewMux(func(context.Context, uuid.UUID) error { return nil }, nil)
	failedBefore := counterValue(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "failed"})
	err := mux.ProcessTask(context.Background(), asynq.NewTask(TaskTypeCompleteUpload, []byte("not-json")))
	if err == nil {
		t.Fatal("invalid payload: want error")
	}
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v, want SkipRetry wrapped", err)
	}
	if d := counterValue(t, "docflow_queue_processed_total", map[string]string{"type": TaskTypeCompleteUpload, "status": "failed"}) - failedBefore; d != 1 {
		t.Fatalf("processed{failed} 差值 = %v, want 1", d)
	}
}

// --- 处理函数语义 ---

// CompleteUploadHandler：成功后记录审计；终态错误归零（不重试）且不记审计；
// 瞬时错误原样返回（交由 redis 驱动重试）。
func TestCompleteUploadHandlerAuditAndErrors(t *testing.T) {
	owner := uuid.New()
	target := uuid.New()
	ok := upload.UploadSession{ID: uuid.New(), UserID: owner, Name: "a<b\"quote.txt", Size: 12, TargetFileID: &target}
	recorder := &fakeRecorder{}
	fn := CompleteUploadHandler(newFakeCompleter(ok, nil), recorder)
	if err := fn(context.Background(), ok.ID); err != nil {
		t.Fatalf("success path: %v", err)
	}
	entries := func() []audit.Entry {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		return append([]audit.Entry(nil), recorder.entries...)
	}()
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d, want 2 (upload_complete + version.create)", len(entries))
	}
	if entries[0].Action != audit.ActionUploadComplete || entries[0].ResourceID != ok.ID.String() {
		t.Fatalf("entry[0] = %+v", entries[0])
	}
	if entries[1].Action != audit.ActionVersionCreate || entries[1].ResourceID != target.String() {
		t.Fatalf("entry[1] = %+v", entries[1])
	}
	if entries[0].Metadata != `{"name":"a_b_quote.txt","size":12}` {
		t.Fatalf("metadata = %s (name 须做注入清洗)", entries[0].Metadata)
	}

	// 终态错误：归零返回、无新增审计。
	for _, terminal := range []error{upload.ErrNotFound, upload.ErrRejected, upload.ErrFailed, upload.ErrChecksum, upload.ErrSize, upload.ErrTargetUnavailable} {
		before := recorder.count()
		if err := CompleteUploadHandler(newFakeCompleter(upload.UploadSession{}, terminal), recorder)(context.Background(), uuid.New()); err != nil {
			t.Fatalf("terminal %v: err = %v, want nil", terminal, err)
		}
		if recorder.count() != before {
			t.Fatalf("terminal %v: 不应记录审计", terminal)
		}
	}

	// 瞬时错误：原样返回。
	cause := errors.New("db down")
	if err := CompleteUploadHandler(newFakeCompleter(upload.UploadSession{}, cause), nil)(context.Background(), uuid.New()); !errors.Is(err, cause) {
		t.Fatalf("transient err = %v, want %v", err, cause)
	}
}

// ExtractWebpkgHandler：触发 AutoExtract；nil 依赖安全返回。
func TestExtractWebpkgHandler(t *testing.T) {
	extractor := &fakeExtractor{called: make(chan uuid.UUID, 4)}
	fn := ExtractWebpkgHandler(extractor)
	fileID := uuid.New()
	if err := fn(context.Background(), fileID); err != nil {
		t.Fatalf("ExtractWebpkgHandler: %v", err)
	}
	if got := waitCalled(t, extractor.called); got != fileID {
		t.Fatalf("AutoExtract id = %s, want %s", got, fileID)
	}
	if err := ExtractWebpkgHandler(nil)(context.Background(), fileID); err != nil {
		t.Fatalf("nil extractor: %v", err)
	}
}
