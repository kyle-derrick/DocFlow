package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type GormSessionStore struct {
	db *gorm.DB
}

// ErrUserNotFound 表示按 ID 查询用户不存在（Role 查询使用）。
var ErrUserNotFound = errors.New("user not found")

func NewGormSessionStore(db *gorm.DB) *GormSessionStore {
	return &GormSessionStore{db: db}
}

func (s *GormSessionStore) Create(session Session) error {
	return s.db.Create(&session).Error
}

func (s *GormSessionStore) Rotate(tokenHash, replacementHash string, now, expiresAt time.Time) (Session, error) {
	var session Session
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Where("refresh_token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now).
			First(&session)
		if result.Error != nil {
			return result.Error
		}
		result = tx.Model(&Session{}).Where("id = ? AND revoked_at IS NULL", session.ID).
			Updates(map[string]any{"refresh_token_hash": replacementHash, "last_active_at": now, "expires_at": expiresAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Session{}, ErrInvalidRefreshToken
	}
	return session, err
}

func (s *GormSessionStore) Revoke(tokenHash string, now time.Time) error {
	result := s.db.Model(&Session{}).Where("refresh_token_hash = ? AND revoked_at IS NULL", tokenHash).
		Update("revoked_at", now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrInvalidRefreshToken
	}
	return nil
}

type UserStore struct {
	db *gorm.DB
}

func NewUserStore(db *gorm.DB) *UserStore {
	return &UserStore{db: db}
}

func (s *UserStore) FindActiveByEmail(email string) (User, error) {
	var user User
	err := s.db.Where("email = ? AND status = ?", email, "active").First(&user).Error
	return user, err
}

// likePrefixPattern 构造前缀匹配的 LIKE 模式：转义 \、%、_ 通配符后追加 %。
func likePrefixPattern(q string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(q) + "%"
}

// Lookup 按邮箱或用户名精确/前缀匹配活跃用户（大小写不敏感前缀），
// 按 username 排序、最多 limit 条；供用户查找/邀请场景使用。
// 注意：返回的 User 含 email，调用方（HTTP 层）序列化时只暴露 id 与 username。
func (s *UserStore) Lookup(q string, limit int) ([]User, error) {
	if limit <= 0 {
		limit = 10
	}
	pattern := likePrefixPattern(q)
	var out []User
	err := s.db.
		Where("status = ? AND (email = ? OR username = ? OR email ILIKE ? ESCAPE '\\' OR username ILIKE ? ESCAPE '\\')",
			"active", q, q, pattern, pattern).
		Order("username, id").Limit(limit).Find(&out).Error
	return out, err
}

// Username 返回用户名（不含 email 等其他字段）；用户不存在时返回错误。
func (s *UserStore) Username(id uuid.UUID) (string, error) {
	var user User
	err := s.db.Select("username").First(&user, "id = ?", id).Error
	if err != nil {
		return "", err
	}
	return user.Username, nil
}

// Role 返回用户角色（user/admin）；用户不存在时返回 ErrUserNotFound，
// 供 RequireRole 中间件鉴权使用。
func (s *UserStore) Role(id uuid.UUID) (string, error) {
	var user User
	err := s.db.Select("role").First(&user, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	return user.Role, nil
}
