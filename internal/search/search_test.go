package search

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// --- 文本判定矩阵 ---

func TestIsTextIndexable(t *testing.T) {
	cases := []struct {
		mime, name string
		want       bool
		note       string
	}{
		{"text/plain", "a.txt", true, "text/* 前缀"},
		{"text/plain; charset=utf-8", "a.txt", true, "带参数的 text/*"},
		{"TEXT/PLAIN", "a.txt", true, "mime 大小写不敏感"},
		{"application/json", "a", true, "json mime"},
		{"application/xml", "a", true, "xml mime"},
		{"application/octet-stream", "notes.md", true, "上传落库 mime 恒 octet-stream，扩展名兜底：md"},
		{"application/octet-stream", "main.go", true, "扩展名：go"},
		{"application/octet-stream", "Data.CSV", true, "扩展名大小写不敏感"},
		{"application/octet-stream", "script.PY", true, "扩展名：py"},
		{"application/octet-stream", "config.yaml", true, "扩展名：yaml"},
		{"", "readme", false, "无扩展名且无 mime"},
		{"application/octet-stream", "photo.jpg", false, "二进制：图片"},
		{"image/png", "a.png", false, "二进制：图片 mime"},
		{"application/pdf", "doc.pdf", false, "二进制：pdf"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "a.docx", false, "二进制：office"},
		{"application/zip", "pkg.zip", false, "二进制：zip"},
		{"application/octet-stream", "binary", false, "无扩展名"},
	}
	for _, c := range cases {
		if got := IsTextIndexable(c.mime, c.name); got != c.want {
			t.Errorf("IsTextIndexable(%q, %q) = %v, want %v (%s)", c.mime, c.name, got, c.want, c.note)
		}
	}
}

// --- q escape 纯函数 ---

func TestEscapeLike(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"", ""},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{"%_\\", `\%\_\\`},
		{"报告 100% 完成", `报告 100\% 完成`},
	}
	for _, c := range cases {
		if got := EscapeLike(c.in); got != c.want {
			t.Errorf("EscapeLike(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := LikePattern("a%b"); got != `%a\%b%` {
		t.Fatalf("LikePattern(\"a%%b\") = %q", got)
	}
}

// containsFold 与 ILIKE 子串语义一致（大小写不敏感字面包含）。
func TestContainsFold(t *testing.T) {
	if !containsFold("Report-2026.md", "report") {
		t.Fatal("大小写不敏感包含应命中")
	}
	if containsFold("report", "port2") {
		t.Fatal("非子串不应命中")
	}
}

// --- Store 参数归一 ---

func TestStoreQueryNormalizes(t *testing.T) {
	s := NewStore(NewMemoryRepo())
	out, err := s.Query(uuid.New(), QueryOptions{Q: "  "})
	if err != nil || len(out) != 0 {
		t.Fatalf("blank q: (%v, %v), want ([], nil)", out, err)
	}
	if _, err := s.Query(uuid.New(), QueryOptions{Q: "x", Limit: 999}); err != nil {
		t.Fatalf("oversize limit should be clamped by Store: %v", err)
	}
}

// --- Query 访问控制与过滤语义（内存 fake repo） ---

type fixture struct {
	store *Store
	repo  *MemoryRepo

	owner, member, stranger uuid.UUID
	space1, space2          uuid.UUID

	personalFile, spaceFile, space2File, deletedFile, strangerFile uuid.UUID
}

func newFixture() *fixture {
	now := time.Now()
	f := &fixture{
		owner: uuid.New(), member: uuid.New(), stranger: uuid.New(),
		space1: uuid.New(), space2: uuid.New(),
		personalFile: uuid.New(), spaceFile: uuid.New(), space2File: uuid.New(),
		deletedFile: uuid.New(), strangerFile: uuid.New(),
	}
	f.repo = NewMemoryRepo()
	f.store = NewStore(f.repo)

	f.repo.PutDoc(Doc{FileID: f.personalFile, OwnerID: f.owner, Name: "notes", Content: "quarterly report numbers"})
	f.repo.PutFile(f.personalFile, MemoryFile{Name: "notes-2026.md", Type: "file", UpdatedAt: now.Add(-time.Hour)})

	f.repo.PutDoc(Doc{FileID: f.spaceFile, OwnerID: uuid.New(), SpaceID: &f.space1, Name: "space-plan", Content: "roadmap for q3"})
	f.repo.PutFile(f.spaceFile, MemoryFile{Name: "space-plan.md", Type: "file", UpdatedAt: now})
	f.repo.AddMember(f.space1, f.member)

	f.repo.PutDoc(Doc{FileID: f.space2File, OwnerID: uuid.New(), SpaceID: &f.space2, Name: "secret", Content: "secret plan"})
	f.repo.PutFile(f.space2File, MemoryFile{Name: "secret.md", Type: "file", UpdatedAt: now})

	f.repo.PutDoc(Doc{FileID: f.deletedFile, OwnerID: f.owner, Name: "deleted report", Content: "removed"})
	f.repo.PutFile(f.deletedFile, MemoryFile{Name: "deleted.md", Type: "file", UpdatedAt: now, Deleted: true})

	f.repo.PutDoc(Doc{FileID: f.strangerFile, OwnerID: f.stranger, Name: "stranger report", Content: "not mine"})
	f.repo.PutFile(f.strangerFile, MemoryFile{Name: "stranger report.md", Type: "file", UpdatedAt: now})
	return f
}

// 名称命中（大小写不敏感）返回名称作为 snippet；内容命中返回 [[..]] 高亮片段。
func TestQueryNameAndContentMatch(t *testing.T) {
	f := newFixture()
	out, err := f.store.Query(f.owner, QueryOptions{Q: "NOTES"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "notes-2026.md" {
		t.Fatalf("name match: %+v", out)
	}
	if out[0].Snippet != "notes-2026.md" {
		t.Fatalf("snippet = %q, want name on name hit", out[0].Snippet)
	}
	out, err = f.store.Query(f.owner, QueryOptions{Q: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !strings.Contains(out[0].Snippet, "[[quarterly]]") {
		t.Fatalf("content match: %+v", out)
	}
}

// 访问控制：owner 本人可读；空间在册成员可读；非成员空间文件、他人文件、
// 软删文件一律不可见。
func TestQueryAccessControl(t *testing.T) {
	f := newFixture()
	// 空间在册成员：命中空间文件内容。
	out, err := f.store.Query(f.member, QueryOptions{Q: "roadmap"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "space-plan.md" {
		t.Fatalf("space member content match: %+v", out)
	}
	// 非成员：空间文件 / 他人文件 / 软删文件全部不可见。
	out, err = f.store.Query(f.member, QueryOptions{Q: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("non-member must not see space2/stranger/soft-deleted: %+v", out)
	}
	out, err = f.store.Query(f.member, QueryOptions{Q: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "space-plan.md" {
		t.Fatalf("member name search should only hit own space: %+v", out)
	}
	// owner 视角：软删与陌生人文件不出现（"report" 命中名称/内容）。
	out, err = f.store.Query(f.owner, QueryOptions{Q: "report"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if strings.Contains(r.Name, "deleted") || strings.Contains(r.Name, "stranger") {
			t.Fatalf("soft-deleted/stranger file leaked: %+v", r)
		}
	}
	// 陌生人视角：只命中自己的文件（他人文件不可见）。
	out, err = f.store.Query(f.stranger, QueryOptions{Q: "report"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Name != "stranger report.md" {
		t.Fatalf("stranger should only see own file: %+v", out)
	}
}

func TestQueryTagStarredFilterAndLimit(t *testing.T) {
	f := newFixture()
	tag := uuid.New()
	f.repo.AddTag(f.personalFile, tag)
	f.repo.PutFile(f.personalFile, MemoryFile{Name: "notes-2026.md", Type: "file", UpdatedAt: time.Now(), Starred: true})

	out, err := f.store.Query(f.owner, QueryOptions{Q: "notes", TagID: &tag})
	if err != nil || len(out) != 1 {
		t.Fatalf("tag filter: (%v, %v)", out, err)
	}
	otherTag := uuid.New()
	if out, err = f.store.Query(f.owner, QueryOptions{Q: "notes", TagID: &otherTag}); err != nil || len(out) != 0 {
		t.Fatalf("unrelated tag filter: (%v, %v)", out, err)
	}
	starred := true
	if out, err = f.store.Query(f.owner, QueryOptions{Q: "notes", Starred: &starred}); err != nil || len(out) != 1 {
		t.Fatalf("starred filter: (%v, %v)", out, err)
	}
	notStarred := false
	if out, err = f.store.Query(f.owner, QueryOptions{Q: "notes", Starred: &notStarred}); err != nil || len(out) != 0 {
		t.Fatalf("starred=false filter: (%v, %v)", out, err)
	}
	if out, err = f.store.Query(f.owner, QueryOptions{Q: "notes", Limit: 0}); err != nil || len(out) != 1 {
		t.Fatalf("default limit: (%v, %v)", out, err)
	}
}

// 名称命中优先于内容命中（即使内容命中者更新时间更近）。
func TestQueryNameHitRankedFirst(t *testing.T) {
	repo := NewMemoryRepo()
	owner := uuid.New()
	now := time.Now()
	contentHit := uuid.New()
	repo.PutDoc(Doc{FileID: contentHit, OwnerID: owner, Name: "a", Content: "the quick brown report"})
	repo.PutFile(contentHit, MemoryFile{Name: "a.txt", Type: "file", UpdatedAt: now})
	nameHit := uuid.New()
	repo.PutDoc(Doc{FileID: nameHit, OwnerID: owner, Name: "b", Content: "filler"})
	repo.PutFile(nameHit, MemoryFile{Name: "report.pdf", Type: "file", UpdatedAt: now.Add(-time.Hour)})
	s := NewStore(repo)
	out, err := s.Query(owner, QueryOptions{Q: "report"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Name != "report.pdf" {
		t.Fatalf("name hit must rank first even if older: %+v", out)
	}
}

// --- Indexer 内容抽取（fake storage） ---

type memStorage struct{ data map[string]string }

func (m *memStorage) Put(string, io.Reader) error             { return nil }
func (m *memStorage) Append(string, io.Reader) (int64, error) { return 0, nil }
func (m *memStorage) Read(key string) (io.ReadCloser, error) {
	v, ok := m.data[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(strings.NewReader(v)), nil
}
func (m *memStorage) Delete(string) error { return nil }

func TestReadContent(t *testing.T) {
	st := &memStorage{data: map[string]string{"k1": "hello searchable world"}}
	// 文本扩展名 + 未超限：读全文。
	if c, err := ReadContent(st, files.ObjectBlob{StorageKey: "k1", Size: 21, MimeType: "application/octet-stream"}, "a.md"); err != nil || c != "hello searchable world" {
		t.Fatalf("text read = (%q, %v)", c, err)
	}
	// 超过 2MB：仅名称（不读存储）。
	if c, err := ReadContent(st, files.ObjectBlob{StorageKey: "k1", Size: MaxContentBytes + 1, MimeType: "text/plain"}, "a.txt"); err != nil || c != "" {
		t.Fatalf("oversize = (%q, %v), want empty", c, err)
	}
	// 二进制：仅名称。
	if c, err := ReadContent(st, files.ObjectBlob{StorageKey: "k1", Size: 10, MimeType: "application/pdf"}, "a.pdf"); err != nil || c != "" {
		t.Fatalf("binary = (%q, %v), want empty", c, err)
	}
	// 读取失败：返回错误（Indexer 降级仅名称）。
	if _, err := ReadContent(st, files.ObjectBlob{StorageKey: "missing", Size: 5, MimeType: "text/plain"}, "a.txt"); err == nil {
		t.Fatal("missing object: want error")
	}
}
