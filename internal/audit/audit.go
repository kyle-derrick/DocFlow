package audit

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// action 常量，对应最小写入需求。
const (
	ActionLoginSuccess   = "login_success"
	ActionLoginFailure   = "login_failure"
	ActionUploadComplete = "upload_complete"
	ActionPurge          = "purge"
	ActionShareCreate    = "share_create"
	ActionShareRevoke    = "share_revoke"
	ActionPublicDownload = "public_download"
	// 文件版本管理：新版本写入（上传覆盖）与 current_version 回滚。
	ActionVersionCreate  = "version.create"
	ActionVersionRestore = "version.restore"
	// 系统设置与后台清理（janitor）。
	ActionSettingsUpdate = "settings.update"
	ActionJanitorUpload  = "janitor.upload"
	ActionJanitorBlob    = "janitor.blob"
	ActionJanitorTrash   = "janitor.trash"
	// ONLYOFFICE 集成：保存回调落新版本与编辑会话清理。
	ActionOnlyOfficeSave    = "onlyoffice.save"
	ActionOnlyOfficeCleanup = "onlyoffice.cleanup"
)

// resource_type 常量。
const (
	ResourceSession = "session"
	ResourceUpload  = "upload"
	ResourceFile    = "file"
	ResourceShare   = "share"
	// ResourceSettings 系统设置键；ResourceBlob 为 object_blobs 行。
	ResourceSettings = "settings"
	ResourceBlob     = "blob"
)

// status 常量。
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// Entry 表示一条审计日志（对应 audit_logs 表）。
type Entry struct {
	ID           int64      `gorm:"primaryKey"`
	UserID       *uuid.UUID `gorm:"type:uuid"`
	Action       string     `gorm:"size:64;not null"`
	ResourceType string     `gorm:"size:32"`
	ResourceID   string     `gorm:"size:64"`
	IP           *string    `gorm:"size:45"`
	UserAgent    string     `gorm:"size:512"`
	Status       string     `gorm:"size:16"`
	Metadata     string     `gorm:"type:jsonb"`
	CreatedAt    time.Time  `gorm:"not null"`
}

// TableName 显式映射到 audit_logs（gorm 默认复数化为 entries）。
func (Entry) TableName() string { return "audit_logs" }

// Store 提供最小写入能力；查询接口不在本里程碑范围。
type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Record(e Entry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	return s.db.Create(&e).Error
}

// Recorder 抽象最小写入接口，便于测试注入内存实现。
type Recorder interface {
	Record(Entry) error
}

// NopRecorder 供审计未启用/测试时使用。
type NopRecorder struct{}

func (NopRecorder) Record(Entry) error { return nil }
