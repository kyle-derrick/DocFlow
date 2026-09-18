package team

import (
	"time"

	"github.com/google/uuid"
)

// 团队内角色（team_members.role CHECK 约束一致）：
// owner 拥有团队全部管理权（即 teams.owner_id 对应的创建者成员）；
// editor 可读写团队空间；viewer 仅只读；custom 表示绑定 roles(id) 的自定义角色
// （role_id 非空，权限按 roles.permissions JSON 求值，migration 029）。
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
	RoleCustom = "custom"
)

// 权限动作（设计 6.5.2 细粒度勾选项；deny 数组可列出任一动作以显式拒绝）。
const (
	PermRead   = "read"
	PermWrite  = "write"
	PermDelete = "delete"
	PermShare  = "share"
	PermAdmin  = "admin"
)

// ValidActions 全部合法权限动作（deny 校验与序列化共用）。
var ValidActions = []string{PermRead, PermWrite, PermDelete, PermShare, PermAdmin}

// Team 对应 teams 表（migrations/008_teams_shares.sql）。
type Team struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string     `gorm:"size:100;uniqueIndex;not null" json:"name"`
	Description string     `json:"description"`
	OwnerID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// Role 对应 roles 表（migrations/026）；permissions 结构：
// {read,write,delete,share,admin: bool, deny: [read|write|delete|share|admin]}，
// deny 中列出的权限显式拒绝且优先于 allow（设计 6.5 显式 deny 规则）。
// MemberCount 为列表查询的聚合列（子查询统计引用该角色的成员数，只读）。
type Role struct {
	ID          uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	TeamID      uuid.UUID      `gorm:"type:uuid;not null;index" json:"team_id"`
	Name        string         `gorm:"size:64;not null" json:"name"`
	Permissions map[string]any `gorm:"serializer:json;type:jsonb" json:"permissions"`
	CreatedAt   time.Time      `json:"created_at"`
	MemberCount int64          `gorm:"->" json:"member_count"`
}

// Member 对应 team_members 表，复合主键 (team_id, user_id)。
// Role 为系统角色（owner/editor/viewer）或 'custom'；RoleID 非空时表示
// 自定义角色（migration 029 的 CHECK 保证 custom 与 role_id 一一对应）。
// RoleName 为列表查询的关联列（roles.name，只读），供前端展示自定义角色名；
// Username/Nickname 为列表查询的关联列（users 表，只读），供成员列表展示
// 用户名（替代 UUID，见 GormStore.ListMembers 的 JOIN）。
type Member struct {
	TeamID    uuid.UUID  `gorm:"type:uuid;primaryKey" json:"-"`
	UserID    uuid.UUID  `gorm:"type:uuid;primaryKey" json:"user_id"`
	Role      string     `gorm:"size:16;not null" json:"role"`
	RoleID    *uuid.UUID `gorm:"type:uuid" json:"role_id,omitempty"`
	RoleName  string     `gorm:"->" json:"role_name,omitempty"`
	Username  string     `gorm:"->" json:"username,omitempty"`
	Nickname  string     `gorm:"->" json:"nickname,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// TableName 显式映射 team_members（gorm 默认复数化为 members，与
// migrations/008 的表名不符——运行时才会暴露）。
func (Member) TableName() string { return "team_members" }
