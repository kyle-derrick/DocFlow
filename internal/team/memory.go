package team

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
	mu        sync.RWMutex
	teams     map[uuid.UUID]Team
	members   map[uuid.UUID]map[uuid.UUID]Member // team_id -> user_id -> member
	roots     map[uuid.UUID]files.File
	teamName  map[string]uuid.UUID
	roles     map[uuid.UUID]Role   // role_id -> role
	roleNames map[string]uuid.UUID // "teamID:roleID"复合键按名称查重
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		teams:     make(map[uuid.UUID]Team),
		members:   make(map[uuid.UUID]map[uuid.UUID]Member),
		roots:     make(map[uuid.UUID]files.File),
		teamName:  make(map[string]uuid.UUID),
		roles:     make(map[uuid.UUID]Role),
		roleNames: make(map[string]uuid.UUID),
	}
}

// Root 返回团队根目录记录（供测试断言事务创建结果）。
func (m *MemoryStore) Root(teamID uuid.UUID) (files.File, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.roots[teamID]
	return f, ok
}

// DeleteRoleDirect 绕过服务层直接删除角色行（测试「角色行缺失 → fail closed」用）。
func (m *MemoryStore) DeleteRoleDirect(roleID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.roles[roleID]; ok {
		delete(m.roleNames, r.TeamID.String()+":"+r.Name)
		delete(m.roles, roleID)
	}
}

func (m *MemoryStore) CreateTeamWithRoot(t Team, owner Member, root files.File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.teamName[t.Name]; exists {
		return ErrNameConflict
	}
	if _, exists := m.members[t.ID][owner.UserID]; exists {
		return ErrMemberExists
	}
	m.teams[t.ID] = t
	m.teamName[t.Name] = t.ID
	if m.members[t.ID] == nil {
		m.members[t.ID] = make(map[uuid.UUID]Member)
	}
	m.members[t.ID][owner.UserID] = owner
	m.roots[t.ID] = root
	return nil
}

func (m *MemoryStore) Update(teamID uuid.UUID, name string, description *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.teams[teamID]
	if !ok || t.DeletedAt != nil {
		return ErrNotFound
	}
	if old, exists := m.teamName[name]; exists && old != teamID {
		return ErrNameConflict
	}
	delete(m.teamName, t.Name)
	t.Name = name
	if description != nil {
		t.Description = *description
	}
	m.teamName[name] = teamID
	m.teams[teamID] = t
	return nil
}
func (m *MemoryStore) Delete(teamID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.teams[teamID]
	if !ok || t.DeletedAt != nil {
		return ErrNotFound
	}
	now := time.Now()
	t.DeletedAt = &now
	m.teams[teamID] = t
	return nil
}

func (m *MemoryStore) ListRoles(teamID uuid.UUID) ([]Role, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Role
	for _, r := range m.roles {
		if r.TeamID != teamID {
			continue
		}
		r.MemberCount = m.countMembersByRoleLocked(teamID, r.ID)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}
func (m *MemoryStore) CreateRole(role Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.roles[role.ID]; exists {
		return ErrMemberExists
	}
	key := role.TeamID.String() + ":" + role.Name
	if _, exists := m.roleNames[key]; exists {
		return ErrNameConflict
	}
	m.roles[role.ID] = role
	m.roleNames[key] = role.ID
	return nil
}
func (m *MemoryStore) UpdateRole(teamID, roleID uuid.UUID, name string, permissions map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.roles[roleID]
	if !ok || r.TeamID != teamID {
		return ErrNotFound
	}
	key := teamID.String() + ":" + name
	if old, exists := m.roleNames[key]; exists && old != roleID {
		return ErrNameConflict
	}
	delete(m.roleNames, r.TeamID.String()+":"+r.Name)
	r.Name, r.Permissions = name, permissions
	m.roles[roleID] = r
	m.roleNames[key] = roleID
	return nil
}
func (m *MemoryStore) DeleteRole(teamID, roleID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.roles[roleID]
	if !ok || r.TeamID != teamID {
		return ErrNotFound
	}
	delete(m.roleNames, r.TeamID.String()+":"+r.Name)
	delete(m.roles, roleID)
	return nil
}

func (m *MemoryStore) Get(id uuid.UUID) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.teams[id]
	if !ok {
		return Team{}, ErrNotFound
	}
	return t, nil
}

func (m *MemoryStore) ListForUser(userID uuid.UUID) ([]Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Team
	for id := range m.members {
		if _, ok := m.members[id][userID]; ok {
			if t, found := m.teams[id]; found && t.DeletedAt == nil {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (m *MemoryStore) AddMember(mem Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.teams[mem.TeamID]; !ok {
		return ErrNotFound
	}
	if m.members[mem.TeamID] == nil {
		m.members[mem.TeamID] = make(map[uuid.UUID]Member)
	}
	if _, exists := m.members[mem.TeamID][mem.UserID]; exists {
		return ErrMemberExists
	}
	m.members[mem.TeamID][mem.UserID] = mem
	return nil
}

func (m *MemoryStore) RemoveMember(teamID, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[teamID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.members[teamID][userID]; !ok {
		return ErrNotFound
	}
	delete(m.members[teamID], userID)
	return nil
}

func (m *MemoryStore) ListMembers(teamID uuid.UUID) ([]Member, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Member
	for _, mem := range m.members[teamID] {
		if mem.RoleID != nil {
			if r, ok := m.roles[*mem.RoleID]; ok {
				mem.RoleName = r.Name
			}
		}
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

func (m *MemoryStore) UpdateMemberRole(teamID, userID uuid.UUID, role string, roleID *uuid.UUID) (Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.members[teamID][userID]
	if !ok {
		return Member{}, ErrNotFound
	}
	mem.Role, mem.RoleID = role, roleID
	if roleID != nil {
		if r, ok := m.roles[*roleID]; ok {
			mem.RoleName = r.Name
		} else {
			mem.RoleName = ""
		}
	} else {
		mem.RoleName = ""
	}
	m.members[teamID][userID] = mem
	return mem, nil
}

func (m *MemoryStore) Role(teamID, userID uuid.UUID) (string, error) {
	role, _, err := m.MemberRole(teamID, userID)
	return role, err
}

func (m *MemoryStore) MemberRole(teamID, userID uuid.UUID) (string, *uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem, ok := m.members[teamID][userID]
	if !ok {
		return "", nil, nil
	}
	return mem.Role, mem.RoleID, nil
}

func (m *MemoryStore) RolePermissions(teamID, roleID uuid.UUID) (map[string]any, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.roles[roleID]
	if !ok || r.TeamID != teamID {
		return nil, ErrNotFound
	}
	return r.Permissions, nil
}

func (m *MemoryStore) countMembersByRoleLocked(teamID, roleID uuid.UUID) int64 {
	var n int64
	for _, mem := range m.members[teamID] {
		if mem.RoleID != nil && *mem.RoleID == roleID {
			n++
		}
	}
	return n
}

func (m *MemoryStore) CountMembersByRole(teamID, roleID uuid.UUID) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.countMembersByRoleLocked(teamID, roleID), nil
}

func (m *MemoryStore) UserInAnyTeam(userID uuid.UUID, teamIDs []uuid.UUID) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, teamID := range teamIDs {
		if _, ok := m.members[teamID][userID]; ok {
			return true, nil
		}
	}
	return false, nil
}
