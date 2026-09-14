package auth

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// fakeStore 模拟 GormSessionStore 的事务语义：map 中保留同一 session 的
// 历史 hash 槽位，使旧 hash 重放可定位到 session 并执行撤销。
type fakeStore struct {
	sessions map[string]Session
}

func newFakeStore() *fakeStore { return &fakeStore{sessions: make(map[string]Session)} }
func (f *fakeStore) Create(session Session, info SessionInfo) error {
	f.sessions[session.RefreshTokenHash] = session
	return nil
}

// ListActive 与 GormSessionStore 语义一致：未撤销未过期，last_active_at 倒序
// （map 无序，测试内按 LastActiveAt 排序后返回）。
func (f *fakeStore) ListActive(userID uuid.UUID, now time.Time) ([]SessionView, error) {
	var out []SessionView
	for _, session := range f.sessions {
		if session.UserID == userID && session.RevokedAt == nil && session.ExpiresAt.After(now) {
			out = append(out, SessionView{ID: session.ID, CreatedAt: session.CreatedAt, LastActiveAt: session.LastActiveAt, ExpiresAt: session.ExpiresAt})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActiveAt.After(out[j].LastActiveAt) })
	return out, nil
}

// RevokeByID 与 GormSessionStore 语义一致：仅属主且未撤销时生效。
func (f *fakeStore) RevokeByID(owner, id uuid.UUID, now time.Time) (bool, error) {
	for hash, session := range f.sessions {
		if session.ID == id && session.UserID == owner && session.RevokedAt == nil {
			session.RevokedAt = &now
			f.sessions[hash] = session
			return true, nil
		}
	}
	return false, nil
}

// writeBack 将 session 回写到其全部 hash 槽位（当前 hash 与历史 hash）。
func (f *fakeStore) writeBack(session Session) {
	for hash, existing := range f.sessions {
		if existing.ID == session.ID {
			f.sessions[hash] = session
		}
	}
	f.sessions[session.RefreshTokenHash] = session
}

// Rotate 与 GormSessionStore.Rotate 语义同步（串行模拟两个事务的时序）：
//   - 未知/已撤销/已过期 hash：ErrInvalidRefreshToken；
//   - 旧 hash 重放（session 已轮换到新 hash，对应 UPDATE 复查 RowsAffected!=1）：
//     撤销整个 session（token family 失效）并返回 ErrInvalidRefreshToken；
//   - 正常轮换：旧 hash 槽位保留指向同一 session 以检测后续重放。
func (f *fakeStore) Rotate(oldHash, newHash string, now, expiresAt time.Time) (Session, error) {
	session, ok := f.sessions[oldHash]
	if !ok || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return Session{}, ErrInvalidRefreshToken
	}
	if session.RefreshTokenHash != oldHash {
		session.RevokedAt = &now
		f.writeBack(session)
		return Session{}, ErrInvalidRefreshToken
	}
	session.RefreshTokenHash, session.LastActiveAt, session.ExpiresAt = newHash, now, expiresAt
	f.writeBack(session)
	return session, nil
}

func (f *fakeStore) Revoke(hash string, now time.Time) error {
	session, ok := f.sessions[hash]
	if !ok || session.RevokedAt != nil {
		return ErrInvalidRefreshToken
	}
	session.RevokedAt = &now
	f.writeBack(session)
	return nil
}

// RevokeAllForUser 与 GormSessionStore 语义一致：撤销 user 的全部未撤销
// 会话，exceptHash 非空时保留对应会话。
func (f *fakeStore) RevokeAllForUser(userID uuid.UUID, exceptHash string, now time.Time) error {
	for hash, session := range f.sessions {
		if session.UserID == userID && session.RevokedAt == nil && hash != exceptHash {
			session.RevokedAt = &now
			f.writeBack(session)
		}
	}
	return nil
}

// fakeCredentials 是 Credentials 的内存实现（用户按 email 存取）。
type fakeCredentials struct {
	users  map[string]User
	byID   map[uuid.UUID]User
	hashes map[uuid.UUID]string
}

func newFakeCredentials(users ...User) *fakeCredentials {
	c := &fakeCredentials{users: make(map[string]User), byID: make(map[uuid.UUID]User), hashes: make(map[uuid.UUID]string)}
	for _, u := range users {
		c.users[u.Email] = u
		c.byID[u.ID] = u
		c.hashes[u.ID] = u.PasswordHash
	}
	return c
}

func (c *fakeCredentials) FindActiveByEmail(email string) (User, error) {
	u, ok := c.users[NormalizeEmail(email)]
	if !ok || u.Status != StatusActive {
		return User{}, ErrUserNotFound
	}
	return u, nil
}

func (c *fakeCredentials) GetByID(id uuid.UUID) (User, error) {
	u, ok := c.byID[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return u, nil
}

func (c *fakeCredentials) UpdatePasswordHash(id uuid.UUID, hash string) error {
	if _, ok := c.byID[id]; !ok {
		return ErrUserNotFound
	}
	c.hashes[id] = hash
	return nil
}

// fakeResetStore 是 PasswordResetStore 的内存实现。
type fakeResetStore struct {
	tokens map[string]PasswordResetToken
}

func newFakeResetStore() *fakeResetStore {
	return &fakeResetStore{tokens: make(map[string]PasswordResetToken)}
}

func (f *fakeResetStore) Create(t PasswordResetToken) error {
	f.tokens[t.TokenHash] = t
	return nil
}

// Consume 与 GormPasswordResetStore 语义一致：仅未使用未过期的令牌可消费一次。
func (f *fakeResetStore) Consume(tokenHash string, now time.Time) (uuid.UUID, bool, error) {
	t, ok := f.tokens[tokenHash]
	if !ok {
		return uuid.Nil, false, nil
	}
	if t.UsedAt != nil || !now.Before(t.ExpiresAt) {
		return uuid.Nil, false, nil
	}
	t.UsedAt = &now
	f.tokens[tokenHash] = t
	return t.UserID, true, nil
}

func TestHashRefreshTokenDeterministic(t *testing.T) {
	if HashRefreshToken("secret") != HashRefreshToken("secret") {
		t.Fatal("hash should be deterministic")
	}
	if HashRefreshToken("secret") == HashRefreshToken("other") {
		t.Fatal("different tokens must hash differently")
	}
}

// TestRefreshRotationRejectsReplay 串行模拟并发重放的时序：
// T1 用 original 正常轮换得到 replacement；T2 再次持 original 轮换即重放，
// 须被拒绝并撤销整个 session（token family 失效，最新 replacement 也不可用）。
func TestRefreshRotationRejectsReplay(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	original, err := service.NewSession(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	_, replacement, err := service.RotateRefreshToken(original)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == original {
		t.Fatal("rotation must replace token")
	}
	// T2：旧 token 重放 → 拒绝。
	if _, _, err := service.RotateRefreshToken(original); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("replay error = %v", err)
	}
	// 重放检测撤销整个 token family：replacement 亦随之失效。
	if _, _, err := service.RotateRefreshToken(replacement); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("family must be revoked after replay, got %v", err)
	}
}

// TestRefreshRotationReplayRevokesSession 断言重放后 session 在 store 中
// 被标记 revoked（撤销真实落库，而非仅返回错误）。
func TestRefreshRotationReplayRevokesSession(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	original, err := service.NewSession(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	sessionID := store.sessions[HashRefreshToken(original)].ID
	if _, _, err := service.RotateRefreshToken(original); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.RotateRefreshToken(original); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("replay error = %v", err)
	}
	rotated, ok := store.sessions[HashRefreshToken(original)]
	if !ok || rotated.ID != sessionID {
		t.Fatal("session must remain addressable via the replayed hash")
	}
	if rotated.RevokedAt == nil {
		t.Fatal("replay must revoke the session (revoked_at set)")
	}
}

func TestRefreshRevocation(t *testing.T) {
	store := newFakeStore()
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	token, err := service.NewSession(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeRefreshToken(token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.RotateRefreshToken(token); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("revoked token error = %v", err)
	}
}

// q 含 @ 时仅按邮箱整串精确匹配（大小写归一），不得退化为前缀/模糊匹配。
func TestLookupConditionEmailQueryExactOnly(t *testing.T) {
	condition, args := lookupCondition("Alice@Example.COM")
	if condition != "lower(email) = ?" {
		t.Fatalf("condition = %q, want email exact match only", condition)
	}
	if len(args) != 1 || args[0] != "alice@example.com" {
		t.Fatalf("args = %v, want [alice@example.com]", args)
	}
	if strings.Contains(condition, "ILIKE") {
		t.Fatal("email query must not fall back to prefix matching")
	}
}

// q 不含 @ 时：邮箱整串精确匹配 或 用户名前缀 ILIKE（保留前缀查找能力）。
func TestLookupConditionNonEmailKeepsUsernamePrefix(t *testing.T) {
	condition, args := lookupCondition("Ali")
	if condition != "(lower(email) = ? OR username ILIKE ? ESCAPE '\\')" {
		t.Fatalf("condition = %q", condition)
	}
	if len(args) != 2 || args[0] != "ali" || args[1] != "Ali%" {
		t.Fatalf("args = %v, want [ali Ali%%]", args)
	}
}

// LIKE 通配符必须转义，避免用户输入 %/_ 被当作通配符放大匹配范围。
func TestLikePrefixPatternEscapesWildcards(t *testing.T) {
	if got := likePrefixPattern(`a%b_c\d`); got != `a\%b\_c\\d%` {
		t.Fatalf("pattern = %q, want a\\%%b\\_c\\\\d%%", got)
	}
}

// ValidatePasswordStrength 与 seed 规则一致：≥12 字符且含大小写与数字。
func TestValidatePasswordStrength(t *testing.T) {
	for _, ok := range []string{"Abcdef123456", "A1bcdefghijk", "UPPER123lower"} {
		if err := ValidatePasswordStrength(ok); err != nil {
			t.Fatalf("password %q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"Ab1", "abcdefghijkm", "ABCDEFGHIJKM1", "abcdefghijk1"} {
		if err := ValidatePasswordStrength(bad); err == nil {
			t.Fatalf("password %q must be rejected", bad)
		}
	}
}

// ValidateUsername / ValidateEmail / NormalizeEmail 基本规则。
func TestValidateUsernameAndEmail(t *testing.T) {
	for _, ok := range []string{"abc", "user_01", "A-b_9"} {
		if err := ValidateUsername(ok); err != nil {
			t.Fatalf("username %q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"ab", strings.Repeat("a", 33), "用户名", "sp ace", "abc@x"} {
		if err := ValidateUsername(bad); err == nil {
			t.Fatalf("username %q must be rejected", bad)
		}
	}
	if err := ValidateEmail("a@b.com"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "a@b@c", "a@", "@b", "nodot"} {
		if err := ValidateEmail(bad); err == nil {
			t.Fatalf("email %q must be rejected", bad)
		}
	}
	if got := NormalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Fatalf("NormalizeEmail = %q, want alice@example.com", got)
	}
}

// ChangePassword：旧密码错误不改哈希；成功后哈希更新且除当前会话外全部撤销。
func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	store := newFakeStore()
	hash, err := HashPassword("OldPassword123")
	if err != nil {
		t.Fatal(err)
	}
	user := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: hash, Status: StatusActive}
	creds := newFakeCredentials(user)
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	service.SetCredentials(creds)
	service.SetPasswordResetStore(newFakeResetStore())

	current, err := service.NewSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.NewSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 旧密码错误：拒绝且不撤销任何会话。
	if err := service.ChangePassword(user.ID, current, "WrongPassword123", "NewPassword123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong old password error = %v", err)
	}
	if s := store.sessions[HashRefreshToken(other)]; s.RevokedAt != nil {
		t.Fatal("sessions must stay alive when old password is wrong")
	}
	// 新密码强度不足：拒绝。
	if err := service.ChangePassword(user.ID, current, "OldPassword123", "short"); err == nil {
		t.Fatal("weak new password must be rejected")
	}

	if err := service.ChangePassword(user.ID, current, "OldPassword123", "NewPassword123"); err != nil {
		t.Fatal(err)
	}
	if s := store.sessions[HashRefreshToken(other)]; s.RevokedAt == nil {
		t.Fatal("other sessions must be revoked after password change")
	}
	if s := store.sessions[HashRefreshToken(current)]; s.RevokedAt != nil {
		t.Fatal("current session must survive password change")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(creds.hashes[user.ID]), []byte("NewPassword123")); err != nil {
		t.Fatal("password hash must be updated")
	}
	// 旧会话（被撤销的 other）不能再轮换。
	if _, _, err := service.RotateRefreshToken(other); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("revoked session rotate error = %v", err)
	}
}

// RequestPasswordReset：活跃用户获得 30 分钟令牌；未知邮箱返回 ErrUserNotFound。
func TestRequestPasswordReset(t *testing.T) {
	hash, _ := HashPassword("SomePassword123")
	user := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: hash, Status: StatusActive}
	service := NewService(newFakeStore(), "a sufficiently long test secret", time.Minute, time.Hour)
	service.SetCredentials(newFakeCredentials(user))
	resetStore := newFakeResetStore()
	service.SetPasswordResetStore(resetStore)

	if _, err := service.RequestPasswordReset("nobody@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown email error = %v, want ErrUserNotFound", err)
	}
	// 大小写归一：仍能匹配到用户。
	token, err := service.RequestPasswordReset("Alice@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("plaintext token must be returned once")
	}
	saved, ok := resetStore.tokens[hashToken(token)]
	if !ok {
		t.Fatal("token hash must be persisted")
	}
	if saved.UserID != user.ID {
		t.Fatalf("token owner = %v, want %v", saved.UserID, user.ID)
	}
	if !saved.ExpiresAt.After(saved.CreatedAt.Add(29 * time.Minute)) {
		t.Fatalf("token TTL = %v, want 30 minutes", saved.ExpiresAt.Sub(saved.CreatedAt))
	}
}

// ResetPassword：一次性消费（二次使用 ErrResetTokenInvalid）、更新哈希并撤销全部会话。
func TestResetPasswordConsumesOnceAndRevokesSessions(t *testing.T) {
	hash, _ := HashPassword("SomePassword123")
	user := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: hash, Status: StatusActive}
	store := newFakeStore()
	creds := newFakeCredentials(user)
	service := NewService(store, "a sufficiently long test secret", time.Minute, time.Hour)
	service.SetCredentials(creds)
	service.SetPasswordResetStore(newFakeResetStore())

	if _, err := service.NewSession(user.ID); err != nil {
		t.Fatal(err)
	}
	token, err := service.RequestPasswordReset(user.Email)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ResetPassword(token, "weak"); err == nil {
		t.Fatal("weak password must be rejected before consuming token")
	}
	if _, err := service.ResetPassword("definitely-unknown-token", "NewPassword123"); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("unknown token error = %v", err)
	}

	uid, err := service.ResetPassword(token, "NewPassword123")
	if err != nil {
		t.Fatal(err)
	}
	if uid != user.ID {
		t.Fatalf("reset user = %v, want %v", uid, user.ID)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(creds.hashes[user.ID]), []byte("NewPassword123")); err != nil {
		t.Fatal("password hash must be updated")
	}
	for h, s := range store.sessions {
		if s.UserID == user.ID && s.RevokedAt == nil {
			t.Fatalf("session via hash %.8s must be revoked after reset", h)
		}
	}
	// 令牌一次性：二次使用被拒绝。
	if _, err := service.ResetPassword(token, "AnotherPassword123"); !errors.Is(err, ErrResetTokenInvalid) {
		t.Fatalf("token reuse error = %v, want ErrResetTokenInvalid", err)
	}
}
