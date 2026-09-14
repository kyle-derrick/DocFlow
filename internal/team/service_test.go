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
	if _, err := svc.AddMember(owner, tm.ID, member, RoleEditor, nil); err != nil {
		t.Fatal(err)
	}
	if out, _ := svc.ListTeams(member); len(out) != 1 || out[0].ID != tm.ID {
		t.Fatalf("member sees %+v", out)
	}
}

func TestAddMemberPermissionAndRoleValidation(t *testing.T) {
	svc, _ := newTestService()
	owner, editor, viewer := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, editor, RoleEditor, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, viewer, RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	// role 校验：owner 角色不可通过 AddMember 授予，未知角色拒绝。
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), RoleOwner, nil); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("add owner role: err = %v, want ErrInvalidRole", err)
	}
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), "admin", nil); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("unknown role: err = %v, want ErrInvalidRole", err)
	}
	// 仅 owner 可管理成员。
	if _, err := svc.AddMember(editor, tm.ID, uuid.New(), RoleViewer, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("add by non-owner: err = %v, want ErrForbidden", err)
	}
	// 重复添加冲突。
	if _, err := svc.AddMember(owner, tm.ID, editor, RoleViewer, nil); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("duplicate member: err = %v, want ErrMemberExists", err)
	}
	// 团队不存在。
	if _, err := svc.AddMember(owner, uuid.New(), uuid.New(), RoleViewer, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing team: err = %v, want ErrNotFound", err)
	}
}

func TestAddMemberWithCustomRole(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-custom", "")
	if err != nil {
		t.Fatal(err)
	}
	role, err := svc.CreateRole(owner, tm.ID, "审计员", map[string]any{PermRead: true})
	if err != nil {
		t.Fatal(err)
	}
	// role_id 指定自定义角色：role 归一为 custom。
	m, err := svc.AddMember(owner, tm.ID, member, "", &role.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != RoleCustom || m.RoleID == nil || *m.RoleID != role.ID {
		t.Fatalf("member = %+v, want custom role %v", m, role.ID)
	}
	// role 与 role_id 同时给出且冲突时拒绝。
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), RoleViewer, &role.ID); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("mixed role and role_id: err = %v, want ErrInvalidRole", err)
	}
	// role_id 不属于该团队 / 不存在：拒绝。
	other, _, err := svc.CreateTeam(owner, "team-other", "")
	if err != nil {
		t.Fatal(err)
	}
	foreignRole, err := svc.CreateRole(owner, other.ID, "外团队角色", map[string]any{PermRead: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), "", &foreignRole.ID); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("foreign role_id: err = %v, want ErrInvalidRole", err)
	}
	// role_id 不存在：拒绝。
	missing := uuid.New()
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), "", &missing); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("missing role_id: err = %v, want ErrInvalidRole", err)
	}
	// 零值 UUID：拒绝。
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), "", &uuid.Nil); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("nil role_id: err = %v, want ErrInvalidRole", err)
	}
}

func TestRemoveMemberAndAccessInvalidation(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-c", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleEditor, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); !ok {
		t.Fatal("editor must be able to write before removal")
	}
	// owner 成员不可移除。
	if err := svc.RemoveMember(owner, tm.ID, owner); !errors.Is(err, ErrOwnerMember) {
		t.Fatalf("remove owner: err = %v, want ErrOwnerMember", err)
	}
	// 非 owner 不可管理。
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

func TestListMembersRequiresMembership(t *testing.T) {
	svc, _ := newTestService()
	owner, member, outsider := uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-d", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	members, err := svc.ListMembers(member, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2 (owner + viewer)", len(members))
	}
	if _, err := svc.ListMembers(outsider, tm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider list members: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.ListMembers(owner, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing team: err = %v, want ErrNotFound", err)
	}
}

func TestCanWriteRoleMatrix(t *testing.T) {
	svc, _ := newTestService()
	owner, editor, viewer, outsider := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-e", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user uuid.UUID
		want bool
	}{
		{owner, true},
		{editor, true},
		{viewer, false},
		{outsider, false},
	} {
		if tc.user == editor || tc.user == viewer {
			role := RoleEditor
			if tc.user == viewer {
				role = RoleViewer
			}
			if _, err := svc.AddMember(owner, tm.ID, tc.user, role, nil); err != nil {
				t.Fatal(err)
			}
		}
		got, err := svc.CanWrite(tc.user, tm.ID)
		if err != nil || got != tc.want {
			t.Errorf("CanWrite(%v) = %v, %v; want %v", tc.user, got, err, tc.want)
		}
	}
}

// TestPermissionMatrix 覆盖设计 6.5 权限模型：系统角色映射、自定义角色
// permissions 勾选、显式 deny 优先、角色行缺失 fail closed、非成员无权限。
func TestPermissionMatrix(t *testing.T) {
	svc, repo := newTestService()
	owner, editor, viewer, custom, denied, ghost, outsider := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-perm", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		user uuid.UUID
		role string
	}{
		{editor, RoleEditor}, {viewer, RoleViewer},
	} {
		if _, err := svc.AddMember(owner, tm.ID, m.user, m.role, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 自定义角色：读/写/删/分享全勾。
	full, err := svc.CreateRole(owner, tm.ID, "全能", map[string]any{PermRead: true, PermWrite: true, PermDelete: true, PermShare: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, custom, "", &full.ID); err != nil {
		t.Fatal(err)
	}
	// 自定义角色：允许写但显式 deny write（deny 优先于 allow）。
	deniedRole, err := svc.CreateRole(owner, tm.ID, "受限", map[string]any{PermRead: true, PermWrite: true, "deny": []any{PermWrite}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, denied, "", &deniedRole.ID); err != nil {
		t.Fatal(err)
	}
	// ghost：绑定后被直接删除的角色行（fail closed 场景）。
	ghostRole, err := svc.CreateRole(owner, tm.ID, "幽灵", map[string]any{PermRead: true, PermAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, ghost, "", &ghostRole.ID); err != nil {
		t.Fatal(err)
	}
	repo.DeleteRoleDirect(ghostRole.ID)

	cases := []struct {
		name                       string
		user                       uuid.UUID
		read, write, delete, share bool
	}{
		// 系统角色：owner 全 true；editor 读/写/分享；viewer 仅读。
		{"owner", owner, true, true, true, true},
		{"editor", editor, true, true, false, true},
		{"viewer", viewer, true, false, false, false},
		// 自定义角色按 permissions JSON。
		{"custom full", custom, true, true, true, true},
		// deny 优先于 allow。
		{"custom deny write", denied, true, false, false, false},
		// 角色行缺失：fail closed（无任何权限）。
		{"missing role row", ghost, false, false, false, false},
		// 非成员：无任何权限。
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
	}
}

// TestUpdateMemberRole 覆盖成员改角色：系统↔自定义互转、owner 成员不可改、
// 非 owner 拒绝、成员不存在 404、非法 role_id 拒绝。
func TestUpdateMemberRole(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-upd", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	role, err := svc.CreateRole(owner, tm.ID, "贡献者", map[string]any{PermRead: true, PermWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	// 系统角色 → 自定义角色。
	m, err := svc.UpdateMemberRole(owner, tm.ID, member, "", &role.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != RoleCustom || m.RoleID == nil || *m.RoleID != role.ID || m.RoleName != "贡献者" {
		t.Fatalf("updated member = %+v", m)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); !ok {
		t.Fatal("member with write-enabled custom role must be able to write")
	}
	// 自定义角色 → 系统角色（role_id 置空）。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, member, RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.CanWrite(member, tm.ID); ok {
		t.Fatal("viewer must not be able to write")
	}
	// owner 成员角色不可修改。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, owner, RoleViewer, nil); !errors.Is(err, ErrOwnerMember) {
		t.Fatalf("update owner member: err = %v, want ErrOwnerMember", err)
	}
	// 非 owner 不可管理。
	if _, err := svc.UpdateMemberRole(member, tm.ID, member, RoleEditor, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("update by non-owner: err = %v, want ErrForbidden", err)
	}
	// 成员不存在。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, uuid.New(), RoleViewer, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member: err = %v, want ErrNotFound", err)
	}
	// 非法 role_id（不属于本团队）。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, member, "", &uuid.Nil); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("nil role_id: err = %v, want ErrInvalidRole", err)
	}
}

// TestDeleteRoleReferenceCheck 覆盖删除角色的成员引用检查与 member_count 统计。
func TestDeleteRoleReferenceCheck(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-role-del", "")
	if err != nil {
		t.Fatal(err)
	}
	used, err := svc.CreateRole(owner, tm.ID, "在用", map[string]any{PermRead: true})
	if err != nil {
		t.Fatal(err)
	}
	free, err := svc.CreateRole(owner, tm.ID, "闲置", map[string]any{PermRead: true, PermWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, "", &used.ID); err != nil {
		t.Fatal(err)
	}
	// 有成员引用：409（ErrRoleInUse）。
	if err := svc.DeleteRole(owner, tm.ID, used.ID); !errors.Is(err, ErrRoleInUse) {
		t.Fatalf("delete in-use role: err = %v, want ErrRoleInUse", err)
	}
	// 列表含 member_count。
	roles, err := svc.Roles(owner, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[uuid.UUID]int64{}
	for _, r := range roles {
		counts[r.ID] = r.MemberCount
	}
	if counts[used.ID] != 1 {
		t.Fatalf("used role member_count = %d, want 1", counts[used.ID])
	}
	if counts[free.ID] != 0 {
		t.Fatalf("free role member_count = %d, want 0", counts[free.ID])
	}
	// 改派成员后可删除。
	if _, err := svc.UpdateMemberRole(owner, tm.ID, member, RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteRole(owner, tm.ID, used.ID); err != nil {
		t.Fatal(err)
	}
	// 未引用角色直接删除成功。
	if err := svc.DeleteRole(owner, tm.ID, free.ID); err != nil {
		t.Fatal(err)
	}
	// 删除不存在的角色 404。
	if err := svc.DeleteRole(owner, tm.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing role: err = %v, want ErrNotFound", err)
	}
}

// TestValidatePermissions 覆盖 permissions JSON 结构校验（非法键/类型/deny 值）。
func TestValidatePermissions(t *testing.T) {
	valid := []map[string]any{
		nil,
		{},
		{PermRead: true, PermWrite: false},
		{PermRead: true, "deny": []any{PermWrite, PermShare}},
	}
	for _, p := range valid {
		if err := ValidatePermissions(p); err != nil {
			t.Errorf("ValidatePermissions(%v) = %v, want nil", p, err)
		}
	}
	invalid := []map[string]any{
		{"execute": true},          // 未知动作
		{PermRead: "yes"},          // 非布尔
		{"deny": "write"},          // deny 非数组
		{"deny": []any{"execute"}}, // deny 含非法动作
		{"deny": []any{42}},        // deny 含非字符串
	}
	for _, p := range invalid {
		if err := ValidatePermissions(p); !errors.Is(err, ErrInvalidPermission) {
			t.Errorf("ValidatePermissions(%v) = %v, want ErrInvalidPermission", p, err)
		}
	}
	// 服务层创建/更新角色同样拒绝非法 permissions。
	svc, _ := newTestService()
	owner := uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-perm-valid", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateRole(owner, tm.ID, "bad", map[string]any{"execute": true}); !errors.Is(err, ErrInvalidPermission) {
		t.Fatalf("create role with invalid permissions: err = %v, want ErrInvalidPermission", err)
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
	if _, err := svc.AddMember(owner, t1.ID, member, RoleViewer, nil); err != nil {
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
