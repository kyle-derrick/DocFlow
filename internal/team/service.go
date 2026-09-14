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
	ErrInvalidName  = errors.New("invalid team name")
	ErrNameConflict = errors.New("team name already exists")
	ErrNotFound     = errors.New("team not found")
	ErrInvalidRole  = errors.New("invalid member role")
	ErrForbidden    = errors.New("not allowed to manage this team")
	ErrMemberExists = errors.New("user is already a member")
	ErrOwnerMember  = errors.New("team owner membership cannot be removed")
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

// Service 提供团队生命周期与成员管理；权限判定委托 Repo 查询实现（未来可换 Casbin）。
type Service struct {
	repo Repo
	now  func() time.Time
}

func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: time.Now}
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

// AddMember 由 actor（须为团队 owner）添加成员并指定角色（editor/viewer）。
func (s *Service) AddMember(actor, teamID, userID uuid.UUID, role string) (Member, error) {
	if role != RoleEditor && role != RoleViewer {
		return Member{}, ErrInvalidRole
	}
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Member{}, err
	}
	if t.OwnerID != actor {
		return Member{}, ErrForbidden
	}
	if userID == uuid.Nil {
		return Member{}, ErrInvalidRole
	}
	m := Member{TeamID: teamID, UserID: userID, Role: role, CreatedAt: s.now()}
	if err := s.repo.AddMember(m); err != nil {
		return Member{}, err
	}
	return m, nil
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

// Role 返回用户在团队中的角色（非成员为空串）。
func (s *Service) Role(teamID, userID uuid.UUID) (string, error) { return s.repo.Role(teamID, userID) }

// CanWrite 判定用户能否写入团队空间（owner/editor）。
func (s *Service) CanWrite(userID, teamID uuid.UUID) (bool, error) {
	return s.repo.CanWrite(userID, teamID)
}

// UserInAnyTeam 实时判定用户是否属于任一给定团队。
func (s *Service) UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error) {
	return s.repo.UserInAnyTeam(userID, teamIDs)
}
