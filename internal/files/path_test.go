package files

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// memPathTree 是路径解析的内存替身：按 parent + lower(name) 索引子项，
// 与 findChildByLowerName 的 DB 查询同口径（未删除、非根、lower 精确匹配）。
type memPathTree struct {
	files  map[uuid.UUID]File
	byName map[uuid.UUID]map[string]uuid.UUID // parent -> lower(name) -> id
}

func newMemPathTree() *memPathTree {
	return &memPathTree{files: map[uuid.UUID]File{}, byName: map[uuid.UUID]map[string]uuid.UUID{}}
}

func (m *memPathTree) add(f File) {
	m.files[f.ID] = f
	if f.ParentID == nil {
		return // 根目录不参与名称索引
	}
	if m.byName[*f.ParentID] == nil {
		m.byName[*f.ParentID] = map[string]uuid.UUID{}
	}
	m.byName[*f.ParentID][strings.ToLower(f.Name)] = f.ID
}

func (m *memPathTree) findChild(parent uuid.UUID, lowerName string) (File, error) {
	if id, ok := m.byName[parent][lowerName]; ok {
		return m.files[id], nil
	}
	return File{}, ErrNotFound
}

// softDelete 软删除子项（解析口径即不可见）。
func (m *memPathTree) softDelete(id uuid.UUID) {
	f := m.files[id]
	if f.ParentID != nil {
		delete(m.byName[*f.ParentID], strings.ToLower(f.Name))
	}
	delete(m.files, id)
}

// newTestPathEnv 组装覆盖 personal/team 根与权限判定的 pathEnv。
func newTestPathEnv(m *memPathTree, personalRoot, teamRoot File, reader, writer func(user, team uuid.UUID) (bool, error)) pathEnv {
	return pathEnv{
		findChild:    m.findChild,
		personalRoot: func(actor uuid.UUID) (File, error) { return personalRoot, nil },
		teamRoot:     func(teamID uuid.UUID) (File, error) { return teamRoot, nil },
		teamReader:   reader,
		teamWriter:   writer,
	}
}

func newTestTree(t *testing.T) (pathEnv, *memPathTree, File, File, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	m := newMemPathTree()
	personalRoot := File{ID: uuid.New(), Name: "根目录", OwnerID: uuid.New(), Type: "folder", IsRoot: true, ScopeType: "personal"}
	teamID := uuid.New()
	teamRoot := File{ID: uuid.New(), Name: "团队根目录", OwnerID: uuid.New(), Type: "folder", IsRoot: true, ScopeType: "team", TeamID: &teamID}
	m.add(personalRoot)
	m.add(teamRoot)
	// 个人：/项目资料/2026 计划/年度报告.docx + /notes.txt
	proj := File{ID: uuid.New(), Name: "项目资料", ParentID: &personalRoot.ID, OwnerID: personalRoot.OwnerID, Type: "folder", ScopeType: "personal"}
	m.add(proj)
	sub := File{ID: uuid.New(), Name: "2026 计划", ParentID: &proj.ID, OwnerID: personalRoot.OwnerID, Type: "folder", ScopeType: "personal"}
	m.add(sub)
	report := File{ID: uuid.New(), Name: "年度报告.docx", ParentID: &sub.ID, OwnerID: personalRoot.OwnerID, Type: "file", ScopeType: "personal"}
	m.add(report)
	notes := File{ID: uuid.New(), Name: "readme.txt", ParentID: &personalRoot.ID, OwnerID: personalRoot.OwnerID, Type: "file", ScopeType: "personal"}
	m.add(notes)
	// 团队：/共享/budget.xlsx
	shared := File{ID: uuid.New(), Name: "共享", ParentID: &teamRoot.ID, OwnerID: teamRoot.OwnerID, Type: "folder", ScopeType: "team", TeamID: &teamID}
	m.add(shared)
	budget := File{ID: uuid.New(), Name: "budget.xlsx", ParentID: &shared.ID, OwnerID: teamRoot.OwnerID, Type: "file", ScopeType: "team", TeamID: &teamID}
	m.add(budget)

	editor := uuid.New()
	viewer := uuid.New()
	member := func(user, team uuid.UUID) (bool, error) {
		return team == teamID && (user == editor || user == viewer), nil
	}
	env := newTestPathEnv(m, personalRoot, teamRoot, member, func(user, team uuid.UUID) (bool, error) {
		return team == teamID && user == editor, nil // 仅 editor 可写
	})
	return env, m, personalRoot, teamRoot, editor, viewer, teamID
}

func TestSplitPathSegments(t *testing.T) {
	// 正常中文路径 + NFC 归一（分解形态 e\u0301 归一为 é）。
	got, err := SplitPathSegments("/项目资料/caf\u00e9/报告.docx/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"项目资料", "café", "报告.docx"}
	if len(got) != len(want) {
		t.Fatalf("segments = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segments[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// 空路径 = 根。
	if segs, err := SplitPathSegments("/"); err != nil || segs != nil {
		t.Fatalf("root path: %v %v", segs, err)
	}
	// 拒绝：穿越、空段、点段、反斜杠、控制字符、Cf、二次编码变体。
	for _, bad := range []string{
		"../etc/passwd", "a/../../b", "a/./b", "a//b", "a\\/b", "a\nb",
		"a\u202Eb", "a\u200Bb", "a%2Fb", "a%2fb", "a%5Cb", "a%252Fb", "a%255cb",
		"CON", "name.", " ", "/../",
	} {
		if _, err := SplitPathSegments(bad); err == nil {
			t.Errorf("SplitPathSegments(%q) 应拒绝", bad)
		}
	}
	// 超深（>32 段）拒绝，恰好 32 段放行。
	deep := strings.Repeat("d/", 32)
	if _, err := SplitPathSegments(deep); err != nil {
		t.Fatalf("32 段应放行: %v", err)
	}
	if _, err := SplitPathSegments(deep + "x"); !errors.Is(err, ErrFolderDepth) {
		t.Fatalf("33 段 err = %v, want ErrFolderDepth", err)
	}
}

func TestResolveNamespacePathPersonal(t *testing.T) {
	env, m, personalRoot, _, editor, _, _ := newTestTree(t)
	// 正常路径命中；canonical 链含实际存储名称。
	f, chain, err := resolveNamespacePath(env, personalRoot.OwnerID, NamespacePersonal, personalRoot.OwnerID, "/项目资料/2026 计划/年度报告.docx", false)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "年度报告.docx" || len(chain) != 4 || chain[1].Name != "项目资料" {
		t.Fatalf("f=%+v chain=%d", f, len(chain))
	}
	// 大小写规范化：请求 README.TXT 命中存储的 readme.txt。
	f2, _, err := resolveNamespacePath(env, personalRoot.OwnerID, NamespacePersonal, personalRoot.OwnerID, "README.txt", false)
	if err != nil || f2.Name != "readme.txt" {
		t.Fatalf("大小写规范化: f=%+v err=%v", f2, err)
	}
	// 中间段是文件 → ErrNotFound（统一不泄露）。
	if _, _, err := resolveNamespacePath(env, personalRoot.OwnerID, NamespacePersonal, personalRoot.OwnerID, "readme.txt/inner", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file as intermediate: %v", err)
	}
	// 断链/不存在 → ErrNotFound。
	if _, _, err := resolveNamespacePath(env, personalRoot.OwnerID, NamespacePersonal, personalRoot.OwnerID, "不存在/x.txt", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// 越权 personal：scopeID != actor 或他人访问。
	if _, _, err := resolveNamespacePath(env, personalRoot.OwnerID, NamespacePersonal, editor, "x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scope mismatch: %v", err)
	}
	if _, _, err := resolveNamespacePath(env, editor, NamespacePersonal, personalRoot.OwnerID, "readme.txt", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user: %v", err)
	}
	// 软删除中间层 → ErrNotFound。
	var projID uuid.UUID
	for _, f := range m.files {
		if f.Name == "项目资料" {
			projID = f.ID
		}
	}
	m.softDelete(projID)
	if _, _, err := resolveNamespacePath(env, personalRoot.OwnerID, NamespacePersonal, personalRoot.OwnerID, "项目资料/2026 计划/年度报告.docx", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("soft-deleted intermediate: %v", err)
	}
}

func TestResolveNamespacePathTeam(t *testing.T) {
	env, _, _, teamRoot, editor, viewer, teamID := newTestTree(t)
	// 成员（viewer）可读团队文件。
	if _, _, err := resolveNamespacePath(env, viewer, NamespaceTeam, teamID, "共享/budget.xlsx", false); err != nil {
		t.Fatal(err)
	}
	// 非成员 404；未注入 teamReader 时 fail closed 403。
	outsider := uuid.New()
	if _, _, err := resolveNamespacePath(env, outsider, NamespaceTeam, teamID, "共享/budget.xlsx", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-member read: %v", err)
	}
	noReader := env
	noReader.teamReader = nil
	if _, _, err := resolveNamespacePath(noReader, viewer, NamespaceTeam, teamID, "共享", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no teamReader: %v", err)
	}
	// viewer 写（mode=edit）→ ErrForbidden；editor 可写。
	if _, _, err := resolveNamespacePath(env, viewer, NamespaceTeam, teamID, "共享/budget.xlsx", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer write: %v", err)
	}
	if _, _, err := resolveNamespacePath(env, editor, NamespaceTeam, teamID, "共享/budget.xlsx", true); err != nil {
		t.Fatalf("editor write: %v", err)
	}
	// 团队根目录自身可解析（空路径）。
	root, _, err := resolveNamespacePath(env, editor, NamespaceTeam, teamID, "", false)
	if err != nil || root.ID != teamRoot.ID {
		t.Fatalf("team root resolve: %+v %v", root, err)
	}
	// 非法 nsType。
	if _, _, err := resolveNamespacePath(env, editor, "bogus", teamID, "", false); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatalf("bogus ns: %v", err)
	}
}

func TestResolvePathLogicCycleDetection(t *testing.T) {
	// 构造脏数据环：a 的子项 b，b 的“子项”指回 a（parent 链被破坏的场景）。
	m := newMemPathTree()
	root := File{ID: uuid.New(), Name: "r", Type: "folder", IsRoot: true, OwnerID: uuid.New()}
	a := File{ID: uuid.New(), Name: "a", ParentID: &root.ID, Type: "folder", OwnerID: root.OwnerID}
	m.add(root)
	m.add(a)
	m.byName[a.ID] = map[string]uuid.UUID{"loop": a.ID} // a/loop -> a
	if _, _, err := resolvePathLogic(m.findChild, root, []string{"a", "loop", "loop", "loop"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cycle: err = %v, want ErrNotFound", err)
	}
	// 根不是目录 → ErrInvalidTarget。
	notFolder := File{ID: uuid.New(), Name: "x.txt", Type: "file"}
	if _, _, err := resolvePathLogic(m.findChild, notFolder, nil); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("non-folder root: %v", err)
	}
}
