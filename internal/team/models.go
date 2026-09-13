package team

import (
	"time"

	"github.com/google/uuid"
)

// 团队内角色（team_members.role CHECK 约束一致）：
// owner 拥有团队全部管理权（即 teams.owner_id 对应的创建者成员）；
// editor 可读写团队空间；viewer 仅只读。
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// Team 对应 teams 表（migrations/008_teams_shares.sql）。
type Team struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string    `gorm:"size:100;uniqueIndex;not null" json:"name"`
	Description string    `json:"description"`
	OwnerID     uuid.UUID `gorm:"type:uuid;not null;index" json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// Member 对应 team_members 表，复合主键 (team_id, user_id)。
type Member struct {
	TeamID    uuid.UUID `gorm:"type:uuid;primaryKey" json:"-"`
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey" json:"user_id"`
	Role      string    `gorm:"size:16;not null" json:"role"`
	CreatedAt time.Time `json:"created_at"`
}
