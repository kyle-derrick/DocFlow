// Package webhook 提供出站 Webhook 通知渠道（v1.1）：用户自助注册回调
// URL，站内通知事件（与 notify 相同四类）发生时经 task:webhook-delivery
// 队列投递 JSON 载荷并附 X-DocFlow-Signature HMAC 签名头；连续失败达
// 阈值自动禁用。内存实现供测试，GormStore 为 PostgreSQL 实现。
package webhook

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Webhook 对应 webhooks 表（migration 018）。
// Secret 为签名密钥明文（取舍见 migration 018 注释：HMAC 出站签名必须
// 持有明文密钥，哈希不可行；JSON 序列化恒剔除，仅创建响应返回一次）。
type Webhook struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID          uuid.UUID  `gorm:"type:uuid;not null;index" json:"-"`
	URL             string     `gorm:"not null" json:"url"`
	Events          EventList  `gorm:"type:text[];not null" json:"events"`
	Secret          string     `gorm:"not null" json:"-"`
	LastStatus      *int16     `json:"last_status"`
	LastDeliveredAt *time.Time `json:"last_delivered_at"`
	FailureCount    int        `gorm:"not null;default:0" json:"failure_count"`
	Enabled         bool       `gorm:"not null;default:true" json:"enabled"`
	CreatedAt       time.Time  `gorm:"not null" json:"created_at"`
	UpdatedAt       time.Time  `gorm:"not null" json:"updated_at"`
}

// TableName 显式映射到 webhooks（gorm 默认复数化一致，显式声明防漂移）。
func (Webhook) TableName() string { return "webhooks" }

// EventList 为 PostgreSQL text[] 列的自定义映射（driver.Valuer 写入 /
// sql.Scanner 读出）。元素为事件类型白名单（不含引号/逗号/反斜杠），
// 引号转义仅防御性处理。
type EventList []string

// Value 编码为 PostgreSQL 数组字面量 {"a","b"}（驱动按列类型 text[] 解析）。
func (e EventList) Value() (driver.Value, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range e {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan 解析 PostgreSQL 数组字面量（text[] 经驱动以 []byte/string 返回）。
func (e *EventList) Scan(src any) error {
	if src == nil {
		*e = nil
		return nil
	}
	var text string
	switch v := src.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return fmt.Errorf("webhook: cannot scan %T into EventList", src)
	}
	text = strings.TrimSuffix(strings.TrimPrefix(text, "{"), "}")
	*e = EventList{}
	if text == "" {
		return nil
	}
	// 白名单值不含逗号/引号，简单分割即可；外层引号仅防御性去除。
	for _, part := range strings.Split(text, ",") {
		*e = append(*e, strings.Trim(part, `"`))
	}
	return nil
}
