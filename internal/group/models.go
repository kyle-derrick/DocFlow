package group

import (
	"time"

	"github.com/google/uuid"
)

// Group 对应 groups 表（migrations/035）：管理端用户组（仅 admin 管理），
// 组织维度的人员集合——不挂文件空间、不参与文件权限判定，与团队
// （teams，协作空间）互补。MemberCount 为列表查询的聚合列（只读）。
type Group struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string    `gorm:"size:100;uniqueIndex;not null" json:"name"`
	Description string    `json:"description"`
	// CreatedBy 为创建该组的管理员（审计/展示用途，不参与权限判定）。
	CreatedBy   uuid.UUID `gorm:"type:uuid;not null;index" json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	MemberCount int64     `gorm:"->" json:"member_count"`
}

// Member 对应 group_members 表，复合主键 (group_id, user_id)。
// Username/Nickname 为列表查询 JOIN users 补齐的展示列（只读），
// 供成员列表展示用户名（替代 UUID，模式同 team_members）。
type Member struct {
	GroupID  uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	UserID   uuid.UUID `gorm:"type:uuid;primaryKey" json:"user_id"`
	JoinedAt time.Time `json:"joined_at"`
	Username string    `gorm:"->" json:"username,omitempty"`
	Nickname string    `gorm:"->" json:"nickname,omitempty"`
}

// TableName 显式映射 group_members（gorm 默认复数化为 members，与
// migrations/035 的表名不符——运行时才会暴露）。
func (Member) TableName() string { return "group_members" }

// Membership 为管理端用户列表聚合的 (user_id, group_id, group_name) 行
// （MembershipsForUsers 只读查询结果）。
type Membership struct {
	UserID    uuid.UUID `gorm:"type:uuid" json:"user_id"`
	GroupID   uuid.UUID `gorm:"type:uuid" json:"group_id"`
	GroupName string    `json:"group_name"`
}
