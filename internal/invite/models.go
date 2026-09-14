package invite

import (
	"time"

	"github.com/google/uuid"
)

// Invitation 对应 invitations 表（migrations/014）。
// TokenHash 为明文 token 的 SHA-256 hex（64 字符）；明文 token 不落库，
// 仅在创建响应中返回一次（与 shares.token_hash 同一模式）。
// Email 已小写归一。撤销为直接删行（实体无 revoked_at）。
type Invitation struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Email      string     `gorm:"size:320;not null" json:"email"`
	InvitedBy  *uuid.UUID `gorm:"type:uuid" json:"invited_by"`
	Role       string     `gorm:"size:16;not null;default:user" json:"role"`
	TokenHash  string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	ExpiresAt  time.Time  `gorm:"not null" json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at"`
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
}

// 邀请派生状态（HTTP 列表展示用，不落库）。
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusExpired  = "expired"
)

// Status 返回派生状态：已接受 / 已过期 / 待接受。
func (v Invitation) Status(now time.Time) string {
	switch {
	case v.AcceptedAt != nil:
		return StatusAccepted
	case !now.Before(v.ExpiresAt):
		return StatusExpired
	default:
		return StatusPending
	}
}
