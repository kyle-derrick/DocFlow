package upload

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusUploading   Status = "uploading"
	StatusVerifying   Status = "verifying"
	StatusScanning    Status = "scanning"
	StatusAvailable   Status = "available"
	StatusQuarantined Status = "quarantined"
	StatusFailed      Status = "failed"
)

type UploadSession struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID         uuid.UUID  `gorm:"type:uuid;not null" json:"-"`
	ParentID       uuid.UUID  `gorm:"type:uuid;not null" json:"parent_id"`
	TusID          string     `gorm:"uniqueIndex;not null" json:"-"`
	Name           string     `gorm:"size:255;not null" json:"name"`
	Size           int64      `gorm:"not null" json:"size"`
	ExpectedSHA256 string     `gorm:"size:64" json:"-"`
	Metadata       string     `gorm:"size:2048" json:"-"` // tus 原始 Upload-Metadata 头，HEAD 回显用
	Status         Status     `gorm:"size:16;not null" json:"status"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RetryCount     int        `json:"retry_count"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Offset         int64      `gorm:"not null;default:0" json:"offset"`
	StorageKey     string     `gorm:"size:1024;not null" json:"-"`
	// TargetFileID 非 nil 时为「覆盖为新版本」会话：Complete 不创建新 File，
	// 而是向该文件追加版本；名称/父目录沿用目标文件现有值。
	TargetFileID *uuid.UUID `gorm:"type:uuid" json:"-"`
	// FileID 完成后的文件 ID 回显（新文件为新建文件 id；版本会话为目标
	// 文件 id）。非持久化：Complete 成功路径填充，幂等重入（available
	// 直接返回）仅版本会话可恢复。此前新文件的 complete 响应不含任何
	// 文件 ID（运行时冒烟暴露的 API 易用性缺口），客户端只能列表反查。
	FileID uuid.UUID `gorm:"-" json:"file_id,omitempty"`
}
