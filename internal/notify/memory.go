package notify

import (
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	_ Store          = (*MemoryStore)(nil)
	_ PreferenceRepo = (*MemoryPreferenceRepo)(nil)
)

// MemoryStore 是 Store 的内存实现，供测试使用（不依赖 PostgreSQL）。
type MemoryStore struct {
	mu    sync.RWMutex
	items map[uuid.UUID]Notification
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{items: make(map[uuid.UUID]Notification)} }

func (m *MemoryStore) Create(n Notification) error {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[n.ID] = n
	return nil
}

func (m *MemoryStore) List(user uuid.UUID, unreadOnly bool, limit int, cursor string) ([]Notification, error) {
	var before time.Time
	if cursor != "" {
		at, err := time.Parse(time.RFC3339Nano, cursor)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		before = at
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Notification
	for _, n := range m.items {
		if n.UserID != user {
			continue
		}
		if unreadOnly && n.IsRead {
			continue
		}
		if cursor != "" && !n.CreatedAt.Before(before) {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStore) MarkRead(user, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.items[id]
	if !ok || n.UserID != user {
		return false, nil
	}
	if !n.IsRead {
		now := time.Now().UTC()
		n.IsRead = true
		n.ReadAt = &now
		m.items[id] = n
	}
	return true, nil
}

func (m *MemoryStore) MarkAllRead(user uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	var n int64
	for id, v := range m.items {
		if v.UserID != user || v.IsRead {
			continue
		}
		v.IsRead = true
		v.ReadAt = &now
		m.items[id] = v
		n++
	}
	return n, nil
}

func (m *MemoryStore) CountUnread(user uuid.UUID) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var count int64
	for _, n := range m.items {
		if n.UserID == user && !n.IsRead {
			count++
		}
	}
	return count, nil
}

// MemoryPreferenceRepo 是 PreferenceRepo 的内存实现，供测试使用。
type MemoryPreferenceRepo struct {
	mu    sync.RWMutex
	items map[uuid.UUID]UserNotificationPreference
}

func NewMemoryPreferenceRepo() *MemoryPreferenceRepo {
	return &MemoryPreferenceRepo{items: make(map[uuid.UUID]UserNotificationPreference)}
}

func (m *MemoryPreferenceRepo) key(user uuid.UUID, eventType string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(user.String()+"|"+eventType))
}

func (m *MemoryPreferenceRepo) Get(user uuid.UUID, eventType string) (UserNotificationPreference, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.items[m.key(user, eventType)]
	return p, ok, nil
}

func (m *MemoryPreferenceRepo) Set(user uuid.UUID, eventType string, enabled bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[m.key(user, eventType)] = UserNotificationPreference{UserID: user, EventType: eventType, Enabled: enabled, UpdatedAt: now}
	return nil
}
