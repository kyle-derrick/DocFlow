// aimemory.go：AI 记忆（用户维度长期偏好，ai_memory 表，migration 050）。
//
// kind=manual 由用户经 POST /api/v1/ai/memory 手动维护；kind=auto 为
// 「AI 记忆自动提取」的服务端内部写入（CreateAutoAIMemory，绕过 API 层
// 的 manual 限制——API 侧 CreateAIMemory 仍拒绝 auto，见下）；/ai/chat
// 的 include_memory=true 时取最近 20 条拼入 system 上下文。存储方法挂
// 在 UserStore（模式同 user_ai_prefs 的 GetAIPrefs/SetAIPrefs）；删除
// 用户经外键 ON DELETE CASCADE 级联清理。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ai_memory.kind 取值（CHECK 约束）：manual=用户手动添加；auto=服务端
// 自动提取（仅经 CreateAutoAIMemory 内部写入，API 拒绝）。
const (
	AIMemoryKindManual = "manual"
	AIMemoryKindAuto   = "auto"
)

// AIMemoryMaxContentRunes 单条记忆 content 长度上限（rune 计）。
const AIMemoryMaxContentRunes = 2000

// AIMemoryAutoMax 单用户自动提取记忆（kind=auto）的保留条数上限：超出时
// TrimAutoAIMemory 删最旧的 auto（manual 永不参与裁剪）。
const AIMemoryAutoMax = 100

// ErrInvalidAIMemory 表示记忆载荷非法（content 空/超长、kind 非法或 API
// 不支持；HTTP 400 语义）。
var ErrInvalidAIMemory = errors.New("invalid ai memory")

// AIMemory 对应 ai_memory 表一行（本人维度）。
type AIMemory struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	Kind      string    `gorm:"size:16;not null" json:"kind"`
	Content   string    `gorm:"not null" json:"content"`
	CreatedAt time.Time `gorm:"not null" json:"created_at"`
}

// TableName 显式映射到 ai_memory。
func (AIMemory) TableName() string { return "ai_memory" }

// ValidateAIMemory 校验一条记忆载荷：kind 限 manual/auto，content 去空白
// 后非空且 ≤2000 字符。
func ValidateAIMemory(kind, content string) error {
	switch kind {
	case AIMemoryKindManual, AIMemoryKindAuto:
	default:
		return fmt.Errorf("%w: kind 须为 manual 或 auto", ErrInvalidAIMemory)
	}
	if trimmed := strings.TrimSpace(content); trimmed == "" {
		return fmt.Errorf("%w: content 不能为空", ErrInvalidAIMemory)
	} else if len([]rune(trimmed)) > AIMemoryMaxContentRunes {
		return fmt.Errorf("%w: content 过长（≤%d 字符）", ErrInvalidAIMemory, AIMemoryMaxContentRunes)
	}
	return nil
}

// ListAIMemory 返回用户最近的记忆（created_at 倒序，最新在前；limit 截断
// 0..500，缺省 200）。
func (s *UserStore) ListAIMemory(userID uuid.UUID, limit int) ([]AIMemory, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows []AIMemory
	err := s.db.Where("user_id = ?", userID).Order("created_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// CreateAIMemory 新增一条记忆（API 写入口——只允许 kind=manual；auto 为
// 服务端内部写入值，不经 API 落库，见 CreateAutoAIMemory；content 去空白
// 后落库）。
func (s *UserStore) CreateAIMemory(userID uuid.UUID, kind, content string) (AIMemory, error) {
	if kind == "" {
		kind = AIMemoryKindManual
	}
	if err := ValidateAIMemory(kind, content); err != nil {
		return AIMemory{}, err
	}
	if kind != AIMemoryKindManual {
		return AIMemory{}, fmt.Errorf("%w: 仅支持 kind=manual（auto 由服务端自动提取写入）", ErrInvalidAIMemory)
	}
	row := AIMemory{ID: uuid.New(), UserID: userID, Kind: kind, Content: strings.TrimSpace(content), CreatedAt: time.Now().UTC()}
	if err := s.db.Create(&row).Error; err != nil {
		return AIMemory{}, err
	}
	return row, nil
}

// CreateAutoAIMemory 服务端内部写入自动提取记忆（kind=auto；绕过 API
// 层 CreateAIMemory 的 manual 限制，content 校验保留——去空白非空、
// ≤2000 字符，失败返回 ErrInvalidAIMemory）。提取链路失败不重试（调用
// 方 best-effort，本方法只保证单条落库语义）。
func (s *UserStore) CreateAutoAIMemory(userID uuid.UUID, content string) (AIMemory, error) {
	if err := ValidateAIMemory(AIMemoryKindAuto, content); err != nil {
		return AIMemory{}, err
	}
	row := AIMemory{ID: uuid.New(), UserID: userID, Kind: AIMemoryKindAuto, Content: strings.TrimSpace(content), CreatedAt: time.Now().UTC()}
	if err := s.db.Create(&row).Error; err != nil {
		return AIMemory{}, err
	}
	return row, nil
}

// TrimAutoAIMemory 自动记忆超过 keep 条时删最旧的 auto（manual 不动）：
// 按该用户 kind=auto 的 created_at 倒序保留最新 keep 条，其余删除；
// 未超限时空删（无副作用）。
func (s *UserStore) TrimAutoAIMemory(userID uuid.UUID, keep int) error {
	if keep < 0 {
		keep = 0
	}
	sub := s.db.Model(&AIMemory{}).
		Where("user_id = ? AND kind = ?", userID, AIMemoryKindAuto).
		Order("created_at DESC").
		Offset(keep).
		Select("id")
	return s.db.Where("id IN (?)", sub).Delete(&AIMemory{}).Error
}

// UpdateAIMemory 编辑本人一条记忆的 content（kind 不变；校验同创建——
// 去空白、≤2000 字符，落库存去空白值）。不存在/非本人返回 found=false
// （语义同 DeleteAIMemory，避免越权探测他人记忆存在性）；校验失败返回
// ErrInvalidAIMemory（HTTP 400 语义），且不触库。
func (s *UserStore) UpdateAIMemory(userID, id uuid.UUID, content string) (AIMemory, bool, error) {
	if err := ValidateAIMemory(AIMemoryKindManual, content); err != nil {
		return AIMemory{}, false, err
	}
	res := s.db.Model(&AIMemory{}).Where("user_id = ? AND id = ?", userID, id).
		Updates(map[string]any{"content": strings.TrimSpace(content)})
	if res.Error != nil {
		return AIMemory{}, false, res.Error
	}
	if res.RowsAffected == 0 {
		return AIMemory{}, false, nil
	}
	var row AIMemory
	if err := s.db.Where("user_id = ? AND id = ?", userID, id).First(&row).Error; err != nil {
		return AIMemory{}, false, err
	}
	return row, true, nil
}

// DeleteAIMemory 删除本人一条记忆；不存在/非本人返回 false（不区分两种
// 情形，避免越权探测他人记忆存在性）。
func (s *UserStore) DeleteAIMemory(userID, id uuid.UUID) (bool, error) {
	res := s.db.Where("user_id = ? AND id = ?", userID, id).Delete(&AIMemory{})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}
