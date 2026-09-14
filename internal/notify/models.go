// Package notify 提供站内通知：事件分发（含偏好短路）、通知存取与
// 每用户每事件类型的开关偏好。内存实现供测试，GormStore 为 PostgreSQL 实现。
package notify

import (
	"time"

	"github.com/google/uuid"
)

// 事件类型常量（v1.0 范围）。
const (
	// EventUploadCompleted 我的上传完成（校验与安全扫描通过，文件可用）。
	EventUploadCompleted = "upload.completed"
	// EventUploadQuarantined 我的上传被隔离（未通过安全扫描）。
	EventUploadQuarantined = "upload.quarantined"
	// EventShareAccessed 我的公开/私有分享被下载。
	EventShareAccessed = "share.accessed"
	// EventFileUpdated 团队文件被他人更新新版本（发给团队其他成员；仅团队文件）。
	EventFileUpdated = "file.updated"
)

// EventTypes 全部事件类型（设置页展示顺序）。
var EventTypes = []string{EventUploadCompleted, EventUploadQuarantined, EventShareAccessed, EventFileUpdated}

// ValidEventType 判定事件类型是否已知。
func ValidEventType(eventType string) bool {
	for _, t := range EventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// defaultEnabled 默认开关表：无偏好记录时按此生效（当前全部默认开启）。
var defaultEnabled = map[string]bool{
	EventUploadCompleted:   true,
	EventUploadQuarantined: true,
	EventShareAccessed:     true,
	EventFileUpdated:       true,
}

// DefaultEnabled 返回事件类型的默认开关（未知类型默认开启，保守不丢通知）。
func DefaultEnabled(eventType string) bool {
	return defaultEnabled[eventType]
}

// Notification 对应 notifications 表（migration 017）。
type Notification struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	Type       string     `gorm:"size:32;not null" json:"type"`
	Title      string     `gorm:"not null" json:"title"`
	Body       string     `gorm:"not null" json:"body"`
	ResourceID *uuid.UUID `gorm:"type:uuid" json:"resource_id"`
	IsRead     bool       `gorm:"not null;default:false" json:"is_read"`
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
	ReadAt     *time.Time `json:"read_at"`
}

// TableName 显式映射到 notifications（gorm 默认复数化一致，显式声明防漂移）。
func (Notification) TableName() string { return "notifications" }

// UserNotificationPreference 对应 user_notification_preferences 表，
// PK (user_id, event_type)；无记录 = 默认开启。
type UserNotificationPreference struct {
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	EventType string    `gorm:"size:32;primaryKey"`
	Enabled   bool      `gorm:"not null;default:true"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 显式映射到 user_notification_preferences。
func (UserNotificationPreference) TableName() string { return "user_notification_preferences" }
