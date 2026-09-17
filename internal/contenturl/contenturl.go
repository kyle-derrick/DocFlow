// Package contenturl 实现受控原始内容 URL（/raw/*）的短期授权：
// HMAC-SHA256 签名的紧凑 token（base64url(claims).base64url(mac)）。
//
// 安全基线：
//   - 密钥经 HKDF-SHA256 从 secret 派生（info 域分离标签 docflow-raw-url-v1），
//     未配置 RAW_URL_SECRET 时以 JWT_SECRET 派生——与 JWT access token、
//     ONLYOFFICE token 的密钥材料/格式/purpose 互不可混用（见测试）；
//   - claims 携带 purpose（raw=用户命名空间、raw-share=公开目录分享）、
//     绑定主体（uid / shid）、命名空间（ns/scope）与 exp+nonce；
//   - 格式恒为 2 段点分（JWT 为 3 段），Verify 拒绝任何其他格式，
//     HMAC 常数时间比较，过期即时失效；
//   - TTL 默认 10 分钟（DefaultTTL），授权只证明“可发起一次解析”，
//     每请求仍实时做读授权与对象可用性校验。
package contenturl

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// purpose 取值：raw 为用户命名空间原始内容（/raw/auth/...）；
// raw-share 为公开目录分享子资源（/raw/share/...，shid 绑定分享 ID）。
const (
	PurposeRaw      = "raw"
	PurposeRawShare = "raw-share"
)

// DefaultTTL 为签发授权的默认有效期（10 分钟）。
const DefaultTTL = 10 * time.Minute

// ErrInvalidToken 表示 token 缺失、格式不符、签名不匹配、已过期或用途不符。
var ErrInvalidToken = errors.New("invalid raw url token")

// Claims 为紧凑 JSON 载荷（字段名刻意短小，控制 URL 长度）。
type Claims struct {
	// Purpose 用途声明（必填）：raw / raw-share。
	Purpose string `json:"purpose"`
	// UserID 授权主体用户（purpose=raw 必填）。
	UserID string `json:"uid,omitempty"`
	// ShareID 绑定的公开分享 ID（purpose=raw-share 必填）。
	ShareID string `json:"shid,omitempty"`
	// NSType / NSScope 命名空间（purpose=raw 必填，personal|team + scope uuid）。
	NSType  string `json:"ns,omitempty"`
	NSScope string `json:"scope,omitempty"`
	// Nonce 随机数（签发时自动生成，保证 token 不可预测、不可复用推断）。
	Nonce string `json:"nonce"`
	// Exp 过期时间（Unix 秒；签发时未填则按 TTL 计算）。
	Exp int64 `json:"exp"`
}

// Signer 签发与校验授权 token。
type Signer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewSigner 以 secret 派生签名密钥并构造签发器；ttl<=0 时取 DefaultTTL。
func NewSigner(secret string, ttl time.Duration) *Signer {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	key := make([]byte, 32)
	reader := hkdf.New(sha256.New, []byte(secret), nil, []byte("docflow-raw-url-v1"))
	if _, err := io.ReadFull(reader, key); err != nil {
		panic("contenturl: derive key: " + err.Error())
	}
	return &Signer{key: key, ttl: ttl, now: time.Now}
}

// TTL 返回签发授权的有效期。
func (s *Signer) TTL() time.Duration { return s.ttl }

// ExpiresAt 返回以当前时钟签发的授权的过期时刻（供响应 grant_expires_at）。
func (s *Signer) ExpiresAt() time.Time { return s.now().Add(s.ttl).UTC() }

// Sign 签发 token：补全 nonce 与 exp 后 JSON 序列化并计算 HMAC。
func (s *Signer) Sign(c Claims) (string, error) {
	if c.Purpose == "" {
		return "", ErrInvalidToken
	}
	if c.Nonce == "" {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		c.Nonce = base64.RawURLEncoding.EncodeToString(buf)
	}
	if c.Exp == 0 {
		c.Exp = s.now().Add(s.ttl).Unix()
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify 校验 token 的格式、签名与有效期，返回 claims。
// 格式恒为 2 段（base64url claims + base64url HMAC）：JWT（3 段）等其他
// token 一律 ErrInvalidToken；purpose 为空同样拒绝。
func (s *Signer) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return Claims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil || c.Purpose == "" {
		return Claims{}, ErrInvalidToken
	}
	if s.now().Unix() >= c.Exp {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

// VerifyPurpose 在 Verify 基础上要求用途匹配：raw 与 raw-share 的 token
// 不可互换使用（签发侧 purpose 不同的 token 在此被拒）。
func (s *Signer) VerifyPurpose(token, purpose string) (Claims, error) {
	c, err := s.Verify(token)
	if err != nil {
		return Claims{}, err
	}
	if c.Purpose != purpose {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}
