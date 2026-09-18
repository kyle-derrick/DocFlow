package group

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var _ Repo = (*GormStore)(nil)

// Repo 是用户组持久化接口；GormStore 为 PostgreSQL 实现（groups/group_members
// 表见 migrations/035），MemoryStore 供测试使用。管理权限（仅 admin）由
// HTTP 层 /admin 路由组保证，Repo 不做属主校验（组无 owner 概念）。
type Repo interface {
	Create(g Group) error
	Get(id uuid.UUID) (Group, error)
	// List 返回全部组（含 member_count 聚合），按创建时间升序。
	List() ([]Group, error)
	// Update 按需更新名称与描述；组不存在返回 ErrNotFound。
	Update(id uuid.UUID, name string, description *string) error
	// Delete 物理删除组（group_members 经 ON DELETE CASCADE 级联清空，不删用户）。
	Delete(id uuid.UUID) error
	// ListMembers 返回组成员（JOIN users 补齐 username/nickname 展示列）。
	ListMembers(groupID uuid.UUID) ([]Member, error)
	// AddMember 写入成员关系；(group_id, user_id) 冲突返回 ErrMemberExists。
	AddMember(m Member) error
	// RemoveMember 删除成员关系；成员不存在返回 ErrNotFound。
	RemoveMember(groupID, userID uuid.UUID) error
	// MembershipsForUsers 批量查询给定用户的组归属（管理端用户列表
	// group_names 聚合；userIDs 为空时直接返回空）。
	MembershipsForUsers(userIDs []uuid.UUID) ([]Membership, error)
}

// GormStore 是 Repo 的 PostgreSQL 实现。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

func (s *GormStore) Create(g Group) error {
	err := s.db.Create(&g).Error
	if err != nil && isUniqueViolation(err) {
		return ErrNameConflict
	}
	return err
}

func (s *GormStore) Get(id uuid.UUID) (Group, error) {
	var g Group
	err := s.db.Where("id = ?", id).First(&g).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Group{}, ErrNotFound
	}
	return g, err
}

func (s *GormStore) List() ([]Group, error) {
	var out []Group
	// member_count：子查询统计组成员数（列表展示与删除前确认共用）。
	err := s.db.Model(&Group{}).
		Select("groups.*, (SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = groups.id) AS member_count").
		Order("groups.created_at, groups.id").Find(&out).Error
	return out, err
}

func (s *GormStore) Update(id uuid.UUID, name string, description *string) error {
	fields := map[string]any{"name": name, "updated_at": gorm.Expr("CURRENT_TIMESTAMP")}
	if description != nil {
		fields["description"] = *description
	}
	result := s.db.Model(&Group{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil && isUniqueViolation(result.Error) {
		return ErrNameConflict
	}
	if result.Error == nil && result.RowsAffected == 0 {
		return ErrNotFound
	}
	return result.Error
}

func (s *GormStore) Delete(id uuid.UUID) error {
	// 硬删除：组是纯组织维度数据，无审计/引用完整性诉求（成员关系级联清理）。
	result := s.db.Where("id = ?", id).Delete(&Group{})
	if result.Error == nil && result.RowsAffected == 0 {
		return ErrNotFound
	}
	return result.Error
}

func (s *GormStore) ListMembers(groupID uuid.UUID) ([]Member, error) {
	var out []Member
	// JOIN users 补齐 username/nickname（成员列表展示用户名，替代 UUID；
	// 用户行随账户删除时成员关系亦不保留，JOIN 不会引入丢行——同 team_members）。
	err := s.db.Model(&Member{}).
		Select("group_members.*, u.username AS username, COALESCE(u.nickname, '') AS nickname").
		Joins("JOIN users u ON u.id = group_members.user_id").
		Where("group_members.group_id = ?", groupID).
		Order("group_members.joined_at, group_members.user_id").Find(&out).Error
	return out, err
}

func (s *GormStore) AddMember(m Member) error {
	err := s.db.Create(&m).Error
	if err != nil && isUniqueViolation(err) {
		return ErrMemberExists
	}
	return err
}

func (s *GormStore) RemoveMember(groupID, userID uuid.UUID) error {
	result := s.db.Where("group_id = ? AND user_id = ?", groupID, userID).Delete(&Member{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *GormStore) MembershipsForUsers(userIDs []uuid.UUID) ([]Membership, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	var out []Membership
	err := s.db.Model(&Member{}).
		Select("group_members.user_id, group_members.group_id, groups.name AS group_name").
		Joins("JOIN groups ON groups.id = group_members.group_id").
		Where("group_members.user_id IN ?", userIDs).
		Order("group_members.joined_at, groups.name").Find(&out).Error
	return out, err
}

// isUniqueViolation 识别唯一约束冲突（SQLSTATE 23505 或驱动错误文本）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}
