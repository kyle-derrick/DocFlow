// Package tagging 实现用户维度的标签管理与文件-标签关联
// （v1.0 设计 3.2.8 Tag / 3.2.9 FileTag）。
//
// 实现取舍（相对设计稿「标签全局唯一」）：标签属于创建者
// （tags.user_id，UNIQUE(user_id, name)），仅能把自己的标签打给
// 有权访问的文件（读权限即可——标签是组织视图而非内容变更）；
// 按标签查文件时做 owner/团队读过滤（files.SearchAccessible）。
package tagging

import (
	"time"

	"github.com/google/uuid"
)

// Tag 对应 tags 表（migrations/015_tags_star.sql）。
type Tag struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null" json:"-"`
	Name      string    `gorm:"size:64;not null" json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 显式映射（gorm 默认复数化即 tags，显式声明保持一致风格）。
func (Tag) TableName() string { return "tags" }

// FileTag 对应 file_tags 表，复合主键 (tag_id, file_id)，两端级联删除。
type FileTag struct {
	TagID     uuid.UUID `gorm:"type:uuid;primaryKey"`
	FileID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt time.Time
}

// TableName 显式映射到 file_tags。
func (FileTag) TableName() string { return "file_tags" }
