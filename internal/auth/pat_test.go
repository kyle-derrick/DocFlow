package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeTokenStore 是 TokenStore 的内存实现（语义与 GormAPITokenStore 同步）。
type fakeTokenStore struct {
	tokens  []APIToken
	touched []uuid.UUID
}

func newFakeTokenStore() *fakeTokenStore { return &fakeTokenStore{} }

func (f *fakeTokenStore) Create(t APIToken) error {
	f.tokens = append(f.tokens, t)
	return nil
}

func (f *fakeTokenStore) List(owner uuid.UUID) ([]APIToken, error) {
	out := make([]APIToken, 0)
	for i := len(f.tokens) - 1; i >= 0; i-- { // created_at 倒序（插入序逆序）
		if f.tokens[i].UserID == owner && f.tokens[i].RevokedAt == nil {
			out = append(out, f.tokens[i])
		}
	}
	return out, nil
}

func (f *fakeTokenStore) Revoke(owner, id uuid.UUID, now time.Time) (bool, error) {
	for i := range f.tokens {
		t := &f.tokens[i]
		if t.ID == id && t.UserID == owner && t.RevokedAt == nil {
			t.RevokedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeTokenStore) Update(owner, id uuid.UUID, name *string, scopes *[]string) (APIToken, error) {
	for i := range f.tokens {
		if f.tokens[i].ID == id && f.tokens[i].UserID == owner && f.tokens[i].RevokedAt == nil {
			if name != nil {
				f.tokens[i].Name = *name
			}
			if scopes != nil {
				f.tokens[i].Scopes = *scopes
			}
			return f.tokens[i], nil
		}
	}
	return APIToken{}, nil
}

func (f *fakeTokenStore) FindActiveByPrefix(prefix string, now time.Time) (PATLookup, bool, error) {
	for i := range f.tokens {
		t := &f.tokens[i]
		if t.Prefix == prefix && t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(now)) {
			return PATLookup{ID: t.ID, UserID: t.UserID, TokenHash: t.TokenHash}, true, nil
		}
	}
	return PATLookup{}, false, nil
}

func (f *fakeTokenStore) TouchLastUsed(id uuid.UUID) {
	f.touched = append(f.touched, id)
}

func (f *fakeTokenStore) DeleteExpired(now time.Time) (int64, error) {
	cutoff := now.Add(-PATRetention)
	kept := f.tokens[:0]
	var n int64
	for _, t := range f.tokens {
		expired := t.ExpiresAt != nil && t.ExpiresAt.Before(cutoff)
		revoked := t.RevokedAt != nil && t.RevokedAt.Before(cutoff)
		if expired || revoked {
			n++
			continue
		}
		kept = append(kept, t)
	}
	f.tokens = kept
	return n, nil
}

func (f *fakeTokenStore) byPlaintext(plaintext string) (APIToken, bool) {
	hash := hashToken(plaintext)
	for _, t := range f.tokens {
		if t.TokenHash == hash {
			return t, true
		}
	}
	return APIToken{}, false
}

// patTestService 构造带内存 PAT 存储与给定状态用户的服务。
func patTestService(users ...User) (*Service, *fakeTokenStore, *fakeCredentials) {
	store := newFakeTokenStore()
	creds := newFakeCredentials(users...)
	service := NewService(newFakeStore(), "a sufficiently long test secret", time.Minute, time.Hour)
	service.SetCredentials(creds)
	service.SetTokenStore(store)
	return service, store, creds
}

// 生成格式：dfpat_ + 43 字符 base64url（总长 49）；hash 为 SHA-256 hex（64）；
// prefix 为明文前 14 字符；明文与哈希不同、两次生成不重复。
func TestPersonalAccessTokenFormat(t *testing.T) {
	user := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: StatusActive}
	service, store, _ := patTestService(user)
	token, plaintext, err := service.NewPersonalAccessToken(user.ID, "  my script  ", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "dfpat_") {
		t.Fatalf("plaintext = %q, want dfpat_ prefix", plaintext)
	}
	if got := len(plaintext); got != len(PATPrefix)+PATBodyLen {
		t.Fatalf("plaintext length = %d, want %d", got, len(PATPrefix)+PATBodyLen)
	}
	if got := len(token.TokenHash); got != 64 {
		t.Fatalf("token hash length = %d, want 64", got)
	}
	if token.TokenHash == plaintext {
		t.Fatal("stored hash must differ from plaintext")
	}
	if got := len(token.Prefix); got != patPrefixLen {
		t.Fatalf("prefix length = %d, want %d", got, patPrefixLen)
	}
	if token.Prefix != plaintext[:patPrefixLen] {
		t.Fatalf("prefix = %q, want first %d chars of plaintext", token.Prefix, patPrefixLen)
	}
	if token.Name != "my script" {
		t.Fatalf("name = %q, want trimmed", token.Name)
	}
	if token.ExpiresAt != nil {
		t.Fatal("expires_in_days=0 must mean never (nil ExpiresAt)")
	}
	// 库中保存的是哈希而非明文，且可按明文反查。
	if saved, ok := store.byPlaintext(plaintext); !ok || saved.ID != token.ID || saved.UserID != user.ID {
		t.Fatal("token row must be persisted keyed by hash of plaintext")
	}
	// 随机性：两次生成的明文不同。
	if _, second, err := service.NewPersonalAccessToken(user.ID, "b", 0); err != nil || second == plaintext {
		t.Fatalf("second token must differ, err=%v", err)
	}
}

// 有效期换算：N 天后过期；参数范围校验（负数/超上限拒绝）。
func TestPersonalAccessTokenExpiryAndValidation(t *testing.T) {
	user := User{ID: uuid.New(), Username: "u", Email: "u@example.com", Status: StatusActive}
	service, _, _ := patTestService(user)
	token, _, err := service.NewPersonalAccessToken(user.ID, "t", 30)
	if err != nil {
		t.Fatal(err)
	}
	if token.ExpiresAt == nil {
		t.Fatal("30-day token must have ExpiresAt")
	}
	if d := token.ExpiresAt.Sub(token.CreatedAt); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("expiry delta = %v, want ~30d", d)
	}
	for _, tc := range []struct {
		name string
		in   int
	}{
		{name: "negative", in: -1},
		{name: "too-large", in: PATMaxExpiryDay + 1},
	} {
		if _, _, err := service.NewPersonalAccessToken(user.ID, "t", tc.in); !errors.Is(err, ErrInvalidTokenExpiry) {
			t.Fatalf("%s days error = %v, want ErrInvalidTokenExpiry", tc.name, err)
		}
	}
	for _, name := range []string{"", "   ", strings.Repeat("x", PATNameMax+1), strings.Repeat("汉", PATNameMax+1)} {
		if _, _, err := service.NewPersonalAccessToken(user.ID, name, 0); !errors.Is(err, ErrInvalidTokenName) {
			t.Fatalf("name %q error = %v, want ErrInvalidTokenName", name, err)
		}
	}
	// 未注入 TokenStore：ErrNotConfigured。
	bare := NewService(newFakeStore(), "a sufficiently long test secret", time.Minute, time.Hour)
	if _, _, err := bare.NewPersonalAccessToken(user.ID, "t", 0); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("bare service error = %v, want ErrNotConfigured", err)
	}
}

// 认证矩阵（服务层）：有效通过且触达 TouchLastUsed；未知前缀、同前缀哈希
// 不匹配、过期、撤销、属主禁用、未配置依赖均拒绝。
func TestVerifyPersonalAccessTokenMatrix(t *testing.T) {
	user := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: "x", Status: StatusActive}
	service, store, creds := patTestService(user)
	plaintextToken, plaintext, err := service.NewPersonalAccessToken(user.ID, "t", 0)
	if err != nil {
		t.Fatal(err)
	}

	// 有效：返回属主并触达 last_used。
	uid, err := service.VerifyPersonalAccessToken(plaintext)
	if err != nil || uid != user.ID {
		t.Fatalf("valid token err=%v uid=%v, want %v", err, uid, user.ID)
	}
	if len(store.touched) != 1 || store.touched[0] != plaintextToken.ID {
		t.Fatalf("touched = %v, want [%s]", store.touched, plaintextToken.ID)
	}

	// 未知前缀。
	if _, err := service.VerifyPersonalAccessToken("dfpat_" + strings.Repeat("A", 43)); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("unknown prefix error = %v", err)
	}
	// 同前缀、哈希不匹配（篡改明文尾部）。
	tampered := plaintext[:len(plaintext)-1] + "0"
	if tampered == plaintext {
		tampered = plaintext[:len(plaintext)-1] + "1"
	}
	if _, err := service.VerifyPersonalAccessToken(tampered); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("tampered error = %v", err)
	}
	// 形态非法：过短 / 无前缀。
	if _, err := service.VerifyPersonalAccessToken("dfpat_short"); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("short token error = %v", err)
	}
	if _, err := service.VerifyPersonalAccessToken(strings.Repeat("A", 49)); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("non-pat token error = %v", err)
	}

	// 过期：改写行 expires_at 为过去。
	past := time.Now().UTC().Add(-time.Hour)
	for i := range store.tokens {
		if store.tokens[i].ID == plaintextToken.ID {
			store.tokens[i].ExpiresAt = &past
		}
	}
	if _, err := service.VerifyPersonalAccessToken(plaintext); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("expired token error = %v", err)
	}
	// 恢复为永久并撤销。
	for i := range store.tokens {
		if store.tokens[i].ID == plaintextToken.ID {
			store.tokens[i].ExpiresAt = nil
		}
	}
	if _, err := service.RevokePersonalAccessToken(user.ID, plaintextToken.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyPersonalAccessToken(plaintext); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("revoked token error = %v", err)
	}

	// 属主禁用：新建令牌后把用户置为 disabled。
	disabled := user
	disabled.Status = StatusDisabled
	creds.byID[user.ID] = disabled
	creds.users[disabled.Email] = disabled
	_, second, err := service.NewPersonalAccessToken(user.ID, "t2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.VerifyPersonalAccessToken(second); !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("disabled owner error = %v", err)
	}
}

// 撤销与列表语义：撤销仅属主生效；列表只含未撤销令牌。
func TestPersonalAccessTokenRevokeAndList(t *testing.T) {
	alice := User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: StatusActive}
	bob := User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", Status: StatusActive}
	service, store, _ := patTestService(alice, bob)
	aliceToken, _, err := service.NewPersonalAccessToken(alice.ID, "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.NewPersonalAccessToken(bob.ID, "b", 0)
	if err != nil {
		t.Fatal(err)
	}

	// 非属主撤销不生效。
	ok, err := service.RevokePersonalAccessToken(bob.ID, aliceToken.ID)
	if err != nil || ok {
		t.Fatalf("non-owner revoke = %v %v, want false nil", ok, err)
	}
	// 属主撤销生效；二次撤销（已撤销）返回 false。
	if ok, err := service.RevokePersonalAccessToken(alice.ID, aliceToken.ID); err != nil || !ok {
		t.Fatalf("owner revoke = %v %v, want true nil", ok, err)
	}
	if ok, _ := service.RevokePersonalAccessToken(alice.ID, aliceToken.ID); ok {
		t.Fatal("double revoke must be false")
	}
	// 列表只含未撤销令牌（bob 的一条）。
	list, err := service.ListPersonalAccessTokens(alice.ID)
	if err == nil && len(list) != 0 {
		t.Fatalf("alice list = %v, want empty", list)
	}
	list, err = service.ListPersonalAccessTokens(bob.ID)
	if err != nil || len(list) != 1 || list[0].UserID != bob.ID {
		t.Fatalf("bob list = %v err=%v, want 1 token", list, err)
	}
	_ = store
}
