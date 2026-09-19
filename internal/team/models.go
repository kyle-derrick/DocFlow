package team

import (
	"time"

	"github.com/google/uuid"
)

// 团队内五级内置角色（team_members.role CHECK 约束一致，migration 037）：
// owner 所有者（唯一，最高权限 + 解散/转让团队）；admin 管理员（读/写/删/
// 分享/管成员/改目录文件权限）；member_share 普通成员可分享（读写删+分享）；
// member 普通成员（读写删）；guest 访客（只读）。自定义角色已移除
//（migration 037 把存量 custom/editor/viewer 迁到最接近的内置级）。
const (
	RoleOwner       = "owner"
	RoleAdmin       = "admin"
	RoleMemberShare = "member_share"
	RoleMember      = "member"
	RoleGuest       = "guest"
)

// 权限动作（五级角色的固定权限矩阵见 service.rolePermissions）。
const (
	PermRead   = "read"
	PermWrite  = "write"
	PermDelete = "delete"
	PermShare  = "share"
	PermAdmin  = "admin"
)

// ValidActions 全部合法权限动作。
var ValidActions = []string{PermRead, PermWrite, PermDelete, PermShare, PermAdmin}

// AssignableRoles 可经成员管理接口授予的内置角色（owner 只能经
// TransferOwnership 产生，不可直接指派）。
var AssignableRoles = []string{RoleAdmin, RoleMemberShare, RoleMember, RoleGuest}

// Team 对应 teams 表（migrations/008_teams_shares.sql）。
type Team struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string     `gorm:"size:100;uniqueIndex;not null" json:"name"`
	Description string     `json:"description"`
	OwnerID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// TeamInfo 为「我的团队」列表条目（GET /teams，v1.7 团队页卡片）：团队
// 基础字段 + 我的角色（五级内置）+ 成员数 + 团队空间存储用量（当前版本
// 对象字节合计，软删不计——与个人仪表盘口径一致，仅卡片展示）。
type TeamInfo struct {
	Team
	MyRole      string `gorm:"->" json:"my_role"`
	MemberCount int64  `gorm:"->" json:"member_count"`
	StorageUsed int64  `gorm:"->" json:"storage_used"`
}

// Member 对应 team_members 表，复合主键 (team_id, user_id)。
// Role 为五级内置角色之一；Username/Nickname/Email 为列表查询的关联列
//（users 表，只读），供成员列表展示用户名/邮箱（替代 UUID，
// 见 GormStore.ListMembers 的 JOIN）。CreatedAt 即加入时间（joined_at）。
type Member struct {
	TeamID    uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey" json:"user_id"`
	Role      string    `gorm:"size:16;not null" json:"role"`
	Username  string    `gorm:"->" json:"username,omitempty"`
	Nickname  string    `gorm:"->" json:"nickname,omitempty"`
	Email     string    `gorm:"->" json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 显式映射 team_members（gorm 默认复数化为 members，与
// migrations/008 的表名不符——运行时才会暴露）。
func (Member) TableName() string { return "team_members" }

// Invite 对应 team_invites 表（migration 039）：团队邮箱邀请。
// TokenHash 为明文 token 的 SHA-256 hex（与 shares/invitations 同模式，
// 明文只在创建响应返回一次）；email 已小写归一；撤销 = 删行。
type Invite struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	TeamID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"team_id"`
	Email      string     `gorm:"size:320;not null" json:"email"`
	Role       string     `gorm:"size:16;not null;default:member" json:"role"`
	TokenHash  string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	InvitedBy  *uuid.UUID `gorm:"type:uuid" json:"invited_by"`
	ExpiresAt  time.Time  `gorm:"not null" json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at"`
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
}

// TableName 显式映射 team_invites。
func (Invite) TableName() string { return "team_invites" }

// 邀请派生状态（HTTP 列表展示用，不落库）。
const (
	InviteStatusPending  = "pending"
	InviteStatusAccepted = "accepted"
	InviteStatusExpired  = "expired"
)

// Status 返回邀请派生状态：已接受 / 已过期 / 待接受。
func (v Invite) Status(now time.Time) string {
	switch {
	case v.AcceptedAt != nil:
		return InviteStatusAccepted
	case !now.Before(v.ExpiresAt):
		return InviteStatusExpired
	default:
		return InviteStatusPending
	}
}
