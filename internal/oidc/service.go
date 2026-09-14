package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/auth"
	"github.com/google/uuid"
)

// 用户映射哨兵错误（HTTP 层据此映射 401/403/500）。
var (
	// ErrInvalidState 表示 state 未知、已消费或已过期（防重放/CSRF）。
	ErrInvalidState = errors.New("invalid or expired oidc state")
	// ErrExchangeFailed 表示授权码换令牌失败（含 IdP 不可达/5xx）。
	ErrExchangeFailed = errors.New("oidc token exchange failed")
	// ErrNoEmail 表示 IdP 身份断言未携带 email（scope 未含 email 或 IdP
	// 未返回），无法与既有账号匹配，也无法开户。
	ErrNoEmail = errors.New("oidc identity has no email claim")
	// ErrUserDisabled 表示关联/匹配到的账号非 active（禁用/锁定/删除）。
	ErrUserDisabled = errors.New("oidc account is not active")
	// ErrNotProvisioned 表示无关联且无 email 匹配，且自动开户关闭
	//（OIDC_AUTO_PROVISION=false）。
	ErrNotProvisioned = errors.New("no matching account; contact an administrator to be invited")
)

// Directory 抽象用户目录查询与开户（生产实现 *auth.UserStore）。
type Directory interface {
	FindActiveByEmail(email string) (auth.User, error)
	GetByID(id uuid.UUID) (auth.User, error)
	UsernameExists(username string) (bool, error)
	CreateUser(u auth.User) error
}

var _ Directory = (*auth.UserStore)(nil)

// Service 编排 SSO 登录：state 签发（/oidc/login）与回调消费
// （/oidc/callback）——换取令牌、解析身份、映射/开户用户并落 sub 关联。
// 安全取舍：SSO 信任 IdP 的认证强度，跳过本地 TOTP 二验（密码登录路径
// 的 TOTP 拦截不变；详见设计文档与 openapi 描述）。
type Service struct {
	client        *Client
	links         LinkStore
	directory     Directory
	autoProvision bool
	states        *StateStore
	audit         audit.Recorder
	now           func() time.Time
}

func NewService(client *Client, links LinkStore, directory Directory, autoProvision bool) *Service {
	return &Service{
		client:        client,
		links:         links,
		directory:     directory,
		autoProvision: autoProvision,
		states:        NewStateStore(),
		audit:         audit.NopRecorder{},
		now:           time.Now,
	}
}

// SetAuditRecorder 注入审计写入器（幂等；nil 保持 Nop）。
func (s *Service) SetAuditRecorder(recorder audit.Recorder) {
	if recorder != nil {
		s.audit = recorder
	}
}

// SetStateStore 覆盖 state 存储（幂等；测试注入可注入可控时钟的实现）。
func (s *Service) SetStateStore(store *StateStore) {
	if store != nil {
		s.states = store
	}
}

// SetNow 覆盖时钟（测试用；nil 不覆盖）。
func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// BeginLogin 签发 state（内存登记 PKCE verifier，TTL 10 分钟）并返回
// 跳转 IdP 的授权地址。HTTP 层同时把 state 写入 HttpOnly cookie 双验证。
func (s *Service) BeginLogin() (state, authorizeURL string, err error) {
	state, verifier, err := s.states.Issue()
	if err != nil {
		return "", "", err
	}
	return state, s.client.AuthorizeURL(state, ChallengeS256(verifier)), nil
}

// CompleteLogin 消费 state（用后即焚）→ 换取令牌 → 解析身份 →
// 映射/开户用户。返回 DocFlow 用户 ID。
func (s *Service) CompleteLogin(ctx context.Context, state, code string) (uuid.UUID, error) {
	verifier, ok := s.states.Consume(state)
	if !ok {
		return uuid.Nil, ErrInvalidState
	}
	tokens, err := s.client.Exchange(ctx, code, verifier)
	if err != nil {
		return uuid.Nil, ErrExchangeFailed
	}
	claims, err := s.client.ResolveClaims(ctx, tokens)
	if err != nil {
		return uuid.Nil, err
	}
	return s.resolveUser(claims)
}

// resolveUser 身份 → 用户映射：
//  1. sub 已关联（oidc_links）→ 复核账号 active（email 变更不影响登录）；
//  2. email 匹配 active 既有用户 → 关联 sub；
//  3. 无匹配：autoProvision 开启则自动开户（role=user、随机不可登录密码、
//     审计 oidc.provision），关闭则 ErrNotProvisioned。
func (s *Service) resolveUser(claims Claims) (uuid.UUID, error) {
	if claims.Sub == "" {
		return uuid.Nil, ErrExchangeFailed
	}
	if link, found, err := s.links.FindBySub(claims.Sub); err != nil {
		return uuid.Nil, err
	} else if found {
		user, err := s.directory.GetByID(link.UserID)
		if err != nil {
			return uuid.Nil, ErrUserDisabled
		}
		if user.Status != auth.StatusActive {
			return uuid.Nil, ErrUserDisabled
		}
		return user.ID, nil
	}
	if claims.Email == "" {
		return uuid.Nil, ErrNoEmail
	}
	if user, err := s.directory.FindActiveByEmail(claims.Email); err == nil {
		if err := s.links.Upsert(Link{Sub: claims.Sub, UserID: user.ID, Issuer: s.client.Issuer(), LinkedAt: s.now().UTC()}); err != nil {
			return uuid.Nil, err
		}
		return user.ID, nil
	}
	if !s.autoProvision {
		return uuid.Nil, ErrNotProvisioned
	}
	user, err := s.provision(claims)
	if err != nil {
		return uuid.Nil, err
	}
	return user.ID, nil
}

// provision 自动开户：username 取 preferred_username（校验/归一失败回退
// email 前缀），冲突时追加 -N 后缀去重；密码为随机 32 字节串的 bcrypt
// 哈希（无人知晓明文，等价不可用密码登录，SSO 是唯一入口）；role=user、
// status=active；根目录延迟到首次访问（EnsureRoot 既有语义）。写
// oidc.provision 审计。
func (s *Service) provision(claims Claims) (auth.User, error) {
	username, err := s.uniqueUsername(deriveUsername(claims.PreferredUsername, claims.Email))
	if err != nil {
		return auth.User{}, err
	}
	passwordHash, err := auth.HashPassword(randomUnusablePassword())
	if err != nil {
		return auth.User{}, err
	}
	user := auth.User{
		ID:           uuid.New(),
		Username:     username,
		Email:        claims.Email,
		PasswordHash: passwordHash,
		Status:       auth.StatusActive,
		Role:         auth.RoleUser,
	}
	if err := s.directory.CreateUser(user); err != nil {
		return auth.User{}, err
	}
	if err := s.links.Upsert(Link{Sub: claims.Sub, UserID: user.ID, Issuer: s.client.Issuer(), LinkedAt: s.now().UTC()}); err != nil {
		return auth.User{}, err
	}
	_ = s.audit.Record(audit.Entry{
		UserID:       &user.ID,
		Action:       audit.ActionOIDCProvision,
		ResourceType: audit.ResourceUser,
		ResourceID:   user.ID.String(),
		Status:       audit.StatusSuccess,
		Metadata:     `{"issuer":"` + sanitizeMetadata(s.client.Issuer()) + `","email":"` + sanitizeMetadata(claims.Email) + `"}`,
	})
	return user, nil
}

// deriveUsername 从 preferred_username 或 email 本地部分推导候选用户名：
// 小写、仅保留 [a-z0-9_-]，不满足 auth.ValidateUsername（3-32 字符）时
// 回退 "user"。
func deriveUsername(preferredUsername, email string) string {
	candidates := []string{preferredUsername}
	if at := strings.IndexByte(email, '@'); at > 0 {
		candidates = append(candidates, email[:at])
	}
	for _, candidate := range candidates {
		sanitized := sanitizeUsername(candidate)
		if auth.ValidateUsername(sanitized) == nil {
			return sanitized
		}
	}
	return "user"
}

// sanitizeUsername 小写并仅保留字母/数字/下划线/连字符（其余丢弃）。
func sanitizeUsername(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// uniqueUsername 去重：base 未占用直接用；否则追加 -1、-2…（总长截到 32）。
func (s *Service) uniqueUsername(base string) (string, error) {
	candidate := base
	for i := 1; ; i++ {
		exists, err := s.directory.UsernameExists(candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
		suffix := "-" + strconv.Itoa(i)
		candidate = base[:min(len(base), 32-len(suffix))] + suffix
	}
}

// randomUnusablePassword 生成 32 字节随机串（base64url）作为不可登录密码。
func randomUnusablePassword() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("oidc: random password: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// sanitizeMetadata 过滤可安全嵌入 JSON 字符串元数据的字符（审计防注入）。
func sanitizeMetadata(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '@', ch == '.', ch == '-', ch == '_', ch == ':', ch == '/':
			b.WriteByte(ch)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
