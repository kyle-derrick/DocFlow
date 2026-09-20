package acl

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// entry 快捷构造。
func entry(subject string, subjectID uuid.UUID, effect string, perms ...string) Entry {
	return Entry{ID: uuid.New(), SubjectType: subject, SubjectID: subjectID, Effect: effect, Permissions: perms}
}

// TestResolveMatrix 覆盖求值矩阵：近覆盖远 / deny 优先 / user 覆盖 space /
// perm 不参与 / 无匹配回退（matched=false）。
func TestResolveMatrix(t *testing.T) {
	user, other, sp := uuid.New(), uuid.New(), uuid.New()
	otherSpace := uuid.New()

	tests := []struct {
		name    string
		chain   []ChainNode
		perm    string
		allowed bool
		matched bool
	}{
		{
			name:    "near allow overrides far deny",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectUser, user, EffectAllow, "read")}}, {Entries: []Entry{entry(SubjectUser, user, EffectDeny, "read")}}},
			perm:    "read",
			allowed: true, matched: true,
		},
		{
			name:    "far allow does not override near deny",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectUser, user, EffectDeny, "write")}}, {Entries: []Entry{entry(SubjectUser, user, EffectAllow, "write")}}},
			perm:    "write",
			allowed: false, matched: true,
		},
		{
			name:    "user deny beats space allow at same node",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectUser, user, EffectDeny, "read"), entry(SubjectSpace, sp, EffectAllow, "read")}}},
			perm:    "read",
			allowed: false, matched: true,
		},
		{
			name:    "user allow beats space deny at same node",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectUser, user, EffectAllow, "read"), entry(SubjectSpace, sp, EffectDeny, "read")}}},
			perm:    "read",
			allowed: true, matched: true,
		},
		{
			name:    "space deny at near node beats user allow at far node",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectSpace, sp, EffectDeny, "delete")}}, {Entries: []Entry{entry(SubjectUser, user, EffectAllow, "delete")}}},
			perm:    "delete",
			allowed: false, matched: true,
		},
		{
			name:    "space allow grants member",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectSpace, sp, EffectAllow, "share")}}},
			perm:    "share",
			allowed: true, matched: true,
		},
		{
			name:    "entries for other perm do not participate",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectSpace, sp, EffectDeny, "write")}}},
			perm:    "read",
			allowed: false, matched: false,
		},
		{
			name:    "entries for other subject do not participate",
			chain:   []ChainNode{{Entries: []Entry{entry(SubjectUser, other, EffectDeny, "read"), entry(SubjectSpace, otherSpace, EffectDeny, "read")}}},
			perm:    "read",
			allowed: false, matched: false,
		},
		{
			name:    "no entries unmatched",
			chain:   []ChainNode{{}, {}},
			perm:    "read",
			allowed: false, matched: false,
		},
		{
			name:  "empty chain unmatched",
			chain: nil,
			perm:  "read",
		},
	}
	for _, tc := range tests {
		allowed, matched := Resolve(tc.chain, user, sp, tc.perm)
		if allowed != tc.allowed || matched != tc.matched {
			t.Errorf("%s: Resolve = (allowed=%v, matched=%v), want (%v, %v)", tc.name, allowed, matched, tc.allowed, tc.matched)
		}
	}
}

// TestServiceReplaceValidation 覆盖 PUT 校验：无空间归属的脏数据目录拒绝、
// 非法条目拒绝、重复主体拒绝、合法整体替换（含清空）。
func TestServiceReplaceValidation(t *testing.T) {
	spaceID := uuid.New()
	owner := uuid.New()
	orphan := files.File{ID: uuid.New(), Name: "p", OwnerID: owner, Type: "folder"}
	spaceFolder := files.File{ID: uuid.New(), Name: "t", OwnerID: owner, Type: "folder", SpaceID: spaceID}
	repo := NewMemoryRepo()
	repo.PutFolder(orphan)
	repo.PutFolder(spaceFolder)
	svc := NewService(repo)

	if _, err := svc.Replace(orphan.ID, []EntryInput{{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectAllow, Permissions: []string{"read"}}}, owner); !errors.Is(err, ErrNotSpaceFolder) {
		t.Fatalf("orphan folder: err = %v, want ErrNotSpaceFolder", err)
	}
	if _, _, err := svc.List(orphan.ID); !errors.Is(err, ErrNotSpaceFolder) {
		t.Fatalf("list orphan folder: err = %v, want ErrNotSpaceFolder", err)
	}

	bad := []EntryInput{
		{SubjectType: "group", SubjectID: owner, Effect: EffectAllow, Permissions: []string{"read"}},         // 非法 subject
		{SubjectType: SubjectUser, SubjectID: uuid.Nil, Effect: EffectAllow, Permissions: []string{"read"}},  // nil subject id
		{SubjectType: SubjectUser, SubjectID: owner, Effect: "block", Permissions: []string{"read"}},         // 非法 effect
		{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectAllow, Permissions: nil},                  // 空 permissions
		{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectAllow, Permissions: []string{"admin"}},    // 越界动作
		{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectAllow, Permissions: []string{"read", ""}}, // 空动作
	}
	for i, in := range bad {
		if _, err := svc.Replace(spaceFolder.ID, []EntryInput{in}, owner); !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("bad entry %d: err = %v, want ErrInvalidEntry", i, err)
		}
	}

	dup := []EntryInput{
		{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectAllow, Permissions: []string{"read"}},
		{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectDeny, Permissions: []string{"write"}},
	}
	if _, err := svc.Replace(spaceFolder.ID, dup, owner); !errors.Is(err, ErrDuplicateEntry) {
		t.Fatalf("duplicate subject: err = %v, want ErrDuplicateEntry", err)
	}

	valid := []EntryInput{
		{SubjectType: SubjectSpace, SubjectID: spaceID, Effect: EffectDeny, Permissions: []string{"write", "delete"}},
		{SubjectType: SubjectUser, SubjectID: owner, Effect: EffectAllow, Permissions: []string{"read", "write", "delete", "share"}},
	}
	f, err := svc.Replace(spaceFolder.ID, valid, owner)
	if err != nil {
		t.Fatalf("valid replace: %v", err)
	}
	if f.ID != spaceFolder.ID {
		t.Fatalf("returned folder = %v, want %v", f.ID, spaceFolder.ID)
	}
	_, entries, err := svc.List(spaceFolder.ID)
	if err != nil {
		t.Fatalf("list after replace: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.CreatedBy != owner || e.CreatedAt.IsZero() {
			t.Fatalf("entry %v missing audit fields (created_by/created_at)", e.ID)
		}
	}

	// 清空：空数组整体替换。
	if _, err := svc.Replace(spaceFolder.ID, nil, owner); err != nil {
		t.Fatalf("clear: %v", err)
	}
	_, entries, err = svc.List(spaceFolder.ID)
	if err != nil {
		t.Fatalf("list after clear: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries after clear = %d, want 0", len(entries))
	}
}

// TestServiceResolveForFileChain 验证链收集语义：目录含自身条目、文件自
// 父目录起、由近及远（近覆盖远）。
func TestServiceResolveForFileChain(t *testing.T) {
	spaceID, owner, member := uuid.New(), uuid.New(), uuid.New()
	root := files.File{ID: uuid.New(), Name: "root", OwnerID: owner, Type: "folder", SpaceID: spaceID, IsRoot: true}
	sub := files.File{ID: uuid.New(), Name: "sub", OwnerID: owner, Type: "folder", SpaceID: spaceID, ParentID: &root.ID}
	doc := files.File{ID: uuid.New(), Name: "doc.md", OwnerID: owner, Type: "file", SpaceID: spaceID, ParentID: &sub.ID}
	repo := NewMemoryRepo()
	repo.PutFolder(root)
	repo.PutFolder(sub)
	repo.PutFolder(doc)
	svc := NewService(repo)

	// 根节点全空间 deny write；子目录对 member allow write → 文件求值取近端 allow。
	if err := repo.Replace(root.ID, []Entry{entry(SubjectSpace, spaceID, EffectDeny, "write")}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Replace(sub.ID, []Entry{entry(SubjectUser, member, EffectAllow, "write")}); err != nil {
		t.Fatal(err)
	}

	allowed, matched, err := svc.ResolveForFile(doc.ID, spaceID, member, "write")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed || !matched {
		t.Fatalf("file chain: (allowed=%v, matched=%v), want (true, true)", allowed, matched)
	}
	// 目录自身（sub）与文件同链；read 无条目 → 回退。
	allowed, matched, err = svc.ResolveForFile(sub.ID, spaceID, member, "read")
	if err != nil {
		t.Fatal(err)
	}
	if allowed || matched {
		t.Fatalf("folder read: (allowed=%v, matched=%v), want (false, false)", allowed, matched)
	}
	// owner 不在 allow 名单：近端 user 条目不匹配、远端 space deny 命中 → 拒绝。
	allowed, matched, err = svc.ResolveForFile(doc.ID, spaceID, owner, "write")
	if err != nil {
		t.Fatal(err)
	}
	if allowed || !matched {
		t.Fatalf("owner write: (allowed=%v, matched=%v), want (false, true)", allowed, matched)
	}
	// 目标行不存在：空链回退（matched=false），不报错。
	allowed, matched, err = svc.ResolveForFile(uuid.New(), spaceID, member, "write")
	if err != nil || allowed || matched {
		t.Fatalf("missing target: (%v, %v, %v), want (false, false, nil)", allowed, matched, err)
	}
}

// 确保 GormRepo 与 MemoryRepo 的 text[] 字面量往返一致。
func TestTextArrayRoundTrip(t *testing.T) {
	if got := parseTextArray(formatTextArray([]string{"read", "write"})); len(got) != 2 || got[0] != "read" || got[1] != "write" {
		t.Fatalf("round trip = %v", got)
	}
	for _, raw := range []string{"", "NULL", "{}"} {
		if got := parseTextArray(raw); len(got) != 0 {
			t.Fatalf("parse(%q) = %v, want empty", raw, got)
		}
	}
	if got := parseTextArray(`{"read"}`); len(got) != 1 || got[0] != "read" {
		t.Fatalf("parse single = %v", got)
	}
}
