package team

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestService() (*Service, *MemoryStore) {
	repo := NewMemoryStore()
	svc := NewService(repo)
	svc.now = func() time.Time { return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) }
	return svc, repo
}

func TestCreateTeamCreatesOwnerMemberAndRoot(t *testing.T) {
	svc, repo := newTestService()
	owner := uuid.New()
	tm, root, err := svc.CreateTeam(owner, "  设计团队  ", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if tm.Name != "设计团队" {
		t.Fatalf("name = %q, want trimmed 设计团队", tm.Name)
	}
	if tm.OwnerID != owner {
		t.Fatalf("owner = %v, want %v", tm.OwnerID, owner)
	}
	// 创建者自动成为 owner 成员。
	role, err := svc.Role(tm.ID, owner)
	if err != nil || role != RoleOwner {
		t.Fatalf("owner role = %q, %v; want owner", role, err)
	}
	// 团队根目录在事务内创建：scope_type=team、team_id、is_root、owner_id=创建者。
	stored, ok := repo.Root(tm.ID)
	if !ok {
		t.Fatal("team root folder must be created with the team")
	}
	if !stored.IsRoot || stored.ScopeType != "team" || stored.Type != "folder" {
		t.Fatalf("root folder = %+v", stored)
	}
	if stored.TeamID == nil || *stored.TeamID != tm.ID {
		t.Fatalf("root team_id = %v, want %v", stored.TeamID, tm.ID)
	}
	if stored.OwnerID != owner {
		t.Fatalf("root owner = %v, want %v", stored.OwnerID, owner)
	}
	if root.ID != stored.ID {
		t.Fatalf("returned root id %v != stored %v", root.ID, stored.ID)
	}
}

func TestCreateTeamValidationAndConflict(t *testing.T) {
	svc, _ := newTestService()
	owner := uuid.New()
	if _, _, err := svc.CreateTeam(owner, "ok", ""); err != nil {
		t.Fatalf("valid create: %v", err)
	}
	// 重名冲突。
	if _, _, err := svc.CreateTeam(uuid.New(), "ok", ""); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("duplicate name: err = %v, want ErrNameConflict", err)
	}
	// 非法名称：空、超长、含控制字符。
	if _, _, err := svc.CreateTeam(owner, "   ", ""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("blank name: err = %v, want ErrInvalidName", err)
	}
	if _, _, err := svc.CreateTeam(owner, strings.Repeat("a", 101), ""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("long name: err = %v, want ErrInvalidName", err)
	}
	if _, _, err := svc.CreateTeam(owner, "bad\nname", ""); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("control char name: err = %v, want ErrInvalidName", err)
	}
}

func TestListTeamsMembershipScoped(t *testing.T) {
	svc, _ := newTestService()
	owner, member, outsider := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	// 仅 owner 时：owner 可见，其他用户不可见。
	if out, _ := svc.ListTeams(owner); len(out) != 1 {
		t.Fatalf("owner sees %d teams, want 1", len(out))
	}
	if out, _ := svc.ListTeams(outsider); len(out) != 0 {
		t.Fatalf("outsider sees %d teams, want 0", len(out))
	}
	// 加入成员后成员可见。
	if _, err := svc.AddMember(owner, tm.ID, member, RoleMember); err != nil {
		t.Fatal(err)
	}
	if out, _ := svc.ListTeams(member); len(out) != 1 || out[0].ID != tm.ID {
		t.Fatalf("member sees %+v", out)
	}
	// ListTeamsDetailed 附带 my_role/member_count（v1.7 团队页卡片数据）。
	infos, err := svc.ListTeamsDetailed(member)
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListTeamsDetailed = %+v, %v; want 1 team", infos, err)
	}
	if infos[0].MyRole != RoleMember || infos[0].MemberCount != 2 {
		t.Fatalf("info = %+v, want my_role=member member_count=2", infos[0])
	}
}

func TestAddMemberPermissionAndRoleValidation(t *testing.T) {
	svc, _ := newTestService()
	owner, admin, guest := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, admin, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, guest, RoleGuest); err != nil {
		t.Fatal(err)
	}
	// owner 角色不可经 AddMember 授予（仅经转让产生）；未知角色拒绝。
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), RoleOwner); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("add owner role: err = %v, want ErrInvalidRole", err)
	}
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), "editor"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("legacy role: err = %v, want ErrInvalidRole", err)
	}
	// admin 角色仅 owner 可授予。
	if _, err := svc.AddMember(admin, tm.ID, uuid.New(), RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("grant admin by admin: err = %v, want ErrForbidden", err)
	}
	// admin 可添加普通成员。
	if _, err := svc.AddMember(admin, tm.ID, uuid.New(), RoleMemberShare); err != nil {
		t.Fatalf("add member by admin: %v", err)
	}
	// 普通成员不可管理。
	if _, err := svc.AddMember(guest, tm.ID, uuid.New(), RoleGuest); !errors.Is(err, ErrForbidden) {
		t.Fatalf("add by guest: err = %v, want ErrForbidden", err)
	}
	// 重复添加冲突。
	if _, err := svc.AddMember(owner, tm.ID, guest, RoleGuest); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("duplicate member: err = %v, want ErrMemberExists", err)
	}
	// 团队不存在。
	if _, err := svc.AddMember(owner, uuid.New(), uuid.New(), RoleGuest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing team: err = %v, want ErrNotFound", err)
	}
	// 零值用户拒绝。
	if _, err := svc.AddMember(owner, tm.ID, uuid.Nil, RoleGuest); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("nil user: err = %v, want ErrInvalidRole", err)
	}
}

func TestRemoveMemberAndAccessInvalidation(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-c", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleMember); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); !ok {
		t.Fatal("member must be able to write before removal")
	}
	// owner 成员不可移除。
	if err := svc.RemoveMember(owner, tm.ID, owner); !errors.Is(err, ErrOwnerMember) {
		t.Fatalf("remove owner: err = %v, want ErrOwnerMember", err)
	}
	// 普通成员不可管理。
	if err := svc.RemoveMember(member, tm.ID, member); !errors.Is(err, ErrForbidden) {
		t.Fatalf("remove by non-owner: err = %v, want ErrForbidden", err)
	}
	// 移除后成员实时失效。
	if err := svc.RemoveMember(owner, tm.ID, member); err != nil {
		t.Fatal(err)
	}
	if role, _ := svc.Role(tm.ID, member); role != "" {
		t.Fatalf("removed member role = %q, want empty", role)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); ok {
		t.Fatal("removed member must not be able to write")
	}
	if in, _ := svc.UserInAnyTeam(member, []uuid.UUID{tm.ID}); in {
		t.Fatal("removed member must not be in team")
	}
}

// TestRemoveMemberAdminBoundary：admin 不可移除其他 admin（owner 边界）。
func TestRemoveMemberAdminBoundary(t *testing.T) {
	svc, _ := newTestService()
	owner, admin, admin2, member := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-admin-boundary", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		u uuid.UUID
		r string
	}{{admin, RoleAdmin}, {admin2, RoleAdmin}, {member, RoleMember}} {
		if _, err := svc.AddMember(owner, tm.ID, m.u, m.r); err != nil {
			t.Fatal(err)
		}
	}
	// admin 不可移除其他 admin。
	if err := svc.RemoveMember(admin, tm.ID, admin2); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin removes admin: err = %v, want ErrForbidden", err)
	}
	// admin 可移除普通成员。
	if err := svc.RemoveMember(admin, tm.ID, member); err != nil {
		t.Fatalf("admin removes member: %v", err)
	}
	// owner 可移除 admin。
	if err := svc.RemoveMember(owner, tm.ID, admin2); err != nil {
		t.Fatalf("owner removes admin: %v", err)
	}
}

func TestListMembersRequiresMembership(t *testing.T) {
	svc, _ := newTestService()
	owner, member, outsider := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-d", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleGuest); err != nil {
		t.Fatal(err)
	}
	members, err := svc.ListMembers(member, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2 (owner + guest)", len(members))
	}
	if _, err := svc.ListMembers(outsider, tm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider list members: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.ListMembers(owner, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing team: err = %v, want ErrNotFound", err)
	}
}

// TestPermissionMatrix 覆盖五级内置角色的固定权限矩阵（migration 037）：
// owner/admin 全权限；member_share 读写删+分享；member 读写删；guest 只读；
// 非成员（含已移除）无任何权限（fail closed）。
func TestPermissionMatrix(t *testing.T) {
	svc, _ := newTestService()
	owner, admin, shareM, member, guest, outsider := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-perm", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		user uuid.UUID
		role string
	}{
		{admin, RoleAdmin}, {shareM, RoleMemberShare}, {member, RoleMember}, {guest, RoleGuest},
	} {
		if _, err := svc.AddMember(owner, tm.ID, m.user, m.role); err != nil {
			t.Fatal(err)
		}
	}
	// 解散/转让仅 owner：Delete/TransferOwnership 的边界另行覆盖（owner 专属）。
	cases := []struct {
		name                       string
		user                       uuid.UUID
		read, write, delete, share bool
	}{
		{"owner", owner, true, true, true, true},
		{"admin", admin, true, true, true, true},
		{"member_share", shareM, true, true, true, true},
		{"member", member, true, true, true, false},
		{"guest", guest, true, false, false, false},
		{"outsider", outsider, false, false, false, false},
	}
	for _, tc := range cases {
		for _, perm := range []struct {
			action string
			want   bool
			can    func(uuid.UUID, uuid.UUID) (bool, error)
		}{
			{PermRead, tc.read, svc.CanRead},
			{PermWrite, tc.write, svc.CanWrite},
			{PermDelete, tc.delete, svc.CanDelete},
			{PermShare, tc.share, svc.CanShare},
		} {
			got, err := perm.can(tc.user, tm.ID)
			if err != nil {
				t.Fatalf("%s Can%s: %v", tc.name, perm.action, err)
			}
			if got != perm.want {
				t.Errorf("%s Can%s = %v, want %v", tc.name, perm.action, got, perm.want)
			}
		}
		// 管理权限（管成员/目录权限）：仅 owner/admin。
		gotAdmin, _ := svc.CanAdmin(tc.user, tm.ID)
		wantAdmin := tc.name == "owner" || tc.name == "admin"
		if gotAdmin != wantAdmin {
			t.Errorf("%s CanAdmin = %v, want %v", tc.name, gotAdmin, wantAdmin)
		}
	}
}

// TestUpdateMemberRole 覆盖五级角色改派：owner 成员不可改、admin 边界
//（不可授 admin / 不可改其他 admin）、普通角色互转、成员不存在 404、
// 非法角色拒绝。
func TestUpdateMemberRole(t *testing.T) {
	svc, _ := newTestService()
	owner, admin, member := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-upd", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, admin, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleGuest); err != nil {
		t.Fatal(err)
	}
	// guest → member：写权限随之生效。
	m, err := svc.UpdateMemberRole(owner, tm.ID, member, RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != RoleMember {
		t.Fatalf("updated member = %+v, want member", m)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); !ok {
		t.Fatal("member must be able to write after role change")
	}
	// member → guest：写权限随之失效。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, member, RoleGuest); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); ok {
		t.Fatal("guest must not be able to write")
	}
	// owner 成员角色不可修改。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, owner, RoleGuest); !errors.Is(err, ErrOwnerMember) {
		t.Fatalf("update owner member: err = %v, want ErrOwnerMember", err)
	}
	// owner 角色不可经改派授予。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, member, RoleOwner); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("assign owner via update: err = %v, want ErrInvalidRole", err)
	}
	// admin 不可授予 admin，也不可修改 owner 成员（owner 成员判定优先，
	// 恒 ErrOwnerMember——owner 只能经转让产生/变更）。
	if _, err := svc.UpdateMemberRole(admin, tm.ID, member, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin grants admin: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.UpdateMemberRole(admin, tm.ID, owner, RoleGuest); !errors.Is(err, ErrOwnerMember) {
		t.Fatalf("admin updates owner: err = %v, want ErrOwnerMember", err)
	}
	// 普通成员不可管理。
	if _, err := svc.UpdateMemberRole(member, tm.ID, member, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update by member: err = %v, want ErrForbidden", err)
	}
	// 成员不存在。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, uuid.New(), RoleGuest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member: err = %v, want ErrNotFound", err)
	}
	// 非法角色。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, member, "editor"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("legacy role: err = %v, want ErrInvalidRole", err)
	}
}

// TestTransferOwnership 覆盖所有权转让：仅 owner 可为、受让人须为成员、
// 新 owner 角色置 owner、原 owner 降为 admin。
func TestTransferOwnership(t *testing.T) {
	svc, _ := newTestService()
	owner, admin, outsider := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-transfer", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, admin, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	// 非 owner 不可转让。
	if err := svc.TransferOwnership(admin, tm.ID, admin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("transfer by admin: err = %v, want ErrForbidden", err)
	}
	// 受让人不是成员。
	if err := svc.TransferOwnership(owner, tm.ID, outsider); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transfer to outsider: err = %v, want ErrNotFound", err)
	}
	// 受让人为空/自身：拒绝。
	if err := svc.TransferOwnership(owner, tm.ID, uuid.Nil); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("transfer to nil: err = %v, want ErrInvalidRole", err)
	}
	if err := svc.TransferOwnership(owner, tm.ID, owner); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("transfer to self: err = %v, want ErrInvalidRole", err)
	}
	// 正常转让。
	if err := svc.TransferOwnership(owner, tm.ID, admin); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Get(tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerID != admin {
		t.Fatalf("owner = %v, want %v", after.OwnerID, admin)
	}
	if role, _ := svc.Role(tm.ID, admin); role != RoleOwner {
		t.Fatalf("new owner role = %q, want owner", role)
	}
	if role, _ := svc.Role(tm.ID, owner); role != RoleAdmin {
		t.Fatalf("old owner role = %q, want admin", role)
	}
}

// TestLeave 覆盖成员主动退出：owner 不可离开（须先转让/解散），普通成员可。
func TestLeave(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-leave", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleMemberShare); err != nil {
		t.Fatal(err)
	}
	if err := svc.Leave(owner, tm.ID); !errors.Is(err, ErrOwnerMember) {
		t.Fatalf("owner leaves: err = %v, want ErrOwnerMember", err)
	}
	if err := svc.Leave(member, tm.ID); err != nil {
		t.Fatal(err)
	}
	if role, _ := svc.Role(tm.ID, member); role != "" {
		t.Fatalf("left member role = %q, want empty", role)
	}
}

// TestDeleteAndUpdateOwnerOnly：解散与改名仅 owner；admin 403。
func TestDeleteAndUpdateOwnerOnly(t *testing.T) {
	svc, _ := newTestService()
	owner, admin := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-owner-only", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, admin, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(admin, tm.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("dissolve by admin: err = %v, want ErrForbidden", err)
	}
	if err := svc.Update(admin, tm.ID, "新名字", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update by admin: err = %v, want ErrForbidden", err)
	}
	if err := svc.Update(owner, tm.ID, "新名字", "描述"); err != nil {
		t.Fatalf("update by owner: %v", err)
	}
	if err := svc.Delete(owner, tm.ID); err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
	// 解散后不可见。
	if out, _ := svc.ListTeams(owner); len(out) != 0 {
		t.Fatalf("dissolved team still listed: %+v", out)
	}
}

func TestUserInAnyTeam(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	t1, _, err := svc.CreateTeam(owner, "team-f", "")
	if err != nil {
		t.Fatal(err)
	}
	t2, _, err := svc.CreateTeam(owner, "team-g", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, t1.ID, member, RoleGuest); err != nil {
		t.Fatal(err)
	}
	if in, _ := svc.UserInAnyTeam(member, []uuid.UUID{t2.ID}); in {
		t.Fatal("member of t1 must not match [t2]")
	}
	if in, _ := svc.UserInAnyTeam(member, []uuid.UUID{t2.ID, t1.ID}); !in {
		t.Fatal("member of t1 must match [t2, t1]")
	}
	if in, _ := svc.UserInAnyTeam(member, nil); in {
		t.Fatal("empty team list must not match")
	}
}
