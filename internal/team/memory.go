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
	mu       sync.RWMutex
	teams    map[uuid.UUID]Team
	members  map[uuid.UUID]map[uuid.UUID]Member // team_id -> user_id -> member
	roots    map[uuid.UUID]files.File
	teamName map[string]uuid.UUID
	invites  map[uuid.UUID]Invite // invite_id -> invite
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		teams:    make(map[uuid.UUID]Team),
		members:  make(map[uuid.UUID]map[uuid.UUID]Member),
		roots:    make(map[uuid.UUID]files.File),
		teamName: make(map[string]uuid.UUID),
		invites:  make(map[uuid.UUID]Invite),
	}
}

// Root 返回团队根目录记录（供测试断言事务创建结果）。
func (m *MemoryStore) Root(teamID uuid.UUID) (files.File, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.roots[teamID]
	return f, ok
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

// ListForUserStats 内存实现：成员数按内存表统计，存储用量恒 0（内存库
// 无 files 数据；仅供单测断言 my_role/member_count）。
func (m *MemoryStore) ListForUserStats(userID uuid.UUID) ([]TeamInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []TeamInfo
	for id := range m.members {
		mem, ok := m.members[id][userID]
		if !ok {
			continue
		}
		t, found := m.teams[id]
		if !found || t.DeletedAt != nil {
			continue
		}
		out = append(out, TeamInfo{Team: t, MyRole: mem.Role, MemberCount: int64(len(m.members[id]))})
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

func (m *MemoryStore) UpdateMemberRole(teamID, userID uuid.UUID, role string) (Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mem, ok := m.members[teamID][userID]
	if !ok {
		return Member{}, ErrNotFound
	}
	mem.Role = role
	m.members[teamID][userID] = mem
	return mem, nil
}

func (m *MemoryStore) Role(teamID, userID uuid.UUID) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem, ok := m.members[teamID][userID]
	if !ok {
		return "", nil
	}
	return mem.Role, nil
}

func (m *MemoryStore) TransferOwnership(teamID, oldOwner, newOwner uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.teams[teamID]
	if !ok || t.DeletedAt != nil || t.OwnerID != oldOwner {
		return ErrNotFound
	}
	mem, ok := m.members[teamID][newOwner]
	if !ok {
		return ErrNotFound
	}
	old, ok := m.members[teamID][oldOwner]
	if !ok {
		return ErrNotFound
	}
	t.OwnerID = newOwner
	m.teams[teamID] = t
	mem.Role = RoleOwner
	m.members[teamID][newOwner] = mem
	old.Role = RoleAdmin
	m.members[teamID][oldOwner] = old
	return nil
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

// ---- 团队邀请（内存实现，测试用） ----

func (m *MemoryStore) CreateInvite(v Invite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.invites[v.ID] = v
	return nil
}

func (m *MemoryStore) GetInvite(id uuid.UUID) (Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.invites[id]
	if !ok {
		return Invite{}, ErrNotFound
	}
	return v, nil
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

func (m *MemoryStore) FindActiveInvite(teamID uuid.UUID, email string, now time.Time) (Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.invites {
		if v.TeamID == teamID && v.Email == email && v.AcceptedAt == nil && now.Before(v.ExpiresAt) {
			return v, nil
		}
	}
	return Invite{}, ErrNotFound
}

func (m *MemoryStore) ListInvites(teamID uuid.UUID, limit int) ([]Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Invite
	for _, v := range m.invites {
		if v.TeamID == teamID {
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

func (m *MemoryStore) DeleteInvite(teamID, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.invites[id]
	if !ok || v.TeamID != teamID {
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
