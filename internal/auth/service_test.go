package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore 模拟 GormSessionStore 的事务语义：map 中保留同一 session 的
// 历史 hash 槽位，使旧 hash 重放可定位到 session 并执行撤销。
type fakeStore struct {
	sessions map[string]Session
}

func newFakeStore() *fakeStore { return &fakeStore{sessions: make(map[string]Session)} }
func (f *fakeStore) Create(session Session) error {
	f.sessions[session.RefreshTokenHash] = session
	return nil
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
