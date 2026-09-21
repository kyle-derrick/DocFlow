package share

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

// fakeTree 是 TreeSource 的内存实现：基于 fakeFiles 的文件表维护父子关系，
// 解析口径与 files.resolvePathLogic 对齐（lower(name) 匹配、中间段须目录、
// 软删除不可见）。
type fakeTree struct {
	*fakeFiles
	children map[uuid.UUID][]uuid.UUID
}

func newFakeTree(ff *fakeFiles) *fakeTree {
	return &fakeTree{fakeFiles: ff, children: map[uuid.UUID][]uuid.UUID{}}
}

func (t *fakeTree) add(owner uuid.UUID, parent uuid.UUID, name, fileType, blobStatus string) uuid.UUID {
	id := t.addFile(owner, name, fileType, blobStatus)
	fl := t.files[id]
	fl.ParentID = &parent
	t.files[id] = fl
	t.children[parent] = append(t.children[parent], id)
	return id
}

func (t *fakeTree) ResolveSubpath(base files.File, path string) (files.File, []files.File, error) {
	if base.Type != "folder" {
		return files.File{}, nil, files.ErrInvalidTarget
	}
	segments, err := files.SplitPathSegments(path)
	if err != nil {
		return files.File{}, nil, err
	}
	cur := base
	chain := []files.File{base}
	for i, seg := range segments {
		next, found := files.File{}, false
		for _, id := range t.children[cur.ID] {
			if c, ok := t.files[id]; ok && c.DeletedAt == nil && strings.EqualFold(c.Name, seg) {
				next, found = c, true
				break
			}
		}
		if !found {
			return files.File{}, nil, files.ErrNotFound
		}
		if i < len(segments)-1 && next.Type != "folder" {
			return files.File{}, nil, files.ErrNotFound
		}
		chain = append(chain, next)
		cur = next
	}
	return cur, chain, nil
}

func (t *fakeTree) ListFolderChildren(folderID uuid.UUID, limit int) ([]files.File, error) {
	out := make([]files.File, 0, len(t.children[folderID]))
	for _, id := range t.children[folderID] {
		if f, ok := t.files[id]; ok && f.DeletedAt == nil {
			out = append(out, f)
		}
	}
	return out, nil
}

func (t *fakeTree) CurrentBlobs(ids []uuid.UUID) map[uuid.UUID]files.ObjectBlob {
	out := make(map[uuid.UUID]files.ObjectBlob, len(ids))
	for _, id := range ids {
		if v, ok := t.versions[id]; ok {
			if b, ok2 := t.blobs[v.ObjectBlobID]; ok2 {
				out[id] = b
			}
		}
	}
	return out
}

// newTreeTestEnv 组装目录分享测试环境：owner 的 /资料/{a.txt, 子目录/b.png}。
// 返回 (svc, repo, tree, owner, 根目录 ID)。
func newTreeTestEnv(t *testing.T) (*Service, *MemoryStore, *fakeTree, uuid.UUID, uuid.UUID) {
	t.Helper()
	repo := NewMemoryStore()
	ff := newFakeFiles()
	tree := newFakeTree(ff)
	svc := NewService(repo, ff)
	svc.SetTreeSource(tree)
	owner := uuid.New()
	rootID := uuid.New()
	ff.files[rootID] = files.File{ID: rootID, Name: "资料", OwnerID: owner, Type: "folder"}
	_ = tree.add(owner, rootID, "a.txt", "file", files.BlobStatusAvailable)
	subID := tree.add(owner, rootID, "子目录", "folder", "")
	_ = tree.add(owner, subID, "b.png", "file", files.BlobStatusAvailable)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	return svc, repo, tree, owner, rootID
}

func TestCreatePublicFolderShare(t *testing.T) {
	svc, repo, _, owner, rootID := newTreeTestEnv(t)
	sh, token, err := svc.CreatePublic(owner, rootID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatalf("create folder share: %v", err)
	}
	if sh.FileID != rootID || len(token) != 43 {
		t.Fatalf("share = %+v token = %q", sh, token)
	}
	if !repo.IsPublic(rootID) {
		t.Fatal("files.is_public must be set for folder share")
	}
	// 目录分享 Resolve：无版本语义但分享有效。
	r, err := svc.Resolve(token)
	if err != nil || r.File.Type != "folder" {
		t.Fatalf("resolve folder share: file=%+v err=%v", r.File, err)
	}
}

func TestResolveTreeListingAndFile(t *testing.T) {
	svc, _, _, owner, rootID := newTreeTestEnv(t)
	_, token, err := svc.CreatePublic(owner, rootID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 根清单：a.txt + 子目录。
	root, err := svc.ResolveTree(token, "")
	if err != nil {
		t.Fatal(err)
	}
	if !root.Folder || len(root.Entries) != 2 {
		t.Fatalf("root = %+v", root)
	}
	byName := map[string]TreeEntry{}
	for _, e := range root.Entries {
		byName[e.Name] = e
	}
	if e := byName["a.txt"]; e.Type != "file" || e.Path != "a.txt" || e.Size != 42 {
		t.Fatalf("a.txt entry = %+v", e)
	}
	if e := byName["子目录"]; e.Type != "folder" || e.Path != "子目录" {
		t.Fatalf("子目录 entry = %+v", e)
	}
	// 子目录清单：相对路径前缀正确。
	sub, err := svc.ResolveTree(token, "子目录")
	if err != nil || !sub.Folder || len(sub.Entries) != 1 || sub.Entries[0].Path != "子目录/b.png" {
		t.Fatalf("sub = %+v err = %v", sub, err)
	}
	// 文件命中：元数据 + available blob。
	f, err := svc.ResolveTree(token, "子目录/b.png")
	if err != nil || f.Folder || f.File == nil || f.Path != "子目录/b.png" || f.Blob == nil || f.Blob.Status != files.BlobStatusAvailable {
		t.Fatalf("file = %+v err = %v", f, err)
	}
	// 大小写规范化命中。
	if _, err := svc.ResolveTree(token, "子目录/B.PNG"); err != nil {
		t.Fatalf("case-insensitive hit: %v", err)
	}
}

func TestResolveTreeOutsideRootAndTraversal(t *testing.T) {
	svc, _, _, owner, rootID := newTreeTestEnv(t)
	_, token, err := svc.CreatePublic(owner, rootID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 子树内不存在 → 404 语义。
	if _, err := svc.ResolveTree(token, "不存在.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// 穿越段被段校验拒绝 → 统一 ErrNotFound。
	for _, bad := range []string{"../a.txt", "a.txt/../../x", "子目录/../a.txt"} {
		if _, err := svc.ResolveTree(token, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("traversal %q: %v", bad, err)
		}
	}
	// 中间段是文件 → ErrNotFound。
	if _, err := svc.ResolveTree(token, "a.txt/inner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file as intermediate: %v", err)
	}
}

func TestResolveTreeRevokedAndExpiredGone(t *testing.T) {
	svc, repo, _, owner, rootID := newTreeTestEnv(t)
	sh, token, err := svc.CreatePublic(owner, rootID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 撤销 → 410 语义。
	if _, err := svc.Revoke(owner, sh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveTree(token, ""); !errors.Is(err, ErrGone) {
		t.Fatalf("revoked: %v", err)
	}
	// 过期 → 410。
	sh2, token2, err := svc.CreatePublic(owner, rootID, PermissionView, time.Hour, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expired := sh2
	past := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	expired.ExpiresAt = &past
	repo.Put(expired)
	if _, err := svc.ResolveTree(token2, ""); !errors.Is(err, ErrGone) {
		t.Fatalf("expired: %v", err)
	}
}

func TestResolveTreeRootDeletedGone(t *testing.T) {
	svc, _, tree, owner, rootID := newTreeTestEnv(t)
	_, token, err := svc.CreatePublic(owner, rootID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 根软删除 → ErrGone。
	tree.setDeleted(rootID, &time.Time{})
	if _, err := svc.ResolveTree(token, ""); !errors.Is(err, ErrGone) {
		t.Fatalf("root deleted: %v", err)
	}
}

func TestResolveTreeRequiresTreeSource(t *testing.T) {
	repo := NewMemoryStore()
	ff := newFakeFiles()
	svc := NewService(repo, ff) // 未注入 TreeSource
	owner := uuid.New()
	folderID := uuid.New()
	ff.files[folderID] = files.File{ID: folderID, Name: "d", OwnerID: owner, Type: "folder"}
	_, token, err := svc.CreatePublic(owner, folderID, PermissionView, 0, nil, ShareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveTree(token, ""); !errors.Is(err, ErrTreeUnavailable) {
		t.Fatalf("err = %v, want ErrTreeUnavailable", err)
	}
}

// 密码保护的目录分享：verify → 会话 cookie 流程与文件分享一致。
func TestFolderSharePasswordFlow(t *testing.T) {
	svc, _, _, owner, rootID := newTreeTestEnv(t)
	pwd := "share-pass-123"
	sh, token, err := svc.CreatePublic(owner, rootID, PermissionView, 0, nil, ShareOptions{Password: pwd})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.HasPassword() {
		t.Fatal("folder share must support password")
	}
	if _, err := svc.VerifySharePassword(token, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	value, err := svc.VerifySharePassword(token, pwd)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.ValidateShareSession(sh.ID, value) {
		t.Fatal("session must validate for folder share")
	}
}
