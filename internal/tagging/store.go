package tagging

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GormRepo 是 Repo 的 PostgreSQL 实现（tags / file_tags 表见
// migrations/015_tags_star.sql）。
type GormRepo struct{ db *gorm.DB }

func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

// isUniqueViolation 统一识别唯一约束冲突（Postgres 错误串含 unique）。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func (g *GormRepo) CreateTag(t Tag) error {
	if err := g.db.Create(&t).Error; err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (g *GormRepo) GetTag(id uuid.UUID) (Tag, error) {
	var t Tag
	if err := g.db.Where("id = ?", id).First(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Tag{}, ErrNotFound
		}
		return Tag{}, err
	}
	return t, nil
}

func (g *GormRepo) ListTags(user uuid.UUID) ([]Tag, error) {
	var out []Tag
	err := g.db.Where("user_id = ?", user).Order("lower(name), id").Find(&out).Error
	return out, err
}

func (g *GormRepo) DeleteTag(id uuid.UUID) error {
	return g.db.Where("id = ?", id).Delete(&Tag{}).Error
}

func (g *GormRepo) FileTagExists(tagID, fileID uuid.UUID) (bool, error) {
	var count int64
	err := g.db.Model(&FileTag{}).Where("tag_id = ? AND file_id = ?", tagID, fileID).Count(&count).Error
	return count > 0, err
}

func (g *GormRepo) AddFileTag(ft FileTag) error {
	if err := g.db.Create(&ft).Error; err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (g *GormRepo) RemoveFileTag(tagID, fileID uuid.UUID) (bool, error) {
	result := g.db.Where("tag_id = ? AND file_id = ?", tagID, fileID).Delete(&FileTag{})
	return result.RowsAffected > 0, result.Error
}

func (g *GormRepo) ListFileTags(user, fileID uuid.UUID) ([]Tag, error) {
	var out []Tag
	err := g.db.
		Where("id IN (SELECT tag_id FROM file_tags WHERE file_id = ?) AND user_id = ?", fileID, user).
		Order("lower(name), id").Find(&out).Error
	return out, err
}
