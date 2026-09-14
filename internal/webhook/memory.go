package webhook

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var _ Store = (*MemoryStore)(nil)

// MemoryStore 是 Store 的内存实现，供测试使用（不依赖 PostgreSQL）。
type MemoryStore struct {
	mu    sync.RWMutex
	items map[uuid.UUID]Webhook
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{items: make(map[uuid.UUID]Webhook)} }

func (m *MemoryStore) Create(w Webhook) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.items {
		if v.UserID == w.UserID && v.URL == w.URL {
			return ErrDuplicateURL
		}
	}
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	m.items[w.ID] = w
	return nil
}

func (m *MemoryStore) sorted(owner uuid.UUID, filter func(Webhook) bool) []Webhook {
	var out []Webhook
	for _, v := range m.items {
		if v.UserID == owner && filter(v) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out
}

func (m *MemoryStore) List(owner uuid.UUID) ([]Webhook, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sorted(owner, func(Webhook) bool { return true }), nil
}

func (m *MemoryStore) ListForEvent(owner uuid.UUID, event string) ([]Webhook, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sorted(owner, func(w Webhook) bool {
		if !w.Enabled {
			return false
		}
		for _, e := range w.Events {
			if e == event {
				return true
			}
		}
		return false
	}), nil
}

func (m *MemoryStore) Get(id uuid.UUID) (Webhook, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.items[id]
	if !ok {
		return Webhook{}, ErrNotFound
	}
	return w, nil
}

func (m *MemoryStore) SetEnabled(owner, id uuid.UUID, enabled bool, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.items[id]
	if !ok || w.UserID != owner {
		return false, nil
	}
	w.Enabled = enabled
	w.UpdatedAt = now
	m.items[id] = w
	return true, nil
}

func (m *MemoryStore) Delete(owner, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.items[id]
	if !ok || w.UserID != owner {
		return false, nil
	}
	delete(m.items, id)
	return true, nil
}

func (m *MemoryStore) RecordSuccess(id uuid.UUID, status int, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.items[id]
	if !ok {
		return nil
	}
	s := int16(status)
	w.LastStatus = &s
	w.LastDeliveredAt = &now
	w.FailureCount = 0
	w.UpdatedAt = now
	m.items[id] = w
	return nil
}

func (m *MemoryStore) RecordFailure(id uuid.UUID, status int, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.items[id]
	if !ok {
		return 0, nil
	}
	w.FailureCount++
	s := int16(status)
	w.LastStatus = &s
	w.LastDeliveredAt = &now
	w.UpdatedAt = now
	m.items[id] = w
	return w.FailureCount, nil
}
