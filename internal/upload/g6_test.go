package upload

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/google/uuid"
)

// newG6Service 构造注入点齐全的测试服务（内存 store 实现 CountActiveByUser）。
func newG6Service(t *testing.T) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	storage, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewService(store, storage, time.Hour, 1<<20, false, nil, nil), store
}

// TestConcurrentUploadsLimit429 每用户并发上传会话上限矩阵：
// 活跃（非终态且未过期）会话达到上限时新建会话被拒（ErrTooManyUploads →
// HTTP 429）；终态流转或过期后配额释放；未注入上限/计数器不可用时不门控；
// 计数按用户隔离。
func TestConcurrentUploadsLimit429(t *testing.T) {
	user, other, parent := uuid.New(), uuid.New(), uuid.New()

	t.Run("达上限拒绝 终态释放", func(t *testing.T) {
		svc, _ := newG6Service(t)
		svc.SetMaxConcurrentUploadsProvider(func() int { return 2 })
		if _, err := svc.Start(user, parent, "a.txt", 3, ""); err != nil {
			t.Fatal(err)
		}
		v2, err := svc.Start(user, parent, "b.txt", 3, "")
		if err != nil {
			t.Fatal(err)
		}
		// 第三会话：达到上限 2 → ErrTooManyUploads。
		if _, err := svc.Start(user, parent, "c.txt", 3, ""); !errors.Is(err, ErrTooManyUploads) {
			t.Fatalf("third session: err = %v, want ErrTooManyUploads", err)
		}
		// 其他用户不受影响（计数按用户隔离）。
		if _, err := svc.Start(other, parent, "x.txt", 3, ""); err != nil {
			t.Fatal(err)
		}
		// 第二会话写入并 Complete（终态 available）→ 配额释放。
		if _, err := svc.Append(v2.ID, 0, bytes.NewBufferString("abc")); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Complete(v2.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Start(user, parent, "d.txt", 3, ""); err != nil {
			t.Fatalf("after terminal release: %v", err)
		}
	})

	t.Run("过期会话不计入活跃数", func(t *testing.T) {
		svc, store := newG6Service(t)
		svc.SetMaxConcurrentUploadsProvider(func() int { return 1 })
		v, err := svc.Start(user, parent, "stale.txt", 3, "")
		if err != nil {
			t.Fatal(err)
		}
		// 上限 1：第二会话被拒。
		if _, err := svc.Start(user, parent, "next.txt", 3, ""); !errors.Is(err, ErrTooManyUploads) {
			t.Fatalf("limit 1: err = %v, want ErrTooManyUploads", err)
		}
		// 将既有会话置为已过期 → 不再计入，新会话放行。
		stale, _ := store.Get(v.ID)
		stale.ExpiresAt = time.Now().Add(-time.Minute)
		_ = store.Update(stale)
		if _, err := svc.Start(user, parent, "next.txt", 3, ""); err != nil {
			t.Fatalf("expired session must not count: %v", err)
		}
	})

	t.Run("未注入上限不门控", func(t *testing.T) {
		svc, _ := newG6Service(t)
		for i := 0; i < 5; i++ {
			if _, err := svc.Start(user, parent, "f.txt", 3, ""); err != nil {
				t.Fatalf("no provider must not gate: %v", err)
			}
		}
	})
}

// TestBlockedExtensionsMatrix 扩展名黑名单矩阵：建会话（新文件/覆盖新版本）
// 与 Complete 双侧拦截；大小写不敏感；无点/空扩展名放行；未注入不拦截；
// 会话中途收紧黑名单时 Complete 拒绝。
func TestBlockedExtensionsMatrix(t *testing.T) {
	user, parent := uuid.New(), uuid.New()

	t.Run("建会话拦截 大小写不敏感", func(t *testing.T) {
		svc, _ := newG6Service(t)
		svc.SetBlockedExtensionsProvider(func() []string { return []string{"exe", "BAT"} })
		if _, err := svc.Start(user, parent, "evil.exe", 3, ""); !errors.Is(err, ErrBlockedExtension) {
			t.Fatalf("exe: err = %v, want ErrBlockedExtension", err)
		}
		if _, err := svc.Start(user, parent, "EVIL.Exe", 3, ""); !errors.Is(err, ErrBlockedExtension) {
			t.Fatalf("case-insensitive: err = %v, want ErrBlockedExtension", err)
		}
		if _, err := svc.Start(user, parent, "run.bat", 3, ""); !errors.Is(err, ErrBlockedExtension) {
			t.Fatalf("bat: err = %v, want ErrBlockedExtension", err)
		}
		// 合法扩展与无扩展名放行。
		if _, err := svc.Start(user, parent, "ok.txt", 3, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Start(user, parent, "noext", 3, ""); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("覆盖新版本沿用目标文件名拦截", func(t *testing.T) {
		svc, _ := newG6Service(t)
		svc.SetBlockedExtensionsProvider(func() []string { return []string{"exe"} })
		target := uuid.New()
		svc.SetVersionTarget(
			func(u, f uuid.UUID) (files.File, error) {
				return files.File{ID: f, Name: "setup.exe", OwnerID: u, Type: "file"}, nil
			},
			func(u, f uuid.UUID, k, s string, n int64, m string) (bool, error) { return true, nil },
		)
		if _, err := svc.StartReplace(user, target, 3, ""); !errors.Is(err, ErrBlockedExtension) {
			t.Fatalf("replace target .exe: err = %v, want ErrBlockedExtension", err)
		}
	})

	t.Run("Complete 复检拦截 会话置 failed", func(t *testing.T) {
		svc, store := newG6Service(t)
		blocked := false
		svc.SetBlockedExtensionsProvider(func() []string {
			if blocked {
				return []string{"txt"}
			}
			return nil
		})
		v, err := svc.Start(user, parent, "doc.txt", 3, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
			t.Fatal(err)
		}
		blocked = true // 会话中途收紧黑名单
		done, err := svc.Complete(v.ID)
		if !errors.Is(err, ErrBlockedExtension) {
			t.Fatalf("complete recheck: err = %v, want ErrBlockedExtension", err)
		}
		if done.Status != StatusFailed {
			t.Fatalf("status = %s, want failed", done.Status)
		}
		got, _ := store.Get(v.ID)
		if got.Status != StatusFailed {
			t.Fatalf("persisted status = %s, want failed", got.Status)
		}
	})

	t.Run("未注入不拦截", func(t *testing.T) {
		svc, _ := newG6Service(t)
		if _, err := svc.Start(user, parent, "anything.exe", 3, ""); err != nil {
			t.Fatalf("no provider must not block: %v", err)
		}
	})
}
