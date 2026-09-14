package oidc

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Link 对应 oidc_links 表（migrations/021）：以 IdP 的 sub（issuer 内稳定
// 标识）为主键关联 DocFlow 用户。email 匹配成功后写入关联——后续 IdP 侧
// email 变更仍可凭 sub 登录。
type Link struct {
	Sub      string    `gorm:"primaryKey"`
	UserID   uuid.UUID `gorm:"type:uuid;not null;index"`
	Issuer   string    `gorm:"not null"`
	LinkedAt time.Time
}

func (Link) TableName() string { return "oidc_links" }

// LinkStore 抽象 sub 关联的持久化（生产实现 *GormLinkStore；
// 测试可用内存实现）。
type LinkStore interface {
	Upsert(link Link) error
	FindBySub(sub string) (Link, bool, error)
}

// GormLinkStore 是 LinkStore 的 PostgreSQL 实现。
type GormLinkStore struct{ db *gorm.DB }

func NewGormLinkStore(db *gorm.DB) *GormLinkStore { return &GormLinkStore{db: db} }

// Upsert 按 sub UPSERT（重复登录同一 IdP 身份时刷新 user_id/issuer/linked_at）。
func (s *GormLinkStore) Upsert(link Link) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "sub"}},
		DoUpdates: clause.AssignmentColumns([]string{"user_id", "issuer", "linked_at"}),
	}).Create(&link).Error
}

// FindBySub 按 sub 查找关联；不存在返回 found=false。
func (s *GormLinkStore) FindBySub(sub string) (Link, bool, error) {
	var link Link
	err := s.db.First(&link, "sub = ?", sub).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	return link, true, nil
}
