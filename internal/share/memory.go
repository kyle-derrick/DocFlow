package share

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var _ Repo = (*MemoryStore)(nil)

// MemoryStore 是 Repo 的内存实现，供测试使用（不依赖 PostgreSQL）。
type MemoryStore struct {
	mu         sync.RWMutex
	items      map[uuid.UUID]Share
	public     map[uuid.UUID]bool
	shareUsers map[uuid.UUID]map[uuid.UUID]time.Time // share_id -> user_id -> created_at
	shareTeams map[uuid.UUID]map[uuid.UUID]time.Time // share_id -> team_id -> created_at
	sessions   map[string]AccessSession              // session_hash -> 会话
	events     []AccessEvent                         // created_at 升序追加
	eventSeq   int64
	// membership 注入的团队成员判定（测试用）：user 是否属于 team；
	// nil 时 share_teams 命中不可达（与未注入 membership 的 Service 一致）。
	membership func(userID, teamID uuid.UUID) bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		items:      make(map[uuid.UUID]Share),
		public:     make(map[uuid.UUID]bool),
		shareUsers: make(map[uuid.UUID]map[uuid.UUID]time.Time),
		shareTeams: make(map[uuid.UUID]map[uuid.UUID]time.Time),
		sessions:   make(map[string]AccessSession),
	}
}

// Put 直接写入/覆盖一条分享，供测试构造特定状态（过期、撤销、达上限等）。
func (m *MemoryStore) Put(v Share) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[v.ID] = v
}

// IsPublic 返回文件 is_public 辅助字段的当前值。
func (m *MemoryStore) IsPublic(fileID uuid.UUID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.public[fileID]
}

func (m *MemoryStore) Create(v Share) error {
	m.Put(v)
	return nil
}

func (m *MemoryStore) Get(id uuid.UUID) (Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.items[id]
	if !ok {
		return Share{}, ErrNotFound
	}
	return v, nil
}

func (m *MemoryStore) GetByOwner(owner, id uuid.UUID) (Share, error) {
	v, err := m.Get(id)
	if err != nil || v.OwnerID != owner {
		return Share{}, ErrNotFound
	}
	return v, nil
}

func (m *MemoryStore) GetByTokenHash(hash string) (Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.items {
		if v.TokenHash == hash {
			return v, nil
		}
	}
	return Share{}, ErrNotFound
}

func (m *MemoryStore) ListByOwner(owner uuid.UUID, limit int) ([]Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Share
	for _, v := range m.items {
		if v.OwnerID == owner {
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

// SetMembership 注入团队成员判定器（测试用）：user 是否属于 team（幂等）。
func (m *MemoryStore) SetMembership(f func(userID, teamID uuid.UUID) bool) {
	if f != nil {
		m.membership = f
	}
}

// ListSharedWithUser 与 GormStore 语义一致：有效私有分享（不含自己创建的），
// share_users 显式授权或（注入的成员判定命中）share_teams 团队成员，created_at 倒序。
func (m *MemoryStore) ListSharedWithUser(user uuid.UUID, now time.Time, limit int) ([]Share, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Share
	for _, v := range m.items {
		if v.OwnerID == user || v.Visibility != VisibilityPrivate || !shareActive(v, now) {
			continue
		}
		granted := false
		if _, ok := m.shareUsers[v.ID][user]; ok {
			granted = true
		}
		if !granted && m.membership != nil {
			for teamID := range m.shareTeams[v.ID] {
				if m.membership(user, teamID) {
					granted = true
					break
				}
			}
		}
		if granted {
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

func (m *MemoryStore) Revoke(id uuid.UUID, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok {
		return ErrNotFound
	}
	if v.RevokedAt == nil {
		v.RevokedAt = &now
		m.items[id] = v
	}
	return nil
}

func (m *MemoryStore) ConsumeDownload(id uuid.UUID, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok || !shareActive(v, now) {
		return false, nil
	}
	v.DownloadCount++
	m.items[id] = v
	return true, nil
}

// DecrementDownload 与 GormStore 语义一致：回退一次 download_count
// （下限 0）；分享不存在或计数为 0 时静默成功。
func (m *MemoryStore) DecrementDownload(id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok || v.DownloadCount <= 0 {
		return nil
	}
	v.DownloadCount--
	m.items[id] = v
	return nil
}

func (m *MemoryStore) CountActiveByFile(fileID uuid.UUID, now time.Time) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var count int64
	for _, v := range m.items {
		// 与 GormStore 一致：is_public 只统计有效「公开」分享。
		if v.FileID == fileID && v.Visibility == VisibilityPublic && shareActive(v, now) {
			count++
		}
	}
	return count, nil
}

func (m *MemoryStore) SetFilePublic(fileID uuid.UUID, value bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.public[fileID] = value
	return nil
}

func (m *MemoryStore) AddShareUsers(shareID uuid.UUID, userIDs []uuid.UUID, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shareUsers[shareID] == nil {
		m.shareUsers[shareID] = make(map[uuid.UUID]time.Time)
	}
	for _, id := range userIDs {
		m.shareUsers[shareID][id] = now
	}
	return nil
}

func (m *MemoryStore) AddShareTeams(shareID uuid.UUID, teamIDs []uuid.UUID, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shareTeams[shareID] == nil {
		m.shareTeams[shareID] = make(map[uuid.UUID]time.Time)
	}
	for _, id := range teamIDs {
		m.shareTeams[shareID][id] = now
	}
	return nil
}

func (m *MemoryStore) ListShareUserIDs(shareID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(m.shareUsers[shareID]))
	for id := range m.shareUsers[shareID] {
		out = append(out, id)
	}
	return out, nil
}

func (m *MemoryStore) ListShareTeamIDs(shareID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(m.shareTeams[shareID]))
	for id := range m.shareTeams[shareID] {
		out = append(out, id)
	}
	return out, nil
}

func (m *MemoryStore) CreateSession(v AccessSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[v.SessionHash] = v
	return nil
}

func (m *MemoryStore) GetSessionByHash(hash string) (AccessSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.sessions[hash]
	if !ok {
		return AccessSession{}, ErrNotFound
	}
	return v, nil
}

// UpdateFields 与 GormStore 语义一致：nil 值清空对应可空列；未知列名报错。
func (m *MemoryStore) UpdateFields(id uuid.UUID, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[id]
	if !ok {
		return ErrNotFound
	}
	for key, val := range fields {
		switch key {
		case "expires_at":
			if t, ok := val.(*time.Time); ok {
				v.ExpiresAt = t
			} else {
				v.ExpiresAt = nil
			}
		case "max_downloads":
			if n, ok := val.(int); ok {
				x := n
				v.MaxDownloads = &x
			} else {
				v.MaxDownloads = nil
			}
		case "watermark_enabled":
			b, ok := val.(bool)
			if !ok {
				return fmt.Errorf("watermark_enabled: unexpected type %T", val)
			}
			v.WatermarkEnabled = b
		case "watermark_text":
			if t, ok := val.(string); ok && t != "" {
				v.WatermarkText = &t
			} else {
				v.WatermarkText = nil
			}
		default:
			return fmt.Errorf("share field %q is not updatable", key)
		}
	}
	m.items[id] = v
	return nil
}

func (m *MemoryStore) RecordAccessEvent(e AccessEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventSeq++
	e.ID = m.eventSeq
	m.events = append(m.events, e)
	return nil
}

// ShareAccessStats 与 GormStore 语义一致：总数、distinct ip_hash 与最近
// 20 条（created_at 倒序；同刻按插入序号倒序）。
func (m *MemoryStore) ShareAccessStats(shareID uuid.UUID) (AccessStats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var stats AccessStats
	visitors := make(map[string]struct{})
	for _, e := range m.events {
		if e.ShareID != shareID {
			continue
		}
		stats.TotalAccess++
		visitors[e.IPHash] = struct{}{}
	}
	stats.UniqueVisitors = int64(len(visitors))
	total := stats.TotalAccess
	start := total - recentAccessEventsLimit
	if start < 0 {
		start = 0
	}
	recent := make([]AccessEvent, 0, total-start)
	for i := total - 1; i >= start; i-- {
		recent = append(recent, m.events[i])
	}
	stats.Recent = recent
	return stats, nil
}
