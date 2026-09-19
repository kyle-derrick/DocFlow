package team

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
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
	// 邀请相关（v1.7.1）。
	ErrInvalidEmail  = errors.New("invalid email address")
	ErrInviteGone    = errors.New("invitation is no longer available")
	ErrEmailMismatch = errors.New("invitation email does not match your account")
)

// InviteTTL 团队邀请有效期（7 天）。
const InviteTTL = 7 * 24 * time.Hour

// NewInviteToken 生成明文邀请 token：32 字节随机数的 URL-safe base64
//（43 字符，与 shares/invitations 同模式；数据库只存 SHA-256 hex 哈希）。
func NewInviteToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashInviteToken 返回 token 的 SHA-256 十六进制小写哈希（64 字符）。
func HashInviteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

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

// rolePermissions 五级内置角色的固定权限矩阵（migration 037 起自定义角色移除）：
//
//	owner        read/write/delete/share/admin（全部）
//	admin        read/write/delete/share/admin（不含解散/转让——仅 owner）
//	member_share read/write/delete/share
//	member       read/write/delete
//	guest        read
//
// 非成员（空角色）无任何权限（fail closed）。
func rolePermissions(role string) map[string]bool {
	switch role {
	case RoleOwner, RoleAdmin:
		return map[string]bool{PermRead: true, PermWrite: true, PermDelete: true, PermShare: true, PermAdmin: true}
	case RoleMemberShare:
		return map[string]bool{PermRead: true, PermWrite: true, PermDelete: true, PermShare: true}
	case RoleMember:
		return map[string]bool{PermRead: true, PermWrite: true, PermDelete: true}
	case RoleGuest:
		return map[string]bool{PermRead: true}
	default:
		return nil
	}
}

// isValidRole 判定角色是否为五级内置角色之一。
func isValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleMemberShare, RoleMember, RoleGuest:
		return true
	}
	return false
}

// isAssignable 判定角色是否可经成员管理接口授予（owner 除外）。
func isAssignable(role string) bool {
	switch role {
	case RoleAdmin, RoleMemberShare, RoleMember, RoleGuest:
		return true
	}
	return false
}

// Service 提供团队生命周期与成员管理及权限判定；权限判定委托 Repo 的
// 成员角色查询（纯查询实现，继承 = 团队成员身份）。
type Service struct {
	repo Repo
	now  func() time.Time
}

func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: time.Now}
}

// can 求值用户在团队内的单个动作权限：按五级内置角色的固定矩阵；
// 非成员无任何权限（fail closed）。
func (s *Service) can(userID, teamID uuid.UUID, action string) (bool, error) {
	role, err := s.repo.Role(teamID, userID)
	if err != nil {
		return false, err
	}
	perms := rolePermissions(role)
	return perms != nil && perms[action], nil
}

// CanRead 判定用户能否读取团队空间（owner/admin/member_share/member/guest）。
func (s *Service) CanRead(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermRead)
}

// CanWrite 判定用户能否写入团队空间（owner/admin/member_share/member）。
func (s *Service) CanWrite(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermWrite)
}

// CanDelete 判定用户能否删除团队文件（owner/admin/member_share/member）。
func (s *Service) CanDelete(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermDelete)
}

// CanShare 判定用户能否创建团队文件分享（owner/admin/member_share）。
func (s *Service) CanShare(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermShare)
}

// CanAdmin 判定用户是否具备团队管理权限（管成员/改目录文件权限：
// owner/admin；解散与转让另行限定仅 owner）。
func (s *Service) CanAdmin(userID, teamID uuid.UUID) (bool, error) {
	return s.can(userID, teamID, PermAdmin)
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

// ListTeamsDetailed 返回用户所属团队的列表条目（我的角色/成员数/存储
// 用量，v1.7 /teams 页卡片数据）。
func (s *Service) ListTeamsDetailed(user uuid.UUID) ([]TeamInfo, error) {
	return s.repo.ListForUserStats(user)
}

// Get 返回团队信息。
func (s *Service) Get(id uuid.UUID) (Team, error) { return s.repo.Get(id) }

// canManageMembers 校验 actor 具备成员管理权限（owner/admin）。
func (s *Service) canManageMembers(actor, teamID uuid.UUID) error {
	ok, err := s.CanAdmin(actor, teamID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// AddMember 由 actor（owner/admin）添加成员并指定内置角色
//（admin/member_share/member/guest；admin 角色仅 owner 可授予）。
func (s *Service) AddMember(actor, teamID, userID uuid.UUID, role string) (Member, error) {
	if userID == uuid.Nil {
		return Member{}, ErrInvalidRole
	}
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Member{}, err
	}
	if err := s.canManageMembers(actor, teamID); err != nil {
		return Member{}, err
	}
	if !isAssignable(role) {
		return Member{}, ErrInvalidRole
	}
	// admin 角色仅 owner 可授予（管理员之间互相制衡的边界由 owner 把持）。
	if role == RoleAdmin && t.OwnerID != actor {
		return Member{}, ErrForbidden
	}
	m := Member{TeamID: teamID, UserID: userID, Role: role, CreatedAt: s.now()}
	if err := s.repo.AddMember(m); err != nil {
		return Member{}, err
	}
	// 回读以携带 username/nickname；回读失败退回构造值（不阻断添加）。
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

// UpdateMemberRole 由 actor（owner/admin）修改成员角色（五级内置下拉；
// owner 成员不可改；admin 角色的授予/修改仅 owner 可为，admin 不可动
// 其他 admin）。
func (s *Service) UpdateMemberRole(actor, teamID, userID uuid.UUID, role string) (Member, error) {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Member{}, err
	}
	if err := s.canManageMembers(actor, teamID); err != nil {
		return Member{}, err
	}
	if userID == t.OwnerID {
		return Member{}, ErrOwnerMember
	}
	if !isAssignable(role) {
		return Member{}, ErrInvalidRole
	}
	if actor != t.OwnerID {
		// admin：不可授予/修改 admin 角色，也不可修改其他 admin 的角色。
		if role == RoleAdmin {
			return Member{}, ErrForbidden
		}
		targetRole, err := s.repo.Role(teamID, userID)
		if err != nil {
			return Member{}, err
		}
		if targetRole == RoleOwner || targetRole == RoleAdmin {
			return Member{}, ErrForbidden
		}
	}
	return s.repo.UpdateMemberRole(teamID, userID, role)
}

// RemoveMember 由 actor（owner/admin）移除成员；owner 成员不可移除，
// admin 不可移除其他 admin（边界同 UpdateMemberRole）。
func (s *Service) RemoveMember(actor, teamID, userID uuid.UUID) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if err := s.canManageMembers(actor, teamID); err != nil {
		return err
	}
	if userID == t.OwnerID {
		return ErrOwnerMember
	}
	if actor != t.OwnerID {
		targetRole, err := s.repo.Role(teamID, userID)
		if err != nil {
			return err
		}
		if targetRole == RoleOwner || targetRole == RoleAdmin {
			return ErrForbidden
		}
	}
	return s.repo.RemoveMember(teamID, userID)
}

// TransferOwnership 由 owner 把团队所有权转让给既有成员：新 owner 成员
// 角色变为 owner、teams.owner_id 更新，原 owner 降为 admin（同一事务）。
func (s *Service) TransferOwnership(actor, teamID, newOwner uuid.UUID) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID != actor {
		return ErrForbidden
	}
	if newOwner == uuid.Nil || newOwner == actor {
		return ErrInvalidRole
	}
	role, err := s.repo.Role(teamID, newOwner)
	if err != nil {
		return err
	}
	if !isValidRole(role) {
		return ErrNotFound // 受让人不是团队成员
	}
	return s.repo.TransferOwnership(teamID, actor, newOwner)
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

// Update 更新团队信息（仅 owner）。
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

// Delete 解散团队（仅 owner，软删除）。
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

// UserInAnyTeam 实时判定用户是否属于任一给定团队。
func (s *Service) UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error) {
	return s.repo.UserInAnyTeam(userID, teamIDs)
}

// Leave 成员主动退出团队（非 owner 成员；owner 须先转让所有权或解散
// 团队，ErrOwnerMember 403）。审计由 HTTP 层记录。
func (s *Service) Leave(actor, teamID uuid.UUID) error {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return err
	}
	if t.OwnerID == actor {
		return ErrOwnerMember
	}
	role, err := s.repo.Role(teamID, actor)
	if err != nil {
		return err
	}
	if role == "" {
		return ErrNotFound
	}
	return s.repo.RemoveMember(teamID, actor)
}

// ---- 团队邀请（v1.7.1 成员管理完善；team_invites，migration 039） ----

// CreateInvite 由 actor（owner/admin）向 email 发出团队邀请，返回邀请与
// 明文 token（仅本次可见，库中只存哈希）。幂等：该团队对同邮箱已有未过期
// 未接受的邀请时返回既有记录（token 为空串）。角色限内置可授予级
//（admin 仅 owner 可授予，与 AddMember 同边界）。
func (s *Service) CreateInvite(actor, teamID uuid.UUID, email, role string) (Invite, string, error) {
	t, err := s.repo.Get(teamID)
	if err != nil {
		return Invite{}, "", err
	}
	if err := s.canManageMembers(actor, teamID); err != nil {
		return Invite{}, "", err
	}
	if !isAssignable(role) {
		return Invite{}, "", ErrInvalidRole
	}
	if role == RoleAdmin && t.OwnerID != actor {
		return Invite{}, "", ErrForbidden
	}
	email = auth.NormalizeEmail(email)
	if err := auth.ValidateEmail(email); err != nil {
		return Invite{}, "", ErrInvalidEmail
	}
	now := s.now().UTC()
	if existing, err := s.repo.FindActiveInvite(teamID, email, now); err == nil {
		return existing, "", nil
	} else if !errors.Is(err, ErrNotFound) {
		return Invite{}, "", err
	}
	token, err := NewInviteToken()
	if err != nil {
		return Invite{}, "", err
	}
	invitedBy := actor
	inv := Invite{
		ID: uuid.New(), TeamID: teamID, Email: email, Role: role,
		TokenHash: HashInviteToken(token), InvitedBy: &invitedBy,
		ExpiresAt: now.Add(InviteTTL), CreatedAt: now,
	}
	if err := s.repo.CreateInvite(inv); err != nil {
		return Invite{}, "", err
	}
	return inv, token, nil
}

// ListInvites 返回团队邀请列表（含已接受/已过期；actor 须 owner/admin）。
func (s *Service) ListInvites(actor, teamID uuid.UUID) ([]Invite, error) {
	if _, err := s.repo.Get(teamID); err != nil {
		return nil, err
	}
	if err := s.canManageMembers(actor, teamID); err != nil {
		return nil, err
	}
	return s.repo.ListInvites(teamID, 200)
}

// RevokeInvite 撤销邀请（删行，token 立即失效；actor 须 owner/admin）。
func (s *Service) RevokeInvite(actor, teamID, inviteID uuid.UUID) error {
	if _, err := s.repo.Get(teamID); err != nil {
		return err
	}
	if err := s.canManageMembers(actor, teamID); err != nil {
		return err
	}
	return s.repo.DeleteInvite(teamID, inviteID)
}

// AcceptInvite 凭一次性 token 加入团队：校验未过期未接受、当前用户邮箱
// 与邀请邮箱一致（GitLab 语义，防链接外泄被冒用），写入成员后原子标记
// accepted_at。已在团队时标记接受并返回 alreadyMember=true（不报错，前端
// 提示后跳转）。返回团队信息供跳转。
func (s *Service) AcceptInvite(user uuid.UUID, token, userEmail string) (Team, Invite, bool, error) {
	inv, err := s.repo.GetInviteByTokenHash(HashInviteToken(strings.TrimSpace(token)))
	if err != nil {
		return Team{}, Invite{}, false, ErrNotFound
	}
	now := s.now().UTC()
	if inv.AcceptedAt != nil || !now.Before(inv.ExpiresAt) {
		return Team{}, Invite{}, false, ErrInviteGone
	}
	if auth.NormalizeEmail(userEmail) != inv.Email {
		return Team{}, Invite{}, false, ErrEmailMismatch
	}
	t, err := s.repo.Get(inv.TeamID)
	if err != nil {
		return Team{}, Invite{}, false, err
	}
	if t.DeletedAt != nil {
		return Team{}, Invite{}, false, ErrNotFound
	}
	alreadyMember := false
	if role, rerr := s.repo.Role(inv.TeamID, user); rerr == nil && role != "" {
		alreadyMember = true
	} else if err := s.repo.AddMember(Member{TeamID: inv.TeamID, UserID: user, Role: inv.Role, CreatedAt: now}); err != nil {
		if errors.Is(err, ErrMemberExists) {
			alreadyMember = true
		} else {
			return Team{}, Invite{}, false, err
		}
	}
	accepted, err := s.repo.MarkInviteAccepted(inv.ID, now)
	if err != nil {
		return Team{}, Invite{}, false, err
	}
	if !accepted {
		return Team{}, Invite{}, false, ErrInviteGone
	}
	inv.AcceptedAt = &now
	return t, inv, alreadyMember, nil
}
