package invite

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var _ Repo = (*GormStore)(nil)

// GormStore 是 Repo 的 PostgreSQL 实现（invitations 表见 migrations/014）。
type GormStore struct{ db *gorm.DB }

func NewGormRepo(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (s *GormStore) Create(v Invitation) error {
	return s.db.Create(&v).Error
}

func (s *GormStore) Get(id uuid.UUID) (Invitation, error) {
	var v Invitation
	err := s.db.First(&v, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invitation{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) GetByTokenHash(hash string) (Invitation, error) {
	var v Invitation
	err := s.db.Where("token_hash = ?", hash).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invitation{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) FindActiveByEmail(email string, now time.Time) (Invitation, error) {
	var v Invitation
	err := s.db.
		Where("email = ? AND accepted_at IS NULL AND expires_at > ?", email, now).
		Order("created_at DESC").
		First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invitation{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) List(limit int) ([]Invitation, error) {
	var out []Invitation
	err := s.db.Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

// Delete 删除邀请行（撤销）；不存在返回 ErrNotFound。
func (s *GormStore) Delete(id uuid.UUID) error {
	result := s.db.Where("id = ?", id).Delete(&Invitation{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

// MarkAccepted 原子标记 accepted_at：条件「未接受且未过期」，RowsAffected
// != 1 说明已被并发接受或刚过期，返回 false。
func (s *GormStore) MarkAccepted(id uuid.UUID, now time.Time) (bool, error) {
	result := s.db.Model(&Invitation{}).
		Where("id = ? AND accepted_at IS NULL AND expires_at > ?", id, now).
		Update("accepted_at", now)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}
