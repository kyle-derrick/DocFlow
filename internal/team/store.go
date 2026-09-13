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
// 权限判定（CanWrite/UserInAnyTeam/Role）集中在此接口，便于以后替换为 Casbin 等策略引擎。
type Repo interface {
	// CreateTeamWithRoot 在同一事务内写入团队、owner 成员与团队根目录。
	CreateTeamWithRoot(t Team, owner Member, root files.File) error
	Get(id uuid.UUID) (Team, error)
	// ListForUser 返回用户所属（成员或 owner，创建者自动为成员）的团队。
	ListForUser(userID uuid.UUID) ([]Team, error)
	AddMember(m Member) error
	RemoveMember(teamID, userID uuid.UUID) error
	ListMembers(teamID uuid.UUID) ([]Member, error)
	// Role 返回用户在团队中的角色，非成员返回空串。
	Role(teamID, userID uuid.UUID) (string, error)
	// CanWrite 判定用户能否写入团队空间（owner/editor）。
	CanWrite(userID, teamID uuid.UUID) (bool, error)
	// UserInAnyTeam 实时判定用户是否属于 teamIDs 中任一团队（供私有分享 share_teams 授权）。
	UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error)
}

// GormStore 是 Repo 的 PostgreSQL 实现（teams/team_members 表见 migrations/008_teams_shares.sql）。
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
	err := s.db.First(&t, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Team{}, ErrNotFound
	}
	return t, err
}

func (s *GormStore) ListForUser(userID uuid.UUID) ([]Team, error) {
	var out []Team
	// 创建团队时 owner 自动写入 team_members，成员关系即访问关系。
	err := s.db.
		Where("id IN (SELECT team_id FROM team_members WHERE user_id = ?)", userID).
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
	err := s.db.Where("team_id = ?", teamID).Order("created_at, user_id").Find(&out).Error
	return out, err
}

func (s *GormStore) Role(teamID, userID uuid.UUID) (string, error) {
	var m Member
	err := s.db.Where("team_id = ? AND user_id = ?", teamID, userID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return m.Role, nil
}

func (s *GormStore) CanWrite(userID, teamID uuid.UUID) (bool, error) {
	var count int64
	err := s.db.Model(&Member{}).
		Where("team_id = ? AND user_id = ? AND role IN (?, ?)", teamID, userID, RoleOwner, RoleEditor).
		Count(&count).Error
	return count > 0, err
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
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}
