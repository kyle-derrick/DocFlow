package upload

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/google/uuid"
)

// TestStartQuotaCheckMatrix 配额校验矩阵（C3）：未超放行 / 恰好等于放行 /
// 超限拒绝（files.ErrQuotaExceeded）；未注入回调不校验；StartReplace 同样
// 走校验。软删计入语义：quotaCheck 的已用值由注入方（files.UsedStorage）
// 提供——含回收站软删文件，本测试以 used 值直接模拟该口径。
func TestStartQuotaCheckMatrix(t *testing.T) {
	newSvc := func(quotaCheck func(uuid.UUID, int64) error) *Service {
		store := NewMemoryStore()
		storage, err := NewLocalStorage(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		svc := NewService(store, storage, time.Hour, 1<<20, false, nil, nil)
		if quotaCheck != nil {
			svc.SetQuotaCheck(quotaCheck)
		}
		return svc
	}
	// quotaGuard 模拟 files.Store.CheckUploadQuota：used 含软删（回收站）文件。
	user, parent := uuid.New(), uuid.New()
	const used = 900 // 含软删文件的当前版本总占用
	quotaGuard := func(_ uuid.UUID, size int64) error {
		if files.QuotaExceeded(used, 1000, size) {
			return files.ErrQuotaExceeded
		}
		return nil
	}

	// 未注入：不校验（旧行为保持）。
	svc := newSvc(nil)
	if _, err := svc.Start(user, parent, "a.txt", 5000, ""); err != nil {
		t.Fatalf("no quota check injected: %v", err)
	}

	svc = newSvc(quotaGuard)
	// 未超（900+100=1000）：放行。
	if _, err := svc.Start(user, parent, "ok.txt", 100, ""); err != nil {
		t.Fatalf("under quota: %v", err)
	}
	// 超限（900+101>1000）：拒绝建会话。
	if _, err := svc.Start(user, parent, "big.txt", 101, ""); !errors.Is(err, files.ErrQuotaExceeded) {
		t.Fatalf("over quota: err = %v, want files.ErrQuotaExceeded", err)
	}

	// StartReplace 同样校验：注入版本覆盖钩子后以覆盖路径复测。
	versionStore := newFakeVersionStore(5)
	target := versionStore.addFile(&fakeTargetFile{name: "t.txt", parent: parent, owner: user})
	versionStore.targets[target].seedVersion(shaOf("v1"))
	replaceSvc := NewService(NewMemoryStore(), mustLocal(t), time.Hour, 1<<20, false, nil, nil)
	replaceSvc.SetVersionTarget(versionStore.validate, versionStore.replace)
	replaceSvc.SetQuotaCheck(quotaGuard)
	if _, err := replaceSvc.StartReplace(user, target, 100, ""); err != nil {
		t.Fatalf("replace under quota: %v", err)
	}
	if _, err := replaceSvc.StartReplace(user, target, 101, ""); !errors.Is(err, files.ErrQuotaExceeded) {
		t.Fatalf("replace over quota: err = %v, want files.ErrQuotaExceeded", err)
	}
}

// TestQuotaWarnDispatcherFiredOnComplete 配额警告回调（C3）：Complete 成功
// 落库（新文件与覆盖新版本两路径）后各回调一次，携带属主/文件名/文件 ID；
// 回调错误不影响会话终态。
func TestQuotaWarnDispatcherFiredOnComplete(t *testing.T) {
	user := uuid.New()
	parent := uuid.New()

	// 新文件路径：createFile 钩子 + quotaWarn 回调。
	var warns []struct {
		user uuid.UUID
		name string
		file uuid.UUID
	}
	newFileID := uuid.New()
	svc := NewService(NewMemoryStore(), mustLocal(t), time.Hour, 1<<20, false, nil, func(_ uuid.UUID, _ uuid.UUID, _ string, _ string, _ int64, _ string, _ string) (uuid.UUID, bool, error) {
		return newFileID, true, nil
	})
	svc.SetQuotaWarnDispatcher(func(u uuid.UUID, name string, fileID uuid.UUID) {
		warns = append(warns, struct {
			user uuid.UUID
			name string
			file uuid.UUID
		}{u, name, fileID})
	})
	v, err := svc.Start(user, parent, "a.txt", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Append(v.ID, 0, bytes.NewBufferString("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(v.ID); err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || warns[0].user != user || warns[0].name != "a.txt" || warns[0].file != newFileID {
		t.Fatalf("new-file warn = %+v, want user/name/fileID", warns)
	}

	// 覆盖新版本路径：replace 钩子 + quotaWarn 回调（资源为目标文件）。
	versionStore := newFakeVersionStore(5)
	target := versionStore.addFile(&fakeTargetFile{name: "t.txt", parent: parent, owner: user})
	versionStore.targets[target].seedVersion(shaOf("v1"))
	replaceWarns := 0
	replaceSvc := NewService(NewMemoryStore(), mustLocal(t), time.Hour, 1<<20, false, nil, nil)
	replaceSvc.SetVersionTarget(versionStore.validate, versionStore.replace)
	replaceSvc.SetQuotaWarnDispatcher(func(uuid.UUID, string, uuid.UUID) { replaceWarns++ })
	rv, err := replaceSvc.StartReplace(user, target, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replaceSvc.Append(rv.ID, 0, bytes.NewBufferString("xyz")); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceSvc.Complete(rv.ID); err != nil {
		t.Fatal(err)
	}
	if replaceWarns != 1 {
		t.Fatalf("replace warn calls = %d, want 1", replaceWarns)
	}
}
