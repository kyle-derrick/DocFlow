package team

import (
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

var (
	ErrInvalidName       = errors.New("invalid team name")
	ErrNameConflict      = errors.New("team name already exists")
	ErrNotFound          = errors.New("team not found")
	ErrInvalidRole       = errors.New("invalid member role")
	ErrInvalidPermission = errors.New("invalid role permissions")
	ErrRoleInUse         = errors.New("role is assigned to team members")
	ErrForbidden         = errors.New("not allowed to manage this team")
	ErrMemberExists      = errors.New("user is already a member")
	ErrOwnerMember       = errors.New("team owner membership cannot be removed")
)

// validateName 校验团队名：NFC 归一（简化为 TrimSpace）、非空、无控制字符、不超过 100 字符。
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

// ValidatePermissions 校验角色 permissions JSON 结构：键只允许
// read/write/delete/share/admin（布尔值）与 deny（合法动作字符串数组）；
// 其余键或类型不匹配一律拒绝（fail closed）。
func ValidatePermissions(p map[string]any) error {
	for k, v := range p {
		switch k {
		case PermRead, PermWrite, PermDelete, PermShare, PermAdmin:
			if _, ok := v.(bool); !ok {
				return ErrInvalidPermission
			}
		case "deny":
			list, ok := v.([]any)
			if !ok {
				return ErrInvalidPermission
			}
			for _, item := range list {
				s, ok := item.(string)
				if !ok || !isValidAction(s) {
					return ErrInvalidPermission
				}
			}
		default:
			return ErrInvalidPermission
		}
	}
	return nil
}

func isValidAction(action string) bool {
	for _, a := range ValidActions {
		if a == action {
			return true
		}
	}
	return false
}

// systemPermissions 系统角色的固定权限映射（设计 6.5.1，保持现状兼容）：
// owner 全部权限；editor 读/写/分享（无删除与管理）；viewer 仅读。
// 非成员（空角色）无任何权限。
func systemPermissions(role string) map[string]any {
	switch role {
	case RoleOwner:
		return map[string]any{PermRead: true, PermWrite: true, PermDelete: true, PermShare: true, PermAdmin: true}
	case RoleEditor:
		return map[string]any{PermRead: true, PermWrite: true, PermShare: true}
	case RoleViewer:
		return map[string]any{PermRead: true}
	default:
		return nil
	}
}

// permissionAllowed 按 permissions JSON 求值单个动作：
// deny 数组中列出的动作显式拒绝且优先于 allow；未列出的动作按布尔勾选，
// 缺失/非真值一律无权限（fail closed）。
func permissionAllowed(perms map[string]any, action string) bool {
	if perms == nil {
		return false
	}
	if denies, ok := perms["deny"].([]any); ok {
		for _, d := range denies {
			if s, ok := d.(string); ok && s == action {
				return false
			}
		}
	}
	allowed, ok := perms[action].(bool)
	return ok && allowed
}

// Service 提供团队生命周期、成员与角色管理及权限判定；权限判定委托 Repo
// 查询实现（纯查询，不引入 Casbin；继承 = 团队成员身份，见 can 注释）。
type Service struct {
	repo Repo
	now  func() time.Time
}

func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: time.Now}
}

// can 求值用户在团队内的单个动作权限（设计 6.5 权限模型，纯查询实现）：
//   - 系统角色按 systemPermissions 固定映射（owner 全 true；editor 读/写/分享）；
//   - 自定义角色按 roles.permissions JSON（deny 优先于 allow）；
//   - 成员的 role_id 指向的角色行不存在（如已被删除）时 fail closed（无任何权限）；
//   - 非成员无任何权限。
//
// 权限继承说明（设计 6.5.3 的最小落地）：继承 = 团队成员身份——团队内全部
// 文件/目录适用同一权限集，无子目录或路径级覆盖（路径通配、子项独立覆盖为
// 6.5.3/6.5.4 的后续扩展，需引入 Casbin 或路径策略表）。
func (s *Service) can(userID, teamID uuid.UUID, action string) (bool, error) {
	role, roleID, err := s.repo.MemberRole(teamID, userID)
	if err != nil {
		return false, err
	}
	if roleID != nil {
		perms, err := s.repo.RolePermissions(teamID, *roleID)
		if errors.Is(err, ErrNotFound) {
			return false, nil // 角色行缺失：fail closed
		}
		if err != nil {
			return false, err
		}
		return permissionAllowed(perms, action), nil
	}
	return permissionAllowed(systemPermissions(role), action), nil
}

// CanRead 判定用户能否读取团队空间（系统 owner/editor/viewer；自定义角色按 read 勾选）。
func (s *Service) CanRead(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermRead)
}

// CanWrite 判定用户能否写入团队空间（系统 owner/editor；自定义角色按 write 勾选且未被 deny）。
func (s *Service) CanWrite(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermWrite)
}

// CanDelete 判定用户能否删除团队文件（系统仅 owner；自定义角色按 delete 勾选且未被 deny）。
func (s *Service) CanDelete(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermDelete)
}

// CanShare 判定用户能否创建团队文件分享（设计 6.5.5：系统 owner/editor；
// 自定义角色按 share 勾选且未被 deny）。
func (s *Service) CanShare(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermShare)
}

// CreateTeam 创建团队：创建者自动成为 owner 成员，并在同一事务内创建团队根目录
// （files.scope_type='team'、team_id、is_root=true、owner_id=创建者）。
func (s *Service) CreateTeam(owner uuid.UUID, name, description string) (Team, files.File, error) {
	n, err := validateName(name)
	if err != nil {
		return Team{}, files.File{}, err
	}
	now := s.now()
	t := Team{ID: uuid.New(), Name: n, Description: description, OwnerID: owner, CreatedAt: now}
	ownerMember := Member{TeamID: t.ID, UserID: owner, Role: RoleOwner, CreatedAt: now}
	teamID := t.ID
	root := files.File{
		ID: uuid.New(), Name: "根目录", OwnerID: owner, TeamID: &teamID,
		Type: "folder", IsRoot: true, ScopeType: "team", CreatedAt: now,
	}
	if err := s.repo.CreateTeamWithRoot(t, ownerMember, root); err != nil {
		if errors.Is(err, ErrNameConflict) || isUniqueViolation(err) {
			return Team{}, files.File{}, ErrNameConflict
		}
		return Team{}, files.File{}, err
	}
	return t, root, nil
}

// ListTeams 返回用户可见的团队（成员或 owner；创建者自动为成员）。
func (s *Service) ListTeams(user uuid.UUID) ([]Team, error) {
	return s.repo.ListForUser(user)
}

// Get 返回团队信息。
func (s *Service) Get(id uuid.UUID) (Team, error) { return s.repo.Get(id) }

// resolveMemberRole 校验并归一成员角色入参：
// roleID 非空时为自定义角色（role 归一为 'custom'，且角色行必须属于该团队）；
// 否则 role 必须为系统角色 editor/viewer（owner 不可经成员接口授予）。
func (s *Service) resolveMemberRole(teamID uuid.UUID, role string, roleID *uuid.UUID) (string, *uuid.UUID, error) {
	if roleID != nil {
		if role != "" && role != RoleCustom {
			return "", nil, ErrInvalidRole
		}
		if *roleID == uuid.Nil {
			return "", nil, ErrInvalidRole
		}
		if _, err := s.repo.RolePermissions(teamID, *roleID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", nil, ErrInvalidRole // 角色不存在或不属于该团队
			}
			return "", nil, err
		}
		return RoleCustom, roleID, nil
	}
	if role != RoleEditor && role != RoleViewer {
		return "", nil, ErrInvalidRole
	}
	return role, nil, nil
}

// AddMember 由 actor（须为团队 owner）添加成员并指定角色：
// 系统角色（editor/viewer）或自定义角色（role_id，须为本团队已有角色）。
func (s *Service) AddMember(actor, teamID, userID uuid.UUID, role string, roleID *uuid.UUID) (Member, error) {
	if userID == uuid.Nil {
		return Member{}, ErrInvalidRole
	}
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Member{}, err
	}
	if t.OwnerID != actor {
		return Member{}, ErrForbidden
	}
	role, roleID, err = s.resolveMemberRole(teamID, role, roleID)
	if err != nil {
		return Member{}, err
	}
	m := Member{TeamID: teamID, UserID: userID, Role: role, RoleID: roleID, CreatedAt: s.now()}
	if err := s.repo.AddMember(m); err != nil {
		return Member{}, err
	}
	// 回读以携带 role_name（自定义角色名）；回读失败退回构造值（不阻断添加）。
	members, err := s.repo.ListMembers(teamID)
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

// UpdateMemberRole 由 actor（须为团队 owner）修改成员角色（系统或自定义）；
// owner 成员的角色不可修改（所有权转移不在本期范围）。
func (s *Service) UpdateMemberRole(actor, teamID, userID uuid.UUID, role string, roleID *uuid.UUID) (Member, error) {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Member{}, err
	}
	if t.OwnerID != actor {
		return Member{}, ErrForbidden
	}
	if userID == t.OwnerID {
		return Member{}, ErrOwnerMember
	}
	role, roleID, err = s.resolveMemberRole(teamID, role, roleID)
	if err != nil {
		return Member{}, err
	}
	return s.repo.UpdateMemberRole(teamID, userID, role, roleID)
}

// RemoveMember 由 actor（须为团队 owner）移除成员；owner 成员不可移除（所有权转移不在本期范围）。
func (s *Service) RemoveMember(actor, teamID, userID uuid.UUID) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID != actor {
		return ErrForbidden
	}
	if userID == t.OwnerID {
		return ErrOwnerMember
	}
	return s.repo.RemoveMember(teamID, userID)
}

// ListMembers 返回团队成员列表；actor 必须是团队成员（含 owner）。
// 非成员返回 ErrNotFound（对外「不存在」语义，不泄露团队存在性，
// HTTP 层映射 404）。
func (s *Service) ListMembers(actor, teamID uuid.UUID) ([]Member, error) {
	if _, err := s.repo.Get(teamID); err != nil {
		return nil, err
	}
	role, err := s.repo.Role(teamID, actor)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrNotFound
	}
	return s.repo.ListMembers(teamID)
}

// Role 返回用户在团队中的角色（非成员为空串；自定义角色返回 'custom'）。
func (s *Service) Role(teamID, userID uuid.UUID) (string, error) { return s.repo.Role(teamID, userID) }

func (s *Service) Update(actor, teamID uuid.UUID, name, description string) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID != actor {
		return ErrForbidden
	}
	n, err := validateName(name)
	if err != nil {
		return err
	}
	return s.repo.Update(teamID, n, &description)
}
func (s *Service) Delete(actor, teamID uuid.UUID) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID != actor {
		return ErrForbidden
	}
	return s.repo.Delete(teamID)
}
func (s *Service) Roles(actor, teamID uuid.UUID) ([]Role, error) {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return nil, err
	}
	if t.OwnerID != actor {
		return nil, ErrForbidden
	}
	return s.repo.ListRoles(teamID)
}
func (s *Service) CreateRole(actor, teamID uuid.UUID, name string, permissions map[string]any) (Role, error) {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Role{}, err
	}
	if t.OwnerID != actor {
		return Role{}, ErrForbidden
	}
	name, err = validateName(name)
	if err != nil {
		return Role{}, err
	}
	if err := ValidatePermissions(permissions); err != nil {
		return Role{}, err
	}
	r := Role{ID: uuid.New(), TeamID: teamID, Name: name, Permissions: permissions, CreatedAt: s.now()}
	if err := s.repo.CreateRole(r); err != nil {
		if isUniqueViolation(err) {
			return Role{}, ErrNameConflict
		}
		return Role{}, err
	}
	return r, nil
}
func (s *Service) UpdateRole(actor, teamID, roleID uuid.UUID, name string, permissions map[string]any) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID != actor {
		return ErrForbidden
	}
	name, err = validateName(name)
	if err != nil {
		return err
	}
	if err := ValidatePermissions(permissions); err != nil {
		return err
	}
	return s.repo.UpdateRole(teamID, roleID, name, permissions)
}

// DeleteRole 删除自定义角色；仍有成员引用该角色时返回 ErrRoleInUse（HTTP 409），
// 须先改派成员角色（数据库侧另有 ON DELETE RESTRICT 兜底，migration 029）。
func (s *Service) DeleteRole(actor, teamID, roleID uuid.UUID) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID != actor {
		return ErrForbidden
	}
	n, err := s.repo.CountMembersByRole(teamID, roleID)
	if err != nil {
		return err
	}
	if n > 0 {
		return ErrRoleInUse
	}
	return s.repo.DeleteRole(teamID, roleID)
}

// UserInAnyTeam 实时判定用户是否属于任一给定团队。
func (s *Service) UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error) {
	return s.repo.UserInAnyTeam(userID, teamIDs)
}
