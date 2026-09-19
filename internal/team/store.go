package team

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/files"
)

var _ Repo = (*GormStore)(nil)

// Repo 是团队持久化与权限查询接口；GormStore 为 PostgreSQL 实现，MemoryStore 供测试使用。
// 权限判定所需的成员角色查询（Role）集中在此接口，五级内置角色的动作求值
// 在 Service 层完成；未来可替换为 Casbin 等策略引擎。
type Repo interface {
	// CreateTeamWithRoot 在同一事务内写入团队、owner 成员与团队根目录。
	CreateTeamWithRoot(t Team, owner Member, root files.File) error
	Get(id uuid.UUID) (Team, error)
	// ListForUser 返回用户所属（成员或 owner，创建者自动为成员）的团队。
	ListForUser(userID uuid.UUID) ([]Team, error)
	// ListForUserInfo 返回用户所属团队的列表条目（含我的角色/成员数/存储
	// 用量，GET /teams 响应；单 SQL 子查询聚合，无 N+1）。
	ListForUserStats(userID uuid.UUID) ([]TeamInfo, error)
	AddMember(m Member) error
	RemoveMember(teamID, userID uuid.UUID) error
	ListMembers(teamID uuid.UUID) ([]Member, error)
	// UpdateMemberRole 修改成员角色（五级内置角色）；成员不存在返回 ErrNotFound。
	UpdateMemberRole(teamID, userID uuid.UUID, role string) (Member, error)
	// Role 返回用户在团队中的角色，非成员返回空串。
	Role(teamID, userID uuid.UUID) (string, error)
	// TransferOwnership 同一事务内转让所有权：teams.owner_id 更新、新 owner
	// 成员角色置 owner、原 owner 置 admin。
	TransferOwnership(teamID, oldOwner, newOwner uuid.UUID) error
	// UserInAnyTeam 实时判定用户是否属于 teamIDs 中任一团队（供私有分享 share_teams 授权）。
	UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error)
	Update(teamID uuid.UUID, name string, description *string) error
	Delete(teamID uuid.UUID) error
	// ---- 团队邀请（team_invites，migration 039；v1.7.1 成员管理完善） ----
	CreateInvite(v Invite) error
	GetInvite(id uuid.UUID) (Invite, error)
	// GetInviteByTokenHash 凭 token 哈希取邀请（接受入口）。
	GetInviteByTokenHash(hash string) (Invite, error)
	// FindActiveInvite 返回该团队发给 email 的未过期未接受邀请（幂等创建探测）。
	FindActiveInvite(teamID uuid.UUID, email string, now time.Time) (Invite, error)
	// ListInvites 返回团队邀请（created_at 倒序，最多 limit 条）。
	ListInvites(teamID uuid.UUID, limit int) ([]Invite, error)
	// DeleteInvite 删除邀请（撤销；token 随之不可用）。限定团队归属，
	// 不存在返回 ErrNotFound。
	DeleteInvite(teamID, id uuid.UUID) error
	// MarkInviteAccepted 原子标记 accepted_at（仅未接受且未过期时成功）。
	MarkInviteAccepted(id uuid.UUID, now time.Time) (bool, error)
}

// GormStore 是 Repo 的 PostgreSQL 实现（teams/team_members 表见
// migrations/008_teams_shares.sql；五级内置角色见 migration 037）。
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

// ListForUserStats 返回用户所属团队的列表条目：JOIN team_members 取我的
// 角色，member_count / storage_used 经相关子查询聚合（每团队一行，无
// N+1；storage 口径 = 团队空间未软删文件的当前版本对象字节合计）。
func (s *GormStore) ListForUserStats(userID uuid.UUID) ([]TeamInfo, error) {
	var out []TeamInfo
	err := s.db.Raw(`
		SELECT t.id, t.name, t.description, t.owner_id, t.created_at, t.deleted_at,
		       tm.role AS my_role,
		       (SELECT COUNT(*) FROM team_members mc WHERE mc.team_id = t.id) AS member_count,
		       (SELECT COALESCE(SUM(fv.size), 0)
		          FROM files f JOIN file_versions fv ON fv.id = f.current_version_id
		         WHERE f.team_id = t.id AND f.scope_type = 'team'
		           AND f.deleted_at IS NULL AND f.is_root = false AND f.type = 'file') AS storage_used
		FROM teams t
		JOIN team_members tm ON tm.team_id = t.id AND tm.user_id = ?
		WHERE t.deleted_at IS NULL
		ORDER BY t.created_at DESC, t.id`, userID).Scan(&out).Error
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
	// JOIN users 补齐成员 username/nickname/email（成员列表展示用户名与
	// 邮箱，替代 UUID；用户行随账户删除时成员关系亦不保留，JOIN 不会
	// 引入丢行）。
	err := s.db.Model(&Member{}).
		Select("team_members.*, u.username AS username, COALESCE(u.nickname, '') AS nickname, u.email AS email").
		Joins("JOIN users u ON u.id = team_members.user_id").
		Where("team_members.team_id = ?", teamID).
		Order("team_members.created_at, team_members.user_id").Find(&out).Error
	return out, err
}

func (s *GormStore) UpdateMemberRole(teamID, userID uuid.UUID, role string) (Member, error) {
	result := s.db.Model(&Member{}).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Update("role", role)
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
	var m Member
	err := s.db.Select("role").Where("team_id = ? AND user_id = ?", teamID, userID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil // 非成员：空角色（无权限）
	}
	if err != nil {
		return "", err
	}
	return m.Role, nil
}

func (s *GormStore) TransferOwnership(teamID, oldOwner, newOwner uuid.UUID) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&Team{}).Where("id = ? AND deleted_at IS NULL AND owner_id = ?", teamID, oldOwner).
			Update("owner_id", newOwner)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Model(&Member{}).Where("team_id = ? AND user_id = ?", teamID, newOwner).
			Update("role", RoleOwner).Error; err != nil {
			return err
		}
		return tx.Model(&Member{}).Where("team_id = ? AND user_id = ?", teamID, oldOwner).
			Update("role", RoleAdmin).Error
	})
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

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}

// ---- 团队邀请（team_invites）----

func (s *GormStore) CreateInvite(v Invite) error {
	return s.db.Create(&v).Error
}

func (s *GormStore) GetInvite(id uuid.UUID) (Invite, error) {
	var v Invite
	err := s.db.First(&v, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invite{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) GetInviteByTokenHash(hash string) (Invite, error) {
	var v Invite
	err := s.db.First(&v, "token_hash = ?", hash).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invite{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) FindActiveInvite(teamID uuid.UUID, email string, now time.Time) (Invite, error) {
	var v Invite
	err := s.db.Where("team_id = ? AND email = ? AND accepted_at IS NULL AND expires_at > ?", teamID, email, now).
		First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invite{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) ListInvites(teamID uuid.UUID, limit int) ([]Invite, error) {
	var out []Invite
	err := s.db.Where("team_id = ?", teamID).Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

func (s *GormStore) DeleteInvite(teamID, id uuid.UUID) error {
	result := s.db.Where("id = ? AND team_id = ?", id, teamID).Delete(&Invite{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *GormStore) MarkInviteAccepted(id uuid.UUID, now time.Time) (bool, error) {
	result := s.db.Model(&Invite{}).
		Where("id = ? AND accepted_at IS NULL AND expires_at > ?", id, now).
		Update("accepted_at", now)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}
