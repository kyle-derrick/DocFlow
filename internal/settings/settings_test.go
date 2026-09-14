package settings

import (
	"errors"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/google/uuid"
)

// memRepo 是 repository 的内存实现（模式参照 files 包测试）。
type memRepo struct {
	rows    map[string]Setting
	getErr  error
	listErr error
}

func newMemRepo() *memRepo { return &memRepo{rows: make(map[string]Setting)} }

func (m *memRepo) GetRow(key string) (Setting, error) {
	if m.getErr != nil {
		return Setting{}, m.getErr
	}
	s, ok := m.rows[key]
	if !ok {
		return Setting{}, ErrNotSet
	}
	return s, nil
}

func (m *memRepo) ListRows() ([]Setting, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []Setting
	for _, s := range m.rows {
		out = append(out, s)
	}
	return out, nil
}

func (m *memRepo) UpsertRow(s Setting) error {
	s.UpdatedAt = time.Now().UTC()
	m.rows[s.Key] = s
	return nil
}

type memAudit struct{ entries []audit.Entry }

func (m *memAudit) Record(e audit.Entry) error {
	m.entries = append(m.entries, e)
	return nil
}

func newTestStore(repo repository, recorder audit.Recorder) *Store {
	return &Store{repo: repo, audit: recorder, now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
}

// 未设置读取默认值；Set 后读取生效。
func TestStoreGetReturnsDefaultThenSetValue(t *testing.T) {
	s := newTestStore(newMemRepo(), audit.NopRecorder{})
	v, err := s.Get(KeyUploadMaxVersionsPerFile)
	if err != nil {
		t.Fatal(err)
	}
	if v.(int64) != 5 {
		t.Fatalf("default = %v, want 5", v)
	}
	if _, err := s.Set(KeyUploadMaxVersionsPerFile, 9, uuid.New()); err != nil {
		t.Fatal(err)
	}
	v, err = s.Get(KeyUploadMaxVersionsPerFile)
	if err != nil {
		t.Fatal(err)
	}
	if v.(int64) != 9 {
		t.Fatalf("value after set = %v, want 9", v)
	}
}

// 未知键拒绝（读与写均 ErrUnknownKey）。
func TestStoreUnknownKey(t *testing.T) {
	s := newTestStore(newMemRepo(), audit.NopRecorder{})
	if _, err := s.Get("jwt.secret"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("get unknown key err = %v, want ErrUnknownKey", err)
	}
	if _, err := s.Set("s3.access_key", "x", uuid.New()); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("set unknown key err = %v, want ErrUnknownKey", err)
	}
}

// 类型校验：bool/int/string 各自的合法与非法值。
func TestStoreSetTypeValidation(t *testing.T) {
	s := newTestStore(newMemRepo(), audit.NopRecorder{})
	actor := uuid.New()

	// int 键：JSON 数字（float64）、int、int64 均接受。
	for _, v := range []any{float64(7), 7, int64(7)} {
		got, err := s.Set(KeyUploadMaxVersionsPerFile, v, actor)
		if err != nil {
			t.Fatalf("set %T(%v): %v", v, v, err)
		}
		if got.(int64) != 7 {
			t.Fatalf("normalized = %v, want 7", got)
		}
	}
	// int 键：布尔/字符串/非整数拒绝。
	for _, v := range []any{true, "7", 7.5} {
		if _, err := s.Set(KeyUploadMaxVersionsPerFile, v, actor); err == nil {
			t.Fatalf("set %T(%v) must fail", v, v)
		}
	}
	// 范围校验：min=1 / max=1000。
	if _, err := s.Set(KeyUploadMaxVersionsPerFile, 0, actor); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("below min err = %v, want ErrInvalidValue", err)
	}
	if _, err := s.Set(KeyUploadMaxVersionsPerFile, 1001, actor); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("above max err = %v, want ErrInvalidValue", err)
	}
	// string 键校验（site.name 已移除，string 归一化经 normalizeValue 直测）：
	// 接受字符串、拒绝数字。
	strDef := Definition{Key: "test.string", Type: TypeString}
	if _, err := normalizeValue(strDef, "DocFlow 站点"); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeValue(strDef, 42); !errors.Is(err, ErrInvalidType) {
		t.Fatalf("string key with int err = %v, want ErrInvalidType", err)
	}
}

// Set 写 settings.update 审计（actor、键与归一化值入 metadata）。
func TestStoreSetWritesAudit(t *testing.T) {
	repo := newMemRepo()
	rec := &memAudit{}
	s := newTestStore(repo, rec)
	actor := uuid.New()
	if _, err := s.Set(KeyRetentionTrashDays, 14, actor); err != nil {
		t.Fatal(err)
	}
	if len(rec.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(rec.entries))
	}
	e := rec.entries[0]
	if e.Action != "settings.update" || e.ResourceID != KeyRetentionTrashDays || e.UserID == nil || *e.UserID != actor {
		t.Fatalf("audit entry = %+v", e)
	}
	if e.Metadata != `{"key":"retention.trash_days","value":14}` {
		t.Fatalf("metadata = %s", e.Metadata)
	}
}

// GetAll 合并默认值与已存值；内置键全部出现。
func TestStoreGetAllMergesDefaults(t *testing.T) {
	repo := newMemRepo()
	s := newTestStore(repo, audit.NopRecorder{})
	actor := uuid.New()
	if _, err := s.Set(KeyShareDefaultExpiryHours, 48, actor); err != nil {
		t.Fatal(err)
	}
	views, err := s.GetAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != len(Definitions) {
		t.Fatalf("views = %d, want %d", len(views), len(Definitions))
	}
	byKey := make(map[string]SettingView, len(views))
	for _, v := range views {
		byKey[v.Key] = v
	}
	if v := byKey[KeyShareDefaultExpiryHours]; v.Value.(int64) != 48 || v.UpdatedBy == nil || *v.UpdatedBy != actor {
		t.Fatalf("share.default_expiry_hours view = %+v", v)
	}
	if v := byKey[KeyRetentionTrashDays]; v.Value.(int64) != 30 {
		t.Fatalf("retention default view = %+v", v)
	}
}

// 热读取回退：存储行损坏或读取故障时 Get 回退默认值；GetInt 未知键报错。
func TestStoreHotReadFallbacks(t *testing.T) {
	repo := newMemRepo()
	repo.rows[KeyUploadMaxVersionsPerFile] = Setting{Key: KeyUploadMaxVersionsPerFile, ValueJSON: "{not json", ValueType: TypeInt}
	s := newTestStore(repo, audit.NopRecorder{})
	v, err := s.Get(KeyUploadMaxVersionsPerFile)
	if err != nil {
		t.Fatal(err)
	}
	if v.(int64) != 5 {
		t.Fatalf("corrupt row fallback = %v, want default 5", v)
	}
	if _, err := s.GetInt("no.such.key"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("GetInt unknown key err = %v, want ErrUnknownKey", err)
	}
	n, err := s.GetInt(KeyRetentionTrashDays)
	if err != nil || n != 30 {
		t.Fatalf("GetInt default = %d, %v; want 30, nil", n, err)
	}
}

// TestDefinitionsEffectMetadata effect 元数据矩阵：每个内置键 effect 必须为
// 合法枚举值；GetAll 视图透传 effect；关键键按真实行为标注
// （热读取键 immediate、启动装配键 restart、本批次接线的门控键 immediate）。
func TestDefinitionsEffectMetadata(t *testing.T) {
	valid := map[string]bool{EffectImmediate: true, EffectNewSession: true, EffectRestart: true}
	for _, d := range Definitions {
		if !valid[d.Effect] {
			t.Errorf("key %s: effect %q not in enum", d.Key, d.Effect)
		}
	}
	// 关键键的 effect 按实现标注抽查。
	want := map[string]string{
		KeyUploadMaxVersionsPerFile:   EffectImmediate,
		KeyUploadVersionRetentionDays: EffectImmediate,
		KeyUploadBlockedExtensions:    EffectImmediate,
		KeyMaxConcurrentUploads:       EffectImmediate,
		KeyBatchMaxItems:              EffectImmediate,
		KeyFolderMaxDepth:             EffectImmediate,
		KeyAuditRetentionDays:         EffectImmediate,
		KeyRateLimitPerMinute:         EffectRestart,
		KeyLoginMaxRetries:            EffectRestart,
		KeyLoginLockMinutes:           EffectRestart,
		KeyBackupEnabled:              EffectRestart,
		KeyBackupRetentionDays:        EffectRestart,
	}
	for key, effect := range want {
		d, err := DefinitionByKey(key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if d.Effect != effect {
			t.Errorf("key %s: effect = %s, want %s", key, d.Effect, effect)
		}
	}
	// GetAll 视图透传 effect；新键默认值语义（0 天窗口/空黑名单/深度 32）。
	views, err := newTestStore(newMemRepo(), audit.NopRecorder{}).GetAll()
	if err != nil {
		t.Fatal(err)
	}
	byKey := make(map[string]SettingView, len(views))
	for _, v := range views {
		byKey[v.Key] = v
	}
	for key, effect := range want {
		if byKey[key].Effect != effect {
			t.Errorf("view %s: effect = %s, want %s", key, byKey[key].Effect, effect)
		}
	}
	if v := byKey[KeyUploadVersionRetentionDays]; v.Value.(int64) != 0 {
		t.Fatalf("retention default = %v, want 0（默认不启用时间窗）", v.Value)
	}
	if v := byKey[KeyUploadBlockedExtensions]; v.Value.(string) != "" {
		t.Fatalf("blocked extensions default = %q, want empty", v.Value)
	}
	if v := byKey[KeyFolderMaxDepth]; v.Value.(int64) != 32 {
		t.Fatalf("folder depth default = %v, want 32", v.Value)
	}
}
