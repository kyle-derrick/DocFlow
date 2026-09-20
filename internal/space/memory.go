package space

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/files"
)

var _ Repo = (*MemoryStore)(nil)

// MemoryStore 是 Repo 的内存实现，供测试使用（不依赖 PostgreSQL）。
type MemoryStore struct {
	mu         sync.RWMutex
	spaces     map[uuid.UUID]Space
	members    map[uuid.UUID]map[uuid.UUID]Member // space_id -> user_id -> member
	groupRoles map[uuid.UUID]map[uuid.UUID]string // space_id -> group_id -> role
	roots      map[uuid.UUID]files.File
	invites    map[uuid.UUID]Invite
	// groupMembers 模拟 group_members（group_id -> user_id 集合），
	// 测试经 SetGroupMembers 注入。
	groupMembers map[uuid.UUID]map[uuid.UUID]struct{}
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		spaces:       make(map[uuid.UUID]Space),
		members:      make(map[uuid.UUID]map[uuid.UUID]Member),
		groupRoles:   make(map[uuid.UUID]map[uuid.UUID]string),
		roots:        make(map[uuid.UUID]files.File),
		invites:      make(map[uuid.UUID]Invite),
		groupMembers: make(map[uuid.UUID]map[uuid.UUID]struct{}),
	}
}

// SetGroupMembers 注入用户组成员关系（group_id -> 成员 user_id 列表），
// 供经用户组的权限判定测试。
func (m *MemoryStore) SetGroupMembers(groupID uuid.UUID, userIDs []uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[uuid.UUID]struct{}, len(userIDs))
	for _, id := range userIDs {
		set[id] = struct{}{}
	}
	m.groupMembers[groupID] = set
}

// Root 返回空间根目录记录（供测试断言事务创建结果）。
func (m *MemoryStore) Root(spaceID uuid.UUID) (files.File, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.roots[spaceID]
	return f, ok
}

func (m *MemoryStore) CreateSpaceWithRoot(sp Space, owner Member, root files.File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spaces[sp.ID] = sp
	if m.members[sp.ID] == nil {
		m.members[sp.ID] = make(map[uuid.UUID]Member)
	}
	if _, exists := m.members[sp.ID][owner.UserID]; exists {
		return ErrMemberExists
	}
	m.members[sp.ID][owner.UserID] = owner
	m.roots[sp.ID] = root
	return nil
}

func (m *MemoryStore) Update(id uuid.UUID, name, description *string, quota *int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.spaces[id]
	if !ok || sp.DeletedAt != nil {
		return ErrNotFound
	}
	if name != nil {
		sp.Name = *name
	}
	if description != nil {
		sp.Description = *description
	}
	if quota != nil {
		sp.QuotaBytes = *quota
	}
	m.spaces[id] = sp
	return nil
}

func (m *MemoryStore) Delete(id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.spaces[id]
	if !ok || sp.DeletedAt != nil {
		return ErrNotFound
	}
	now := time.Now()
	sp.DeletedAt = &now
	m.spaces[id] = sp
	return nil
}

func (m *MemoryStore) Get(id uuid.UUID) (Space, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sp, ok := m.spaces[id]
	if !ok || sp.DeletedAt != nil {
		return Space{}, ErrNotFound
	}
	return sp, nil
}

func (m *MemoryStore) GetDefault(owner uuid.UUID) (Space, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sp := range m.spaces {
		if sp.OwnerID == owner && sp.IsDefault && sp.DeletedAt == nil {
			return sp, nil
		}
	}
	return Space{}, ErrNotFound
}

func (m *MemoryStore) SpaceRoot(spaceID uuid.UUID) (files.File, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.roots[spaceID]
	if !ok {
		return files.File{}, files.ErrNotFound
	}
	return f, nil
}

// visible 判定用户对空间可见（直接成员或用户组命中）。
func (m *MemoryStore) visible(spaceID, userID uuid.UUID) bool {
	if _, ok := m.members[spaceID][userID]; ok {
		return true
	}
	for groupID := range m.groupRoles[spaceID] {
		if _, ok := m.groupMembers[groupID][userID]; ok {
			return true
		}
	}
	return false
}

func (m *MemoryStore) ListForUser(userID uuid.UUID) ([]Space, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Space
	for id := range m.spaces {
		if m.visible(id, userID) {
			if sp, found := m.spaces[id]; found && sp.DeletedAt == nil {
				out = append(out, sp)
			}
		}
	}
	sortSpaces(out)
	return out, nil
}

// ListForUserStats 内存实现：成员数按内存表统计，存储用量恒 0（内存库
// 无 files 数据；仅供单测断言 my_role/member_count）。
func (m *MemoryStore) ListForUserStats(userID uuid.UUID) ([]SpaceInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []SpaceInfo
	for id := range m.spaces {
		if !m.visible(id, userID) {
			continue
		}
		sp, found := m.spaces[id]
		if !found || sp.DeletedAt != nil {
			continue
		}
		out = append(out, SpaceInfo{Space: sp, MyRole: m.roleLocked(id, userID), MemberCount: int64(len(m.members[id]))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func sortSpaces(out []Space) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
}

func (m *MemoryStore) CountForUser(owner uuid.UUID) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var count int64
	for _, sp := range m.spaces {
		if sp.OwnerID == owner && sp.DeletedAt == nil {
			count++
		}
	}
	return count, nil
}

func (m *MemoryStore) AddMember(mem Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.spaces[mem.SpaceID]; !ok {
		return ErrNotFound
	}
	if m.members[mem.SpaceID] == nil {
		m.members[mem.SpaceID] = make(map[uuid.UUID]Member)
	}
	if _, exists := m.members[mem.SpaceID][mem.UserID]; exists {
		return ErrMemberExists
	}
	m.members[mem.SpaceID][mem.UserID] = mem
	return nil
}

func (m *MemoryStore) RemoveMember(spaceID, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[spaceID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.members[spaceID][userID]; !ok {
		return ErrNotFound
	}
	delete(m.members[spaceID], userID)
	return nil
}

func (m *MemoryStore) ListMembers(spaceID uuid.UUID) ([]Member, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Member
	for _, mem := range m.members[spaceID] {
		out = append(out, mem)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].UserID.String() < out[j].UserID.String()
	})
	return out, nil
}

func (m *MemoryStore) UpdateMemberRole(spaceID, userID uuid.UUID, role string) (Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.members[spaceID][userID]
	if !ok {
		return Member{}, ErrNotFound
	}
	mem.Role = role
	m.members[spaceID][userID] = mem
	return mem, nil
}

func (m *MemoryStore) DirectRole(spaceID, userID uuid.UUID) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem, ok := m.members[spaceID][userID]
	if !ok {
		return "", nil
	}
	return mem.Role, nil
}

// roleLocked 返回有效角色（直接 ∪ 组取最高）；须持读锁调用。
func (m *MemoryStore) roleLocked(spaceID, userID uuid.UUID) string {
	direct := ""
	if mem, ok := m.members[spaceID][userID]; ok {
		direct = mem.Role
	}
	viaGroup := ""
	for groupID, role := range m.groupRoles[spaceID] {
		if _, ok := m.groupMembers[groupID][userID]; ok {
			if RoleLevel(role) > RoleLevel(viaGroup) {
				viaGroup = role
			}
		}
	}
	return HigherRole(direct, viaGroup)
}

func (m *MemoryStore) Role(spaceID, userID uuid.UUID) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roleLocked(spaceID, userID), nil
}

func (m *MemoryStore) MemberUserIDs(spaceID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[uuid.UUID]struct{})
	for userID := range m.members[spaceID] {
		seen[userID] = struct{}{}
	}
	for groupID := range m.groupRoles[spaceID] {
		for userID := range m.groupMembers[groupID] {
			seen[userID] = struct{}{}
		}
	}
	out := make([]uuid.UUID, 0, len(seen))
	for userID := range seen {
		out = append(out, userID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func (m *MemoryStore) TransferOwnership(spaceID, oldOwner, newOwner uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp, ok := m.spaces[spaceID]
	if !ok || sp.DeletedAt != nil || sp.OwnerID != oldOwner {
		return ErrNotFound
	}
	mem, ok := m.members[spaceID][newOwner]
	if !ok {
		return ErrNotFound
	}
	old, ok := m.members[spaceID][oldOwner]
	if !ok {
		return ErrNotFound
	}
	sp.OwnerID = newOwner
	m.spaces[spaceID] = sp
	mem.Role = RoleOwner
	m.members[spaceID][newOwner] = mem
	old.Role = RoleAdmin
	m.members[spaceID][oldOwner] = old
	return nil
}

func (m *MemoryStore) UserInAnySpace(userID uuid.UUID, spaceIDs []uuid.UUID) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, spaceID := range spaceIDs {
		if m.visible(spaceID, userID) {
			return true, nil
		}
	}
	return false, nil
}

// ---- 用户组授权（内存实现） ----

func (m *MemoryStore) AddGroupMember(g GroupMember) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.spaces[g.SpaceID]; !ok {
		return ErrNotFound
	}
	if len(m.groupMembers[g.GroupID]) == 0 {
		return ErrGroupNotFound
	}
	if m.groupRoles[g.SpaceID] == nil {
		m.groupRoles[g.SpaceID] = make(map[uuid.UUID]string)
	}
	if _, exists := m.groupRoles[g.SpaceID][g.GroupID]; exists {
		return ErrGroupExists
	}
	m.groupRoles[g.SpaceID][g.GroupID] = g.Role
	return nil
}

func (m *MemoryStore) UpdateGroupMemberRole(spaceID, groupID uuid.UUID, role string) (GroupMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groupRoles[spaceID][groupID]; !ok {
		return GroupMember{}, ErrNotFound
	}
	m.groupRoles[spaceID][groupID] = role
	return GroupMember{SpaceID: spaceID, GroupID: groupID, Role: role, CreatedAt: time.Now()}, nil
}

func (m *MemoryStore) RemoveGroupMember(spaceID, groupID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groupRoles[spaceID][groupID]; !ok {
		return ErrNotFound
	}
	delete(m.groupRoles[spaceID], groupID)
	return nil
}

func (m *MemoryStore) ListGroupMembers(spaceID uuid.UUID) ([]GroupMember, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []GroupMember
	for groupID, role := range m.groupRoles[spaceID] {
		count := int64(len(m.groupMembers[groupID]))
		out = append(out, GroupMember{SpaceID: spaceID, GroupID: groupID, Role: role, MemberCount: count, CreatedAt: time.Now()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID.String() < out[j].GroupID.String() })
	return out, nil
}

func (m *MemoryStore) GroupRole(spaceID, groupID uuid.UUID) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	role, ok := m.groupRoles[spaceID][groupID]
	if !ok {
		return "", ErrNotFound
	}
	return role, nil
}

// ---- 邀请（内存实现，测试用） ----

func (m *MemoryStore) CreateInvite(v Invite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[v.ID] = v
	return nil
}

func (m *MemoryStore) GetInviteByTokenHash(hash string) (Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.invites {
		if v.TokenHash == hash {
			return v, nil
		}
	}
	return Invite{}, ErrNotFound
}

func (m *MemoryStore) FindActiveInvite(spaceID uuid.UUID, email string, now time.Time) (Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.invites {
		if v.SpaceID == spaceID && v.Email == email && v.AcceptedAt == nil && now.Before(v.ExpiresAt) {
			return v, nil
		}
	}
	return Invite{}, ErrNotFound
}

func (m *MemoryStore) ListInvites(spaceID uuid.UUID, limit int) ([]Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Invite
	for _, v := range m.invites {
		if v.SpaceID == spaceID {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) DeleteInvite(spaceID, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.invites[id]
	if !ok || v.SpaceID != spaceID {
		return ErrNotFound
	}
	delete(m.invites, id)
	return nil
}

func (m *MemoryStore) MarkInviteAccepted(id uuid.UUID, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.invites[id]
	if !ok || v.AcceptedAt != nil || !now.Before(v.ExpiresAt) {
		return false, nil
	}
	v.AcceptedAt = &now
	m.invites[id] = v
	return true, nil
}
