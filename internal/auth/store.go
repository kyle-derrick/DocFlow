package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// Rotate 原子轮换 refresh token 哈希（单条 session 记录即一个 token family）：
//   - 事务内 SELECT ... FOR UPDATE（clause.Locking）行锁串行化并发轮换；
//   - UPDATE WHERE 复查 refresh_token_hash：RowsAffected != 1 说明该行已被
//     并发事务轮换（本次持旧 hash 即为重放），此时撤销整个 session
//     （revoked_at=now，token family 全部失效）并返回 ErrInvalidRefreshToken；
//     事务须以 nil 错误返回以提交撤销，错误在事务提交后返回；
//   - 旧 hash 未命中（已被轮换/撤销/过期或本就未知）同样返回
//     ErrInvalidRefreshToken——此时无法由旧 hash 反查 session 定位 family，
//     撤销只能覆盖上述 UPDATE 复查命中的重放窗口。
func (s *GormSessionStore) Rotate(tokenHash, replacementHash string, now, expiresAt time.Time) (Session, error) {
	var session Session
	replayed := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("refresh_token_hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash, now).
			First(&session)
		if result.Error != nil {
			return result.Error
		}
		result = tx.Model(&Session{}).
			Where("id = ? AND refresh_token_hash = ? AND revoked_at IS NULL", session.ID, tokenHash).
			Updates(map[string]any{"refresh_token_hash": replacementHash, "last_active_at": now, "expires_at": expiresAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			if err := tx.Model(&Session{}).Where("id = ?", session.ID).Update("revoked_at", now).Error; err != nil {
				return err
			}
			replayed = true
			return nil // 提交事务以持久化撤销，错误延后到事务外返回
		}
		return nil
	})
	if replayed {
		return Session{}, ErrInvalidRefreshToken
	}
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

// lookupCondition 构造 Lookup 的匹配条件与参数：
//   - q 含 @：仅邮箱整串精确匹配（lower(email) = lower(q)），防止以邮箱
//     片段做前缀探测枚举用户；
//   - 否则：邮箱整串精确匹配 或 用户名前缀 ILIKE（ILIKE 自身大小写不敏感）。
func lookupCondition(q string) (string, []any) {
	email := strings.ToLower(q)
	if strings.Contains(q, "@") {
		return "lower(email) = ?", []any{email}
	}
	return "(lower(email) = ? OR username ILIKE ? ESCAPE '\\')", []any{email, likePrefixPattern(q)}
}

// Lookup 按邮箱整串精确匹配（大小写不敏感）或用户名前缀匹配活跃用户，
// 按 username 排序、最多 limit 条；供用户查找/邀请场景使用。
// 防邮箱枚举：email 永不做前缀/模糊匹配；q 含 @ 时仅按邮箱精确匹配。
// 注意：返回的 User 含 email，调用方（HTTP 层）序列化时只暴露 id 与 username。
func (s *UserStore) Lookup(q string, limit int) ([]User, error) {
	if limit <= 0 {
		limit = 10
	}
	condition, args := lookupCondition(q)
	var out []User
	err := s.db.
		Where("status = ?", StatusActive).
		Where(condition, args...).
		Order("username, id").Limit(limit).Find(&out).Error
	return out, err
}

// Status 返回用户状态（active/disabled/locked）；用户不存在返回 ErrUserNotFound。
// 供 refresh 轮换成功后复查账号是否仍 active（禁用账号立即失效会话）。
func (s *UserStore) Status(id uuid.UUID) (string, error) {
	var user User
	err := s.db.Select("status").First(&user, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	return user.Status, nil
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
