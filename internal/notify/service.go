package notify

import (
	"time"

	"github.com/google/uuid"
)

// Dispatcher 是事件分发接口：检查接收者偏好（无记录 = 默认开启；
// disabled 跳过）后写入通知。NotifyMany 为批量变体（file.updated 发给
// 团队多个成员等场景）。resourceID 为 uuid.Nil 时存 NULL。
type Dispatcher interface {
	Notify(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID) error
	NotifyMany(userIDs []uuid.UUID, eventType, title, body string, resourceID uuid.UUID) error
}

// Service 实现 Dispatcher，并暴露通知/偏好的本人维度查询（HTTP 层使用）。
type Service struct {
	store Store
	prefs PreferenceRepo
	now   func() time.Time
}

var _ Dispatcher = (*Service)(nil)

func NewService(store Store, prefs PreferenceRepo) *Service {
	return &Service{store: store, prefs: prefs, now: time.Now}
}

// SetClock 注入时钟（测试用，幂等）。
func (s *Service) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// enabled 判定偏好：读取失败按默认开关处理（保守不丢通知）。
func (s *Service) enabled(user uuid.UUID, eventType string) bool {
	p, found, err := s.prefs.Get(user, eventType)
	if err != nil || !found {
		return DefaultEnabled(eventType)
	}
	return p.Enabled
}

// Notify 分发单条通知：偏好 disabled 时静默跳过（返回 nil，非错误）。
func (s *Service) Notify(userID uuid.UUID, eventType, title, body string, resourceID uuid.UUID) error {
	if !s.enabled(userID, eventType) {
		return nil
	}
	n := Notification{ID: uuid.New(), UserID: userID, Type: eventType, Title: title, Body: body, CreatedAt: s.now().UTC()}
	if resourceID != uuid.Nil {
		id := resourceID
		n.ResourceID = &id
	}
	return s.store.Create(n)
}

// NotifyMany 批量分发：逐个检查偏好（各自短路），userIDs 去重；
// 单条写入失败中断返回（已写入的不回滚）。
func (s *Service) NotifyMany(userIDs []uuid.UUID, eventType, title, body string, resourceID uuid.UUID) error {
	seen := make(map[uuid.UUID]struct{}, len(userIDs))
	for _, id := range userIDs {
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if err := s.Notify(id, eventType, title, body, resourceID); err != nil {
			return err
		}
	}
	return nil
}

// List 见 Store.List。
func (s *Service) List(user uuid.UUID, unreadOnly bool, limit int, cursor string) ([]Notification, error) {
	if limit < 1 {
		limit = 20
	}
	return s.store.List(user, unreadOnly, limit, cursor)
}

// MarkRead 见 Store.MarkRead。
func (s *Service) MarkRead(user, id uuid.UUID) (bool, error) { return s.store.MarkRead(user, id) }

// MarkAllRead 见 Store.MarkAllRead。
func (s *Service) MarkAllRead(user uuid.UUID) (int64, error) { return s.store.MarkAllRead(user) }

// CountUnread 见 Store.CountUnread。
func (s *Service) CountUnread(user uuid.UUID) (int64, error) { return s.store.CountUnread(user) }

// PreferenceEnabled 返回事件类型的生效开关（无记录 = 默认）；
// 偏好读取失败返回错误（HTTP 层 500，不静默吞掉）。
func (s *Service) PreferenceEnabled(user uuid.UUID, eventType string) (bool, error) {
	p, found, err := s.prefs.Get(user, eventType)
	if err != nil {
		return false, err
	}
	if !found {
		return DefaultEnabled(eventType), nil
	}
	return p.Enabled, nil
}

// SetPreference 写入偏好（upsert）；未知事件类型返回 ErrUnknownEventType。
func (s *Service) SetPreference(user uuid.UUID, eventType string, enabled bool) error {
	if !ValidEventType(eventType) {
		return ErrUnknownEventType
	}
	return s.prefs.Set(user, eventType, enabled, s.now().UTC())
}
