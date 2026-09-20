package files

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// agedFile 构造一个含 len(ages) 个版本的文件（无 seed 额外版本），
// 各版本 CreatedAt 为 now-ages[i]*24h（索引 0 为 v1）；current 指向最新版本。
// 返回文件行与倒序版本列表。
func agedFile(t *testing.T, repo *memVersionsRepo, owner uuid.UUID, agesDays []float64) (File, []FileVersion) {
	t.Helper()
	shas := []string{shaA, shaB, shaC, shaD, shaE, shaF, shaG, shaH}
	now := time.Now().UTC()
	f := File{ID: uuid.New(), Name: "doc.txt", OwnerID: owner, Type: "file"}
	repo.files[f.ID] = f
	for i, age := range agesDays {
		sha := shas[i%len(shas)]
		v, _, err := addVersionLogic(repo, f.ID, "objects/"+sha, sha, 3, "text/plain", owner)
		if err != nil {
			t.Fatal(err)
		}
		v.CreatedAt = now.Add(-time.Duration(age * 24 * float64(time.Hour)))
		repo.versions[v.ID] = v
	}
	versions, err := repo.ListVersionsDesc(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, versions
}

// TestPruneVersionsRetentionWindowMatrix 组合保留策略矩阵：
// 「最新 keep 个」∪「时间窗内（createdAt > now-retentionDays）」∪「current」
// 三者并集保留，其余裁剪。窗口关闭（0 天）退化为纯数量裁剪。
func TestPruneVersionsRetentionWindowMatrix(t *testing.T) {
	// 7 个版本：v1(40d)/v2(35d) 旧，v3..v7(1d) 新。
	ages := []float64{40, 35, 1, 1, 1, 1, 1}

	t.Run("窗口关闭退化为数量裁剪", func(t *testing.T) {
		repo := newMemVersionsRepo()
		f, _ := agedFile(t, repo, uuid.New(), ages)
		pruned, err := pruneVersionsLogic(repo, f.ID, 5, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if pruned != 2 {
			t.Fatalf("pruned = %d, want 2（v1/v2 超出 keep=5）", pruned)
		}
		after, _ := repo.ListVersionsDesc(f.ID)
		if len(after) != 5 {
			t.Fatalf("kept = %d, want 5", len(after))
		}
	})

	t.Run("时间窗保护窗口内版本", func(t *testing.T) {
		repo := newMemVersionsRepo()
		f, _ := agedFile(t, repo, uuid.New(), ages)
		// keep=2 保留 v7/v6；v5/v4/v3 在 30 天窗内（1d 前）不裁；v2/v1（35d/40d）裁剪。
		cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
		pruned, err := pruneVersionsLogic(repo, f.ID, 2, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if pruned != 2 {
			t.Fatalf("pruned = %d, want 2（仅旧版本被裁）", pruned)
		}
		after, _ := repo.ListVersionsDesc(f.ID)
		if len(after) != 5 { // v7/v6（keep）+ v5/v4/v3（窗口）
			t.Fatalf("kept = %d, want 5 (keep 2 + 窗口 3)", len(after))
		}
		for _, v := range after {
			if v.Version <= 2 {
				t.Fatalf("old version v%d must be pruned", v.Version)
			}
		}
	})

	t.Run("窗口宽于版本全集时不裁剪", func(t *testing.T) {
		repo := newMemVersionsRepo()
		f, _ := agedFile(t, repo, uuid.New(), ages)
		cutoff := time.Now().UTC().Add(-400 * 24 * time.Hour) // 400 天窗覆盖全部
		pruned, err := pruneVersionsLogic(repo, f.ID, 1, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		if pruned != 0 {
			t.Fatalf("pruned = %d, want 0（全部版本都在窗口内）", pruned)
		}
	})

	t.Run("current 在窗口与数量窗之外仍受保护", func(t *testing.T) {
		repo := newMemVersionsRepo()
		f, versions := agedFile(t, repo, uuid.New(), ages)
		// 回滚 current 到 v1（最旧、窗口外、数量窗外）。
		if _, err := setCurrentVersionLogic(repo, f.ID, versions[len(versions)-1].ID); err != nil {
			t.Fatal(err)
		}
		pruned, err := pruneVersionsLogic(repo, f.ID, 1, time.Now().UTC().Add(-30*24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if pruned != 1 { // 仅 v2（旧、非 current）被裁
			t.Fatalf("pruned = %d, want 1", pruned)
		}
		after, _ := repo.ListVersionsDesc(f.ID)
		found := false
		for _, v := range after {
			if v.Version == 1 {
				found = true
			}
		}
		if !found {
			t.Fatal("current-pointed v1 must survive prune")
		}
		if len(after) != 6 {
			t.Fatalf("kept = %d, want 6", len(after))
		}
	})
}

// TestEffectiveRetentionDaysHotReadAndFallback 保留时间窗热读取语义：
// 提供器优先（即时生效），未注入/负值回退 0（不启用）。
func TestEffectiveRetentionDaysHotReadAndFallback(t *testing.T) {
	s := NewStore(nil)
	if got := s.effectiveRetentionDays(); got != 0 {
		t.Fatalf("no provider = %d, want 0", got)
	}
	value := 30
	s.SetVersionRetentionDaysProvider(func() int { return value })
	if got := s.effectiveRetentionDays(); got != 30 {
		t.Fatalf("provider = %d, want 30", got)
	}
	value = 7
	if got := s.effectiveRetentionDays(); got != 7 {
		t.Fatalf("hot read = %d, want 7", got)
	}
	value = -1
	if got := s.effectiveRetentionDays(); got != 0 {
		t.Fatalf("negative fallback = %d, want 0", got)
	}
	if cutoff := s.retentionCutoff(time.Now().UTC()); !cutoff.IsZero() {
		t.Fatalf("disabled window cutoff = %v, want zero", cutoff)
	}
}

// TestDeleteVersionBoundary 删除边界矩阵：current 不可删（ErrCurrentVersion），
// 删空所有非 current 后剩余版本恒为 1（current 本身），不会出现 0 版本文件。
func TestDeleteVersionBoundary(t *testing.T) {
	repo := newMemVersionsRepo()
	f, versions := agedFile(t, repo, uuid.New(), []float64{0, 0, 0, 0}) // v1..v4
	current := repo.files[f.ID].CurrentVersionID

	// current（v4）不可删。
	if _, err := deleteVersionLogic(repo, f.ID, *current); !errors.Is(err, ErrCurrentVersion) {
		t.Fatalf("delete current: err = %v, want ErrCurrentVersion", err)
	}
	// 逐个删除全部非 current 版本。
	for _, v := range versions {
		if v.ID == *current {
			continue
		}
		if _, err := deleteVersionLogic(repo, f.ID, v.ID); err != nil {
			t.Fatalf("delete v%d: %v", v.Version, err)
		}
	}
	after, _ := repo.ListVersionsDesc(f.ID)
	if len(after) != 1 || after[0].ID != *current {
		t.Fatalf("remaining versions = %+v, want exactly the current one", after)
	}
	// 剩余唯一版本即 current：继续删除仍被拒绝 → 剩余版本数不可能为 0。
	if _, err := deleteVersionLogic(repo, f.ID, *current); !errors.Is(err, ErrCurrentVersion) {
		t.Fatalf("delete last current: err = %v, want ErrCurrentVersion", err)
	}
}

// TestDispatchVersionDeletedFilters 版本删除通知的过滤语义（与
// dispatchVersionAdded 一致）：删除者非文件行 owner 时回调。
func TestDispatchVersionDeletedFilters(t *testing.T) {
	owner, editor := uuid.New(), uuid.New()
	spaceID := uuid.New()
	version := FileVersion{ID: uuid.New(), FileID: uuid.New(), Version: 1}

	calls := 0
	cb := func(File, uuid.UUID, FileVersion) { calls++ }

	spaceFile := File{ID: version.FileID, OwnerID: owner, SpaceID: spaceID, Type: "file"}
	dispatchVersionDeleted(cb, spaceFile, editor, version)
	if calls != 1 {
		t.Fatalf("space file by non-owner: calls = %d, want 1", calls)
	}
	dispatchVersionDeleted(cb, spaceFile, owner, version)
	if calls != 1 {
		t.Fatalf("owner self-delete must not notify, calls = %d", calls)
	}
	dispatchVersionDeleted(nil, spaceFile, editor, version) // nil 回调不 panic
}

// TestFolderDepthValidationPureLogic 目录深度校验纯逻辑矩阵：
// 建子项深度 = parentDepth+1 ≤ max；移动子树最深深度 = targetDepth+height ≤ max。
func TestFolderDepthValidationPureLogic(t *testing.T) {
	// 建目录：parent 深度 31 → 子 32 ≤ 32 通过；parent 32 → 子 33 超限。
	if err := validateCreateDepth(31, 32); err != nil {
		t.Fatalf("create at boundary: %v", err)
	}
	if err := validateCreateDepth(32, 32); !errors.Is(err, ErrFolderDepth) {
		t.Fatalf("create over limit: err = %v, want ErrFolderDepth", err)
	}
	// 移动：目标 30 层 + 子树高 2 = 32 ≤ 32 通过；高 3 超限。
	if err := validateMoveDepth(30, 2, 32); err != nil {
		t.Fatalf("move at boundary: %v", err)
	}
	if err := validateMoveDepth(30, 3, 32); !errors.Is(err, ErrFolderDepth) {
		t.Fatalf("move over limit: err = %v, want ErrFolderDepth", err)
	}
	// 文件（高度恒 1）：目标 31 + 1 = 32 通过；目标 32 超限。
	if err := validateMoveDepth(31, 1, 32); err != nil {
		t.Fatalf("move file at boundary: %v", err)
	}
	if err := validateMoveDepth(32, 1, 32); !errors.Is(err, ErrFolderDepth) {
		t.Fatalf("move file over limit: err = %v, want ErrFolderDepth", err)
	}
}

// TestEffectiveMaxFolderDepthHotRead 目录深度上限热读取：提供器优先，
// 未注入/非正回退静态默认 32。
func TestEffectiveMaxFolderDepthHotRead(t *testing.T) {
	s := NewStore(nil)
	if got := s.effectiveMaxFolderDepth(); got != 32 {
		t.Fatalf("default depth = %d, want 32", got)
	}
	value := 64
	s.SetMaxFolderDepthProvider(func() int { return value })
	if got := s.effectiveMaxFolderDepth(); got != 64 {
		t.Fatalf("provider depth = %d, want 64", got)
	}
	value = 0
	if got := s.effectiveMaxFolderDepth(); got != 32 {
		t.Fatalf("fallback depth = %d, want 32", got)
	}
}
