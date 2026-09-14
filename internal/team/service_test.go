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
	if _, err := svc.AddMember(owner, tm.ID, member, RoleEditor); err != nil {
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
	if _, err := svc.AddMember(owner, tm.ID, editor, RoleEditor); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, viewer, RoleViewer); err != nil {
		t.Fatal(err)
	}
	// role 校验：owner 角色不可通过 AddMember 授予，未知角色拒绝。
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), RoleOwner); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("add owner role: err = %v, want ErrInvalidRole", err)
	}
	if _, err := svc.AddMember(owner, tm.ID, uuid.New(), "admin"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("unknown role: err = %v, want ErrInvalidRole", err)
	}
	// 仅 owner 可管理成员。
	if _, err := svc.AddMember(editor, tm.ID, uuid.New(), RoleViewer); !errors.Is(err, ErrForbidden) {
		t.Fatalf("add by non-owner: err = %v, want ErrForbidden", err)
	}
	// 重复添加冲突。
	if _, err := svc.AddMember(owner, tm.ID, editor, RoleViewer); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("duplicate member: err = %v, want ErrMemberExists", err)
	}
	// 团队不存在。
	if _, err := svc.AddMember(owner, uuid.New(), uuid.New(), RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing team: err = %v, want ErrNotFound", err)
	}
}

func TestRemoveMemberAndAccessInvalidation(t *testing.T) {
	svc, _ := newTestService()
	owner, member := uuid.New(), uuid.New()
	tm, _, err := svc.CreateTeam(owner, "team-c", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(owner, tm.ID, member, RoleEditor); err != nil {
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
	if _, err := svc.AddMember(owner, tm.ID, member, RoleViewer); err != nil {
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
			if _, err := svc.AddMember(owner, tm.ID, tc.user, role); err != nil {
				t.Fatal(err)
			}
		}
		got, err := svc.CanWrite(tc.user, tm.ID)
		if err != nil || got != tc.want {
			t.Errorf("CanWrite(%v) = %v, %v; want %v", tc.user, got, err, tc.want)
		}
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
	if _, err := svc.AddMember(owner, t1.ID, member, RoleViewer); err != nil {
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
