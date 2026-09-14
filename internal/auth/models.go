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
	CreatedAt        time.Time
	LastActiveAt     time.Time `gorm:"not null"`
	ExpiresAt        time.Time `gorm:"not null"`
	RevokedAt        *time.Time
}
