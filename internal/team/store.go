package team

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/files"
)

var _ Repo = (*GormStore)(nil)

// Repo 是团队持久化与权限查询接口；GormStore 为 PostgreSQL 实现，MemoryStore 供测试使用。
// 权限判定所需的成员/角色查询（MemberRole/RolePermissions）集中在此接口，
// 动作求值（deny 优先等）在 Service 层完成；未来可替换为 Casbin 等策略引擎。
type Repo interface {
	// CreateTeamWithRoot 在同一事务内写入团队、owner 成员与团队根目录。
	CreateTeamWithRoot(t Team, owner Member, root files.File) error
	Get(id uuid.UUID) (Team, error)
	// ListForUser 返回用户所属（成员或 owner，创建者自动为成员）的团队。
	ListForUser(userID uuid.UUID) ([]Team, error)
	AddMember(m Member) error
	RemoveMember(teamID, userID uuid.UUID) error
	ListMembers(teamID uuid.UUID) ([]Member, error)
	// UpdateMemberRole 修改成员角色（系统角色或自定义 role_id）；成员不存在返回 ErrNotFound。
	UpdateMemberRole(teamID, userID uuid.UUID, role string, roleID *uuid.UUID) (Member, error)
	// Role 返回用户在团队中的角色，非成员返回空串（自定义角色返回 'custom'）。
	Role(teamID, userID uuid.UUID) (string, error)
	// MemberRole 返回成员的角色字符串与自定义角色 ID（非成员空串 + nil）。
	MemberRole(teamID, userID uuid.UUID) (string, *uuid.UUID, error)
	// RolePermissions 返回自定义角色的 permissions JSON；角色不存在或不属于
	// 该团队返回 ErrNotFound。
	RolePermissions(teamID, roleID uuid.UUID) (map[string]any, error)
	// CountMembersByRole 统计引用该自定义角色的成员数（删除角色前的引用检查）。
	CountMembersByRole(teamID, roleID uuid.UUID) (int64, error)
	// UserInAnyTeam 实时判定用户是否属于 teamIDs 中任一团队（供私有分享 share_teams 授权）。
	UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error)
	Update(teamID uuid.UUID, name string, description *string) error
	Delete(teamID uuid.UUID) error
	// ListRoles 返回团队自定义角色（含 member_count 引用统计）。
	ListRoles(teamID uuid.UUID) ([]Role, error)
	CreateRole(role Role) error
	UpdateRole(teamID, roleID uuid.UUID, name string, permissions map[string]any) error
	DeleteRole(teamID, roleID uuid.UUID) error
}

// GormStore 是 Repo 的 PostgreSQL 实现（teams/team_members 表见 migrations/008_teams_shares.sql，
// roles 表与 team_members.role_id 见 migrations/026/029）。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (s *GormStore) CreateTeamWithRoot(t Team, owner Member, root files.File) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&t).Error; err != nil {
			return err
		}
		if err := tx.Create(&owner).Error; err != nil {
			return err
		}
		return tx.Create(&root).Error
	})
}

func (s *GormStore) Get(id uuid.UUID) (Team, error) {
	var t Team
	err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Team{}, ErrNotFound
	}
	return t, err
}

func (s *GormStore) ListForUser(userID uuid.UUID) ([]Team, error) {
	var out []Team
	// 创建团队时 owner 自动写入 team_members，成员关系即访问关系。
	err := s.db.
		Where("deleted_at IS NULL AND id IN (SELECT team_id FROM team_members WHERE user_id = ?)", userID).
		Order("created_at DESC, id").Find(&out).Error
	return out, err
}

func (s *GormStore) AddMember(m Member) error {
	err := s.db.Create(&m).Error
	if err != nil && isUniqueViolation(err) {
		return ErrMemberExists
	}
	return err
}

func (s *GormStore) RemoveMember(teamID, userID uuid.UUID) error {
	result := s.db.Where("team_id = ? AND user_id = ?", teamID, userID).Delete(&Member{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *GormStore) ListMembers(teamID uuid.UUID) ([]Member, error) {
	var out []Member
	// LEFT JOIN roles 解析自定义角色名（role_name，系统角色为空）。
	err := s.db.Model(&Member{}).
		Select("team_members.*, r.name AS role_name").
		Joins("LEFT JOIN roles r ON r.id = team_members.role_id").
		Where("team_members.team_id = ?", teamID).
		Order("team_members.created_at, team_members.user_id").Find(&out).Error
	return out, err
}

func (s *GormStore) UpdateMemberRole(teamID, userID uuid.UUID, role string, roleID *uuid.UUID) (Member, error) {
	result := s.db.Model(&Member{}).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Updates(map[string]any{"role": role, "role_id": roleID})
	if result.Error != nil {
		return Member{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Member{}, ErrNotFound
	}
	m, err := s.ListMembers(teamID)
	if err != nil {
		return Member{}, err
	}
	for _, item := range m {
		if item.UserID == userID {
			return item, nil
		}
	}
	return Member{}, ErrNotFound
}

func (s *GormStore) Role(teamID, userID uuid.UUID) (string, error) {
	role, _, err := s.MemberRole(teamID, userID)
	return role, err
}

func (s *GormStore) MemberRole(teamID, userID uuid.UUID) (string, *uuid.UUID, error) {
	var m Member
	err := s.db.Select("role", "role_id").
		Where("team_id = ? AND user_id = ?", teamID, userID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	return m.Role, m.RoleID, nil
}

func (s *GormStore) RolePermissions(teamID, roleID uuid.UUID) (map[string]any, error) {
	var r Role
	err := s.db.Select("permissions").
		Where("id = ? AND team_id = ?", roleID, teamID).First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.Permissions, nil
}

func (s *GormStore) CountMembersByRole(teamID, roleID uuid.UUID) (int64, error) {
	var count int64
	err := s.db.Model(&Member{}).
		Where("team_id = ? AND role_id = ?", teamID, roleID).Count(&count).Error
	return count, err
}

func (s *GormStore) UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error) {
	if len(teamIDs) == 0 {
		return false, nil
	}
	var count int64
	err := s.db.Model(&Member{}).
		Where("user_id = ? AND team_id IN ?", userID, teamIDs).
		Limit(1).Count(&count).Error
	return count > 0, err
}

// isUniqueViolation 识别唯一约束冲突（SQLSTATE 23505 或驱动错误文本）。
func (s *GormStore) Update(teamID uuid.UUID, name string, description *string) error {
	fields := map[string]any{"name": name}
	if description != nil {
		fields["description"] = *description
	}
	result := s.db.Model(&Team{}).Where("id = ? AND deleted_at IS NULL", teamID).Updates(fields)
	if result.Error != nil && isUniqueViolation(result.Error) {
		return ErrNameConflict
	}
	if result.Error == nil && result.RowsAffected == 0 {
		return ErrNotFound
	}
	return result.Error
}
func (s *GormStore) Delete(teamID uuid.UUID) error {
	result := s.db.Model(&Team{}).Where("id = ? AND deleted_at IS NULL", teamID).Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP"))
	if result.Error == nil && result.RowsAffected == 0 {
		return ErrNotFound
	}
	return result.Error
}
func (s *GormStore) ListRoles(teamID uuid.UUID) ([]Role, error) {
	var out []Role
	// member_count：引用该角色的成员数（删除角色的引用检查与前端展示共用）。
	err := s.db.Model(&Role{}).
		Select("roles.*, (SELECT COUNT(*) FROM team_members tm WHERE tm.role_id = roles.id) AS member_count").
		Where("team_id = ?", teamID).Order("created_at, id").Find(&out).Error
	return out, err
}
func (s *GormStore) CreateRole(role Role) error { return s.db.Create(&role).Error }
func (s *GormStore) UpdateRole(teamID, roleID uuid.UUID, name string, permissions map[string]any) error {
	r := s.db.Model(&Role{}).Where("id = ? AND team_id = ?", roleID, teamID).Updates(map[string]any{"name": name, "permissions": permissions})
	if r.Error != nil && isUniqueViolation(r.Error) {
		return ErrNameConflict
	}
	if r.Error == nil && r.RowsAffected == 0 {
		return ErrNotFound
	}
	return r.Error
}
func (s *GormStore) DeleteRole(teamID, roleID uuid.UUID) error {
	r := s.db.Where("id = ? AND team_id = ?", roleID, teamID).Delete(&Role{})
	if r.Error == nil && r.RowsAffected == 0 {
		return ErrNotFound
	}
	return r.Error
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}
