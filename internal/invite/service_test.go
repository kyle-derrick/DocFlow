package invite

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
)

// fakeRegistry 是 UserRegistry 的内存实现：用户名/邮箱唯一冲突返回
// auth.ErrUserExists（与 *auth.UserStore 的 PostgreSQL 23505 映射一致）。
type fakeRegistry struct {
	users     []auth.User
	byEmail   map[string]bool
	byName    map[string]bool
	createErr error
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{byEmail: make(map[string]bool), byName: make(map[string]bool)}
}

func (f *fakeRegistry) CreateUser(u auth.User) error {
	if f.createErr != nil {
		return f.createErr
	}
	if f.byEmail[u.Email] || f.byName[u.Username] {
		return auth.ErrUserExists
	}
	f.byEmail[u.Email] = true
	f.byName[u.Username] = true
	f.users = append(f.users, u)
	return nil
}

// newTestService 构造可变时钟的测试服务（各邀请创建时间互不相同）。
func newTestService(now time.Time) (*Service, *MemoryStore, *fakeRegistry, func() time.Time) {
	repo := NewMemoryStore()
	registry := newFakeRegistry()
	current := now
	clock := func() time.Time { return current }
	svc := NewService(repo, registry)
	svc.SetNow(clock)
	advance := func() time.Time {
		current = current.Add(time.Hour)
		return current
	}
	return svc, repo, registry, advance
}

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// Create：邮箱归一小写、格式/角色校验、明文 token 仅返回一次且库中只存哈希。
func TestCreateStoresHashOnly(t *testing.T) {
	svc, repo, _, _ := newTestService(testNow)
	admin := uuid.New()

	inv, token, err := svc.Create(admin, "  NewUser@Example.COM ", "")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Email != "newuser@example.com" {
		t.Fatalf("email = %q, want normalized lowercase", inv.Email)
	}
	if inv.Role != auth.RoleUser {
		t.Fatalf("default role = %q, want user", inv.Role)
	}
	if token == "" || len(token) != 43 {
		t.Fatalf("token = %q, want 43-char URL-safe base64", token)
	}
	saved, err := repo.Get(inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TokenHash != HashToken(token) {
		t.Fatal("stored hash must equal sha256(token)")
	}
	if saved.InvitedBy == nil || *saved.InvitedBy != admin {
		t.Fatal("invited_by must be the actor")
	}
	if !saved.ExpiresAt.Equal(testNow.Add(DefaultTTL)) {
		t.Fatalf("expires_at = %v, want now+7d", saved.ExpiresAt)
	}
}

// Create 入参校验：非法邮箱与非法角色。
func TestCreateValidatesInput(t *testing.T) {
	svc, _, _, _ := newTestService(testNow)
	if _, _, err := svc.Create(uuid.New(), "not-an-email", "user"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("email error = %v", err)
	}
	if _, _, err := svc.Create(uuid.New(), "a@b.com", "root"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("role error = %v", err)
	}
}

// Create 幂等：同邮箱未过期未接受的邀请重复创建返回既有记录（无新 token）。
func TestCreateIdempotentForActiveEmail(t *testing.T) {
	svc, _, _, _ := newTestService(testNow)
	first, token, err := svc.Create(uuid.New(), "dup@example.com", "admin")
	if err != nil {
		t.Fatal(err)
	}
	second, secondToken, err := svc.Create(uuid.New(), "DUP@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent create returned %v, want existing %v", second.ID, first.ID)
	}
	if secondToken != "" {
		t.Fatal("idempotent create must not mint a new token")
	}
	if second.Role != first.Role {
		t.Fatalf("existing invitation role = %q, want %q (unchanged)", second.Role, first.Role)
	}
	if token == "" {
		t.Fatal("first create must return token")
	}
}

// 已接受或已过期的邀请不阻断再次创建（返回新邀请）。
func TestCreateAllowsNewInviteAfterAccepted(t *testing.T) {
	svc, repo, _, advance := newTestService(testNow)
	first, _, err := svc.Create(uuid.New(), "again@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkAccepted(first.ID, testNow); err != nil {
		t.Fatal(err)
	}
	advance()
	second, token, err := svc.Create(uuid.New(), "again@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || token == "" {
		t.Fatal("new invitation must be created after previous accepted")
	}
}

// Accept：正常注册创建 active 用户（角色继承邀请）、标记 accepted、token 一次性。
func TestAcceptCreatesUserAndConsumes(t *testing.T) {
	svc, repo, registry, _ := newTestService(testNow)
	inv, token, err := svc.Create(uuid.New(), "join@example.com", "admin")
	if err != nil {
		t.Fatal(err)
	}

	user, consumed, err := svc.Accept(token, " newuser ", "StrongPass123")
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "newuser" {
		t.Fatalf("username = %q, want trimmed newuser", user.Username)
	}
	if user.Email != "join@example.com" || user.Status != auth.StatusActive || user.Role != auth.RoleAdmin {
		t.Fatalf("user = %+v", user)
	}
	if consumed.ID != inv.ID {
		t.Fatalf("consumed invitation = %v, want %v", consumed.ID, inv.ID)
	}
	if len(registry.users) != 1 {
		t.Fatalf("created users = %d, want 1", len(registry.users))
	}
	saved, _ := repo.Get(inv.ID)
	if saved.AcceptedAt == nil {
		t.Fatal("accepted_at must be set")
	}
	// 一次性：同一 token 二次使用返回 ErrGone。
	if _, _, err := svc.Accept(token, "another", "StrongPass123"); !errors.Is(err, ErrGone) {
		t.Fatalf("token reuse error = %v, want ErrGone", err)
	}
}

// Accept：未知 token 404、过期/已接受 410、用户名与密码规则复用 auth 校验。
func TestAcceptRejects(t *testing.T) {
	svc, repo, _, _ := newTestService(testNow)
	if _, _, err := svc.Accept("unknown-token", "newuser", "StrongPass123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token error = %v", err)
	}

	expired, token, err := svc.Create(uuid.New(), "expired@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	stale := expired
	stale.ExpiresAt = testNow.Add(-time.Minute)
	repo.Put(stale)
	if _, _, err := svc.Accept(token, "newuser", "StrongPass123"); !errors.Is(err, ErrGone) {
		t.Fatalf("expired invitation error = %v, want ErrGone", err)
	}

	// 用户名/密码规则复用 auth 校验（用另一条有效邀请验证）。
	_, validToken, err := svc.Create(uuid.New(), "valid@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Accept(validToken, "ab", "StrongPass123"); !errors.Is(err, auth.ErrInvalidUsername) {
		t.Fatalf("short username error = %v", err)
	}
	if _, _, err := svc.Accept(validToken, "newuser", "weak"); !errors.Is(err, auth.ErrPasswordTooShort) {
		t.Fatalf("weak password error = %v", err)
	}
}

// Accept：用户名/邮箱已被占用（auth.ErrUserExists）时邀请保持未接受（可重试）。
func TestAcceptUserExistsKeepsInvitation(t *testing.T) {
	svc, _, registry, _ := newTestService(testNow)
	_, token, err := svc.Create(uuid.New(), "taken@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	registry.byName["existing"] = true
	if _, _, err := svc.Accept(token, "existing", "StrongPass123"); !errors.Is(err, auth.ErrUserExists) {
		t.Fatalf("conflict error = %v", err)
	}
	list, err := svc.List(10)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (err %v), want 1 invitation", list, err)
	}
	if list[0].AcceptedAt != nil {
		t.Fatal("invitation must stay pending when username is taken")
	}
	// 换个用户名可重试成功。
	if _, _, err := svc.Accept(token, "freshname", "StrongPass123"); err != nil {
		t.Fatalf("retry accept error = %v", err)
	}
}

// Revoke：删除后 token 不可解析；不存在返回 ErrNotFound。
func TestRevoke(t *testing.T) {
	svc, _, _, _ := newTestService(testNow)
	inv, token, err := svc.Create(uuid.New(), "gone@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Accept(token, "newuser", "StrongPass123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token error = %v, want ErrNotFound", err)
	}
	if err := svc.Revoke(inv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke again error = %v, want ErrNotFound", err)
	}
}

// Status 派生状态与 List 倒序（时钟逐条推进，创建时间互不相同）。
func TestStatusAndListOrder(t *testing.T) {
	svc, repo, _, advance := newTestService(testNow)
	if _, _, err := svc.Create(uuid.New(), "a@example.com", "user"); err != nil {
		t.Fatal(err)
	}
	advance()
	if _, acceptedToken, err := svc.Create(uuid.New(), "b@example.com", "user"); err != nil {
		t.Fatal(err)
	} else if _, _, err := svc.Accept(acceptedToken, "bob", "StrongPass123"); err != nil {
		t.Fatal(err)
	}
	advance()
	expiredInv, _, err := svc.Create(uuid.New(), "c@example.com", "user")
	if err != nil {
		t.Fatal(err)
	}
	stale := expiredInv
	stale.ExpiresAt = testNow.Add(-time.Hour)
	repo.Put(stale)
	// 断言时钟回到创建时刻附近（testNow+2h），派生状态按当前时间判定。
	list, err := svc.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list length = %d, want 3", len(list))
	}
	if !list[0].CreatedAt.After(list[1].CreatedAt) || !list[1].CreatedAt.After(list[2].CreatedAt) {
		t.Fatal("list must be ordered by created_at desc")
	}
	statuses := map[string]string{
		list[0].Email: list[0].Status(testNow.Add(2 * time.Hour)),
		list[1].Email: list[1].Status(testNow.Add(2 * time.Hour)),
		list[2].Email: list[2].Status(testNow.Add(2 * time.Hour)),
	}
	if statuses["a@example.com"] != StatusPending {
		t.Fatalf("a status = %q, want pending", statuses["a@example.com"])
	}
	if statuses["b@example.com"] != StatusAccepted {
		t.Fatalf("b status = %q, want accepted", statuses["b@example.com"])
	}
	if statuses["c@example.com"] != StatusExpired {
		t.Fatalf("c status = %q, want expired", statuses["c@example.com"])
	}
}
