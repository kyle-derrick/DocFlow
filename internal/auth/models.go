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

// DefaultStorageQuota 新用户默认存储配额（10 GiB，字节），与 migration 024
// 的 storage_quota 列默认值一致；开户默认可经 system_settings 的
// upload.default_quota 热覆盖（UserStore.SetDefaultQuotaProvider）。
const DefaultStorageQuota int64 = 10 << 30

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
	// StorageQuota 个人空间存储配额（字节，migration 024）；软删文件计入已用。
	StorageQuota int64 `gorm:"not null;default:10737418240"`
	StorageUsed  int64 `gorm:"not null;default:0"`
	// FailedLoginCount / LockedUntil 为连续登录失败锁定（C9）：达到
	// LOGIN_MAX_RETRIES 后置 LockedUntil=now+锁定时长并清零计数，成功登录
	// 清零；锁定判定按时间比较，status 列不改写。
	FailedLoginCount int        `gorm:"not null;default:0"`
	LockedUntil      *time.Time `gorm:"type:timestamptz"`
	// 档案字段（C21a，migration 024）：可空文本列用 *string（空串在写入侧
	// 归一为 NULL）；头像不落库（avatar_text 由前端按首字母计算）。
	Nickname   *string `gorm:"size:64"`
	Department *string `gorm:"size:128"`
	Position   *string `gorm:"size:128"`
	Phone      *string `gorm:"size:32"`
	Bio        *string `gorm:"size:512"`
	Language   string  `gorm:"size:8;not null;default:zh-CN"`
	Timezone   string  `gorm:"size:64;not null;default:Asia/Shanghai"`
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
	Scopes    []string  `gorm:"serializer:json;type:jsonb" json:"scopes"`
	TokenHash string    `gorm:"size:64;uniqueIndex;not null"`
	Prefix    string    `gorm:"size:14;not null;index"`
	// LastUsedAt 由认证路径 best-effort 更新（TouchLastUsed）。
	LastUsedAt *time.Time
	// ExpiresAt 为空表示永久（不推荐但不禁止）。
	ExpiresAt *time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// UserTOTP 对应 user_totp 表（migrations/020）。Secret 为 Base32 共享密钥
// （明文存储：自托管边界内数据库属信任域，见 migration 注释）；Enabled=false
// 表示 setup 已开始但未 confirm。RecoveryCodes 为 JSON 数组文本，存恢复码
// 的 SHA-256 hex 哈希；明文仅 ConfirmSetup 响应返回一次。
type UserTOTP struct {
	UserID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	Secret        string    `gorm:"not null"`
	Enabled       bool      `gorm:"not null;default:false"`
	RecoveryCodes string    `gorm:"not null;default:'[]'"`
	ConfirmedAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (UserTOTP) TableName() string { return "user_totp" }

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
