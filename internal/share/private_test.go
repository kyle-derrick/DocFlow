package share

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/team"
)

// newPrivateTestEnv 构造带团队判定器的内存环境：
// share 服务通过 team.Service（内存 store）实时判定团队成员关系。
func newPrivateTestEnv(t *testing.T) (*Service, *MemoryStore, *fakeFiles, *team.Service, *team.MemoryStore, uuid.UUID, uuid.UUID, *time.Time) {
	t.Helper()
	repo := NewMemoryStore()
	ff := newFakeFiles()
	teamRepo := team.NewMemoryStore()
	teamSvc := team.NewService(teamRepo)
	svc := NewService(repo, ff)
	svc.SetTeamMembership(teamSvc)
	owner := uuid.New()
	fileID := ff.addFile(owner, "secret.txt", "file", files.BlobStatusAvailable)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	return svc, repo, ff, teamSvc, teamRepo, owner, fileID, &now
}

func TestCreatePrivateNoTokenAndAuthorizationRows(t *testing.T) {
	svc, repo, _, teamSvc, _, owner, fileID, _ := newPrivateTestEnv(t)
	teamOwner, explicit := uuid.New(), uuid.New()
	tm, _, err := teamSvc.CreateTeam(teamOwner, "设计组", "")
	if err != nil {
		t.Fatal(err)
	}

	sh, err := svc.CreatePrivate(owner, fileID, PermissionDownload, time.Hour, nil, []uuid.UUID{explicit, explicit, uuid.Nil}, []uuid.UUID{tm.ID})
	if err != nil {
		t.Fatal(err)
	}
	if sh.TokenHash != "" {
		t.Fatal("private share must not carry a token hash")
	}
	if sh.Visibility != VisibilityPrivate {
		t.Fatalf("visibility = %q, want private", sh.Visibility)
	}
	// 去重后 share_users 恰好一条（nil 过滤）。
	users, _ := repo.ListShareUserIDs(sh.ID)
	if len(users) != 1 || users[0] != explicit {
		t.Fatalf("share_users = %v, want [%v]", users, explicit)
	}
	teams, _ := repo.ListShareTeamIDs(sh.ID)
	if len(teams) != 1 || teams[0] != tm.ID {
		t.Fatalf("share_teams = %v, want [%v]", teams, tm.ID)
	}
	// 私有分享不影响 files.is_public。
	if repo.IsPublic(fileID) {
		t.Fatal("private share must not set files.is_public")
	}
	// 私有分享不能通过 token 解析（空 token 直接 404 语义）。
	if _, err := svc.Resolve(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve empty token: err = %v, want ErrNotFound", err)
	}
}

func TestPrivateShareAccessMatrix(t *testing.T) {
	svc, _, _, teamSvc, _, owner, fileID, _ := newPrivateTestEnv(t)
	teamOwner, explicit, member, viewer, outsider := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tm, _, err := teamSvc.CreateTeam(teamOwner, "矩阵组", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamSvc.AddMember(teamOwner, tm.ID, member, team.RoleEditor, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := teamSvc.AddMember(teamOwner, tm.ID, viewer, team.RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	sh, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, []uuid.UUID{tm.ID})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		user uuid.UUID
		want bool
	}{
		{"share owner", owner, true},
		{"explicitly granted user", explicit, true},
		{"team member (editor)", member, true},
		{"team member (viewer role still reads share)", viewer, true},
		{"outsider", outsider, false},
	}
	for _, tc := range tests {
		if got := svc.CanAccess(sh, tc.user); got != tc.want {
			t.Errorf("%s: CanAccess = %v, want %v", tc.name, got, tc.want)
		}
	}
	// 零值用户不可访问。
	if svc.CanAccess(sh, uuid.Nil) {
		t.Error("nil user must not access")
	}
}

func TestPrivateShareAccessInvalidatedByRemovalAndRevoke(t *testing.T) {
	svc, _, _, teamSvc, _, owner, fileID, _ := newPrivateTestEnv(t)
	teamOwner, member := uuid.New(), uuid.New()
	tm, _, err := teamSvc.CreateTeam(teamOwner, "移除组", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamSvc.AddMember(teamOwner, tm.ID, member, team.RoleEditor, nil); err != nil {
		t.Fatal(err)
	}
	sh, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, nil, []uuid.UUID{tm.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !svc.CanAccess(sh, member) {
		t.Fatal("team member must have access before removal")
	}
	// 团队移除成员 → 实时失效。
	if err := teamSvc.RemoveMember(teamOwner, tm.ID, member); err != nil {
		t.Fatal(err)
	}
	if svc.CanAccess(sh, member) {
		t.Fatal("removed member must lose access immediately")
	}
	if _, err := svc.ResolveForUser(sh.ID, fileID, member); !errors.Is(err, ErrForbidden) {
		t.Fatalf("resolve after removal: err = %v, want ErrForbidden", err)
	}
	// 重新加入后可访问，随后撤销分享 → ErrGone。
	if _, err := teamSvc.AddMember(teamOwner, tm.ID, member, team.RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForUser(sh.ID, fileID, member); err != nil {
		t.Fatalf("re-added member must resolve: %v", err)
	}
	if _, err := svc.Revoke(owner, sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForUser(sh.ID, fileID, member); !errors.Is(err, ErrGone) {
		t.Fatalf("revoked share: err = %v, want ErrGone", err)
	}
	// 非 owner 不能撤销（兼容现有语义）。
	sh2, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Revoke(member, sh2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke by non-owner: err = %v, want ErrNotFound", err)
	}
}

func TestResolveForUserLifecycle(t *testing.T) {
	svc, _, ff, teamSvc, _, owner, fileID, now := newPrivateTestEnv(t)
	teamOwner, explicit := uuid.New(), uuid.New()
	tm, _, err := teamSvc.CreateTeam(teamOwner, "生命周期组", "")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := svc.CreatePrivate(owner, fileID, PermissionDownload, time.Hour, nil, []uuid.UUID{explicit}, []uuid.UUID{tm.ID})
	if err != nil {
		t.Fatal(err)
	}
	otherFile := ff.addFile(owner, "other.txt", "file", files.BlobStatusAvailable)

	// 分享不存在 / 文件不匹配 → 404 语义（不泄露）。
	if _, err := svc.ResolveForUser(uuid.New(), fileID, explicit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing share: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.ResolveForUser(sh.ID, otherFile, explicit); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file mismatch: err = %v, want ErrNotFound", err)
	}
	// 未授权用户 → 403。
	if _, err := svc.ResolveForUser(sh.ID, fileID, uuid.New()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized: err = %v, want ErrForbidden", err)
	}
	// 授权用户元数据可解析。
	if _, err := svc.ResolveForUser(sh.ID, fileID, explicit); err != nil {
		t.Fatalf("authorized resolve: %v", err)
	}
	// 过期 → 410。
	*now = now.Add(2 * time.Hour)
	if _, err := svc.ResolveForUser(sh.ID, fileID, explicit); !errors.Is(err, ErrGone) {
		t.Fatalf("expired: err = %v, want ErrGone", err)
	}
	*now = now.Add(-2 * time.Hour)

	// 下载：计数递增；view 权限拒绝下载。
	if _, err := svc.ResolveForUserForDownload(sh.ID, fileID, explicit); err != nil {
		t.Fatal(err)
	}
	if got := sh.ID; got == uuid.Nil {
		t.Fatal("unreachable")
	}
	stored, err := svc.repo.Get(sh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DownloadCount != 1 {
		t.Fatalf("download_count = %d, want 1", stored.DownloadCount)
	}
	if ff.downloads[fileID] != 1 {
		t.Fatalf("files.download_count = %d, want 1", ff.downloads[fileID])
	}

	viewShare, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForUserForDownload(viewShare.ID, fileID, explicit); !errors.Is(err, ErrDownloadForbidden) {
		t.Fatalf("view-only download: err = %v, want ErrDownloadForbidden", err)
	}
	if _, err := svc.ResolveForUserForPreview(viewShare.ID, fileID, explicit); err != nil {
		t.Fatalf("view preview: %v", err)
	}

	// 文件软删除后 → ErrGone（分享存在但不泄露细节）。
	deleted := *now
	ff.setDeleted(fileID, &deleted)
	if _, err := svc.ResolveForUser(sh.ID, fileID, explicit); !errors.Is(err, ErrGone) {
		t.Fatalf("deleted file: err = %v, want ErrGone", err)
	}
}

func TestPublicShareNotAccessibleByUserEntry(t *testing.T) {
	svc, _, _, _, _, owner, fileID, _ := newPrivateTestEnv(t)
	// 公开分享：token 解析不变；按用户入口（CanAccess）非 owner 一律拒绝，
	// 且对外呈现「不存在」（ErrNotFound→404，不泄露分享存在性）。
	sh, token, err := svc.Create(owner, fileID, PermissionView, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sh.Visibility != VisibilityPublic {
		t.Fatalf("public share visibility = %q", sh.Visibility)
	}
	other := uuid.New()
	if svc.CanAccess(sh, other) {
		t.Fatal("public share must not be accessible via user entry for strangers")
	}
	if !svc.CanAccess(sh, owner) {
		t.Fatal("owner must always access own share")
	}
	if _, err := svc.ResolveForUser(sh.ID, fileID, other); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public share by user entry: err = %v, want ErrNotFound", err)
	}
	// 私有分享未授权仍是 403 语义（ErrForbidden）。
	private, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForUser(private.ID, fileID, other); !errors.Is(err, ErrForbidden) {
		t.Fatalf("private share by unauthorized user: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.Resolve(token); err != nil {
		t.Fatalf("public token resolution must stay unchanged: %v", err)
	}
}

// fakeDirectory 是 UserDirectory 的内存实现（不依赖 PostgreSQL）。
type fakeDirectory struct{ names map[uuid.UUID]string }

func (d *fakeDirectory) Username(id uuid.UUID) (string, error) {
	if n, ok := d.names[id]; ok {
		return n, nil
	}
	return "", errors.New("user not found")
}

// newSharedWithMeEnv 在内存环境上接入成员判定与用户目录。
func newSharedWithMeEnv(t *testing.T) (*Service, *MemoryStore, *fakeFiles, *team.Service, uuid.UUID, uuid.UUID, *time.Time) {
	t.Helper()
	svc, repo, ff, teamSvc, _, owner, fileID, now := newPrivateTestEnv(t)
	repo.SetMembership(func(user, teamID uuid.UUID) bool {
		ok, _ := teamSvc.UserInAnyTeam(user, []uuid.UUID{teamID})
		return ok
	})
	svc.SetUserDirectory(&fakeDirectory{names: map[uuid.UUID]string{owner: "alice"}})
	return svc, repo, ff, teamSvc, owner, fileID, now
}

func TestSharedWithMeExplicitGrant(t *testing.T) {
	svc, _, ff, _, owner, fileID, _ := newSharedWithMeEnv(t)
	explicit, stranger := uuid.New(), uuid.New()
	sh, err := svc.CreatePrivate(owner, fileID, PermissionDownload, time.Hour, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := svc.SharedWithMe(explicit, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("shared-with-me items = %d, want 1", len(items))
	}
	got := items[0]
	if got.Share.ID != sh.ID {
		t.Fatalf("share id = %v, want %v", got.Share.ID, sh.ID)
	}
	if got.FileName != "secret.txt" {
		t.Fatalf("file name = %q, want secret.txt", got.FileName)
	}
	if got.FileSize != 42 || got.FileMime != "text/plain" {
		t.Fatalf("file meta = (%d, %q), want (42, text/plain)", got.FileSize, got.FileMime)
	}
	if got.OwnerName != "alice" {
		t.Fatalf("owner name = %q, want alice", got.OwnerName)
	}
	if got.Share.Permission != PermissionDownload {
		t.Fatalf("permission = %q, want download", got.Share.Permission)
	}
	// 陌生人、分享 owner 自身（自己创建的不算「分享给我」）均看不到。
	if items, _ := svc.SharedWithMe(stranger, 100); len(items) != 0 {
		t.Fatalf("stranger sees %d items, want 0", len(items))
	}
	if items, _ := svc.SharedWithMe(owner, 100); len(items) != 0 {
		t.Fatalf("owner must not see own shares in shared-with-me, got %d", len(items))
	}
	// 未注入目录的副本：owner_username 退化为空串。
	plain := NewService(NewMemoryStore(), ff)
	if _, err := plain.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil); err != nil {
		t.Fatal(err)
	}
	plainItems, err := plain.SharedWithMe(explicit, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(plainItems) != 1 || plainItems[0].OwnerName != "" {
		t.Fatalf("without directory: items = %+v, want empty OwnerName", plainItems)
	}
}

func TestSharedWithMeTeamGrantAndRemoval(t *testing.T) {
	svc, _, _, teamSvc, owner, fileID, _ := newSharedWithMeEnv(t)
	teamOwner, member := uuid.New(), uuid.New()
	tm, _, err := teamSvc.CreateTeam(teamOwner, "与我共享组", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := teamSvc.AddMember(teamOwner, tm.ID, member, team.RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	sh, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, nil, []uuid.UUID{tm.ID})
	if err != nil {
		t.Fatal(err)
	}
	items, err := svc.SharedWithMe(member, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Share.ID != sh.ID {
		t.Fatalf("team member items = %+v, want single share %v", items, sh.ID)
	}
	// 团队移除成员 → 实时失效。
	if err := teamSvc.RemoveMember(teamOwner, tm.ID, member); err != nil {
		t.Fatal(err)
	}
	if items, _ := svc.SharedWithMe(member, 100); len(items) != 0 {
		t.Fatalf("removed member sees %d items, want 0", len(items))
	}
}

func TestSharedWithMeExclusions(t *testing.T) {
	svc, repo, ff, _, owner, fileID, now := newSharedWithMeEnv(t)
	explicit := uuid.New()
	// 公开分享不算（即使 owner 是同一人）。
	if _, _, err := svc.Create(owner, fileID, PermissionView, 0, nil); err != nil {
		t.Fatal(err)
	}
	// 撤销的私有分享不算。
	revoked, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Revoke(owner, revoked.ID); err != nil {
		t.Fatal(err)
	}
	// 过期的私有分享不算。
	*now = now.Add(-2 * time.Hour)
	expiredAt := now.Add(time.Hour)
	expired, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo.Put(func() Share {
		s, _ := repo.Get(expired.ID)
		s.ExpiresAt = &expiredAt
		return s
	}())
	*now = now.Add(2 * time.Hour)
	// 达到下载上限的私有分享不算。
	maxOne := 1
	limited, err := svc.CreatePrivate(owner, fileID, PermissionDownload, 0, &maxOne, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveForUserForDownload(limited.ID, fileID, explicit); err != nil {
		t.Fatal(err)
	}
	if items, err := svc.SharedWithMe(explicit, 100); err != nil {
		t.Fatal(err)
	} else if len(items) != 0 {
		t.Fatalf("explicit user sees %d items, want 0 (public/revoked/expired/exhausted excluded)", len(items))
	}
	// 文件软删除后对应分享不再列出。
	active, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := svc.SharedWithMe(explicit, 100); len(items) != 1 || items[0].Share.ID != active.ID {
		t.Fatalf("active share missing, items = %+v", items)
	}
	deleted := *now
	ff.setDeleted(fileID, &deleted)
	if items, _ := svc.SharedWithMe(explicit, 100); len(items) != 0 {
		t.Fatalf("deleted file shares must not be listed, got %d", len(items))
	}
}

func TestSharedWithMeOrderByCreatedAtDesc(t *testing.T) {
	svc, _, _, _, owner, fileID, now := newSharedWithMeEnv(t)
	explicit := uuid.New()
	first, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	second, err := svc.CreatePrivate(owner, fileID, PermissionDownload, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	third, err := svc.CreatePrivate(owner, fileID, PermissionView, 0, nil, []uuid.UUID{explicit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := svc.SharedWithMe(explicit, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	wantOrder := []uuid.UUID{third.ID, second.ID, first.ID}
	for i, want := range wantOrder {
		if items[i].Share.ID != want {
			t.Fatalf("order[%d] = %v, want %v", i, items[i].Share.ID, want)
		}
	}
	// limit 生效。
	if limited, _ := svc.SharedWithMe(explicit, 2); len(limited) != 2 || limited[0].Share.ID != third.ID {
		t.Fatalf("limit=2 items = %+v", limited)
	}
}

func TestListWithFileNames(t *testing.T) {
	svc, _, ff, _, owner, fileID, now := newSharedWithMeEnv(t)
	if _, _, err := svc.Create(owner, fileID, PermissionView, 0, nil); err != nil {
		t.Fatal(err)
	}
	gone := ff.addFile(owner, "gone.txt", "file", files.BlobStatusAvailable)
	if _, err := svc.CreatePrivate(owner, gone, PermissionView, 0, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	items, err := svc.ListWithFileNames(owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	byFile := map[uuid.UUID]string{}
	for _, it := range items {
		byFile[it.Share.FileID] = it.FileName
	}
	if byFile[fileID] != "secret.txt" {
		t.Fatalf("file_name for live file = %q, want secret.txt", byFile[fileID])
	}
	if byFile[gone] != "gone.txt" {
		t.Fatalf("file_name for gone file = %q, want gone.txt", byFile[gone])
	}
	// 文件软删除后：条目保留但 file_name 为空串。
	deleted := *now
	ff.setDeleted(gone, &deleted)
	items, err = svc.ListWithFileNames(owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Share.FileID == gone && it.FileName != "" {
			t.Fatalf("deleted file name = %q, want empty", it.FileName)
		}
	}
}
