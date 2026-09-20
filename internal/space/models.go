package space

import (
	"time"

	"github.com/google/uuid"
)

// 空间内五级内置角色（space_members.role CHECK 约束一致，migration 040）：
// owner 所有者（唯一，最高权限 + 解散/转让空间）；admin 管理员（读/写/删/
// 分享/管成员/改目录文件权限）；member_share 普通成员可分享（读写删+分享）；
// member 普通成员（读写删）；guest 访客（只读）。
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

// RoleLevel 返回角色等级（取最高权限判定用）：owner > admin >
// member_share > member > guest；未知角色 -1（非成员）。
func RoleLevel(role string) int {
	switch role {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleMemberShare:
		return 2
	case RoleMember:
		return 1
	case RoleGuest:
		return 0
	}
	return -1
}

// HigherRole 返回两个角色中权限更高者（等级并列取前者；空串视为非成员）。
func HigherRole(a, b string) string {
	if RoleLevel(a) >= RoleLevel(b) {
		return a
	}
	return b
}

// Space 对应 spaces 表（migration 040，统一空间模型）：唯一文件容器，
// 每用户注册自动建默认空间（is_default，可改名不可删除）。
type Space struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string     `gorm:"size:100;not null" json:"name"`
	Description string     `json:"description"`
	QuotaBytes  int64      `gorm:"not null;default:0" json:"quota_bytes"`
	OwnerID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	IsDefault   bool       `gorm:"not null;default:false" json:"is_default"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// SpaceInfo 为「我的空间」列表条目（GET /spaces）：空间基础字段 + 我的
// 角色（直接成员与用户组命中取最高）+ 成员数 + 存储用量（当前版本对象
// 字节合计，软删计入——与配额校验口径一致）。
type SpaceInfo struct {
	Space
	MyRole      string `gorm:"->" json:"my_role"`
	MemberCount int64  `gorm:"->" json:"member_count"`
	StorageUsed int64  `gorm:"->" json:"storage_used"`
}

// Member 对应 space_members 表，复合主键 (space_id, user_id)。
// Username/Nickname/Email 为列表查询的关联列（users 表，只读）。
type Member struct {
	SpaceID   uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey" json:"user_id"`
	Role      string    `gorm:"size:16;not null" json:"role"`
	Username  string    `gorm:"->" json:"username,omitempty"`
	Nickname  string    `gorm:"->" json:"nickname,omitempty"`
	Email     string    `gorm:"->" json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 显式映射 space_members。
func (Member) TableName() string { return "space_members" }

// GroupMember 对应 space_group_members 表（空间 × 用户组，migration 040）：
// 组内全部用户按该角色参与权限判定（与直接成员取最高）。GroupName /
// MemberCount 为列表查询的关联列（只读）。
type GroupMember struct {
	SpaceID     uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	GroupID     uuid.UUID `gorm:"type:uuid;primaryKey" json:"group_id"`
	Role        string    `gorm:"size:16;not null" json:"role"`
	GroupName   string    `gorm:"->" json:"group_name,omitempty"`
	MemberCount int64     `gorm:"->" json:"member_count,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// GroupUser 为空间经用户组加入的用户条目（成员列表合并展示用）：
// 组来源（group_id/group_name/group_role，取该用户命中的最高组角色）
// + 用户展示信息（users 表 JOIN）。
type GroupUser struct {
	UserID    uuid.UUID `gorm:"column:user_id" json:"user_id"`
	GroupID   uuid.UUID `gorm:"column:group_id" json:"group_id"`
	GroupRole string    `gorm:"column:group_role" json:"group_role"`
	GroupName string    `gorm:"column:group_name" json:"group_name"`
	Username  string    `gorm:"column:username" json:"username,omitempty"`
	Nickname  string    `gorm:"column:nickname" json:"nickname,omitempty"`
	Email     string    `gorm:"column:email" json:"email,omitempty"`
}

// TableName 显式映射 space_group_members。
func (GroupMember) TableName() string { return "space_group_members" }

// Invite 对应 space_invites 表（migration 040）：空间邮箱邀请。
// TokenHash 为明文 token 的 SHA-256 hex（明文只在创建响应返回一次）；
// email 已小写归一；撤销 = 删行。
type Invite struct {
	ID         uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	SpaceID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"space_id"`
	Email      string     `gorm:"size:320;not null" json:"email"`
	Role       string     `gorm:"size:16;not null;default:member" json:"role"`
	TokenHash  string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	InvitedBy  *uuid.UUID `gorm:"type:uuid" json:"invited_by"`
	ExpiresAt  time.Time  `gorm:"not null" json:"expires_at"`
	AcceptedAt *time.Time `json:"accepted_at"`
	CreatedAt  time.Time  `gorm:"not null" json:"created_at"`
}

// TableName 显式映射 space_invites。
func (Invite) TableName() string { return "space_invites" }

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
