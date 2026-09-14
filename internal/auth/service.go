package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidRefreshToken = errors.New("invalid refresh token")

// ErrInvalidCredentials 表示旧密码校验失败（改密）。
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrResetTokenInvalid 表示重置令牌不存在、已使用或已过期。
var ErrResetTokenInvalid = errors.New("invalid or expired reset token")

// ErrNotConfigured 表示依赖（用户凭据源/重置令牌存储）未注入。
var ErrNotConfigured = errors.New("service dependency is not configured")

// PasswordResetTokenTTL 为密码重置令牌有效期（30 分钟）。
const PasswordResetTokenTTL = 30 * time.Minute

// PAT（个人访问令牌）格式与约束：
//   - 明文 = "dfpat_" + 32 字节随机数的 URL-safe base64（6 + 43 字符）；
//   - token_hash = 明文 SHA-256 十六进制小写（64 字符，只存哈希）；
//   - prefix = 明文前 14 字符（dfpat_ + 随机体前 8 字符），认证按其索引定位；
//   - 名称 1-100 rune；有效期 0（永久）或 1-3650 天。
const (
	PATPrefix       = "dfpat_"
	patPrefixLen    = len(PATPrefix) + 8
	PATBodyLen      = 43
	PATNameMax      = 100
	PATMaxExpiryDay = 3650
	// PATRetention 为过期/撤销令牌行的清理保留期（janitor 与 store 共用）。
	PATRetention = 30 * 24 * time.Hour
)

// ErrInvalidAccessToken 表示 PAT 无效（未知/撤销/过期/哈希不匹配/属主非
// active），认证中间件统一映射 401。
var ErrInvalidAccessToken = errors.New("invalid access token")

// ErrInvalidTokenName / ErrInvalidTokenExpiry 为 PAT 创建参数校验错误。
var (
	ErrInvalidTokenName   = errors.New("token name must be 1-100 characters")
	ErrInvalidTokenExpiry = errors.New("token expiry must be 0 (never) or 1-3650 days")
)

// SessionInfo 为创建会话时记录的请求环境（sessions.ip/user_agent，审计用途）。
type SessionInfo struct {
	IP        string
	UserAgent string
}

// PATLookup 为按前缀定位到的活跃令牌最小信息（认证路径专用）。
type PATLookup struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash string
}

type SessionStore interface {
	Create(session Session, info SessionInfo) error
	Rotate(tokenHash, replacementHash string, now time.Time, expiresAt time.Time) (Session, error)
	Revoke(tokenHash string, now time.Time) error
	// RevokeAllForUser 撤销 user 的全部未撤销会话；exceptHash 非空时保留
	// 该 refresh token hash 对应的会话（改密场景保留当前会话）。
	RevokeAllForUser(userID uuid.UUID, exceptHash string, now time.Time) error
	// ListActive 返回 user 的全部活跃会话（未撤销未过期，last_active_at 倒序）。
	ListActive(userID uuid.UUID, now time.Time) ([]SessionView, error)
	// RevokeByID 撤销属主的指定会话；不存在/非属主/已撤销返回 false。
	RevokeByID(owner, id uuid.UUID, now time.Time) (bool, error)
}

// TokenStore 抽象个人访问令牌（PAT）的持久化（生产实现为
// *GormAPITokenStore；测试可用内存实现）。
type TokenStore interface {
	Create(token APIToken) error
	// List 返回 owner 的未撤销令牌（含已过期，created_at 倒序）。
	List(owner uuid.UUID) ([]APIToken, error)
	// Revoke 撤销属主令牌（仅未撤销时生效），返回是否生效。
	Revoke(owner, id uuid.UUID, now time.Time) (bool, error)
	// FindActiveByPrefix 按 prefix 定位未撤销且未过期的令牌，返回认证所需
	// 最小字段（id/user_id/token_hash）。
	FindActiveByPrefix(prefix string, now time.Time) (PATLookup, bool, error)
	// TouchLastUsed 更新 last_used_at（UpdateColumn 单列，best-effort）。
	TouchLastUsed(id uuid.UUID)
	// DeleteExpired 删除 expires_at 或 revoked_at 早于 now-30d 的行。
	DeleteExpired(now time.Time) (int64, error)
}

// PasswordResetStore 抽象一次性重置令牌的持久化（生产实现为
// *GormPasswordResetStore；Consume 的条件更新保证一次性语义）。
type PasswordResetStore interface {
	Create(token PasswordResetToken) error
	// Consume 原子标记 used_at（仅未使用且未过期时成功），返回令牌属主与
	// 是否消费成功；并发/重复使用返回 false。
	Consume(tokenHash string, now time.Time) (uuid.UUID, bool, error)
}

// Credentials 抽象改密与密码重置所需的用户凭据读写
// （生产实现为 *UserStore）。
type Credentials interface {
	FindActiveByEmail(email string) (User, error)
	GetByID(id uuid.UUID) (User, error)
	UpdatePasswordHash(id uuid.UUID, passwordHash string) error
}

type Service struct {
	store           SessionStore
	jwtSecret       []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
	now             func() time.Time
	// creds/resets/tokens 为可选依赖（Set* 注入）：改密、密码重置与 PAT
	// 链路使用，未注入时对应方法返回 ErrNotConfigured（会话/令牌签发不受影响）。
	creds  Credentials
	resets PasswordResetStore
	tokens TokenStore
}

func NewService(store SessionStore, jwtSecret string, accessTokenTTL, refreshTokenTTL time.Duration) *Service {
	return &Service{store: store, jwtSecret: []byte(jwtSecret), accessTokenTTL: accessTokenTTL, refreshTokenTTL: refreshTokenTTL, now: time.Now}
}

// SetCredentials 注入用户凭据读写源（幂等；nil 不覆盖）。
func (s *Service) SetCredentials(c Credentials) {
	if c != nil {
		s.creds = c
	}
}

// SetPasswordResetStore 注入重置令牌存储（幂等；nil 不覆盖）。
func (s *Service) SetPasswordResetStore(store PasswordResetStore) {
	if store != nil {
		s.resets = store
	}
}

// SetTokenStore 注入个人访问令牌存储（幂等；nil 不覆盖）。
func (s *Service) SetTokenStore(store TokenStore) {
	if store != nil {
		s.tokens = store
	}
}

func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost+2)
	return string(hash), err
}

func (s *Service) VerifyPassword(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

func (s *Service) NewSession(userID uuid.UUID) (string, error) {
	return s.NewSessionWithInfo(userID, SessionInfo{})
}

// NewSessionWithInfo 创建刷新会话并记录请求环境（ip/user_agent，审计用途）。
func (s *Service) NewSessionWithInfo(userID uuid.UUID, info SessionInfo) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	return token, s.store.Create(Session{ID: uuid.New(), UserID: userID, RefreshTokenHash: HashRefreshToken(token), CreatedAt: now, LastActiveAt: now, ExpiresAt: now.Add(s.refreshTokenTTL)}, info)
}

// ListSessions 列出 user 的全部活跃会话（未撤销未过期，last_active_at 倒序）。
func (s *Service) ListSessions(userID uuid.UUID) ([]SessionView, error) {
	return s.store.ListActive(userID, s.now().UTC())
}

// RevokeSessionByID 撤销属主自己的指定会话；不存在、非属主或已撤销返回 false。
func (s *Service) RevokeSessionByID(owner, id uuid.UUID) (bool, error) {
	return s.store.RevokeByID(owner, id, s.now().UTC())
}

// RevokeAllSessions 撤销 user 的全部会话（含当前会话；调用方负责让前端
// 登出跳转——服务端无法从 Bearer 凭证反推「当前」session）。
func (s *Service) RevokeAllSessions(userID uuid.UUID) error {
	return s.store.RevokeAllForUser(userID, "", s.now().UTC())
}

// NewPersonalAccessToken 创建 PAT 并返回一次性明文（dfpat_ + 43 字符
// base64url）。name 去首尾空白后限 1-100 rune；expiresInDays 为 0 表示
// 永久，否则 1-3650 天。
func (s *Service) NewPersonalAccessToken(userID uuid.UUID, name string, expiresInDays int) (APIToken, string, error) {
	if s.tokens == nil {
		return APIToken{}, "", ErrNotConfigured
	}
	trimmed := strings.TrimSpace(name)
	if n := len([]rune(trimmed)); n < 1 || n > PATNameMax {
		return APIToken{}, "", ErrInvalidTokenName
	}
	if expiresInDays < 0 || expiresInDays > PATMaxExpiryDay {
		return APIToken{}, "", ErrInvalidTokenExpiry
	}
	body, err := randomToken()
	if err != nil {
		return APIToken{}, "", err
	}
	plaintext := PATPrefix + body
	now := s.now().UTC()
	token := APIToken{
		ID:        uuid.New(),
		UserID:    userID,
		Name:      trimmed,
		TokenHash: hashToken(plaintext),
		Prefix:    plaintext[:patPrefixLen],
		CreatedAt: now,
	}
	if expiresInDays > 0 {
		expires := now.AddDate(0, 0, expiresInDays)
		token.ExpiresAt = &expires
	}
	if err := s.tokens.Create(token); err != nil {
		return APIToken{}, "", err
	}
	return token, plaintext, nil
}

// ListPersonalAccessTokens 返回 owner 的未撤销令牌（含已过期）。
func (s *Service) ListPersonalAccessTokens(owner uuid.UUID) ([]APIToken, error) {
	if s.tokens == nil {
		return nil, ErrNotConfigured
	}
	return s.tokens.List(owner)
}

// RevokePersonalAccessToken 撤销属主令牌；不存在、非属主或已撤销返回 false。
func (s *Service) RevokePersonalAccessToken(owner, id uuid.UUID) (bool, error) {
	if s.tokens == nil {
		return false, ErrNotConfigured
	}
	return s.tokens.Revoke(owner, id, s.now().UTC())
}

// VerifyPersonalAccessToken 校验 PAT：prefix 定位候选行 → 未过期未撤销
// （store 侧条件过滤）→ 全量 SHA-256 常量时间比对 → 属主账号仍为 active。
// 通过则 best-effort 触达 TouchLastUsed 并返回属主 user id；任何失败均
// 返回 ErrInvalidAccessToken（fail closed，不区分原因以防探测）。
func (s *Service) VerifyPersonalAccessToken(token string) (uuid.UUID, error) {
	if s.tokens == nil || s.creds == nil || len(token) <= patPrefixLen || !strings.HasPrefix(token, PATPrefix) {
		return uuid.Nil, ErrInvalidAccessToken
	}
	lookup, found, err := s.tokens.FindActiveByPrefix(token[:patPrefixLen], s.now().UTC())
	if err != nil || !found {
		return uuid.Nil, ErrInvalidAccessToken
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(lookup.TokenHash)) != 1 {
		return uuid.Nil, ErrInvalidAccessToken
	}
	user, err := s.creds.GetByID(lookup.UserID)
	if err != nil || user.Status != StatusActive {
		return uuid.Nil, ErrInvalidAccessToken
	}
	s.tokens.TouchLastUsed(lookup.ID)
	return lookup.UserID, nil
}

func (s *Service) RotateRefreshToken(token string) (Session, string, error) {
	replacement, err := randomToken()
	if err != nil {
		return Session{}, "", err
	}
	now := s.now().UTC()
	session, err := s.store.Rotate(HashRefreshToken(token), HashRefreshToken(replacement), now, now.Add(s.refreshTokenTTL))
	if err != nil {
		return Session{}, "", ErrInvalidRefreshToken
	}
	return session, replacement, nil
}

func (s *Service) RevokeRefreshToken(token string) error {
	if err := s.store.Revoke(HashRefreshToken(token), s.now().UTC()); err != nil {
		return ErrInvalidRefreshToken
	}
	return nil
}

func (s *Service) AccessToken(userID uuid.UUID) (string, error) {
	now := s.now().UTC()
	claims := jwt.RegisteredClaims{Subject: userID.String(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.accessTokenTTL))}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

// hashToken 返回 token 的 SHA-256 十六进制小写哈希（64 字符），
// 与 invitations/password_reset_tokens 的 token_hash CHAR(64) 一致
// （share 包同款模式）。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ChangePassword 校验旧密码与新密码强度后更新哈希，并撤销该用户全部会话
// （保留 currentRefreshToken 对应的当前会话；空串表示全部撤销）。
// 任何校验失败都不产生变更；成功后由调用方轮换当前会话
// （RotateRefreshToken）下发新 refresh cookie。
func (s *Service) ChangePassword(userID uuid.UUID, currentRefreshToken, oldPassword, newPassword string) error {
	if s.creds == nil {
		return ErrNotConfigured
	}
	user, err := s.creds.GetByID(userID)
	if err != nil {
		return err
	}
	if s.VerifyPassword(user.PasswordHash, oldPassword) != nil {
		return ErrInvalidCredentials
	}
	if err := ValidatePasswordStrength(newPassword); err != nil {
		return err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.creds.UpdatePasswordHash(user.ID, hash); err != nil {
		return err
	}
	exceptHash := ""
	if currentRefreshToken != "" {
		exceptHash = HashRefreshToken(currentRefreshToken)
	}
	return s.store.RevokeAllForUser(user.ID, exceptHash, s.now().UTC())
}

// RequestPasswordReset 为邮箱对应的活跃用户创建 30 分钟有效的一次性重置
// 令牌，返回明文 token（仅此一次可见，由调用方经邮件发送）。
// 用户不存在或非活跃返回 ErrUserNotFound，由 HTTP 层统一回 202 防枚举。
func (s *Service) RequestPasswordReset(email string) (string, error) {
	if s.creds == nil || s.resets == nil {
		return "", ErrNotConfigured
	}
	user, err := s.creds.FindActiveByEmail(NormalizeEmail(email))
	if err != nil {
		return "", ErrUserNotFound
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	if err := s.resets.Create(PasswordResetToken{
		ID:        uuid.New(),
		UserID:    user.ID,
		TokenHash: hashToken(token),
		ExpiresAt: now.Add(PasswordResetTokenTTL),
		CreatedAt: now,
	}); err != nil {
		return "", err
	}
	return token, nil
}

// ResetPassword 消费一次性重置令牌（未使用且未过期，原子标记 used_at），
// 更新密码并撤销该用户全部会话。令牌无效/已用/过期返回 ErrResetTokenInvalid。
func (s *Service) ResetPassword(token, newPassword string) (uuid.UUID, error) {
	if s.creds == nil || s.resets == nil {
		return uuid.Nil, ErrNotConfigured
	}
	if err := ValidatePasswordStrength(newPassword); err != nil {
		return uuid.Nil, err
	}
	now := s.now().UTC()
	userID, consumed, err := s.resets.Consume(hashToken(strings.TrimSpace(token)), now)
	if err != nil {
		return uuid.Nil, err
	}
	if !consumed {
		return uuid.Nil, ErrResetTokenInvalid
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.creds.UpdatePasswordHash(userID, hash); err != nil {
		return uuid.Nil, err
	}
	// 重置后全部会话失效（无「当前会话」概念）。
	if err := s.store.RevokeAllForUser(userID, "", now); err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}
