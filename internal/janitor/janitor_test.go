package janitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/upload"
	"github.com/google/uuid"
)

// ---- 内存 fake ----

// memRepo 是 Repo 的内存实现。
type memRepo struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]upload.UploadSession
	blobs    map[uuid.UUID]files.ObjectBlob
	files    map[uuid.UUID]files.File
	failErr  error
	// recheckHook 在 DeleteBlobRechecked 复核前调用，模拟「先复活后清理」竞速。
	recheckHook func(id uuid.UUID)
}

func newMemRepo() *memRepo {
	return &memRepo{sessions: make(map[uuid.UUID]upload.UploadSession), blobs: make(map[uuid.UUID]files.ObjectBlob), files: make(map[uuid.UUID]files.File)}
}

func (m *memRepo) ExpiredActiveSessions(now time.Time, limit int) ([]upload.UploadSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []upload.UploadSession
	for _, s := range m.sessions {
		if s.ExpiresAt.Before(now) && s.Status != upload.StatusAvailable && s.Status != upload.StatusQuarantined && s.Status != upload.StatusFailed {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memRepo) FailSession(id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failErr != nil {
		return false, m.failErr
	}
	s, ok := m.sessions[id]
	if !ok {
		return false, nil
	}
	switch s.Status {
	case upload.StatusAvailable, upload.StatusQuarantined, upload.StatusFailed:
		return false, nil // 已终态：幂等跳过
	}
	s.Status = upload.StatusFailed
	m.sessions[id] = s
	return true, nil
}

func (m *memRepo) StaleTerminalSessions(now time.Time, retain time.Duration, limit int) ([]upload.UploadSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := now.Add(-retain)
	var out []upload.UploadSession
	for _, s := range m.sessions {
		if s.Status != upload.StatusAvailable && s.Status != upload.StatusQuarantined && s.Status != upload.StatusFailed {
			continue
		}
		terminal := s.ExpiresAt
		if s.CompletedAt != nil && s.CompletedAt.Before(terminal) {
			terminal = *s.CompletedAt
		}
		if terminal.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memRepo) DeleteSessions(ids []uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range ids {
		if _, ok := m.sessions[id]; ok {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

func (m *memRepo) DeletingBlobs(limit int) ([]files.ObjectBlob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []files.ObjectBlob
	for _, b := range m.blobs {
		if b.Status == files.BlobStatusDeleting && b.RefCount == 0 {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *memRepo) DeleteBlobRechecked(id uuid.UUID, deleteObject func(string) error) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.recheckHook != nil {
		m.recheckHook(id)
	}
	b, ok := m.blobs[id]
	if !ok || b.Status != files.BlobStatusDeleting || b.RefCount != 0 {
		return false, nil // 行锁复核不通过：已复活/仍被引用/已删除
	}
	if err := deleteObject(b.StorageKey); err != nil {
		return false, err
	}
	delete(m.blobs, id)
	return true, nil
}

func (m *memRepo) ExpiredTrashTopLevel(now time.Time, retain time.Duration, limit int) ([]files.File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := now.Add(-retain)
	var out []files.File
	for _, f := range m.files {
		if f.DeletedAt == nil || !f.DeletedAt.Before(cutoff) || f.IsRoot {
			continue
		}
		if f.ParentID != nil {
			if p, ok := m.files[*f.ParentID]; ok && p.DeletedAt != nil {
				continue // 父目录也在回收站：由父目录统一处理
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// fakePurger 实现 Purger：记录调用并模拟硬删除（含返回待物理删除的 blob）。
type fakePurger struct {
	repo     *memRepo
	purged   []uuid.UUID
	blobBack []files.ObjectBlob
	blobsN   int
}

func (p *fakePurger) Purge(owner, id uuid.UUID) ([]files.File, []files.ObjectBlob, error) {
	p.repo.mu.Lock()
	defer p.repo.mu.Unlock()
	f, ok := p.repo.files[id]
	if !ok || f.OwnerID != owner || f.DeletedAt == nil || f.IsRoot {
		return nil, nil, files.ErrNotFound
	}
	delete(p.repo.files, id)
	p.purged = append(p.purged, id)
	return []files.File{f}, p.blobBack, nil
}

func (p *fakePurger) PurgeBlobs(blobs []files.ObjectBlob, deleteObject func(string) error) error {
	p.blobsN += len(blobs)
	for _, b := range blobs {
		if err := deleteObject(b.StorageKey); err != nil {
			return err
		}
	}
	return nil
}

// fakeStorage 实现 ObjectDeleter。
type fakeStorage struct {
	mu      sync.Mutex
	deleted []string
	errFor  map[string]error
}

func (s *fakeStorage) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	return s.errFor[key]
}

func (s *fakeStorage) deletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// fakeSettings 实现 SettingsProvider。
type fakeSettings struct {
	days int
	err  error
}

func (f *fakeSettings) GetInt(key string) (int, error) {
	if key != settings.KeyRetentionTrashDays {
		return 0, errors.New("unexpected key")
	}
	return f.days, f.err
}

// memAudit 记录审计条目。
type memAudit struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (m *memAudit) Record(e audit.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAudit) find(action string) *audit.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.entries {
		if m.entries[i].Action == action {
			return &m.entries[i]
		}
	}
	return nil
}

var testNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func newTestJanitor(repo Repo, purger Purger, storage ObjectDeleter, sp SettingsProvider, recorder audit.Recorder) *Janitor {
	j := New(repo, purger, storage, sp, recorder)
	j.SetClock(func() time.Time { return testNow })
	j.logf = func(string, ...any) {}
	return j
}

func addSession(repo *memRepo, s upload.UploadSession) upload.UploadSession {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.StorageKey == "" {
		s.StorageKey = "tmp/" + s.ID.String()
	}
	repo.sessions[s.ID] = s
	return s
}

// ---- 上传会话清理 ----

// 过期非终态会话置 failed 并删 tmp 对象；非 tmp 对象不动；未过期不动；
// 终态超 24h 删行、未超期保留。
func TestSweepUploadSessions(t *testing.T) {
	repo := newMemRepo()
	storage := &fakeStorage{}
	expired := addSession(repo, upload.UploadSession{Status: upload.StatusUploading, ExpiresAt: testNow.Add(-time.Hour), StorageKey: "tmp/expired"})
	expiredObjectKey := addSession(repo, upload.UploadSession{Status: upload.StatusUploading, ExpiresAt: testNow.Add(-time.Hour), StorageKey: "objects/user/final"})
	fresh := addSession(repo, upload.UploadSession{Status: upload.StatusUploading, ExpiresAt: testNow.Add(time.Hour)})
	completedAt := testNow.Add(-48 * time.Hour)
	staleTerminal := addSession(repo, upload.UploadSession{Status: upload.StatusAvailable, ExpiresAt: testNow.Add(-49 * time.Hour), CompletedAt: &completedAt})
	// 终态但未超 24h：保留行。
	recentTerminal := addSession(repo, upload.UploadSession{Status: upload.StatusFailed, ExpiresAt: testNow.Add(-2 * time.Hour)})

	j := newTestJanitor(repo, &fakePurger{}, storage, nil, audit.NopRecorder{})
	j.RunOnce()

	if got := repo.sessions[expired.ID].Status; got != upload.StatusFailed {
		t.Fatalf("expired session status = %v, want failed", got)
	}
	if got := repo.sessions[expiredObjectKey.ID].Status; got != upload.StatusFailed {
		t.Fatalf("expired non-tmp session status = %v, want failed", got)
	}
	if got := repo.sessions[fresh.ID].Status; got != upload.StatusUploading {
		t.Fatalf("fresh session status = %v, want uploading", got)
	}
	keys := storage.deletedKeys()
	if len(keys) != 1 || keys[0] != "tmp/expired" {
		t.Fatalf("physical deletes = %v, want only tmp/expired", keys)
	}
	if _, ok := repo.sessions[staleTerminal.ID]; ok {
		t.Fatal("stale terminal session row must be removed")
	}
	if _, ok := repo.sessions[recentTerminal.ID]; !ok {
		t.Fatal("recent terminal session row must be kept")
	}
}

// 置 failed 与对象删除失败不中断：其余会话仍被处理；审计记录数量。
func TestSweepUploadSessionsAuditAndErrorIsolation(t *testing.T) {
	repo := newMemRepo()
	storage := &fakeStorage{errFor: map[string]error{"tmp/broken": errors.New("disk full")}}
	rec := &memAudit{}
	a := addSession(repo, upload.UploadSession{Status: upload.StatusUploading, ExpiresAt: testNow.Add(-time.Hour), StorageKey: "tmp/broken"})
	b := addSession(repo, upload.UploadSession{Status: upload.StatusUploading, ExpiresAt: testNow.Add(-time.Hour), StorageKey: "tmp/ok"})

	j := newTestJanitor(repo, &fakePurger{}, storage, nil, rec)
	j.RunOnce()

	if got := repo.sessions[a.ID].Status; got != upload.StatusFailed {
		t.Fatalf("session a status = %v, want failed (storage error must not undo state)", got)
	}
	if got := repo.sessions[b.ID].Status; got != upload.StatusFailed {
		t.Fatalf("session b status = %v, want failed (error isolation)", got)
	}
	e := rec.find(audit.ActionJanitorUpload)
	if e == nil {
		t.Fatal("janitor.upload audit missing")
	}
	var meta map[string]int
	if err := json.Unmarshal([]byte(e.Metadata), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["failed"] != 2 || meta["rows_removed"] != 0 {
		t.Fatalf("metadata = %v, want failed=2 rows_removed=0", meta)
	}
}

// ---- deleting blob 回收 ----

// deleting 且零引用 → 物理删对象并删行；仍被引用的不处理。
func TestSweepDeletingBlobs(t *testing.T) {
	repo := newMemRepo()
	storage := &fakeStorage{}
	rec := &memAudit{}
	blob := files.ObjectBlob{ID: uuid.New(), SHA256: "aa", StorageKey: "objects/a", RefCount: 0, Status: files.BlobStatusDeleting}
	referenced := files.ObjectBlob{ID: uuid.New(), SHA256: "bb", StorageKey: "objects/b", RefCount: 2, Status: files.BlobStatusDeleting}
	available := files.ObjectBlob{ID: uuid.New(), SHA256: "cc", StorageKey: "objects/c", RefCount: 0, Status: files.BlobStatusAvailable}
	repo.blobs[blob.ID] = blob
	repo.blobs[referenced.ID] = referenced
	repo.blobs[available.ID] = available

	j := newTestJanitor(repo, &fakePurger{}, storage, nil, rec)
	j.RunOnce()

	if keys := storage.deletedKeys(); len(keys) != 1 || keys[0] != "objects/a" {
		t.Fatalf("physical deletes = %v, want only objects/a", keys)
	}
	if _, ok := repo.blobs[blob.ID]; ok {
		t.Fatal("blob row must be removed")
	}
	if _, ok := repo.blobs[referenced.ID]; !ok {
		t.Fatal("referenced blob must survive")
	}
	if _, ok := repo.blobs[available.ID]; !ok {
		t.Fatal("available blob must survive")
	}
	e := rec.find(audit.ActionJanitorBlob)
	if e == nil {
		t.Fatal("janitor.blob audit missing")
	}
	if !strings.Contains(e.Metadata, `"deleted":1`) {
		t.Fatalf("metadata = %s, want deleted=1", e.Metadata)
	}
}

// 复活竞速：janitor 锁行复核前 blob 已被 AddVersion 复活（available/ref=1），
// 复核不通过 → 不删物理对象、不删行。
func TestSweepDeletingBlobsResurrectRace(t *testing.T) {
	repo := newMemRepo()
	storage := &fakeStorage{}
	blob := files.ObjectBlob{ID: uuid.New(), SHA256: "dd", StorageKey: "objects/raced", RefCount: 0, Status: files.BlobStatusDeleting}
	repo.blobs[blob.ID] = blob
	// 行锁复核瞬间的竞速：另一个 AddVersion 已把行复活。
	repo.recheckHook = func(id uuid.UUID) {
		b := repo.blobs[id]
		b.Status = files.BlobStatusAvailable
		b.RefCount = 1
		repo.blobs[id] = b
	}

	j := newTestJanitor(repo, &fakePurger{}, storage, nil, audit.NopRecorder{})
	j.RunOnce()

	if keys := storage.deletedKeys(); len(keys) != 0 {
		t.Fatalf("physical deletes = %v, want none (blob resurrected)", keys)
	}
	got, ok := repo.blobs[blob.ID]
	if !ok || got.Status != files.BlobStatusAvailable || got.RefCount != 1 {
		t.Fatalf("blob = %+v ok=%v, want resurrected row preserved", got, ok)
	}
}

// ---- 回收站清理 ----

// 软删除超 retention.trash_days 触发 Purge + PurgeBlobs；未超期不动；
// settings 读取失败回退默认 30 天。
func TestSweepTrash(t *testing.T) {
	owner := uuid.New()
	repo := newMemRepo()
	storage := &fakeStorage{}
	rec := &memAudit{}
	oldDeleted := testNow.Add(-40 * 24 * time.Hour)
	freshDeleted := testNow.Add(-5 * 24 * time.Hour)
	old := files.File{ID: uuid.New(), Name: "old.txt", OwnerID: owner, Type: "file", DeletedAt: &oldDeleted}
	fresh := files.File{ID: uuid.New(), Name: "fresh.txt", OwnerID: owner, Type: "file", DeletedAt: &freshDeleted}
	repo.files[old.ID] = old
	repo.files[fresh.ID] = fresh
	blob := files.ObjectBlob{ID: uuid.New(), SHA256: "ee", StorageKey: "objects/trash", RefCount: 0, Status: files.BlobStatusAvailable}
	purger := &fakePurger{repo: repo, blobBack: []files.ObjectBlob{blob}}

	// settings：7 天保留（40 天前删除的超期，5 天前的未超期）。
	j := newTestJanitor(repo, purger, storage, &fakeSettings{days: 7}, rec)
	j.RunOnce()

	if len(purger.purged) != 1 || purger.purged[0] != old.ID {
		t.Fatalf("purged = %v, want only old", purger.purged)
	}
	if purger.blobsN != 1 {
		t.Fatalf("PurgeBlobs called with %d blobs, want 1", purger.blobsN)
	}
	if keys := storage.deletedKeys(); len(keys) != 1 || keys[0] != "objects/trash" {
		t.Fatalf("physical deletes = %v, want objects/trash", keys)
	}
	if _, ok := repo.files[fresh.ID]; !ok {
		t.Fatal("fresh trash item must be kept")
	}
	e := rec.find(audit.ActionJanitorTrash)
	if e == nil {
		t.Fatal("janitor.trash audit missing")
	}
	if !strings.Contains(e.Metadata, `"files":1`) || !strings.Contains(e.Metadata, `"trash_days":7`) {
		t.Fatalf("metadata = %s, want files=1 trash_days=7", e.Metadata)
	}
}

// settings 读失败回退默认 30 天：20 天前的项不删，31 天前的项删。
func TestSweepTrashSettingsFallback(t *testing.T) {
	owner := uuid.New()
	repo := newMemRepo()
	storage := &fakeStorage{}
	d20 := testNow.Add(-20 * 24 * time.Hour)
	d31 := testNow.Add(-31 * 24 * time.Hour)
	inWindow := files.File{ID: uuid.New(), Name: "20d.txt", OwnerID: owner, Type: "file", DeletedAt: &d20}
	beyond := files.File{ID: uuid.New(), Name: "31d.txt", OwnerID: owner, Type: "file", DeletedAt: &d31}
	repo.files[inWindow.ID] = inWindow
	repo.files[beyond.ID] = beyond
	purger := &fakePurger{repo: repo}

	j := newTestJanitor(repo, purger, storage, &fakeSettings{err: errors.New("db down")}, audit.NopRecorder{})
	j.RunOnce()

	if len(purger.purged) != 1 || purger.purged[0] != beyond.ID {
		t.Fatalf("purged = %v, want only 31d item (fallback 30 days)", purger.purged)
	}
}

// Purge 失败（如用户恰好恢复）不中断：后续项仍处理。
func TestSweepTrashErrorIsolation(t *testing.T) {
	owner := uuid.New()
	repo := newMemRepo()
	storage := &fakeStorage{}
	d := testNow.Add(-40 * 24 * time.Hour)
	a := files.File{ID: uuid.New(), Name: "a.txt", OwnerID: owner, Type: "file", DeletedAt: &d}
	b := files.File{ID: uuid.New(), Name: "b.txt", OwnerID: owner, Type: "file", DeletedAt: &d}
	repo.files[a.ID] = a
	repo.files[b.ID] = b
	purger := &fakePurger{repo: repo}
	// 模拟 a 恰好被用户恢复（Purge 拒绝非软删除文件 → ErrNotFound）。
	restored := a
	alive := testNow.Add(-time.Minute)
	restored.DeletedAt = &alive
	repo.files[a.ID] = restored

	j := newTestJanitor(repo, purger, storage, nil, audit.NopRecorder{})
	j.RunOnce()

	if len(purger.purged) != 1 || purger.purged[0] != b.ID {
		t.Fatalf("purged = %v, want only b (a raced with restore)", purger.purged)
	}
}

// ---- RunForever ----

// 启动即跑一次，随后按 interval 周期执行，ctx 取消后退出。
func TestRunForeverRunsImmediatelyThenTicker(t *testing.T) {
	repo := newMemRepo()
	storage := &fakeStorage{}
	j := newTestJanitor(repo, &fakePurger{}, storage, nil, audit.NopRecorder{})
	j.SetInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.RunForever(ctx); close(done) }()

	// 启动即跑：不等 ticker 就应完成第一轮。
	deadline := time.Now().Add(time.Second)
	for j.runs.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := j.runs.Load(); n < 3 {
		t.Fatalf("runs = %d, want >= 3 (immediate first run + ticker)", n)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunForever did not exit after cancel")
	}
}
