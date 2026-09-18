package group

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var _ Repo = (*MemoryStore)(nil)

// MemoryStore 是 Repo 的内存实现，供测试使用（不依赖 PostgreSQL）。
type MemoryStore struct {
	mu        sync.RWMutex
	groups    map[uuid.UUID]Group
	members   map[uuid.UUID]map[uuid.UUID]Member // group_id -> user_id -> member
	groupName map[string]uuid.UUID
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		groups:    make(map[uuid.UUID]Group),
		members:   make(map[uuid.UUID]map[uuid.UUID]Member),
		groupName: make(map[string]uuid.UUID),
	}
}

func (m *MemoryStore) Create(g Group) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.groupName[g.Name]; exists {
		return ErrNameConflict
	}
	m.groups[g.ID] = g
	m.groupName[g.Name] = g.ID
	return nil
}

func (m *MemoryStore) Get(id uuid.UUID) (Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[id]
	if !ok {
		return Group{}, ErrNotFound
	}
	return g, nil
}

func (m *MemoryStore) List() ([]Group, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Group
	for id, g := range m.groups {
		g.MemberCount = int64(len(m.members[id]))
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

func (m *MemoryStore) Update(id uuid.UUID, name string, description *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[id]
	if !ok {
		return ErrNotFound
	}
	if old, exists := m.groupName[name]; exists && old != id {
		return ErrNameConflict
	}
	delete(m.groupName, g.Name)
	g.Name = name
	if description != nil {
		g.Description = *description
	}
	g.UpdatedAt = time.Now()
	m.groupName[name] = id
	m.groups[id] = g
	return nil
}

func (m *MemoryStore) Delete(id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.groups[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.groupName, g.Name)
	delete(m.groups, id)
	delete(m.members, id)
	return nil
}

func (m *MemoryStore) ListMembers(groupID uuid.UUID) ([]Member, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Member
	for _, mem := range m.members[groupID] {
		out = append(out, mem)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].JoinedAt.Equal(out[j].JoinedAt) {
			return out[i].JoinedAt.Before(out[j].JoinedAt)
		}
		return out[i].UserID.String() < out[j].UserID.String()
	})
	return out, nil
}

func (m *MemoryStore) AddMember(mem Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.groups[mem.GroupID]; !ok {
		return ErrNotFound
	}
	if m.members[mem.GroupID] == nil {
		m.members[mem.GroupID] = make(map[uuid.UUID]Member)
	}
	if _, exists := m.members[mem.GroupID][mem.UserID]; exists {
		return ErrMemberExists
	}
	m.members[mem.GroupID][mem.UserID] = mem
	return nil
}

func (m *MemoryStore) RemoveMember(groupID, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[groupID]; !ok {
		return ErrNotFound
	}
	if _, ok := m.members[groupID][userID]; !ok {
		return ErrNotFound
	}
	delete(m.members[groupID], userID)
	return nil
}

func (m *MemoryStore) MembershipsForUsers(userIDs []uuid.UUID) ([]Membership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	wanted := make(map[uuid.UUID]bool, len(userIDs))
	for _, id := range userIDs {
		wanted[id] = true
	}
	var out []Membership
	for groupID, members := range m.members {
		g, ok := m.groups[groupID]
		if !ok {
			continue
		}
		for _, mem := range members {
			if wanted[mem.UserID] {
				out = append(out, Membership{UserID: mem.UserID, GroupID: groupID, GroupName: g.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupName != out[j].GroupName {
			return out[i].GroupName < out[j].GroupName
		}
		return out[i].UserID.String() < out[j].UserID.String()
	})
	return out, nil
}
