// Package invite 实现管理员邀请制注册：邀请的创建/列表/撤销与接受
// （凭一次性 token 完成注册）。明文 token 与 share 包同一模式：
// 32 字节随机数的 URL-safe base64，仅存 SHA-256 hex 哈希，明文只返回一次。
package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
)

var (
	ErrInvalidEmail = errors.New("invalid email address")
	ErrInvalidRole  = errors.New("role must be user or admin")
	// ErrNotFound 表示邀请不存在（未知 token / 未知 ID）。
	ErrNotFound = errors.New("invitation not found")
	// ErrGone 表示邀请已失效（过期或已接受），对外呈现 410。
	ErrGone = errors.New("invitation is no longer available")
)

// DefaultTTL 为邀请有效期（7 天）。
const DefaultTTL = 7 * 24 * time.Hour

// NewToken 生成明文邀请 token：32 字节随机数的 URL-safe base64（43 字符）。
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 返回 token 的 SHA-256 十六进制小写哈希（64 字符），
// 对应 invitations.token_hash CHAR(64)。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// UserRegistry 抽象接受邀请时的用户创建（生产实现为 *auth.UserStore；
// 用户名/邮箱唯一冲突以 auth.ErrUserExists 返回）。
type UserRegistry interface {
	CreateUser(user auth.User) error
}

// Repo 是邀请持久化接口；GormStore 为 PostgreSQL 实现，MemoryStore 供测试。
type Repo interface {
	Create(v Invitation) error
	Get(id uuid.UUID) (Invitation, error)
	GetByTokenHash(hash string) (Invitation, error)
	// FindActiveByEmail 返回该邮箱未过期未接受的邀请（幂等创建探测）。
	FindActiveByEmail(email string, now time.Time) (Invitation, error)
	List(limit int) ([]Invitation, error)
	// Delete 删除邀请（撤销；token 随之不可解析），幂等语义由实现保证。
	Delete(id uuid.UUID) error
	// MarkAccepted 原子标记 accepted_at（仅未接受且未过期时成功）。
	MarkAccepted(id uuid.UUID, now time.Time) (bool, error)
}

// Service 提供邀请的生命周期管理。个人根目录不做即时创建：
// 接受注册仅建用户，根目录按既有行为在首次访问文件时 EnsureRoot 延迟生成。
type Service struct {
	repo  Repo
	users UserRegistry
	ttl   time.Duration
	now   func() time.Time
}

func NewService(repo Repo, users UserRegistry) *Service {
	return &Service{repo: repo, users: users, ttl: DefaultTTL, now: time.Now}
}

// SetNow 注入时钟（测试用）。
func (s *Service) SetNow(now func() time.Time) { s.now = now }

// Create 为 actor（admin）创建发给 email 的邀请，返回邀请记录与明文 token
// （仅此一次可见，数据库只保存哈希）。幂等：该邮箱已有未过期未接受的邀请时
// 直接返回既有记录（token 为空串——明文已不可恢复，接受链接以首次创建时为准）。
// 邮箱小写归一并做格式校验；role 限 user|admin。
func (s *Service) Create(actor uuid.UUID, email, role string) (Invitation, string, error) {
	email = auth.NormalizeEmail(email)
	if err := auth.ValidateEmail(email); err != nil {
		return Invitation{}, "", ErrInvalidEmail
	}
	if role == "" {
		role = auth.RoleUser
	}
	if role != auth.RoleUser && role != auth.RoleAdmin {
		return Invitation{}, "", ErrInvalidRole
	}
	now := s.now().UTC()
	if existing, err := s.repo.FindActiveByEmail(email, now); err == nil {
		return existing, "", nil
	} else if !errors.Is(err, ErrNotFound) {
		return Invitation{}, "", err
	}
	token, err := NewToken()
	if err != nil {
		return Invitation{}, "", err
	}
	invitedBy := actor
	inv := Invitation{
		ID:        uuid.New(),
		Email:     email,
		InvitedBy: &invitedBy,
		Role:      role,
		TokenHash: HashToken(token),
		ExpiresAt: now.Add(s.ttl),
		CreatedAt: now,
	}
	if err := s.repo.Create(inv); err != nil {
		return Invitation{}, "", err
	}
	return inv, token, nil
}

// List 返回邀请列表（created_at 倒序，最多 limit 条）。
func (s *Service) List(limit int) ([]Invitation, error) {
	if limit <= 0 {
		limit = 100
	}
	return s.repo.List(limit)
}

// Revoke 删除邀请（token 立即不可解析）；不存在返回 ErrNotFound。
func (s *Service) Revoke(id uuid.UUID) error {
	return s.repo.Delete(id)
}

// Accept 凭一次性 token 完成注册：校验邀请未过期未接受、username 规则
// （auth.ValidateUsername）与密码强度（auth.ValidatePasswordStrength，与
// seed 共享），创建 active 用户（角色继承邀请），原子标记 accepted_at。
// 返回创建的用户与被消费的邀请。个人根目录延迟生成（首次访问文件时
// EnsureRoot），此处不创建。
func (s *Service) Accept(token, username, password string) (auth.User, Invitation, error) {
	inv, err := s.repo.GetByTokenHash(HashToken(strings.TrimSpace(token)))
	if err != nil {
		return auth.User{}, Invitation{}, ErrNotFound
	}
	now := s.now().UTC()
	if inv.AcceptedAt != nil || !now.Before(inv.ExpiresAt) {
		return auth.User{}, Invitation{}, ErrGone
	}
	if err := auth.ValidateUsername(strings.TrimSpace(username)); err != nil {
		return auth.User{}, Invitation{}, err
	}
	if err := auth.ValidatePasswordStrength(password); err != nil {
		return auth.User{}, Invitation{}, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return auth.User{}, Invitation{}, err
	}
	user := auth.User{
		ID:           uuid.New(),
		Username:     strings.TrimSpace(username),
		Email:        inv.Email,
		PasswordHash: hash,
		Status:       auth.StatusActive,
		Role:         inv.Role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.users.CreateUser(user); err != nil {
		if errors.Is(err, auth.ErrUserExists) {
			return auth.User{}, Invitation{}, auth.ErrUserExists
		}
		return auth.User{}, Invitation{}, err
	}
	// 原子标记接受：并发双注册时，后者 CreateUser 已因邮箱唯一失败，
	// 此处的条件更新兜底剩余竞态窗口（标记失败视为邀请刚被消费）。
	accepted, err := s.repo.MarkAccepted(inv.ID, now)
	if err != nil {
		return auth.User{}, Invitation{}, err
	}
	if !accepted {
		return auth.User{}, Invitation{}, ErrGone
	}
	inv.AcceptedAt = &now
	return user, inv, nil
}
