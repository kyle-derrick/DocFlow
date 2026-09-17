package contenturl

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newTestSigner(secret string, ttl time.Duration) *Signer {
	s := NewSigner(secret, ttl)
	s.now = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newTestSigner("secret-0123456789abcdef0123456789abcdef", DefaultTTL)
	token, err := s.Sign(Claims{Purpose: PurposeRaw, UserID: "u-1", NSType: "personal", NSScope: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(token, ".") != 1 {
		t.Fatalf("token must be 2 dot-separated segments, got %q", token)
	}
	c, err := s.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if c.Purpose != PurposeRaw || c.UserID != "u-1" || c.NSType != "personal" || c.NSScope != "s-1" || c.Nonce == "" || c.Exp == 0 {
		t.Fatalf("claims = %+v", c)
	}
	if got := s.ExpiresAt(); !got.Equal(time.Date(2026, 1, 1, 12, 10, 0, 0, time.UTC)) {
		t.Fatalf("ExpiresAt = %v, want +10m", got)
	}
	// VerifyPurpose 匹配用途。
	if _, err := s.VerifyPurpose(token, PurposeRaw); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyExpired(t *testing.T) {
	s := newTestSigner("secret-0123456789abcdef0123456789abcdef", time.Minute)
	token, err := s.Sign(Claims{Purpose: PurposeRaw, UserID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(token); err != nil {
		t.Fatalf("within ttl must pass: %v", err)
	}
	// 推进时钟到过期后（exp 边界含等号：达到 exp 即失效）。
	s.now = func() time.Time { return time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC) }
	if _, err := s.Verify(token); err == nil {
		t.Fatal("expired token must be rejected")
	}
}

func TestPurposeIsolation(t *testing.T) {
	s := newTestSigner("secret-0123456789abcdef0123456789abcdef", DefaultTTL)
	raw, _ := s.Sign(Claims{Purpose: PurposeRaw, UserID: "u-1", NSType: "personal", NSScope: "s"})
	share, _ := s.Sign(Claims{Purpose: PurposeRawShare, ShareID: "sh-1"})
	if _, err := s.VerifyPurpose(raw, PurposeRawShare); err == nil {
		t.Fatal("raw token must not verify as raw-share")
	}
	if _, err := s.VerifyPurpose(share, PurposeRaw); err == nil {
		t.Fatal("raw-share token must not verify as raw")
	}
	// 空 purpose 拒绝签发；篡改 purpose 的 payload 签名不符。
	if _, err := s.Sign(Claims{}); err == nil {
		t.Fatal("empty purpose must be rejected at signing")
	}
}

func TestVerifyTamperedAndWrongSecret(t *testing.T) {
	s1 := newTestSigner("secret-0123456789abcdef0123456789abcdef", DefaultTTL)
	token, _ := s1.Sign(Claims{Purpose: PurposeRaw, UserID: "u-1"})
	// 换密钥（如 JWT_SECRET 轮换前的派生差异）：签名不匹配。
	s2 := newTestSigner("another-0123456789abcdef0123456789ab", DefaultTTL)
	if _, err := s2.Verify(token); err == nil {
		t.Fatal("token signed with different key must be rejected")
	}
	// 篡改 payload（解码改 JSON 再编码）/ 签名。
	parts := strings.Split(token, ".")
	if _, err := s1.Verify(parts[0] + "." + parts[0]); err == nil {
		t.Fatal("tampered signature must be rejected")
	}
	rawPayload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	forged := base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(rawPayload), "u-1", "u-2")))
	if _, err := s1.Verify(forged + "." + parts[1]); err == nil {
		t.Fatal("tampered payload must be rejected")
	}
}

// TestJWTTokensRejected 验证与 JWT access token / onlyoffice token 的严格
// 区分：3 段 JWT（无论同密钥与否）与任意垃圾串一律拒绝；反向地，本包
// token 也不是合法 JWT（签名段不是 base64 的 JWT 结构可解析）。
func TestJWTTokensRejected(t *testing.T) {
	s := newTestSigner("jwt-secret-0123456789abcdef0123456789", DefaultTTL)
	// 用与派生密钥同源的 secret 签一个 JWT（模拟拿 access token 当 grant）。
	jwtToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "u-1", "purpose": PurposeRaw, "exp": s.now().Add(time.Hour).Unix(),
	}).SignedString([]byte("jwt-secret-0123456789abcdef0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(jwtToken); err == nil {
		t.Fatal("JWT access token (3 segments) must be rejected")
	}
	for _, bad := range []string{"", "a", "a.b.c", "...", "e30.a", "!! ?.##"} {
		if _, err := s.Verify(bad); err == nil {
			t.Fatalf("Verify(%q) must be rejected", bad)
		}
	}
	// 本包 token 用作 JWT 解析同样失败（格式/密钥域不同）。
	ours, _ := s.Sign(Claims{Purpose: PurposeRaw, UserID: "u-1"})
	if _, err := s.Verify(strings.Split(ours, ".")[0] + "." + strings.Split(ours, ".")[1] + ".x"); err == nil {
		t.Fatal("3-segment variant must be rejected")
	}
}
