package webhook

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	// ErrNotFound webhook 不存在或不属于该用户（HTTP 层映射 404，不泄露存在性）。
	ErrNotFound = errors.New("webhook not found")
	// ErrInvalidURL 回调 URL 未通过格式校验（HTTP 层映射 400）。
	ErrInvalidURL = errors.New("invalid webhook url")
	// ErrInvalidEvents 事件列表为空或含未知事件类型（HTTP 层映射 400）。
	ErrInvalidEvents = errors.New("invalid webhook events")
	// ErrDuplicateURL 同一用户重复注册同一 URL（UNIQUE(user_id,url)，HTTP 层 409）。
	ErrDuplicateURL = errors.New("webhook url already registered")
)

// Store 抽象 webhook 存取与投递结果记账；GormStore 为 PostgreSQL 实现，
// MemoryStore 供测试使用。写方法均以 owner 或 id 定位，归属校验在 SQL
// 条件中完成（非属主更新/删除影响 0 行 → not found）。
type Store interface {
	// Create 写入新行；(user_id,url) 唯一冲突返回 ErrDuplicateURL。
	Create(w Webhook) error
	// List 返回 owner 的全部 webhook（created_at 倒序，含已禁用）。
	List(owner uuid.UUID) ([]Webhook, error)
	// ListForEvent 返回 owner 启用且订阅了 event 的 webhook
	//（通知分发回调使用；event 由调用方保证为白名单值）。
	ListForEvent(owner uuid.UUID, event string) ([]Webhook, error)
	// Get 按 ID 读取（投递任务定位 hook 用，无归属过滤）。
	Get(id uuid.UUID) (Webhook, error)
	// SetEnabled 更新启停（owner 归属过滤），返回是否命中。
	SetEnabled(owner, id uuid.UUID, enabled bool, now time.Time) (bool, error)
	// Delete 删除（owner 归属过滤），返回是否命中。
	Delete(owner, id uuid.UUID) (bool, error)
	// RecordSuccess 记录一次成功投递：last_status/last_delivered_at 更新、
	// failure_count 清零。
	RecordSuccess(id uuid.UUID, status int, now time.Time) error
	// RecordFailure 记录一次失败投递并原子递增 failure_count，返回递增后的
	// 连续失败计数（行不存在返回 0）。
	RecordFailure(id uuid.UUID, status int, now time.Time) (int, error)
}

// GormStore 是 Store 的 PostgreSQL 实现。
type GormStore struct{ db *gorm.DB }

func NewGormStore(db *gorm.DB) *GormStore { return &GormStore{db: db} }

var _ Store = (*GormStore)(nil)

// isUniqueViolation 识别唯一约束冲突（SQLSTATE 23505 或驱动错误文本），
// 与 internal/team 同款实现（不引 pgconn，兼容测试替身错误）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "SQLSTATE 23505") ||
		strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}

func (s *GormStore) Create(w Webhook) error {
	if err := s.db.Create(&w).Error; err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateURL
		}
		return err
	}
	return nil
}

func (s *GormStore) List(owner uuid.UUID) ([]Webhook, error) {
	var out []Webhook
	err := s.db.Where("user_id = ?", owner).Order("created_at DESC").Find(&out).Error
	return out, err
}

func (s *GormStore) ListForEvent(owner uuid.UUID, event string) ([]Webhook, error) {
	var out []Webhook
	err := s.db.Where("user_id = ? AND enabled = ? AND ? = ANY(events)", owner, true, event).
		Order("created_at DESC").Find(&out).Error
	return out, err
}

func (s *GormStore) Get(id uuid.UUID) (Webhook, error) {
	var w Webhook
	err := s.db.Where("id = ?", id).First(&w).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Webhook{}, ErrNotFound
	}
	if err != nil {
		return Webhook{}, err
	}
	return w, nil
}

func (s *GormStore) SetEnabled(owner, id uuid.UUID, enabled bool, now time.Time) (bool, error) {
	result := s.db.Model(&Webhook{}).
		Where("id = ? AND user_id = ?", id, owner).
		Updates(map[string]any{"enabled": enabled, "updated_at": now})
	return result.RowsAffected > 0, result.Error
}

func (s *GormStore) Delete(owner, id uuid.UUID) (bool, error) {
	result := s.db.Where("id = ? AND user_id = ?", id, owner).Delete(&Webhook{})
	return result.RowsAffected > 0, result.Error
}

func (s *GormStore) RecordSuccess(id uuid.UUID, status int, now time.Time) error {
	return s.db.Model(&Webhook{}).Where("id = ?", id).Updates(map[string]any{
		"last_status":       int16(status),
		"last_delivered_at": now,
		"failure_count":     0,
		"updated_at":        now,
	}).Error
}

func (s *GormStore) RecordFailure(id uuid.UUID, status int, now time.Time) (int, error) {
	var count int
	err := s.db.Raw(`UPDATE webhooks SET failure_count = failure_count + 1, last_status = ?, last_delivered_at = ?, updated_at = ? WHERE id = ? RETURNING failure_count`,
		int16(status), now, now, id).Scan(&count).Error
	return count, err
}
