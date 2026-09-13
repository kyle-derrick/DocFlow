package team

import (
	"sort"
	"sync"

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
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		teams:    make(map[uuid.UUID]Team),
		members:  make(map[uuid.UUID]map[uuid.UUID]Member),
		roots:    make(map[uuid.UUID]files.File),
		teamName: make(map[string]uuid.UUID),
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
			if t, found := m.teams[id]; found {
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

func (m *MemoryStore) Role(teamID, userID uuid.UUID) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mem, ok := m.members[teamID][userID]
	if !ok {
		return "", nil
	}
	return mem.Role, nil
}

func (m *MemoryStore) CanWrite(userID, teamID uuid.UUID) (bool, error) {
	role, err := m.Role(teamID, userID)
	if err != nil {
		return false, err
	}
	return role == RoleOwner || role == RoleEditor, nil
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
