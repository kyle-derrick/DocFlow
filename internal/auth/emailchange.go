package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// 换绑邮箱（账号安全，v2.4）：两段式确认流——
//   1. POST /me/email/change-request（验证密码 + 新邮箱）：生成 6 位数字
//      验证码，邮件投递到新邮箱；本表落 code_hash = SHA-256(user_id:code)；
//   2. POST /me/email/change-confirm（验证码）：原子消费未用未过期行并更新
//      users.email。
// 验证码 10 分钟有效、一次性；同用户重复请求多行并存（消费取 created_at
// 最新且匹配哈希的行），旧请求的行在过期后自然失效。

// EmailChangeTokenTTL 为换绑邮箱验证码有效期（10 分钟）。
const EmailChangeTokenTTL = 10 * time.Minute

// ErrEmailChangeInvalid 表示验证码不存在、已使用或已过期。
var ErrEmailChangeInvalid = errors.New("invalid or expired email change code")

// EmailChangeCode 对应 email_change_codes 表（migration 042）。
type EmailChangeCode struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID  `gorm:"type:uuid;not null;index"`
	NewEmail  string     `gorm:"type:text;not null"`
	CodeHash  string     `gorm:"size:64;not null"`
	ExpiresAt time.Time  `gorm:"not null"`
	UsedAt    *time.Time `gorm:"type:timestamptz"`
	CreatedAt time.Time  `gorm:"not null"`
}

// TableName 显式映射 email_change_codes。
func (EmailChangeCode) TableName() string { return "email_change_codes" }

// GenerateEmailChangeCode 生成 6 位数字验证码（crypto/rand）。
func GenerateEmailChangeCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HashEmailChangeCode 计算 SHA-256(userID + ":" + code) 的十六进制小写哈希
// （user_id 充当盐，跨用户防彩虹表比对）。
func HashEmailChangeCode(userID uuid.UUID, code string) string {
	sum := sha256.Sum256([]byte(userID.String() + ":" + strings.TrimSpace(code)))
	return hex.EncodeToString(sum[:])
}

// EmailChangeStore 抽象换绑邮箱验证码的持久化（生产实现为
// *GormEmailChangeStore；测试可用内存实现）。
type EmailChangeStore interface {
	Create(code EmailChangeCode) error
	// ConsumeForUser 原子消费 user 的匹配 codeHash 且未用未过期的行
	//（created_at 最新优先），返回该行登记的新邮箱与是否命中。
	ConsumeForUser(userID uuid.UUID, codeHash string, now time.Time) (newEmail string, ok bool, err error)
}

// GormEmailChangeStore 为 EmailChangeStore 的 PostgreSQL 实现。
type GormEmailChangeStore struct{ db *gorm.DB }

// NewGormEmailChangeStore 构造验证码存储。
func NewGormEmailChangeStore(db *gorm.DB) *GormEmailChangeStore {
	return &GormEmailChangeStore{db: db}
}

func (s *GormEmailChangeStore) Create(code EmailChangeCode) error {
	return s.db.Create(&code).Error
}

func (s *GormEmailChangeStore) ConsumeForUser(userID uuid.UUID, codeHash string, now time.Time) (string, bool, error) {
	var row EmailChangeCode
	err := s.db.
		Where("user_id = ? AND code_hash = ? AND used_at IS NULL AND expires_at > ?", userID, codeHash, now).
		Order("created_at DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	// 条件更新保证一次性语义（并发确认只有一方成功）。
	res := s.db.Model(&EmailChangeCode{}).
		Where("id = ? AND used_at IS NULL", row.ID).
		Update("used_at", now)
	if res.Error != nil {
		return "", false, res.Error
	}
	if res.RowsAffected == 0 {
		return "", false, nil
	}
	return row.NewEmail, true, nil
}

// EmailChangeMemoryStore 为测试用内存实现。
type EmailChangeMemoryStore struct {
	codes []EmailChangeCode
}

// NewEmailChangeMemoryStore 构造内存验证码存储。
func NewEmailChangeMemoryStore() *EmailChangeMemoryStore { return &EmailChangeMemoryStore{} }

func (s *EmailChangeMemoryStore) Create(code EmailChangeCode) error {
	s.codes = append(s.codes, code)
	return nil
}

func (s *EmailChangeMemoryStore) ConsumeForUser(userID uuid.UUID, codeHash string, now time.Time) (string, bool, error) {
	for i := len(s.codes) - 1; i >= 0; i-- {
		c := s.codes[i]
		if c.UserID == userID && c.CodeHash == codeHash && c.UsedAt == nil && c.ExpiresAt.After(now) {
			used := now
			s.codes[i].UsedAt = &used
			return c.NewEmail, true, nil
		}
	}
	return "", false, nil
}
