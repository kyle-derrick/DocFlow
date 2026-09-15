package share

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
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

// 访问事件动作（file_access_events.action，migration 023）。
const (
	ActionDownload = "download"
	ActionPreview  = "preview"
)

// DefaultWatermarkTemplate 为水印默认模板（settings share.watermark_text
// 未设置时的回退值）；公开访问无登录身份，{email}/{ip} 占位符渲染为
// 脱敏 IP 前缀（见 RenderWatermark）。
const DefaultWatermarkTemplate = "{date} {name}"

// MaxWatermarkTextLen 为自定义水印模板长度上限（rune 数）。
const MaxWatermarkTextLen = 256

// Share 对应 shares 表（migrations/006/008/022）。
// TokenHash 为明文 token 的 SHA-256 hex；明文 token 不落库，仅在创建响应中返回一次。
// 私有分享 TokenHash 为空串（列可空），不做 token 解析。
// PasswordHash 为 SHA-256(password || id) hex（022），空串表示未设密码；
// WatermarkEnabled/WatermarkText 为水印开关与自定义模板（NULL=渲染时用默认模板）。
type Share struct {
	ID               uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	OwnerID          uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	FileID           uuid.UUID  `gorm:"type:uuid;not null;index" json:"file_id"`
	TokenHash        string     `gorm:"size:64;uniqueIndex" json:"-"`
	Visibility       string     `gorm:"size:16;not null;default:public;index" json:"visibility"`
	Permission       string     `gorm:"size:16;not null" json:"permission"`
	PasswordHash     string     `gorm:"size:64" json:"-"`
	WatermarkEnabled bool       `gorm:"not null;default:true" json:"watermark_enabled"`
	WatermarkText    *string    `gorm:"type:text" json:"watermark_text,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	MaxDownloads     *int       `json:"max_downloads,omitempty"`
	DownloadCount    int        `gorm:"not null;default:0" json:"download_count"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// HasPassword 表示公开分享是否受密码保护（仅公开分享可设密码）。
func (s Share) HasPassword() bool { return s.PasswordHash != "" }

// AfterFind 清洗 CHAR(64) 列读值：无密码分享的空串入库被 PostgreSQL CHAR
// 填充为 64 空格，读出 TrimSpace 恢复空语义——否则无密码公开分享被误判
// HasPassword，info/download 一律 401 PASSWORD_REQUIRED（运行时冒烟暴露，
// 与 upload.expected_sha256 同型的 CHAR 填充陷阱）。钩子必须是 GORM 标准
// 签名（*gorm.DB 参数 + error 返回），无参变体会被静默忽略。
func (s *Share) AfterFind(_ *gorm.DB) error {
	s.PasswordHash = strings.TrimSpace(s.PasswordHash)
	return nil
}

// WatermarkTemplate 返回生效的水印模板：自定义模板优先，NULL/空回退默认模板。
func (s Share) WatermarkTemplate() string {
	if s.WatermarkText != nil && *s.WatermarkText != "" {
		return *s.WatermarkText
	}
	return DefaultWatermarkTemplate
}

// AccessSession 对应 share_access_sessions 表（migration 022）：
// 密码校验通过后发放的公开访问会话。cookie 只存随机值本身（HttpOnly），
// 库中仅存 SessionHash = SHA-256(share_id || random)，明文随机值不落库。
type AccessSession struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	ShareID     uuid.UUID `gorm:"type:uuid;not null;index" json:"share_id"`
	SessionHash string    `gorm:"size:64;uniqueIndex" json:"-"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// TableName 显式映射 share_access_sessions（gorm 默认复数化为
// access_sessions，与 migrations/022 的表名不符——运行时才会暴露）。
func (AccessSession) TableName() string { return "share_access_sessions" }

// AccessEvent 对应 file_access_events 表（migration 023）：公开分享下载/预览
// 成功事件。IPHash = SHA-256(盐 || ip)（明文 IP 不落库）；IPPrefix 为展示用
// 脱敏前缀（如 203.0.*）；UserAgent 已截断至 512 字节。
type AccessEvent struct {
	ID        int64      `gorm:"primaryKey" json:"id"`
	FileID    uuid.UUID  `gorm:"type:uuid;not null" json:"file_id"`
	ShareID   uuid.UUID  `gorm:"type:uuid;index" json:"share_id"`
	UserID    *uuid.UUID `gorm:"type:uuid" json:"user_id,omitempty"`
	Action    string     `gorm:"size:16;not null" json:"action"`
	IPHash    string     `gorm:"size:64;not null" json:"-"`
	IPPrefix  string     `gorm:"size:24;not null;default:''" json:"ip_prefix"`
	UserAgent string     `gorm:"type:text" json:"user_agent"`
	CreatedAt time.Time  `json:"created_at"`
}

// TableName 显式映射到 file_access_events。
func (AccessEvent) TableName() string { return "file_access_events" }

// AccessStats 为分享访问统计聚合（GET /api/v1/shares/:id 的 stats 字段）。
type AccessStats struct {
	TotalAccess    int64         `json:"total_access"`
	UniqueVisitors int64         `json:"unique_visitors"`
	Recent         []AccessEvent `json:"recent"`
}
