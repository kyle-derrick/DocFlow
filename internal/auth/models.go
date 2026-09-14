package auth

import (
	"time"

	"github.com/google/uuid"
)

// 用户角色，与 users.role CHECK 约束一致。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// 用户状态，与 users.status CHECK 约束一致。
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusLocked   = "locked"
)

type User struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	Username     string    `gorm:"uniqueIndex;not null"`
	Email        string    `gorm:"uniqueIndex;not null"`
	PasswordHash string    `gorm:"not null"`
	Status       string    `gorm:"not null"`
	// Role 为 user（默认）或 admin，RequireRole 据此鉴权。
	Role      string `gorm:"size:16;not null;default:user"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Session struct {
	ID               uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID           uuid.UUID `gorm:"type:uuid;not null"`
	RefreshTokenHash string    `gorm:"uniqueIndex;not null"`
	// IP/UserAgent 为创建会话时记录的请求环境（sessions.ip/user_agent 列，
	// 写入方式与 audit_logs.ip 的 *string → INET 一致；列表读取经 host(ip)
	// 归一为文本，见 SessionView）。
	IP           *string
	UserAgent    string `gorm:"column:user_agent"`
	CreatedAt    time.Time
	LastActiveAt time.Time `gorm:"not null"`
	ExpiresAt    time.Time `gorm:"not null"`
	RevokedAt    *time.Time
}

// SessionView 为会话列表的对外视图（GET /api/v1/auth/sessions）：不含
// refresh_token_hash（任何形态的凭据材料都不离开服务端）。
type SessionView struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt    time.Time
	LastActiveAt time.Time
	ExpiresAt    time.Time
	// IP 已经 host() 归一为文本（INET → string，规避驱动解码差异）。
	IP        *string `gorm:"column:ip"`
	UserAgent string  `gorm:"column:user_agent"`
}

// APIToken 对应 api_tokens 表（migrations/016）。TokenHash 为明文 token 的
// SHA-256 十六进制哈希（64 字符）；明文仅创建响应返回一次。Prefix 为明文
// 前 14 字符（dfpat_ + 随机体前 8 字符），认证按其定位候选行。
type APIToken struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index"`
	Name      string    `gorm:"size:100;not null"`
	TokenHash string    `gorm:"size:64;uniqueIndex;not null"`
	Prefix    string    `gorm:"size:14;not null;index"`
	// LastUsedAt 由认证路径 best-effort 更新（TouchLastUsed）。
	LastUsedAt *time.Time
	// ExpiresAt 为空表示永久（不推荐但不禁止）。
	ExpiresAt *time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// PasswordResetToken 对应 password_reset_tokens 表（migrations/014）。
// TokenHash 为明文 token 的 SHA-256 hex（64 字符）；明文不落库，仅在
// 请求重置时经邮件发送一次。used_at 原子条件更新保证一次性语义。
type PasswordResetToken struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index"`
	TokenHash string    `gorm:"size:64;uniqueIndex;not null"`
	ExpiresAt time.Time `gorm:"not null"`
	UsedAt    *time.Time
	CreatedAt time.Time
}
