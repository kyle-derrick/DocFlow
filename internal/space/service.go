package space

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
	ErrInvalidName  = errors.New("invalid space name")
	ErrNotFound     = errors.New("space not found")
	ErrInvalidRole  = errors.New("invalid member role")
	ErrForbidden    = errors.New("not allowed to manage this space")
	ErrMemberExists = errors.New("user is already a member")
	ErrOwnerMember  = errors.New("space owner membership cannot be removed")
	// 默认空间不可删除（可改名）。
	ErrDefaultSpace = errors.New("default space cannot be deleted")
	// 空间配额非法 / 超出系统上限 / 建空间数超限。
	ErrInvalidQuota  = errors.New("invalid quota")
	ErrQuotaTooLarge = errors.New("quota exceeds the system maximum")
	ErrTooManySpaces = errors.New("space limit reached for this user")
	// 用户组相关。
	ErrGroupNotFound  = errors.New("group not found")
	ErrGroupExists    = errors.New("group is already a member")
	ErrInvalidGroupID = errors.New("invalid group id")
	// 彻底删除仅接受已解散（软删）空间。
	ErrNotDissolved = errors.New("space is not dissolved")
	// 邀请相关。
	ErrInvalidEmail  = errors.New("invalid email address")
	ErrInviteGone    = errors.New("invitation is no longer available")
	ErrEmailMismatch = errors.New("invitation email does not match your account")
)

// InviteTTL 空间邀请有效期（7 天）。
const InviteTTL = 7 * 24 * time.Hour

// 默认配额策略（未注入 provider 时的回退值）：新空间默认 10GiB，
// 空间配额上限 1TiB（0=不限），每用户空间数上限 20。
const (
	DefaultDefaultQuota int64 = 10 << 30
	DefaultMaxQuota     int64 = 1 << 40
	DefaultMaxSpaces    int   = 20
)

// NewInviteToken 生成明文邀请 token：32 字节随机数的 URL-safe base64。
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

// validateName 校验空间名：非空、无控制字符、不超过 100 字符。
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

// rolePermissions 五级内置角色的固定权限矩阵：
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
	return RoleLevel(role) >= 0
}

// isAssignable 判定角色是否可经成员管理接口授予（owner 除外）。
func isAssignable(role string) bool {
	switch role {
	case RoleAdmin, RoleMemberShare, RoleMember, RoleGuest:
		return true
	}
	return false
}

// Service 提供空间生命周期、成员/用户组管理与权限判定；权限判定 =
// 直接成员角色 ∪ 用户组角色取最高（repo.Role 统一解析）。
type Service struct {
	repo Repo
	now  func() time.Time
	// quotaDefaults 为 (default, max) 空间配额热读取（settings 注入）：
	// default 用于新空间；max 为非系统 admin 可设的上限（<=0 不限）。
	quotaDefaults func() (int64, int64)
	// maxSpaces 为每用户（owner 维度）空间数上限热读取（settings 注入）。
	maxSpaces func() int
}

func NewService(repo Repo) *Service {
	return &Service{repo: repo, now: time.Now}
}

// SetQuotaDefaultsProvider 注入空间配额默认值/上限热读取（幂等）。
func (s *Service) SetQuotaDefaultsProvider(fn func() (int64, int64)) {
	if fn != nil {
		s.quotaDefaults = fn
	}
}

// SetMaxSpacesProvider 注入每用户空间数上限热读取（幂等）。
func (s *Service) SetMaxSpacesProvider(fn func() int) {
	if fn != nil {
		s.maxSpaces = fn
	}
}

func (s *Service) defaultQuota() int64 {
	if s.quotaDefaults == nil {
		return DefaultDefaultQuota
	}
	def, _ := s.quotaDefaults()
	if def < 0 {
		return DefaultDefaultQuota
	}
	return def
}

func (s *Service) maxQuota() int64 {
	if s.quotaDefaults == nil {
		return DefaultMaxQuota
	}
	_, max := s.quotaDefaults()
	if max < 0 {
		return DefaultMaxQuota
	}
	return max
}

func (s *Service) maxSpacesLimit() int {
	if s.maxSpaces == nil {
		return DefaultMaxSpaces
	}
	if n := s.maxSpaces(); n >= 1 {
		return n
	}
	return DefaultMaxSpaces
}

// can 求值用户在空间内的单个动作权限：按直接成员与用户组角色的最高级
// 固定矩阵；非成员无任何权限（fail closed）。
func (s *Service) can(userID, spaceID uuid.UUID, action string) (bool, error) {
	role, err := s.repo.Role(spaceID, userID)
	if err != nil {
		return false, err
	}
	perms := rolePermissions(role)
	return perms != nil && perms[action], nil
}

// CanRead 判定用户能否读取空间（owner/admin/member_share/member/guest）。
func (s *Service) CanRead(userID, spaceID uuid.UUID) (bool, error) {
	return s.can(userID, spaceID, PermRead)
}

// CanWrite 判定用户能否写入空间（owner/admin/member_share/member）。
func (s *Service) CanWrite(userID, spaceID uuid.UUID) (bool, error) {
	return s.can(userID, spaceID, PermWrite)
}

// CanDelete 判定用户能否删除空间文件（owner/admin/member_share/member）。
func (s *Service) CanDelete(userID, spaceID uuid.UUID) (bool, error) {
	return s.can(userID, spaceID, PermDelete)
}

// CanShare 判定用户能否创建空间文件分享（owner/admin/member_share）。
func (s *Service) CanShare(userID, spaceID uuid.UUID) (bool, error) {
	return s.can(userID, spaceID, PermShare)
}

// CanAdmin 判定用户是否具备空间管理权限（管成员/用户组/改目录文件权限：
// owner/admin；解散与转让另行限定仅 owner）。
func (s *Service) CanAdmin(userID, spaceID uuid.UUID) (bool, error) {
	return s.can(userID, spaceID, PermAdmin)
}

// DefaultSpaceName 由展示名推导默认空间名：「{name}的空间」。
func DefaultSpaceName(displayName string) string {
	n := strings.TrimSpace(displayName)
	if n == "" {
		return "我的空间"
	}
	return n + "的空间"
}

// EnsureDefaultSpace 幂等确保用户拥有默认空间（注册/seed 调用）：
// 已存在（未删除）直接返回；不存在则创建（is_default、配额=default、
// owner 成员、空间根目录，同一事务）。并发创建撞唯一索引时回读。
func (s *Service) EnsureDefaultSpace(owner uuid.UUID, displayName string) (Space, files.File, error) {
	if sp, err := s.repo.GetDefault(owner); err == nil {
		root, rerr := s.repo.SpaceRoot(sp.ID)
		if rerr != nil {
			return sp, files.File{}, nil
		}
		return sp, root, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Space{}, files.File{}, err
	}
	now := s.now()
	sp := Space{ID: uuid.New(), Name: DefaultSpaceName(displayName), QuotaBytes: s.defaultQuota(), OwnerID: owner, IsDefault: true, CreatedAt: now}
	ownerMember := Member{SpaceID: sp.ID, UserID: owner, Role: RoleOwner, CreatedAt: now}
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, SpaceID: sp.ID, Type: "folder", IsRoot: true, CreatedAt: now}
	if err := s.repo.CreateSpaceWithRoot(sp, ownerMember, root); err != nil {
		// 并发注册/重试撞唯一索引：回读既有默认空间。
		if sp2, gerr := s.repo.GetDefault(owner); gerr == nil {
			root2, _ := s.repo.SpaceRoot(sp2.ID)
			return sp2, root2, nil
		}
		return Space{}, files.File{}, err
	}
	return sp, root, nil
}

// CreateSpace 创建空间：创建者自动成为 owner 成员，并在同一事务内创建
// 空间根目录（files.space_id、is_root=true）。校验每用户空间数上限
// （owner 维度计数，含默认空间）。新空间配额 = space.default_quota。
func (s *Service) CreateSpace(owner uuid.UUID, name, description string) (Space, files.File, error) {
	n, err := validateName(name)
	if err != nil {
		return Space{}, files.File{}, err
	}
	count, err := s.repo.CountForUser(owner)
	if err != nil {
		return Space{}, files.File{}, err
	}
	if count >= int64(s.maxSpacesLimit()) {
		return Space{}, files.File{}, ErrTooManySpaces
	}
	now := s.now()
	sp := Space{ID: uuid.New(), Name: n, Description: description, QuotaBytes: s.defaultQuota(), OwnerID: owner, CreatedAt: now}
	ownerMember := Member{SpaceID: sp.ID, UserID: owner, Role: RoleOwner, CreatedAt: now}
	root := files.File{ID: uuid.New(), Name: "根目录", OwnerID: owner, SpaceID: sp.ID, Type: "folder", IsRoot: true, CreatedAt: now}
	if err := s.repo.CreateSpaceWithRoot(sp, ownerMember, root); err != nil {
		return Space{}, files.File{}, err
	}
	return sp, root, nil
}

// ListSpaces 返回用户可见的空间（直接成员或用户组命中）。
func (s *Service) ListSpaces(user uuid.UUID) ([]Space, error) {
	return s.repo.ListForUser(user)
}

// ListSpacesDetailed 返回用户所属空间的列表条目（我的角色/成员数/存储
// 用量，/spaces 页卡片数据）。
func (s *Service) ListSpacesDetailed(user uuid.UUID) ([]SpaceInfo, error) {
	return s.repo.ListForUserStats(user)
}

// Get 返回空间信息（软删排除）。
func (s *Service) Get(id uuid.UUID) (Space, error) { return s.repo.Get(id) }

// DefaultSpace 返回用户默认空间。
func (s *Service) DefaultSpace(owner uuid.UUID) (Space, error) { return s.repo.GetDefault(owner) }

// canManageMembers 校验 actor 具备成员管理权限（owner/admin）。
func (s *Service) canManageMembers(actor, spaceID uuid.UUID) error {
	ok, err := s.CanAdmin(actor, spaceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// Update 更新空间名称/描述（owner/admin 可改；默认空间可改名）。
func (s *Service) Update(actor, spaceID uuid.UUID, name, description string) error {
	if _, err := s.repo.Get(spaceID); err != nil {
		return err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return err
	}
	n, err := validateName(name)
	if err != nil {
		return err
	}
	return s.repo.Update(spaceID, &n, &description, nil)
}

// UpdateQuota 更新空间配额（owner/admin 可改；非系统 admin 不可超过
// space.max_quota，系统 admin 越权不限）。quota<0 非法；0=不限。
func (s *Service) UpdateQuota(actor, spaceID uuid.UUID, quota int64, isSysAdmin bool) error {
	if _, err := s.repo.Get(spaceID); err != nil {
		return err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return err
	}
	if quota < 0 {
		return ErrInvalidQuota
	}
	if !isSysAdmin && quota > 0 {
		if max := s.maxQuota(); max > 0 && quota > max {
			return ErrQuotaTooLarge
		}
	}
	return s.repo.Update(spaceID, nil, nil, &quota)
}

// Delete 解散空间（仅 owner 且非默认空间，软删除）。
func (s *Service) Delete(actor, spaceID uuid.UUID) error {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return err
	}
	if sp.OwnerID != actor {
		return ErrForbidden
	}
	if sp.IsDefault {
		return ErrDefaultSpace
	}
	return s.repo.Delete(spaceID)
}

// permanentRepo 收窄「已解散空间彻底删除」所需的仓储能力（避免扩大 Repo
// 接口波及内存实现；生产 GormStore 恒满足）。
type permanentRepo interface {
	GetAny(id uuid.UUID) (Space, error)
	DeletePermanent(id uuid.UUID) error
}

// Purge 物理删除已解散空间的空间行（成员/用户组授权/邀请经外键级联清除）。
// 仅接受已软删（解散）空间（未解散返回 ErrNotDissolved，409）；文件与
// 对象存储的清理由 HTTP 层在调用本方法前经 files.PurgeSpace 完成。
func (s *Service) Purge(spaceID uuid.UUID) error {
	perm, ok := s.repo.(permanentRepo)
	if !ok {
		return ErrForbidden
	}
	sp, err := perm.GetAny(spaceID)
	if err != nil {
		return err
	}
	if sp.DeletedAt == nil {
		return ErrNotDissolved
	}
	return perm.DeletePermanent(spaceID)
}

// AddMember 由 actor（owner/admin）添加成员并指定内置角色
// （admin/member_share/member/guest；admin 角色仅 owner 可授予）。
func (s *Service) AddMember(actor, spaceID, userID uuid.UUID, role string) (Member, error) {
	if userID == uuid.Nil {
		return Member{}, ErrInvalidRole
	}
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return Member{}, err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return Member{}, err
	}
	if !isAssignable(role) {
		return Member{}, ErrInvalidRole
	}
	// admin 角色仅 owner 可授予（管理员之间互相制衡的边界由 owner 把持）。
	if role == RoleAdmin && sp.OwnerID != actor {
		return Member{}, ErrForbidden
	}
	m := Member{SpaceID: spaceID, UserID: userID, Role: role, CreatedAt: s.now()}
	if err := s.repo.AddMember(m); err != nil {
		return Member{}, err
	}
	// 回读以携带 username/nickname；回读失败退回构造值（不阻断添加）。
	members, err := s.repo.ListMembers(spaceID)
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
func (s *Service) UpdateMemberRole(actor, spaceID, userID uuid.UUID, role string) (Member, error) {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return Member{}, err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return Member{}, err
	}
	if userID == sp.OwnerID {
		return Member{}, ErrOwnerMember
	}
	if !isAssignable(role) {
		return Member{}, ErrInvalidRole
	}
	if actor != sp.OwnerID {
		// admin：不可授予/修改 admin 角色，也不可修改其他 admin 的角色。
		if role == RoleAdmin {
			return Member{}, ErrForbidden
		}
		targetRole, err := s.repo.DirectRole(spaceID, userID)
		if err != nil {
			return Member{}, err
		}
		if targetRole == RoleOwner || targetRole == RoleAdmin {
			return Member{}, ErrForbidden
		}
	}
	return s.repo.UpdateMemberRole(spaceID, userID, role)
}

// RemoveMember 由 actor（owner/admin）移除成员；owner 成员不可移除，
// admin 不可移除其他 admin（边界同 UpdateMemberRole）。
func (s *Service) RemoveMember(actor, spaceID, userID uuid.UUID) error {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return err
	}
	if userID == sp.OwnerID {
		return ErrOwnerMember
	}
	if actor != sp.OwnerID {
		targetRole, err := s.repo.DirectRole(spaceID, userID)
		if err != nil {
			return err
		}
		if targetRole == RoleOwner || targetRole == RoleAdmin {
			return ErrForbidden
		}
	}
	return s.repo.RemoveMember(spaceID, userID)
}

// TransferOwnership 由 owner 把空间所有权转让给既有成员：新 owner 成员
// 角色变为 owner、spaces.owner_id 更新，原 owner 降为 admin（同一事务）。
// 受让人可为直接成员，或经用户组加入的组内用户（后者先按其当前组角色
// 落为直接成员再转让——转让候选=成员列表全部用户，含组内）。
func (s *Service) TransferOwnership(actor, spaceID, newOwner uuid.UUID) error {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return err
	}
	if sp.OwnerID != actor {
		return ErrForbidden
	}
	if newOwner == uuid.Nil || newOwner == actor {
		return ErrInvalidRole
	}
	role, err := s.repo.DirectRole(spaceID, newOwner)
	if err != nil {
		return err
	}
	if !isValidRole(role) {
		// 非直接成员：组内用户以其当前有效（组）角色补录直接成员后转让。
		effective, rerr := s.repo.Role(spaceID, newOwner)
		if rerr != nil {
			return rerr
		}
		if !isValidRole(effective) {
			return ErrNotFound // 既非直接成员也非组内用户
		}
		if !isAssignable(effective) {
			return ErrInvalidRole
		}
		if aerr := s.repo.AddMember(Member{SpaceID: spaceID, UserID: newOwner, Role: effective, CreatedAt: s.now()}); aerr != nil {
			return aerr
		}
	}
	return s.repo.TransferOwnership(spaceID, actor, newOwner)
}

// ListMembers 返回空间直接成员列表；actor 须为空间成员（直接或经用户组）。
// 非成员返回 ErrNotFound（对外「不存在」语义，不泄露空间存在性）。
func (s *Service) ListMembers(actor, spaceID uuid.UUID) ([]Member, error) {
	if _, err := s.repo.Get(spaceID); err != nil {
		return nil, err
	}
	role, err := s.repo.Role(spaceID, actor)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrNotFound
	}
	return s.repo.ListMembers(spaceID)
}

// groupUsersRepo 收窄「成员列表合并展示组内用户」所需的仓储能力（生产
// GormStore 恒满足；内存实现缺省返回空）。
type groupUsersRepo interface {
	ListGroupUsers(spaceID uuid.UUID) ([]GroupUser, error)
}

// ListGroupUsers 返回空间经用户组加入的用户条目（组来源标注用）；actor
// 须为空间成员（与 ListMembers 同口径）。
func (s *Service) ListGroupUsers(actor, spaceID uuid.UUID) ([]GroupUser, error) {
	if _, err := s.repo.Get(spaceID); err != nil {
		return nil, err
	}
	role, err := s.repo.Role(spaceID, actor)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrNotFound
	}
	gu, ok := s.repo.(groupUsersRepo)
	if !ok {
		return nil, nil
	}
	return gu.ListGroupUsers(spaceID)
}

// Role 返回用户在空间中的有效角色（直接成员 ∪ 用户组取最高；非成员空串）。
func (s *Service) Role(spaceID, userID uuid.UUID) (string, error) {
	return s.repo.Role(spaceID, userID)
}

// UserInAnySpace 实时判定用户是否属于任一给定空间（直接成员或用户组）。
func (s *Service) UserInAnySpace(userID uuid.UUID, spaceIDs []uuid.UUID) (bool, error) {
	return s.repo.UserInAnySpace(userID, spaceIDs)
}

// MemberUserIDs 返回空间全部参与者的用户 ID（直接成员 ∪ 用户组成员展开，
// 通知广播用）。
func (s *Service) MemberUserIDs(spaceID uuid.UUID) ([]uuid.UUID, error) {
	return s.repo.MemberUserIDs(spaceID)
}

// Leave 成员主动退出空间（非 owner 直接成员；owner 须先转让所有权或解散
// 空间，ErrOwnerMember 403）。审计由 HTTP 层记录。
func (s *Service) Leave(actor, spaceID uuid.UUID) error {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return err
	}
	if sp.OwnerID == actor {
		return ErrOwnerMember
	}
	role, err := s.repo.DirectRole(spaceID, actor)
	if err != nil {
		return err
	}
	if role == "" {
		return ErrNotFound
	}
	return s.repo.RemoveMember(spaceID, actor)
}

// ---- 用户组成员管理（space_group_members，migration 040） ----

// AddGroup 由 actor（owner/admin）把用户组加入空间并指定角色
// （admin/member_share/member/guest；admin 角色仅 owner 可授予）。
func (s *Service) AddGroup(actor, spaceID, groupID uuid.UUID, role string) (GroupMember, error) {
	if groupID == uuid.Nil {
		return GroupMember{}, ErrInvalidGroupID
	}
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return GroupMember{}, err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return GroupMember{}, err
	}
	if !isAssignable(role) {
		return GroupMember{}, ErrInvalidRole
	}
	if role == RoleAdmin && sp.OwnerID != actor {
		return GroupMember{}, ErrForbidden
	}
	gm := GroupMember{SpaceID: spaceID, GroupID: groupID, Role: role, CreatedAt: s.now()}
	if err := s.repo.AddGroupMember(gm); err != nil {
		return GroupMember{}, err
	}
	// 回读以携带组名/成员数。
	groups, err := s.repo.ListGroupMembers(spaceID)
	if err != nil {
		return gm, nil
	}
	for _, item := range groups {
		if item.GroupID == groupID {
			return item, nil
		}
	}
	return gm, nil
}

// UpdateGroupRole 由 actor（owner/admin）修改用户组角色（边界同 AddGroup）。
func (s *Service) UpdateGroupRole(actor, spaceID, groupID uuid.UUID, role string) (GroupMember, error) {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return GroupMember{}, err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return GroupMember{}, err
	}
	if !isAssignable(role) {
		return GroupMember{}, ErrInvalidRole
	}
	if actor != sp.OwnerID {
		current, err := s.repo.GroupRole(spaceID, groupID)
		if err != nil {
			return GroupMember{}, err
		}
		if current == RoleAdmin || role == RoleAdmin {
			return GroupMember{}, ErrForbidden
		}
	}
	return s.repo.UpdateGroupMemberRole(spaceID, groupID, role)
}

// RemoveGroup 由 actor（owner/admin）移除空间的用户组授权。
func (s *Service) RemoveGroup(actor, spaceID, groupID uuid.UUID) error {
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return err
	}
	return s.repo.RemoveGroupMember(spaceID, groupID)
}

// ListGroups 返回空间的用户组授权列表；actor 须为空间成员。
func (s *Service) ListGroups(actor, spaceID uuid.UUID) ([]GroupMember, error) {
	if _, err := s.repo.Get(spaceID); err != nil {
		return nil, err
	}
	role, err := s.repo.Role(spaceID, actor)
	if err != nil {
		return nil, err
	}
	if role == "" {
		return nil, ErrNotFound
	}
	return s.repo.ListGroupMembers(spaceID)
}

// ---- 空间邀请（space_invites，migration 040） ----

// CreateInvite 由 actor（owner/admin）向 email 发出空间邀请，返回邀请与
// 明文 token（仅本次可见，库中只存哈希）。幂等：该空间对同邮箱已有未过期
// 未接受的邀请时返回既有记录（token 为空串）。
func (s *Service) CreateInvite(actor, spaceID uuid.UUID, email, role string) (Invite, string, error) {
	sp, err := s.repo.Get(spaceID)
	if err != nil {
		return Invite{}, "", err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return Invite{}, "", err
	}
	if !isAssignable(role) {
		return Invite{}, "", ErrInvalidRole
	}
	if role == RoleAdmin && sp.OwnerID != actor {
		return Invite{}, "", ErrForbidden
	}
	email = auth.NormalizeEmail(email)
	if err := auth.ValidateEmail(email); err != nil {
		return Invite{}, "", ErrInvalidEmail
	}
	now := s.now().UTC()
	if existing, err := s.repo.FindActiveInvite(spaceID, email, now); err == nil {
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
		ID: uuid.New(), SpaceID: spaceID, Email: email, Role: role,
		TokenHash: HashInviteToken(token), InvitedBy: &invitedBy,
		ExpiresAt: now.Add(InviteTTL), CreatedAt: now,
	}
	if err := s.repo.CreateInvite(inv); err != nil {
		return Invite{}, "", err
	}
	return inv, token, nil
}

// ListInvites 返回空间邀请列表（含已接受/已过期；actor 须 owner/admin）。
func (s *Service) ListInvites(actor, spaceID uuid.UUID) ([]Invite, error) {
	if _, err := s.repo.Get(spaceID); err != nil {
		return nil, err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return nil, err
	}
	return s.repo.ListInvites(spaceID, 200)
}

// RevokeInvite 撤销邀请（删行，token 立即失效；actor 须 owner/admin）。
func (s *Service) RevokeInvite(actor, spaceID, inviteID uuid.UUID) error {
	if _, err := s.repo.Get(spaceID); err != nil {
		return err
	}
	if err := s.canManageMembers(actor, spaceID); err != nil {
		return err
	}
	return s.repo.DeleteInvite(spaceID, inviteID)
}

// AcceptInvite 凭一次性 token 加入空间：校验未过期未接受、当前用户邮箱
// 与邀请邮箱一致，写入直接成员后原子标记 accepted_at。已在空间（直接
// 成员）时标记接受并返回 alreadyMember=true。返回空间信息供跳转。
func (s *Service) AcceptInvite(user uuid.UUID, token, userEmail string) (Space, Invite, bool, error) {
	inv, err := s.repo.GetInviteByTokenHash(HashInviteToken(strings.TrimSpace(token)))
	if err != nil {
		return Space{}, Invite{}, false, ErrNotFound
	}
	now := s.now().UTC()
	if inv.AcceptedAt != nil || !now.Before(inv.ExpiresAt) {
		return Space{}, Invite{}, false, ErrInviteGone
	}
	if auth.NormalizeEmail(userEmail) != inv.Email {
		return Space{}, Invite{}, false, ErrEmailMismatch
	}
	sp, err := s.repo.Get(inv.SpaceID)
	if err != nil {
		return Space{}, Invite{}, false, err
	}
	if sp.DeletedAt != nil {
		return Space{}, Invite{}, false, ErrNotFound
	}
	alreadyMember := false
	if role, rerr := s.repo.DirectRole(inv.SpaceID, user); rerr == nil && role != "" {
		alreadyMember = true
	} else if err := s.repo.AddMember(Member{SpaceID: inv.SpaceID, UserID: user, Role: inv.Role, CreatedAt: now}); err != nil {
		if errors.Is(err, ErrMemberExists) {
			alreadyMember = true
		} else {
			return Space{}, Invite{}, false, err
		}
	}
	accepted, err := s.repo.MarkInviteAccepted(inv.ID, now)
	if err != nil {
		return Space{}, Invite{}, false, err
	}
	if !accepted {
		return Space{}, Invite{}, false, ErrInviteGone
	}
	inv.AcceptedAt = &now
	return sp, inv, alreadyMember, nil
}
