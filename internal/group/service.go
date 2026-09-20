package group

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

var (
	ErrInvalidName  = errors.New("invalid group name")
	ErrNameConflict = errors.New("group name already exists")
	ErrNotFound     = errors.New("group not found")
	ErrMemberExists = errors.New("user is already a group member")
)

// validateName 校验组名：TrimSpace、非空、无控制字符、不超过 100 字符
// （与空间名同规则，见 space.validateName）。
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 {
		return "", ErrInvalidName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", ErrInvalidName
		}
	}
	return name, nil
}

// Service 提供用户组 CRUD 与成员管理。管理权限（仅 admin）由 HTTP 层
// /admin 路由组的 RequireRole(admin) 保证，Service 不按 actor 二次校验
// （组无 owner 概念，与 space.Service 的 owner 校验区分）。
type Service struct {
	repo Repo
	now  func() time.Time
}

func NewService(repo Repo) *Service { return &Service{repo: repo, now: time.Now} }

// Create 创建用户组；组名冲突返回 ErrNameConflict。
func (s *Service) Create(name, description string, createdBy uuid.UUID) (Group, error) {
	n, err := validateName(name)
	if err != nil {
		return Group{}, err
	}
	now := s.now()
	g := Group{ID: uuid.New(), Name: n, Description: strings.TrimSpace(description), CreatedBy: createdBy, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.Create(g); err != nil {
		return Group{}, err
	}
	return g, nil
}

// List 返回全部组（含 member_count 聚合）。
func (s *Service) List() ([]Group, error) { return s.repo.List() }

// Get 返回组信息；不存在返回 ErrNotFound。
func (s *Service) Get(id uuid.UUID) (Group, error) { return s.repo.Get(id) }

// Update 按指针语义更新组名/描述（nil 表示不更新；两者均 nil 为无操作）；
// 返回更新后的组。组不存在返回 ErrNotFound。
func (s *Service) Update(id uuid.UUID, name, description *string) (Group, error) {
	current, err := s.repo.Get(id)
	if err != nil {
		return Group{}, err
	}
	if name == nil && description == nil {
		return current, nil
	}
	nextName := current.Name
	if name != nil {
		n, err := validateName(*name)
		if err != nil {
			return Group{}, err
		}
		nextName = n
	}
	var nextDesc *string
	if description != nil {
		d := strings.TrimSpace(*description)
		nextDesc = &d
	}
	if err := s.repo.Update(id, nextName, nextDesc); err != nil {
		return Group{}, err
	}
	return s.repo.Get(id)
}

// Delete 物理删除组；group_members 级联清空（组不删用户）。
func (s *Service) Delete(id uuid.UUID) error { return s.repo.Delete(id) }

// ListMembers 返回组成员列表；组不存在返回 ErrNotFound。
func (s *Service) ListMembers(id uuid.UUID) ([]Member, error) {
	if _, err := s.repo.Get(id); err != nil {
		return nil, err
	}
	return s.repo.ListMembers(id)
}

// AddMember 添加成员；组不存在返回 ErrNotFound，已在组返回 ErrMemberExists
// （用户存在性由 HTTP 层先行校验，见 adminAddGroupMember）。
func (s *Service) AddMember(id, userID uuid.UUID) (Member, error) {
	if _, err := s.repo.Get(id); err != nil {
		return Member{}, err
	}
	if userID == uuid.Nil {
		return Member{}, ErrNotFound
	}
	m := Member{GroupID: id, UserID: userID, JoinedAt: s.now()}
	if err := s.repo.AddMember(m); err != nil {
		return Member{}, err
	}
	// 回读以携带 username/nickname（展示列）；回读失败退回构造值（不阻断添加）。
	members, err := s.repo.ListMembers(id)
	if err != nil {
		return m, nil
	}
	for _, item := range members {
		if item.UserID == userID {
			return item, nil
		}
	}
	return m, nil
}

// RemoveMember 移除成员；组或成员不存在返回 ErrNotFound。
func (s *Service) RemoveMember(id, userID uuid.UUID) error {
	if _, err := s.repo.Get(id); err != nil {
		return err
	}
	return s.repo.RemoveMember(id, userID)
}

// NamesForUsers 批量返回用户 → 组名列表（管理端用户列表 group_names 聚合；
// 按加入时间升序，与 MembershipsForUsers 排序一致）。
func (s *Service) NamesForUsers(userIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	rows, err := s.repo.MembershipsForUsers(userIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]string, len(rows))
	for _, row := range rows {
		out[row.UserID] = append(out[row.UserID], row.GroupName)
	}
	return out, nil
}
