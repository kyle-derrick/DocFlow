package files

import (
	"time"

	"github.com/google/uuid"
)

// ObjectBlob 状态与 object_blobs.status CHECK 约束一致。
const (
	BlobStatusCreated     = "created"
	BlobStatusScanning    = "scanning"
	BlobStatusAvailable   = "available"
	BlobStatusQuarantined = "quarantined"
	BlobStatusFailed      = "failed"
	BlobStatusDeleting    = "deleting"
)

type File struct {
	ID               uuid.UUID  `gorm:"type:uuid;primaryKey"`
	Name             string     `gorm:"size:255;not null"`
	ParentID         *uuid.UUID `gorm:"type:uuid"`
	OwnerID          uuid.UUID  `gorm:"type:uuid;not null"`
	TeamID           *uuid.UUID `gorm:"type:uuid"`
	Type             string     `gorm:"size:16;not null"`
	IsRoot           bool       `gorm:"not null"`
	ScopeType        string     `gorm:"size:16;not null"`
	TreePath         string     `gorm:"type:ltree"`
	CurrentVersionID *uuid.UUID `gorm:"type:uuid"`
	Description      string
	IsPublic         bool
	IsStarred        bool `gorm:"not null;default:false"`
	ViewCount        int64
	DownloadCount    int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
	LastAccessAt     *time.Time `gorm:"column:last_access_at"`
	DeletedAt        *time.Time
	ThumbnailURL     *string `gorm:"-" json:"thumbnail_url,omitempty"`
}

type FileVersion struct {
	ID            uuid.UUID `gorm:"type:uuid;primaryKey"`
	FileID        uuid.UUID `gorm:"type:uuid;not null"`
	Version       int
	ObjectBlobID  uuid.UUID `gorm:"type:uuid;not null"`
	ContentSHA256 string
	Size          int64
	Comment       *string
	UserID        uuid.UUID `gorm:"type:uuid;not null"`
	CreatedAt     time.Time
}

type ObjectBlob struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey"`
	SHA256     string    `gorm:"size:64;uniqueIndex;not null"`
	StorageKey string    `gorm:"not null"`
	Size       int64
	MimeType   string
	RefCount   int64
	Status     string
	CreatedAt  time.Time
}
