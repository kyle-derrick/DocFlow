package share

import (
	"time"

	"github.com/google/uuid"
)

// 分享权限：view 仅可查看元数据，download 可下载。
const (
	PermissionView     = "view"
	PermissionDownload = "download"
)

// 分享可见性：public 为公开 token 链接（现有行为不变）；
// private 为私有分享，不生成公开 token，仅登录用户按显式授权
// （share_users 指定用户 / share_teams 团队成员）访问。
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Share 对应 shares 表（migrations/006_shares.sql、008_teams_shares.sql）。
// TokenHash 为明文 token 的 SHA-256 hex；明文 token 不落库，仅在创建响应中返回一次。
// 私有分享 TokenHash 为空串（列可空），不做 token 解析。
type Share struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	OwnerID       uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	FileID        uuid.UUID  `gorm:"type:uuid;not null;index" json:"file_id"`
	TokenHash     string     `gorm:"size:64;uniqueIndex" json:"-"`
	Visibility    string     `gorm:"size:16;not null;default:public;index" json:"visibility"`
	Permission    string     `gorm:"size:16;not null" json:"permission"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	MaxDownloads  *int       `json:"max_downloads,omitempty"`
	DownloadCount int        `gorm:"not null;default:0" json:"download_count"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}
