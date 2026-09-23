// Package ai —— usage.go：AI 用量记录（ai_usage 表，migration 044）。
//
// 每次对话/摘要补全记一行（user 维度由 HTTP/MCP 层注入 UserID——
// ChatService 不感知用户身份，recordUsage 在服务层填 provider/model/
// tokens/ms，user 侧经 UserContext 中间件包装写入）。
package ai

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UsageEntry 为一条用量记录（UserID 由调用方补齐；0 = 未关联）。
// Personal 标记命中用户个人 Provider 池（双轨制）——内存语义字段，
// ai_usage 表（migration 044）无对应列、不落库，供 HTTP 层审计与
// 即时统计消费。
type UsageEntry struct {
	UserID           uuid.UUID `json:"user_id"`
	ProviderID       string    `json:"provider_id"`
	Model            string    `json:"model"`
	Personal         bool      `json:"personal"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	DurationMS       int64     `json:"duration_ms"`
	CreatedAt        time.Time `json:"created_at"`
}

// UsageSink 抽象用量写入（HTTP/MCP 层以 UserID 包装后落库）。
type UsageSink interface {
	Record(entry UsageEntry) error
}

// NopUsageSink 空实现（测试/未注入）。
var NopUsageSink UsageSink = nopUsageSink{}

type nopUsageSink struct{}

func (nopUsageSink) Record(UsageEntry) error { return nil }

// AIUsage 对应 ai_usage 表一行（migration 044）。
type AIUsage struct {
	ID               uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID           uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	ProviderID       string    `gorm:"size:64;not null" json:"provider_id"`
	Model            string    `gorm:"size:128;not null" json:"model"`
	PromptTokens     int       `gorm:"not null;default:0" json:"prompt_tokens"`
	CompletionTokens int       `gorm:"not null;default:0" json:"completion_tokens"`
	DurationMS       int64     `gorm:"not null;default:0" json:"duration_ms"`
	CreatedAt        time.Time `gorm:"not null" json:"created_at"`
}

// TableName 显式映射到 ai_usage。
func (AIUsage) TableName() string { return "ai_usage" }

// ErrNoUser 表示用量记录缺少用户身份（调用方编程错误）。
var ErrNoUser = errors.New("ai usage entry has no user")

// UserUsageSink 以固定用户包装写入（HTTP/MCP 每请求构造）。
type UserUsageSink struct {
	Store *UsageStore
	User  uuid.UUID
}

// Record 校验并落库一条用量（UserID 取包装用户）。
func (u UserUsageSink) Record(entry UsageEntry) error {
	if u.Store == nil || u.User == uuid.Nil {
		return nil // 未接线：静默跳过（best-effort 语义）
	}
	entry.UserID = u.User
	return u.Store.Record(entry)
}

// UsageStore 为 ai_usage 的 GORM 读写实现。
type UsageStore struct{ db *gorm.DB }

// NewUsageStore 构造用量存储。
func NewUsageStore(db *gorm.DB) *UsageStore { return &UsageStore{db: db} }

// Record 落库一条用量。
func (s *UsageStore) Record(entry UsageEntry) error {
	if entry.UserID == uuid.Nil {
		return ErrNoUser
	}
	row := AIUsage{
		ID: uuid.New(), UserID: entry.UserID, ProviderID: entry.ProviderID,
		Model: entry.Model, PromptTokens: entry.PromptTokens,
		CompletionTokens: entry.CompletionTokens, DurationMS: entry.DurationMS,
		CreatedAt: entry.CreatedAt,
	}
	return s.db.Create(&row).Error
}

// UsageAggregateRow 为按用户聚合的用量行（管理面板统计表）。
type UsageAggregateRow struct {
	UserID           uuid.UUID `json:"user_id"`
	Username         string    `json:"username"`
	ProviderID       string    `json:"provider_id"`
	Model            string    `json:"model"`
	Calls            int64     `json:"calls"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	AvgDurationMS    int64     `json:"avg_duration_ms"`
}

// Aggregate 按用户+Provider+模型聚合指定时间窗的用量（from/to 零值 =
// 不限；limit 截断，缺省 100）。
func (s *UsageStore) Aggregate(from, to time.Time, limit int) ([]UsageAggregateRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := s.db.Table("ai_usage").
		Select("ai_usage.user_id, COALESCE(users.username, '') AS username, ai_usage.provider_id, ai_usage.model, COUNT(*) AS calls, COALESCE(SUM(ai_usage.prompt_tokens),0) AS prompt_tokens, COALESCE(SUM(ai_usage.completion_tokens),0) AS completion_tokens, COALESCE(SUM(ai_usage.prompt_tokens + ai_usage.completion_tokens),0) AS total_tokens, COALESCE(AVG(ai_usage.duration_ms),0)::bigint AS avg_duration_ms").
		Joins("LEFT JOIN users ON users.id = ai_usage.user_id")
	if !from.IsZero() {
		q = q.Where("ai_usage.created_at >= ?", from)
	}
	if !to.IsZero() {
		q = q.Where("ai_usage.created_at < ?", to)
	}
	var rows []UsageAggregateRow
	if err := q.Group("ai_usage.user_id, users.username, ai_usage.provider_id, ai_usage.model").
		Order("calls DESC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
