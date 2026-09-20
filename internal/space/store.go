package space

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/docflow/docflow/internal/files"
)

var _ Repo = (*GormStore)(nil)

// Repo 是空间持久化与权限查询接口；GormStore 为 PostgreSQL 实现，
// MemoryStore 供测试使用。权限判定所需的角色查询（Role = 直接成员 ∪
// 用户组取最高）集中在此接口，五级角色的动作求值在 Service 层完成。
type Repo interface {
	// CreateSpaceWithRoot 在同一事务内写入空间、owner 成员与空间根目录。
	CreateSpaceWithRoot(s Space, owner Member, root files.File) error
	Get(id uuid.UUID) (Space, error)
	// GetDefault 返回用户的未删除默认空间；不存在返回 ErrNotFound。
	GetDefault(owner uuid.UUID) (Space, error)
	// SpaceRoot 返回空间根目录（files.space_id、is_root）；缺失返回 ErrNotFound。
	SpaceRoot(spaceID uuid.UUID) (files.File, error)
	// ListForUser 返回用户可见的空间（直接成员或用户组命中，未软删）。
	ListForUser(userID uuid.UUID) ([]Space, error)
	// ListForUserStats 返回用户可见空间的列表条目（含我的角色/成员数/
	// 存储用量，GET /spaces 响应；单 SQL 聚合，无 N+1）。
	ListForUserStats(userID uuid.UUID) ([]SpaceInfo, error)
	// CountForUser 统计用户名下（owner_id 维度，未软删）空间数，
	// 供 max_spaces_per_user 校验（含默认空间）。
	CountForUser(owner uuid.UUID) (int64, error)
	// Update 按指针语义更新名称/描述/配额（nil 表示不更新）。
	Update(id uuid.UUID, name, description *string, quota *int64) error
	Delete(id uuid.UUID) error
	// 成员 CRUD。
	AddMember(m Member) error
	RemoveMember(spaceID, userID uuid.UUID) error
	ListMembers(spaceID uuid.UUID) ([]Member, error)
	UpdateMemberRole(spaceID, userID uuid.UUID, role string) (Member, error)
	// DirectRole 返回用户的直接成员角色（不含用户组）；非成员空串。
	DirectRole(spaceID, userID uuid.UUID) (string, error)
	// Role 返回用户的有效角色（直接成员 ∪ 用户组取最高）；非成员空串。
	Role(spaceID, userID uuid.UUID) (string, error)
	// MemberUserIDs 返回空间全部参与者用户 ID（直接成员 ∪ 组成员展开）。
	MemberUserIDs(spaceID uuid.UUID) ([]uuid.UUID, error)
	// TransferOwnership 同一事务内转让所有权：spaces.owner_id 更新、
	// 新 owner 成员角色置 owner、原 owner 置 admin。
	TransferOwnership(spaceID, oldOwner, newOwner uuid.UUID) error
	// UserInAnySpace 实时判定用户是否属于 spaceIDs 中任一空间
	//（直接成员或用户组，供私有分享 share_spaces 授权）。
	UserInAnySpace(userID uuid.UUID, spaceIDs []uuid.UUID) (bool, error)
	// 用户组授权 CRUD（space_group_members）。
	AddGroupMember(g GroupMember) error
	UpdateGroupMemberRole(spaceID, groupID uuid.UUID, role string) (GroupMember, error)
	RemoveGroupMember(spaceID, groupID uuid.UUID) error
	ListGroupMembers(spaceID uuid.UUID) ([]GroupMember, error)
	// GroupRole 返回空间内用户组角色；未授权返回 ErrNotFound。
	GroupRole(spaceID, groupID uuid.UUID) (string, error)
	// 邀请（space_invites）。
	CreateInvite(v Invite) error
	GetInviteByTokenHash(hash string) (Invite, error)
	FindActiveInvite(spaceID uuid.UUID, email string, now time.Time) (Invite, error)
	ListInvites(spaceID uuid.UUID, limit int) ([]Invite, error)
	DeleteInvite(spaceID, id uuid.UUID) error
	MarkInviteAccepted(id uuid.UUID, now time.Time) (bool, error)
}

// GormStore 是 Repo 的 PostgreSQL 实现（spaces/space_members/
// space_group_members/space_invites 表见 migration 040）。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (s *GormStore) CreateSpaceWithRoot(sp Space, owner Member, root files.File) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&sp).Error; err != nil {
			return err
		}
		if err := tx.Create(&owner).Error; err != nil {
			return err
		}
		return tx.Create(&root).Error
	})
}

func (s *GormStore) Get(id uuid.UUID) (Space, error) {
	var sp Space
	err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&sp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Space{}, ErrNotFound
	}
	return sp, err
}

func (s *GormStore) GetDefault(owner uuid.UUID) (Space, error) {
	var sp Space
	err := s.db.Where("owner_id = ? AND is_default AND deleted_at IS NULL", owner).First(&sp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Space{}, ErrNotFound
	}
	return sp, err
}

func (s *GormStore) SpaceRoot(spaceID uuid.UUID) (files.File, error) {
	var f files.File
	err := s.db.Where("space_id = ? AND is_root = true AND deleted_at IS NULL", spaceID).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return files.File{}, files.ErrNotFound
	}
	return f, err
}

// visibleSpacesSQL 返回「用户可见空间 ID 集」的 SQL 片段（直接成员 ∪
// 用户组命中）与参数：EXISTS (space_members) OR EXISTS (space_group_members
// JOIN group_members)。
func visibleSpacesSQL(user uuid.UUID) (string, []any) {
	return `EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = s.id AND sm.user_id = ?)
        OR EXISTS (SELECT 1 FROM space_group_members sgm
                   JOIN group_members gm ON gm.group_id = sgm.group_id
                   WHERE sgm.space_id = s.id AND gm.user_id = ?)`, []any{user, user}
}

// roleLevelSQL 把角色列名映射为等级表达式（取最高用）。
func roleLevelSQL(column string) string {
	return `CASE ` + column + ` WHEN 'owner' THEN 4 WHEN 'admin' THEN 3
	          WHEN 'member_share' THEN 2 WHEN 'member' THEN 1 WHEN 'guest' THEN 0 ELSE -1 END`
}

func (s *GormStore) ListForUser(userID uuid.UUID) ([]Space, error) {
	var out []Space
	cond, args := visibleSpacesSQL(userID)
	err := s.db.
		Where("deleted_at IS NULL AND ("+cond+")", args...).
		Order("is_default DESC, created_at DESC, id").Find(&out).Error
	return out, err
}

// ListForUserStats 返回用户可见空间的列表条目：my_role = 直接成员角色与
// 用户组角色取最高（等级表达式 GREATEST）；member_count / storage_used
// 经相关子查询聚合（每空间一行，无 N+1；storage 口径 = 空间内未软删
// 文件的当前版本对象字节合计）。
func (s *GormStore) ListForUserStats(userID uuid.UUID) ([]SpaceInfo, error) {
	var out []SpaceInfo
	err := s.db.Raw(`
		WITH direct AS (
			SELECT space_id, `+roleLevelSQL("role")+` AS lvl
			FROM space_members WHERE user_id = ?
		), viagroup AS (
			SELECT sgm.space_id, MAX(`+roleLevelSQL("sgm.role")+`) AS lvl
			FROM space_group_members sgm
			JOIN group_members gm ON gm.group_id = sgm.group_id
			WHERE gm.user_id = ?
			GROUP BY sgm.space_id
		), myrole AS (
			SELECT d.space_id, GREATEST(d.lvl, COALESCE(v.lvl, -1)) AS lvl
			FROM direct d LEFT JOIN viagroup v ON v.space_id = d.space_id
			UNION ALL
			SELECT v.space_id, v.lvl FROM viagroup v
			WHERE NOT EXISTS (SELECT 1 FROM direct d WHERE d.space_id = v.space_id)
		), best AS (
			SELECT space_id, MAX(lvl) AS lvl FROM myrole GROUP BY space_id
		)
		SELECT s.id, s.name, s.description, s.quota_bytes, s.owner_id,
		       s.is_default, s.created_at, s.deleted_at,
		       CASE b.lvl WHEN 4 THEN 'owner' WHEN 3 THEN 'admin'
		                  WHEN 2 THEN 'member_share' WHEN 1 THEN 'member'
		                  WHEN 0 THEN 'guest' ELSE '' END AS my_role,
		       (SELECT COUNT(*) FROM space_members mc WHERE mc.space_id = s.id) AS member_count,
		       (SELECT COALESCE(SUM(fv.size), 0)
		          FROM files f JOIN file_versions fv ON fv.id = f.current_version_id
		         WHERE f.space_id = s.id
		           AND f.deleted_at IS NULL AND f.is_root = false AND f.type = 'file') AS storage_used
		FROM spaces s
		JOIN best b ON b.space_id = s.id
		WHERE s.deleted_at IS NULL
		ORDER BY s.is_default DESC, s.created_at DESC, s.id`, userID, userID).Scan(&out).Error
	return out, err
}

func (s *GormStore) CountForUser(owner uuid.UUID) (int64, error) {
	var count int64
	err := s.db.Model(&Space{}).Where("owner_id = ? AND deleted_at IS NULL", owner).Count(&count).Error
	return count, err
}

func (s *GormStore) Update(id uuid.UUID, name, description *string, quota *int64) error {
	fields := map[string]any{}
	if name != nil {
		fields["name"] = *name
	}
	if description != nil {
		fields["description"] = *description
	}
	if quota != nil {
		fields["quota_bytes"] = *quota
	}
	if len(fields) == 0 {
		return nil
	}
	result := s.db.Model(&Space{}).Where("id = ? AND deleted_at IS NULL", id).Updates(fields)
	if result.Error == nil && result.RowsAffected == 0 {
		return ErrNotFound
	}
	return result.Error
}

func (s *GormStore) Delete(id uuid.UUID) error {
	result := s.db.Model(&Space{}).Where("id = ? AND deleted_at IS NULL", id).Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP"))
	if result.Error == nil && result.RowsAffected == 0 {
		return ErrNotFound
	}
	return result.Error
}

// GetAny 返回空间（含已软删的「已解散」空间）；不存在返回 ErrNotFound。
func (s *GormStore) GetAny(id uuid.UUID) (Space, error) {
	var sp Space
	err := s.db.Where("id = ?", id).First(&sp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Space{}, ErrNotFound
	}
	return sp, err
}

// DeletePermanent 物理删除空间行（成员/用户组授权/邀请经外键级联清除）。
func (s *GormStore) DeletePermanent(id uuid.UUID) error {
	return s.db.Where("id = ?", id).Delete(&Space{}).Error
}

func (s *GormStore) AddMember(m Member) error {
	err := s.db.Create(&m).Error
	if err != nil && isUniqueViolation(err) {
		return ErrMemberExists
	}
	return err
}

func (s *GormStore) RemoveMember(spaceID, userID uuid.UUID) error {
	result := s.db.Where("space_id = ? AND user_id = ?", spaceID, userID).Delete(&Member{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *GormStore) ListMembers(spaceID uuid.UUID) ([]Member, error) {
	var out []Member
	// JOIN users 补齐成员 username/nickname/email（展示替代 UUID）。
	err := s.db.Model(&Member{}).
		Select("space_members.*, u.username AS username, COALESCE(u.nickname, '') AS nickname, u.email AS email").
		Joins("JOIN users u ON u.id = space_members.user_id").
		Where("space_members.space_id = ?", spaceID).
		Order("space_members.created_at, space_members.user_id").Find(&out).Error
	return out, err
}

// ListGroupUsers 返回空间经用户组加入的用户条目（组来源 + 用户展示信息；
// 同一用户命中多个组时每个组一行，由调用方聚合）。
func (s *GormStore) ListGroupUsers(spaceID uuid.UUID) ([]GroupUser, error) {
	var out []GroupUser
	err := s.db.Raw(`
		SELECT gm.user_id, sgm.group_id, sgm.role AS group_role,
		       COALESCE(g.name, '') AS group_name,
		       COALESCE(u.username, '') AS username,
		       COALESCE(u.nickname, '') AS nickname,
		       COALESCE(u.email, '') AS email
		FROM space_group_members sgm
		JOIN group_members gm ON gm.group_id = sgm.group_id
		JOIN groups g ON g.id = sgm.group_id
		LEFT JOIN users u ON u.id = gm.user_id
		WHERE sgm.space_id = ?
		ORDER BY g.name, u.username, gm.user_id`, spaceID).Scan(&out).Error
	return out, err
}

func (s *GormStore) UpdateMemberRole(spaceID, userID uuid.UUID, role string) (Member, error) {
	result := s.db.Model(&Member{}).
		Where("space_id = ? AND user_id = ?", spaceID, userID).
		Update("role", role)
	if result.Error != nil {
		return Member{}, result.Error
	}
	if result.RowsAffected == 0 {
		return Member{}, ErrNotFound
	}
	m, err := s.ListMembers(spaceID)
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

func (s *GormStore) DirectRole(spaceID, userID uuid.UUID) (string, error) {
	var m Member
	err := s.db.Select("role").Where("space_id = ? AND user_id = ?", spaceID, userID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil // 非直接成员
	}
	if err != nil {
		return "", err
	}
	return m.Role, nil
}

// Role 返回有效角色：直接成员角色与用户组内最高角色取等级更高者。
func (s *GormStore) Role(spaceID, userID uuid.UUID) (string, error) {
	direct, err := s.DirectRole(spaceID, userID)
	if err != nil {
		return "", err
	}
	var groupLvl *int
	if err := s.db.Raw(`
		SELECT MAX(`+roleLevelSQL("sgm.role")+`) FROM space_group_members sgm
		JOIN group_members gm ON gm.group_id = sgm.group_id
		WHERE sgm.space_id = ? AND gm.user_id = ?`, spaceID, userID).Scan(&groupLvl).Error; err != nil {
		return "", err
	}
	if groupLvl == nil || *groupLvl < 0 {
		return direct, nil
	}
	// 等级 → 角色。
	roles := map[int]string{4: RoleOwner, 3: RoleAdmin, 2: RoleMemberShare, 1: RoleMember, 0: RoleGuest}
	viaGroup, ok := roles[*groupLvl]
	if !ok {
		return direct, nil
	}
	return HigherRole(direct, viaGroup), nil
}

func (s *GormStore) MemberUserIDs(spaceID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.db.Raw(`
		SELECT user_id FROM space_members WHERE space_id = ?
		UNION
		SELECT gm.user_id FROM space_group_members sgm
		JOIN group_members gm ON gm.group_id = sgm.group_id
		WHERE sgm.space_id = ?`, spaceID, spaceID).Scan(&ids).Error
	return ids, err
}

func (s *GormStore) TransferOwnership(spaceID, oldOwner, newOwner uuid.UUID) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&Space{}).Where("id = ? AND deleted_at IS NULL AND owner_id = ?", spaceID, oldOwner).
			Update("owner_id", newOwner)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Model(&Member{}).Where("space_id = ? AND user_id = ?", spaceID, newOwner).
			Update("role", RoleOwner).Error; err != nil {
			return err
		}
		return tx.Model(&Member{}).Where("space_id = ? AND user_id = ?", spaceID, oldOwner).
			Update("role", RoleAdmin).Error
	})
}

func (s *GormStore) UserInAnySpace(userID uuid.UUID, spaceIDs []uuid.UUID) (bool, error) {
	if len(spaceIDs) == 0 {
		return false, nil
	}
	var count int64
	err := s.db.Raw(`
		SELECT COUNT(*) FROM (
			SELECT 1 FROM space_members WHERE user_id = ? AND space_id IN ?
			UNION
			SELECT 1 FROM space_group_members sgm
			JOIN group_members gm ON gm.group_id = sgm.group_id
			WHERE gm.user_id = ? AND sgm.space_id IN ?
		) t`, userID, spaceIDs, userID, spaceIDs).Scan(&count).Error
	return count > 0, err
}

// ---- 用户组授权（space_group_members） ----

func (s *GormStore) AddGroupMember(g GroupMember) error {
	err := s.db.Create(&g).Error
	if err != nil && isUniqueViolation(err) {
		return ErrGroupExists
	}
	if err != nil && (isForeignKeyViolation(err)) {
		return ErrGroupNotFound
	}
	return err
}

func (s *GormStore) UpdateGroupMemberRole(spaceID, groupID uuid.UUID, role string) (GroupMember, error) {
	result := s.db.Model(&GroupMember{}).
		Where("space_id = ? AND group_id = ?", spaceID, groupID).
		Update("role", role)
	if result.Error != nil {
		return GroupMember{}, result.Error
	}
	if result.RowsAffected == 0 {
		return GroupMember{}, ErrNotFound
	}
	groups, err := s.ListGroupMembers(spaceID)
	if err != nil {
		return GroupMember{}, err
	}
	for _, item := range groups {
		if item.GroupID == groupID {
			return item, nil
		}
	}
	return GroupMember{}, ErrNotFound
}

func (s *GormStore) RemoveGroupMember(spaceID, groupID uuid.UUID) error {
	result := s.db.Where("space_id = ? AND group_id = ?", spaceID, groupID).Delete(&GroupMember{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *GormStore) ListGroupMembers(spaceID uuid.UUID) ([]GroupMember, error) {
	var out []GroupMember
	err := s.db.Model(&GroupMember{}).
		Select("space_group_members.*, g.name AS group_name, "+
			"(SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = space_group_members.group_id) AS member_count").
		Joins("JOIN groups g ON g.id = space_group_members.group_id").
		Where("space_group_members.space_id = ?", spaceID).
		Order("space_group_members.created_at, space_group_members.group_id").Find(&out).Error
	return out, err
}

func (s *GormStore) GroupRole(spaceID, groupID uuid.UUID) (string, error) {
	var g GroupMember
	err := s.db.Select("role").Where("space_id = ? AND group_id = ?", spaceID, groupID).First(&g).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return g.Role, nil
}

// ---- 邀请（space_invites） ----

func (s *GormStore) CreateInvite(v Invite) error {
	return s.db.Create(&v).Error
}

func (s *GormStore) GetInviteByTokenHash(hash string) (Invite, error) {
	var v Invite
	err := s.db.First(&v, "token_hash = ?", hash).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invite{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) FindActiveInvite(spaceID uuid.UUID, email string, now time.Time) (Invite, error) {
	var v Invite
	err := s.db.Where("space_id = ? AND email = ? AND accepted_at IS NULL AND expires_at > ?", spaceID, email, now).
		First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Invite{}, ErrNotFound
	}
	return v, err
}

func (s *GormStore) ListInvites(spaceID uuid.UUID, limit int) ([]Invite, error) {
	var out []Invite
	err := s.db.Where("space_id = ?", spaceID).Order("created_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

func (s *GormStore) DeleteInvite(spaceID, id uuid.UUID) error {
	result := s.db.Where("id = ? AND space_id = ?", id, spaceID).Delete(&Invite{})
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

// isUniqueViolation 识别唯一约束冲突（SQLSTATE 23505 或驱动错误文本）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}

// isForeignKeyViolation 识别外键冲突（SQLSTATE 23503：组不存在）。
func isForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "foreign key") ||
		strings.Contains(strings.ToLower(err.Error()), "violates foreign key constraint")
}
