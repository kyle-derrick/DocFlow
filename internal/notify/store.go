package notify

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrNotFound 通知不存在或不属于该用户。
	ErrNotFound = errors.New("notification not found")
	// ErrUnknownEventType 偏好设置遇到未知事件类型。
	ErrUnknownEventType = errors.New("unknown notification event type")
	// ErrInvalidCursor 列表游标不是合法的 RFC3339Nano 时间文本。
	ErrInvalidCursor = errors.New("invalid notification cursor")
)

// Store 抽象通知存取；GormStore 为 PostgreSQL 实现，MemoryStore 供测试使用。
type Store interface {
	Create(n Notification) error
	// List 按 created_at 倒序返回 user 的通知。unreadOnly 只取未读；
	// cursor 为上一页最后一条的 created_at（RFC3339Nano 文本），取该时间点
	//（不含）之前的记录；空串为第一页（简化游标：同一秒多条可能跨页跳漏，
	// v1.0 可接受）。limit 由调用方约束。
	List(user uuid.UUID, unreadOnly bool, limit int, cursor string) ([]Notification, error)
	// MarkRead 标记 user 的通知为已读（幂等）；通知不存在或不属于 user
	// 返回 found=false（HTTP 层映射 404，不泄露存在性）。
	MarkRead(user, id uuid.UUID) (bool, error)
	// MarkAllRead 标记 user 的全部未读通知为已读，返回影响行数。
	MarkAllRead(user uuid.UUID) (int64, error)
	// CountUnread 返回 user 的未读数（铃铛徽标）。
	CountUnread(user uuid.UUID) (int64, error)
}

// PreferenceRepo 抽象通知偏好存取；Get 的 found=false 表示无记录（默认开关）。
type PreferenceRepo interface {
	Get(user uuid.UUID, eventType string) (p UserNotificationPreference, found bool, err error)
	// Set upsert（PK user_id+event_type 冲突时覆盖 enabled/updated_at）。
	Set(user uuid.UUID, eventType string, enabled bool, now time.Time) error
}

// GormStore 是 Store 的 PostgreSQL 实现。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

var _ Store = (*GormStore)(nil)

func (s *GormStore) Create(n Notification) error {
	return s.db.Create(&n).Error
}

func (s *GormStore) List(user uuid.UUID, unreadOnly bool, limit int, cursor string) ([]Notification, error) {
	q := s.db.Where("user_id = ?", user)
	if unreadOnly {
		q = q.Where("is_read = false")
	}
	if cursor != "" {
		at, err := time.Parse(time.RFC3339Nano, cursor)
		if err != nil {
			return nil, ErrInvalidCursor
		}
		q = q.Where("created_at < ?", at)
	}
	var out []Notification
	err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error
	return out, err
}

func (s *GormStore) MarkRead(user, id uuid.UUID) (bool, error) {
	var n Notification
	err := s.db.Where("id = ? AND user_id = ?", id, user).First(&n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n.IsRead {
		return true, nil // 幂等：已读不重复更新 read_at
	}
	now := time.Now().UTC()
	err = s.db.Model(&Notification{}).
		Where("id = ? AND user_id = ? AND is_read = false", id, user).
		Updates(map[string]any{"is_read": true, "read_at": now}).Error
	return err == nil, err
}

func (s *GormStore) MarkAllRead(user uuid.UUID) (int64, error) {
	result := s.db.Model(&Notification{}).
		Where("user_id = ? AND is_read = false", user).
		Updates(map[string]any{"is_read": true, "read_at": time.Now().UTC()})
	return result.RowsAffected, result.Error
}

func (s *GormStore) CountUnread(user uuid.UUID) (int64, error) {
	var count int64
	err := s.db.Model(&Notification{}).Where("user_id = ? AND is_read = false", user).Count(&count).Error
	return count, err
}

// GormPreferenceRepo 是 PreferenceRepo 的 PostgreSQL 实现。
type GormPreferenceRepo struct{ db *gorm.DB }

func NewGormPreferenceRepo(db *gorm.DB) *GormPreferenceRepo { return &GormPreferenceRepo{db: db} }

var _ PreferenceRepo = (*GormPreferenceRepo)(nil)

func (r *GormPreferenceRepo) Get(user uuid.UUID, eventType string) (UserNotificationPreference, bool, error) {
	var p UserNotificationPreference
	err := r.db.Where("user_id = ? AND event_type = ?", user, eventType).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UserNotificationPreference{}, false, nil
	}
	if err != nil {
		return UserNotificationPreference{}, false, err
	}
	return p, true, nil
}

func (r *GormPreferenceRepo) Set(user uuid.UUID, eventType string, enabled bool, now time.Time) error {
	p := UserNotificationPreference{UserID: user, EventType: eventType, Enabled: enabled, UpdatedAt: now}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "event_type"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "updated_at"}),
	}).Create(&p).Error
}
