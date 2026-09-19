// Package settings 提供运行时可调的系统设置（system_settings 表）：
// 内置键定义（类型/默认值/范围/描述）、类型校验、热读取接口与审计写入。
// 非密钥原则：密钥类配置（JWT/S3 凭据等）一律走环境变量，不入库、不暴露
// 于管理 API。例外：SMTP 投递参数（含密码，只写不读）允许经管理端入库
// 覆盖 env——邮件通道是管理员日常运维项，且 DB 值经专用端点读写、密码
// 永不回显（见 SMTPSettings 与 /admin/settings/smtp）。
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// effect 取值：设置变更的生效方式元数据（管理端展示用）。
const (
	// EffectImmediate 变更即时生效（消费方每次请求热读取 system_settings）。
	EffectImmediate = "immediate"
	// EffectNewSession 变更对新会话/新登录生效。
	EffectNewSession = "new_session"
	// EffectRestart 变更须重启服务进程生效（启动时装配读取的配置）。
	EffectRestart = "restart"
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
	KeyUploadMaxVersionsPerFile  = "upload.max_versions_per_file"
	KeyUploadMaxFileSize         = "upload.max_file_size"
	KeyUploadDefaultQuota        = "upload.default_quota"
	KeyShareDefaultExpiryHours   = "share.default_expiry_hours"
	KeyShareDefaultWatermark     = "share.default_watermark"
	KeyShareWatermarkText        = "share.watermark_text"
	KeyRetentionTrashDays        = "retention.trash_days"
	KeyRetentionAccessEventsDays = "retention.access_events_days"
	KeyRateLimitPerMinute        = "security.rate_limit_per_minute"
	KeyLoginMaxRetries           = "security.login_max_retries"
	KeyLoginLockMinutes          = "security.login_lock_minutes"
	KeyMaxConcurrentUploads      = "upload.max_concurrent_uploads_per_user"
	KeyBatchMaxItems             = "batch.max_items"
	KeyFolderMaxDepth            = "folder.max_depth"
	KeySharePublicEnabled        = "share.public_enabled"
	KeyScanQuarantinePolicy      = "security.scan_quarantine_policy"
	KeyBackupEnabled             = "backup.enabled"
	KeyBackupRetentionDays       = "backup.retention_days"
	KeyBackupEncryptionRequired  = "backup.encryption_required"
	KeyBackupLastVerify          = "backup.last_verify"
	KeyAuditRetentionDays        = "audit.retention_days"
	// KeyUploadVersionRetentionDays 版本保留时间窗（天）：与
	// upload.max_versions_per_file 组合——时间窗内的版本不因数量裁剪删除；
	// 0 = 不启用时间窗（仅按数量裁剪，默认，避免行为突变）。
	KeyUploadVersionRetentionDays = "upload.version_retention_days"
	// KeyUploadBlockedExtensions 上传扩展名黑名单（逗号分隔，如
	// "exe,bat,sh"）；默认空 = 不拦截。建会话与 Complete 双侧校验。
	KeyUploadBlockedExtensions = "upload.blocked_extensions"
)

// SMTP 运行时配置键（system_settings 存储，邮件发送处优先读库回退 env）。
// 刻意不进 Definitions：通用设置列表（GET /admin/settings）不展示、通用
// 写入口（PUT /admin/settings/:key）不可写，统一走专用 /admin/settings/smtp
// 端点（密码只写不读、审计打码）。
const (
	KeySMTPEnabled = "smtp.enabled"
	KeySMTPHost    = "smtp.host"
	KeySMTPPort    = "smtp.port"
	KeySMTPUser    = "smtp.user"
	KeySMTPPass    = "smtp.pass"
	KeySMTPFrom    = "smtp.from"
	KeySMTPTLSMode = "smtp.tls_mode"
)

// SMTP TLS 模式（KeySMTPTLSMode 取值）。
const (
	SMTPTLSModeAuto = "auto" // STARTTLS（服务器通告时自动协商，默认）
	SMTPTLSModeSSL  = "ssl"  // 隐式 TLS（SMTPS，465 端口常见）
	SMTPTLSModeNone = "none" // 不协商 TLS（仅内网中继场景）
)

// SMTPSettings 为 SMTP 投递参数全集（env 基线 / 入库覆盖 / 请求载荷共用）。
// Pass 仅在 PUT 请求中携带（空 = 保持现值不覆盖）；任何读路径都不回显。
type SMTPSettings struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	User    string `json:"user"`
	Pass    string `json:"pass,omitempty"`
	From    string `json:"from"`
	TLSMode string `json:"tls_mode"`
}

// SMTPOverride 为 system_settings 中已入库的 SMTP 覆盖项（nil 指针 = 该键
// 未入库，沿用 env 基线；Pass 仅在入库为非空字符串时覆盖）。
type SMTPOverride struct {
	Enabled *bool
	Host    *string
	Port    *int
	User    *string
	Pass    *string
	From    *string
	TLSMode *string
}

// Apply 把入库覆盖合并到 env 基线，返回生效配置。
func (o SMTPOverride) Apply(env SMTPSettings) SMTPSettings {
	out := env
	if o.Enabled != nil {
		out.Enabled = *o.Enabled
	}
	if o.Host != nil {
		out.Host = *o.Host
	}
	if o.Port != nil {
		out.Port = *o.Port
	}
	if o.User != nil {
		out.User = *o.User
	}
	if o.Pass != nil && *o.Pass != "" {
		out.Pass = *o.Pass
	}
	if o.From != nil {
		out.From = *o.From
	}
	if o.TLSMode != nil && *o.TLSMode != "" {
		out.TLSMode = *o.TLSMode
	}
	return out
}

// smtpKeyTypes 为各 SMTP 键的 value_type（写入 system_settings 行用）。
var smtpKeyTypes = map[string]string{
	KeySMTPEnabled: TypeBool,
	KeySMTPHost:    TypeString,
	KeySMTPPort:    TypeInt,
	KeySMTPUser:    TypeString,
	KeySMTPPass:    TypeString,
	KeySMTPFrom:    TypeString,
	KeySMTPTLSMode: TypeString,
}

// ValidateSMTP 校验生效配置的完整性（enabled 时 host/from 必填、port 与
// TLS 模式合法；Pass 不参与校验）。返回 ErrInvalidValue 语义错误。
func ValidateSMTP(s SMTPSettings) error {
	if !s.Enabled {
		return nil
	}
	if s.Host == "" {
		return fmt.Errorf("%w: smtp.host is required when smtp.enabled", ErrInvalidValue)
	}
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("%w: smtp.port must be 1-65535", ErrInvalidValue)
	}
	if s.From == "" {
		return fmt.Errorf("%w: smtp.from is required when smtp.enabled", ErrInvalidValue)
	}
	switch s.TLSMode {
	case "", SMTPTLSModeAuto, SMTPTLSModeSSL, SMTPTLSModeNone:
	default:
		return fmt.Errorf("%w: smtp.tls_mode must be auto/ssl/none", ErrInvalidValue)
	}
	return nil
}

// Definition 是一个内置键的元数据：类型、默认值、取值范围、描述与生效方式。
type Definition struct {
	Key         string
	Type        string
	Default     any
	Description string
	// Min/Max 仅对 int 类型生效（含边界）；nil 表示不限。
	Min *int64
	Max *int64
	// Effect 表示变更的生效方式（EffectImmediate/NewSession/Restart）：
	// immediate 为消费方每次请求热读取；restart 为启动装配读取（当前
	// security.* 限流与登录锁定参数均由 env 在启动时注入，须重启生效）。
	Effect string
}

func intPtr(v int64) *int64 { return &v }

// Definitions 是全部内置键定义；键顺序即 GetAll 输出顺序。
// int 类型键的默认值统一存 int64（与 normalizeValue 归一化结果一致）。
var Definitions = []Definition{
	{Key: KeyUploadMaxVersionsPerFile, Type: TypeInt, Default: int64(5), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "每文件保留的版本数上限（覆盖上传后按版本号裁剪历史版本；与 upload.version_retention_days 组合生效）"},
	{Key: KeyUploadVersionRetentionDays, Type: TypeInt, Default: int64(0), Min: intPtr(0), Max: intPtr(3650), Effect: EffectImmediate, Description: "版本保留时间窗（天）：创建时间在窗口内的版本不因数量裁剪删除；0 = 不启用时间窗（默认，仅按数量上限裁剪）"},
	{Key: KeyUploadBlockedExtensions, Type: TypeString, Default: "", Effect: EffectImmediate, Description: "上传扩展名黑名单（逗号分隔，如 exe,bat,sh；不含点、大小写不敏感）：命中的文件名在建会话与完成时拒绝（400）；默认空 = 不拦截"},
	{Key: KeyUploadMaxFileSize, Type: TypeInt, Default: int64(1 << 30), Min: intPtr(1), Max: intPtr(1 << 40), Effect: EffectImmediate, Description: "单文件上传大小上限（字节）"},
	{Key: KeyUploadDefaultQuota, Type: TypeInt, Default: int64(10 << 30), Min: intPtr(1), Max: intPtr(1 << 50), Effect: EffectImmediate, Description: "新用户开户默认存储配额（字节，默认 10GiB）；仅对新创建用户生效，存量用户经管理端单独调整"},
	{Key: KeyShareDefaultExpiryHours, Type: TypeInt, Default: int64(168), Min: intPtr(1), Max: intPtr(8760), Effect: EffectImmediate, Description: "公开分享默认有效期（小时）"},
	{Key: KeyShareDefaultWatermark, Type: TypeBool, Default: true, Effect: EffectImmediate, Description: "新分享默认启用水印（创建请求未显式指定 watermark_enabled 时采用）"},
	{Key: KeyShareWatermarkText, Type: TypeString, Default: "{date} {name}", Effect: EffectImmediate, Description: "水印默认模板，支持 {email}/{date}/{name} 占位符（公开访问无登录身份，{email} 渲染为脱敏 IP 前缀）"},
	{Key: KeyRetentionTrashDays, Type: TypeInt, Default: int64(30), Min: intPtr(1), Max: intPtr(3650), Effect: EffectImmediate, Description: "回收站保留天数：软删除超过该天数后由后台清理任务彻底删除"},
	{Key: KeyRetentionAccessEventsDays, Type: TypeInt, Default: int64(90), Min: intPtr(1), Max: intPtr(3650), Effect: EffectImmediate, Description: "文件访问事件保留天数：超过该天数后由后台清理任务删除"},
	{Key: KeyRateLimitPerMinute, Type: TypeInt, Default: int64(120), Min: intPtr(0), Max: intPtr(100000), Effect: EffectRestart, Description: "认证 API 每分钟请求上限（须重启生效：限流器在启动时按 env 装配，本键当前无热读取消费方）"},
	{Key: KeyLoginMaxRetries, Type: TypeInt, Default: int64(5), Min: intPtr(1), Max: intPtr(100), Effect: EffectRestart, Description: "连续登录失败锁定阈值（须重启生效：由 env LOGIN_MAX_RETRIES 启动时注入）"},
	{Key: KeyLoginLockMinutes, Type: TypeInt, Default: int64(15), Min: intPtr(1), Max: intPtr(10080), Effect: EffectRestart, Description: "登录失败锁定时长（分钟；须重启生效：由 env LOGIN_LOCK_MINUTES 启动时注入）"},
	{Key: KeyMaxConcurrentUploads, Type: TypeInt, Default: int64(3), Min: intPtr(1), Max: intPtr(100), Effect: EffectImmediate, Description: "每用户并发上传会话上限：非终态会话（uploading/verifying/scanning）达到上限时新建会话返回 429"},
	{Key: KeyBatchMaxItems, Type: TypeInt, Default: int64(100), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "批量操作单次最大项目数"},
	{Key: KeyFolderMaxDepth, Type: TypeInt, Default: int64(32), Min: intPtr(1), Max: intPtr(1000), Effect: EffectImmediate, Description: "目录最大深度（根为 1）：创建子目录与目录移动超过上限拒绝"},
	{Key: KeySharePublicEnabled, Type: TypeBool, Default: true, Effect: EffectImmediate, Description: "是否允许创建公开分享"},
	{Key: KeyScanQuarantinePolicy, Type: TypeString, Default: "quarantine", Effect: EffectImmediate, Description: "扫描失败处理策略：quarantine 或 reject（当前版本未接线：扫描失败一律隔离，隔离区可经管理端处置）"},
	{Key: KeyBackupEnabled, Type: TypeBool, Default: false, Effect: EffectRestart, Description: "是否启用备份任务"},
	{Key: KeyBackupRetentionDays, Type: TypeInt, Default: int64(30), Min: intPtr(1), Max: intPtr(3650), Effect: EffectRestart, Description: "备份保留天数"},
	{Key: KeyBackupEncryptionRequired, Type: TypeBool, Default: true, Effect: EffectRestart, Description: "是否要求备份加密"},
	{Key: KeyBackupLastVerify, Type: TypeString, Default: "", Effect: EffectRestart, Description: "最近一次备份验证时间"},
	{Key: KeyAuditRetentionDays, Type: TypeInt, Default: int64(90), Min: intPtr(1), Max: intPtr(3650), Effect: EffectImmediate, Description: "审计日志保留天数"},
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

// SettingView 是管理 API 的响应视图：解析后的值 + 类型、描述与生效方式。
type SettingView struct {
	Key         string     `json:"key"`
	Value       any        `json:"value"`
	Type        string     `json:"type"`
	Description string     `json:"description"`
	Default     any        `json:"default"`
	Effect      string     `json:"effect"`
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
	err := g.db.First(&s, "key = ?", key).Error
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
		view := SettingView{Key: d.Key, Type: d.Type, Description: d.Description, Default: d.Default, Value: d.Default, Effect: d.Effect}
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

// GetBool 返回 bool 类型键的当前值（热读取）；未知键或值非法返回错误，
// 由调用方决定回退。
func (s *Store) GetBool(key string) (bool, error) {
	value, err := s.Get(key)
	if err != nil {
		return false, err
	}
	b, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("settings key %s is not bool", key)
	}
	return b, nil
}

// GetString 返回 string 类型键的当前值（热读取）；未知键或值非法返回错误，
// 由调用方决定回退。
func (s *Store) GetString(key string) (string, error) {
	value, err := s.Get(key)
	if err != nil {
		return "", err
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("settings key %s is not string", key)
	}
	return str, nil
}

// SMTPOverrides 读取全部 smtp.* 入库覆盖项（未入库的键保持 nil 指针，
// 与入库值恰为零值区分）。DB 异常返回错误，调用方回退 env 基线投递。
func (s *Store) SMTPOverrides() (SMTPOverride, error) {
	var out SMTPOverride
	// read：行存在且 JSON 可解析时置指针；行不存在（ErrNotSet）保持 nil。
	readBool := func(key string, dst **bool) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var v bool
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return err
		}
		*dst = &v
		return nil
	}
	readInt := func(key string, dst **int) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var v int64
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return err
		}
		n := int(v)
		*dst = &n
		return nil
	}
	readString := func(key string, dst **string) error {
		row, err := s.repo.GetRow(key)
		if errors.Is(err, ErrNotSet) {
			return nil
		}
		if err != nil {
			return err
		}
		var v string
		if err := json.Unmarshal([]byte(row.ValueJSON), &v); err != nil {
			return err
		}
		*dst = &v
		return nil
	}
	if err := readBool(KeySMTPEnabled, &out.Enabled); err != nil {
		return out, err
	}
	if err := readString(KeySMTPHost, &out.Host); err != nil {
		return out, err
	}
	if err := readInt(KeySMTPPort, &out.Port); err != nil {
		return out, err
	}
	if err := readString(KeySMTPUser, &out.User); err != nil {
		return out, err
	}
	if err := readString(KeySMTPPass, &out.Pass); err != nil {
		return out, err
	}
	if err := readString(KeySMTPFrom, &out.From); err != nil {
		return out, err
	}
	if err := readString(KeySMTPTLSMode, &out.TLSMode); err != nil {
		return out, err
	}
	return out, nil
}

// SetSMTP 写入 SMTP 配置（与 env 基线合并校验：Pass 留空 = 保持现值），
// 成功后写 settings.update 审计（Pass 以 "******" 打码，绝不记录明文）。
func (s *Store) SetSMTP(in SMTPSettings, env SMTPSettings, actor uuid.UUID) (SMTPSettings, error) {
	// 与现值合并出完整生效配置再校验（避免「只改 host、库里无 port」被误拒）。
	current := env
	if ov, err := s.SMTPOverrides(); err == nil {
		current = ov.Apply(env)
	}
	merged := SMTPSettings{
		Enabled: in.Enabled,
		Host:    strings.TrimSpace(in.Host),
		Port:    in.Port,
		User:    strings.TrimSpace(in.User),
		Pass:    in.Pass,
		From:    strings.TrimSpace(in.From),
		TLSMode: in.TLSMode,
	}
	if merged.Host == "" {
		merged.Host = current.Host
	}
	if merged.Port == 0 {
		merged.Port = current.Port
	}
	if merged.User == "" {
		merged.User = current.User
	}
	if merged.From == "" {
		merged.From = current.From
	}
	if merged.TLSMode == "" {
		merged.TLSMode = current.TLSMode
	}
	if err := ValidateSMTP(merged); err != nil {
		return SMTPSettings{}, err
	}
	now := s.now().UTC()
	writes := []struct {
		key  string
		val  any
		skip bool
	}{
		{KeySMTPEnabled, merged.Enabled, false},
		{KeySMTPHost, merged.Host, false},
		{KeySMTPPort, int64(merged.Port), false},
		{KeySMTPUser, merged.User, false},
		// 密码只写不读：请求为空 = 保持现值（不覆盖为空串）。
		{KeySMTPPass, merged.Pass, merged.Pass == ""},
		{KeySMTPFrom, merged.From, false},
		{KeySMTPTLSMode, merged.TLSMode, false},
	}
	for _, w := range writes {
		if w.skip {
			continue
		}
		raw, err := json.Marshal(w.val)
		if err != nil {
			return SMTPSettings{}, err
		}
		row := Setting{Key: w.key, ValueJSON: string(raw), ValueType: smtpKeyTypes[w.key], Description: "SMTP runtime override", UpdatedBy: &actor, UpdatedAt: now}
		if err := s.repo.UpsertRow(row); err != nil {
			return SMTPSettings{}, err
		}
	}
	audited := SMTPSettings{
		Enabled: merged.Enabled, Host: merged.Host, Port: merged.Port,
		User: merged.User, Pass: "", From: merged.From, TLSMode: merged.TLSMode,
	}
	if merged.User != "" {
		audited.Pass = "******"
	}
	metadata, _ := json.Marshal(map[string]any{"key": "smtp", "value": audited})
	_ = s.audit.Record(audit.Entry{UserID: &actor, Action: audit.ActionSettingsUpdate, ResourceType: audit.ResourceSettings, ResourceID: "smtp", Status: audit.StatusSuccess, Metadata: string(metadata)})
	return merged, nil
}
