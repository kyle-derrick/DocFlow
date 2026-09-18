package group

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func newTestService() *Service { return NewService(NewMemoryStore()) }

// 组 CRUD：非法名 400；重名 409（ErrNameConflict）；改名/描述按指针语义生效。
func TestGroupCRUD(t *testing.T) {
	s := newTestService()
	owner := uuid.New()

	for _, bad := range []string{"", "  ", string([]byte{0x01})} {
		if _, err := s.Create(bad, "", owner); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name %q: err = %v, want ErrInvalidName", bad, err)
		}
	}
	if _, err := s.Create(string(make([]rune, 101)), "", owner); !errors.Is(err, ErrInvalidName) {
		t.Fatal("oversized name must be rejected")
	}

	g, err := s.Create("研发组", "描述", owner)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "研发组" || g.Description != "描述" || g.CreatedBy != owner {
		t.Fatalf("created = %+v", g)
	}
	if _, err := s.Create("研发组", "", owner); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("duplicate name: err = %v, want ErrNameConflict", err)
	}

	// 改名 + 描述（指针语义：nil 不更新）。
	updated, err := s.Update(g.ID, strp("平台组"), strp("新描述"))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "平台组" || updated.Description != "新描述" {
		t.Fatalf("updated = %+v", updated)
	}
	// 仅改描述，名称保持。
	updated, err = s.Update(g.ID, nil, strp(""))
	if err != nil || updated.Name != "平台组" || updated.Description != "" {
		t.Fatalf("desc-only update = %+v, err = %v", updated, err)
	}
	// 两者均 nil：无操作。
	updated, err = s.Update(g.ID, nil, nil)
	if err != nil || updated.Name != "平台组" {
		t.Fatalf("no-op update = %+v, err = %v", updated, err)
	}
	// 改成既有他组名称冲突。
	if _, err := s.Create("研发组", "", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(g.ID, strp("研发组"), nil); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("rename conflict: err = %v, want ErrNameConflict", err)
	}
	// 不存在的组。
	if _, err := s.Update(uuid.New(), strp("x"), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group: err = %v, want ErrNotFound", err)
	}
}

// 列表带成员数聚合。
func TestGroupListMemberCount(t *testing.T) {
	s := newTestService()
	owner := uuid.New()
	g, _ := s.Create("g1", "", owner)
	h, _ := s.Create("g2", "", owner)
	u1, u2 := uuid.New(), uuid.New()
	if _, err := s.AddMember(g.ID, u1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(g.ID, u2); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[uuid.UUID]int64{}
	for _, item := range list {
		counts[item.ID] = item.MemberCount
	}
	if counts[g.ID] != 2 || counts[h.ID] != 0 {
		t.Fatalf("member counts = %v", counts)
	}
}

// 成员增删与用户 → 组名聚合。
func TestGroupMembers(t *testing.T) {
	s := newTestService()
	owner := uuid.New()
	g, _ := s.Create("g1", "", owner)
	h, _ := s.Create("g2", "", owner)
	u := uuid.New()

	if _, err := s.AddMember(g.ID, u); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(g.ID, u); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("duplicate member: err = %v, want ErrMemberExists", err)
	}
	if _, err := s.AddMember(h.ID, u); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListMembers(g.ID)
	if err != nil || len(members) != 1 || members[0].UserID != u {
		t.Fatalf("members = %+v, err = %v", members, err)
	}
	if _, err := s.ListMembers(uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group members: err = %v, want ErrNotFound", err)
	}

	names, err := s.NamesForUsers([]uuid.UUID{u, uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if len(names[u]) != 2 {
		t.Fatalf("names[u] = %v, want 2 groups", names[u])
	}

	if err := s.RemoveMember(g.ID, u); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(g.ID, u); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove twice: err = %v, want ErrNotFound", err)
	}
	// 删除组：成员关系随之清空（MemoryStore 同步模拟级联）。
	if _, err := s.AddMember(g.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(g.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted group must not be readable")
	}
	// 空入参聚合直接返回空 map。
	names, err = s.NamesForUsers(nil)
	if err != nil || len(names) != 0 {
		t.Fatalf("empty aggregation = %v, err = %v", names, err)
	}
}

func strp(s string) *string { return &s }
