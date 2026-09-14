package invite

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var _ Repo = (*MemoryStore)(nil)

// MemoryStore 是 Repo 的内存实现，供测试使用（不依赖 PostgreSQL）。
type MemoryStore struct {
	mu    sync.Mutex
	items map[uuid.UUID]Invitation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[uuid.UUID]Invitation)}
}

// Put 直接写入/覆盖一条邀请，供测试构造特定状态（过期、已接受等）。
func (m *MemoryStore) Put(v Invitation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[v.ID] = v
}

func (m *MemoryStore) Create(v Invitation) error {
	m.Put(v)
	return nil
}

func (m *MemoryStore) Get(id uuid.UUID) (Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok {
		return Invitation{}, ErrNotFound
	}
	return v, nil
}

func (m *MemoryStore) GetByTokenHash(hash string) (Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.items {
		if v.TokenHash == hash {
			return v, nil
		}
	}
	return Invitation{}, ErrNotFound
}

func (m *MemoryStore) FindActiveByEmail(email string, now time.Time) (Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found *Invitation
	for _, v := range m.items {
		if v.Email != email || v.AcceptedAt != nil || !now.Before(v.ExpiresAt) {
			continue
		}
		if found == nil || v.CreatedAt.After(found.CreatedAt) {
			candidate := v
			found = &candidate
		}
	}
	if found == nil {
		return Invitation{}, ErrNotFound
	}
	return *found, nil
}

func (m *MemoryStore) List(limit int) ([]Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Invitation, 0, len(m.items))
	for _, v := range m.items {
		out = append(out, v)
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

func (m *MemoryStore) Delete(id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrNotFound
	}
	delete(m.items, id)
	return nil
}

// MarkAccepted 与 GormStore 语义一致：仅未接受且未过期时成功。
func (m *MemoryStore) MarkAccepted(id uuid.UUID, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok || v.AcceptedAt != nil || !now.Before(v.ExpiresAt) {
		return false, nil
	}
	v.AcceptedAt = &now
	m.items[id] = v
	return true, nil
}
