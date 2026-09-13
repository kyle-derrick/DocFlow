package upload

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore       { return &GormStore{db: db} }
func (s *GormStore) Save(v UploadSession) error { return s.db.Create(&v).Error }
func (s *GormStore) Get(id uuid.UUID) (UploadSession, error) {
	var v UploadSession
	err := s.db.First(&v, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UploadSession{}, ErrNotFound
	}
	return v, err
}
func (s *GormStore) Update(v UploadSession) error {
	result := s.db.Model(&UploadSession{}).Where("id = ?", v.ID).Updates(&v)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
