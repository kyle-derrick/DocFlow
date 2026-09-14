// Package settings 提供运行时可调的系统设置（system_settings 表）：
// 内置键定义（类型/默认值/范围/描述）、类型校验、热读取接口与审计写入。
// 非密钥原则：密钥类配置（JWT/S3/SMTP 凭据等）一律走环境变量，
// 不入库、不暴露于管理 API。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// value_type 取值，与 system_settings.value_type CHECK 约束一致。
const (
	TypeBool   = "bool"
	TypeInt    = "int"
	TypeString = "string"
)

var (
	// ErrUnknownKey 表示键不在内置定义中（不可设置/读取）。
	ErrUnknownKey = errors.New("unknown settings key")
	// ErrInvalidType 表示值类型与键定义不符（如 bool 键传入字符串）。
	ErrInvalidType = errors.New("invalid settings value type")
	// ErrInvalidValue 表示值类型正确但超出允许范围（如 int 下限/上限）。
	ErrInvalidValue = errors.New("invalid settings value")
	// ErrNotSet 表示键在表中没有行（读取时回退默认值）。
	ErrNotSet = errors.New("settings key not set")
)

// 内置键（唯一可设置的键集合；新增键须在此登记定义）。
// site.name 已移除：无任何消费方（前端标题等为静态配置），保留即死设置。
const (
	KeyUploadMaxVersionsPerFile = "upload.max_versions_per_file"
	KeyUploadMaxFileSize        = "upload.max_file_size"
	KeyShareDefaultExpiryHours  = "share.default_expiry_hours"
	KeyRetentionTrashDays       = "retention.trash_days"
)

// Definition 是一个内置键的元数据：类型、默认值、取值范围与描述。
type Definition struct {
	Key         string
	Type        string
	Default     any
	Description string
	// Min/Max 仅对 int 类型生效（含边界）；nil 表示不限。
	Min *int64
	Max *int64
}

func intPtr(v int64) *int64 { return &v }

// Definitions 是全部内置键定义；键顺序即 GetAll 输出顺序。
// int 类型键的默认值统一存 int64（与 normalizeValue 归一化结果一致）。
var Definitions = []Definition{
	{Key: KeyUploadMaxVersionsPerFile, Type: TypeInt, Default: int64(5), Min: intPtr(1), Max: intPtr(1000), Description: "每文件保留的版本数上限（覆盖上传后按版本号裁剪历史版本）"},
	{Key: KeyUploadMaxFileSize, Type: TypeInt, Default: int64(1 << 30), Min: intPtr(1), Max: intPtr(1 << 40), Description: "单文件上传大小上限（字节）"},
	{Key: KeyShareDefaultExpiryHours, Type: TypeInt, Default: int64(168), Min: intPtr(1), Max: intPtr(8760), Description: "公开分享默认有效期（小时）"},
	{Key: KeyRetentionTrashDays, Type: TypeInt, Default: int64(30), Min: intPtr(1), Max: intPtr(3650), Description: "回收站保留天数：软删除超过该天数后由后台清理任务彻底删除"},
}

// DefinitionByKey 返回键定义；未知键返回 ErrUnknownKey。
func DefinitionByKey(key string) (Definition, error) {
	for _, d := range Definitions {
		if d.Key == key {
			return d, nil
		}
	}
	return Definition{}, ErrUnknownKey
}

// normalizeValue 按定义校验并归一化请求值（JSON 反序列化后 bool→bool、
// 数字→float64、字符串→string），返回按定义类型的强类型值。
func normalizeValue(d Definition, value any) (any, error) {
	switch d.Type {
	case TypeBool:
		b, ok := value.(bool)
		if !ok {
			return nil, ErrInvalidType
		}
		return b, nil
	case TypeInt:
		var n int64
		switch v := value.(type) {
		case float64: // encoding/json 的数字类型
			if v != float64(int64(v)) {
				return nil, ErrInvalidValue
			}
			n = int64(v)
		case int:
			n = int64(v)
		case int64:
			n = v
		default:
			return nil, ErrInvalidType
		}
		if (d.Min != nil && n < *d.Min) || (d.Max != nil && n > *d.Max) {
			return nil, ErrInvalidValue
		}
		return n, nil
	case TypeString:
		s, ok := value.(string)
		if !ok {
			return nil, ErrInvalidType
		}
		return s, nil
	default:
		return nil, ErrInvalidType
	}
}

// Setting 对应 system_settings 表一行。
type Setting struct {
	Key         string     `gorm:"column:key;primaryKey"`
	ValueJSON   string     `gorm:"column:value_json;type:jsonb;not null"`
	ValueType   string     `gorm:"column:value_type;size:16;not null"`
	Description string     `gorm:"column:description;not null"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null"`
	UpdatedBy   *uuid.UUID `gorm:"column:updated_by;type:uuid"`
}

// TableName 显式映射到 system_settings（gorm 默认复数化为 settings）。
func (Setting) TableName() string { return "system_settings" }

// SettingView 是管理 API 的响应视图：解析后的值 + 类型与描述。
type SettingView struct {
	Key         string     `json:"key"`
	Value       any        `json:"value"`
	Type        string     `json:"type"`
	Description string     `json:"description"`
	Default     any        `json:"default"`
	UpdatedAt   time.Time  `json:"updated_at,omitempty"`
	UpdatedBy   *uuid.UUID `json:"updated_by,omitempty"`
}

// repository 抽象 system_settings 的数据访问；生产实现为 gormRepository，
// 测试可用内存实现（模式同 files 包的 repo 抽象）。
type repository interface {
	// GetRow 返回键行；无行返回 ErrNotSet。
	GetRow(key string) (Setting, error)
	// ListRows 返回全部已存储行。
	ListRows() ([]Setting, error)
	// UpsertRow 按主键 upsert。
	UpsertRow(s Setting) error
}

type gormRepository struct{ db *gorm.DB }

func (g *gormRepository) GetRow(key string) (Setting, error) {
	var s Setting
	err := g.db.First(&s, "`key` = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Setting{}, ErrNotSet
	}
	return s, err
}

func (g *gormRepository) ListRows() ([]Setting, error) {
	var out []Setting
	err := g.db.Order("key").Find(&out).Error
	return out, err
}

func (g *gormRepository) UpsertRow(s Setting) error {
	return g.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value_json", "value_type", "description", "updated_by", "updated_at"}),
	}).Create(&s).Error
}

// Store 提供设置的读取与写入；每次 Get 直读数据库（热读取，变更即时生效）。
type Store struct {
	repo  repository
	audit audit.Recorder
	now   func() time.Time
}

func NewStore(db *gorm.DB) *Store {
	return &Store{repo: &gormRepository{db: db}, audit: audit.NopRecorder{}, now: time.Now}
}

// SetAuditRecorder 注入审计写入器；nil 保持 Nop（模式同 http.Handler）。
func (s *Store) SetAuditRecorder(recorder audit.Recorder) {
	if recorder != nil {
		s.audit = recorder
	}
}

// decode 按定义类型把 value_json 解析为强类型值。
func decode(d Definition, raw string) (any, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, err
	}
	return normalizeValue(d, value)
}

// Get 返回键的当前值；未设置或解析失败时回退默认值（热读取的容错默认）。
func (s *Store) Get(key string) (any, error) {
	d, err := DefinitionByKey(key)
	if err != nil {
		return nil, err
	}
	row, err := s.repo.GetRow(key)
	if err != nil {
		if errors.Is(err, ErrNotSet) {
			return d.Default, nil
		}
		return nil, err
	}
	value, err := decode(d, row.ValueJSON)
	if err != nil {
		return d.Default, nil
	}
	return value, nil
}

// GetAll 返回全部内置键的视图：已设置的取存储值，未设置的取默认值。
func (s *Store) GetAll() ([]SettingView, error) {
	rows, err := s.repo.ListRows()
	if err != nil {
		return nil, err
	}
	stored := make(map[string]Setting, len(rows))
	for _, r := range rows {
		stored[r.Key] = r
	}
	out := make([]SettingView, 0, len(Definitions))
	for _, d := range Definitions {
		view := SettingView{Key: d.Key, Type: d.Type, Description: d.Description, Default: d.Default, Value: d.Default}
		if row, ok := stored[d.Key]; ok {
			if value, err := decode(d, row.ValueJSON); err == nil {
				view.Value = value
			}
			view.UpdatedAt = row.UpdatedAt
			view.UpdatedBy = row.UpdatedBy
		}
		out = append(out, view)
	}
	return out, nil
}

// Set 校验并写入键值（actor 记入 updated_by），成功后写 settings.update 审计。
// 未知键返回 ErrUnknownKey；类型/范围不符返回 ErrInvalidType/ErrInvalidValue。
// 返回归一化后的值（供响应回显）。
func (s *Store) Set(key string, value any, actor uuid.UUID) (any, error) {
	d, err := DefinitionByKey(key)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeValue(d, value)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	if err := s.repo.UpsertRow(Setting{Key: key, ValueJSON: string(raw), ValueType: d.Type, Description: d.Description, UpdatedBy: &actor, UpdatedAt: s.now().UTC()}); err != nil {
		return nil, err
	}
	metadata, _ := json.Marshal(map[string]any{"key": key, "value": normalized})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: key, Status: audit.StatusSuccess, Metadata: string(metadata)})
	return normalized, nil
}

// GetInt 返回 int 类型键的当前值（热读取）；未知键或值非法返回错误，
// 由调用方决定回退（如回退 config 默认值）。
func (s *Store) GetInt(key string) (int, error) {
	value, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	n, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("settings key %s is not int", key)
	}
	return int(n), nil
}
