package settings

import (
	"errors"
	"fmt"
	"strings"
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

// TestSetAIPersonasViaSetAI 平台人设并入 PUT /admin/settings/ai 的保存链
// （载荷未带 = 保持现值；非 nil = trim 规整 + 校验 + 整块写入，空数组清空）。
func TestSetAIPersonasViaSetAI(t *testing.T) {
	store := newTestStore(newMemRepo(), &memAudit{})
	actor := uuid.New()
	// 预置：独立端点写入两条（SetAIPersonas trim 规整 id/name）。
	seed := []AIPersonaDef{{ID: " p1 ", Name: " 翻译 ", SystemPrompt: "你是翻译助手"}}
	if _, err := store.SetAIPersonas(seed, actor); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 载荷未带（nil）→ merged 回填现值，键值不变。
	merged, err := store.SetAI(AIConfig{}, AIConfig{}, actor)
	if err != nil {
		t.Fatalf("SetAI without personas: %v", err)
	}
	if len(merged.Personas) != 1 || merged.Personas[0].ID != "p1" || merged.Personas[0].Name != "翻译" {
		t.Fatalf("未带 personas 应保持现值: %+v", merged.Personas)
	}
	if list, _ := store.AIPersonas(); len(list) != 1 || list[0].ID != "p1" {
		t.Fatalf("键值不应被覆盖: %+v", list)
	}

	// 带载荷 → trim 规整写入并回显。
	in := AIConfig{Personas: []AIPersonaDef{{ID: " fin ", Name: " 财务 ", SystemPrompt: "你是财务助手"}}}
	merged, err = store.SetAI(in, AIConfig{}, actor)
	if err != nil {
		t.Fatalf("SetAI with personas: %v", err)
	}
	if len(merged.Personas) != 1 || merged.Personas[0].ID != "fin" || merged.Personas[0].Name != "财务" {
		t.Fatalf("应回显规整后的 personas: %+v", merged.Personas)
	}
	if list, _ := store.AIPersonas(); len(list) != 1 || list[0].ID != "fin" {
		t.Fatalf("应写入 KeyAIPersonas: %+v", list)
	}

	// 非法载荷（name 空 / system_prompt 超长 / id 重复）→ ErrInvalidValue，
	// 且不落库（现值保留）。
	bad := []AIPersonaDef{{ID: "x1", Name: "", SystemPrompt: "p"}}
	if _, err := store.SetAI(AIConfig{Personas: bad}, AIConfig{}, actor); err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("空 name 应报 ErrInvalidValue: %v", err)
	}
	dup := []AIPersonaDef{{ID: "x1", Name: "A", SystemPrompt: "p"}, {ID: "x1", Name: "B", SystemPrompt: "p"}}
	if _, err := store.SetAI(AIConfig{Personas: dup}, AIConfig{}, actor); err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("重复 id 应报 ErrInvalidValue: %v", err)
	}
	long := strings.Repeat("字", AIPlatformMaxPromptRunes+1)
	if _, err := store.SetAI(AIConfig{Personas: []AIPersonaDef{{ID: "x1", Name: "A", SystemPrompt: long}}}, AIConfig{}, actor); err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("超长 system_prompt 应报 ErrInvalidValue: %v", err)
	}
	if list, _ := store.AIPersonas(); len(list) != 1 || list[0].ID != "fin" {
		t.Fatalf("非法载荷不应落库: %+v", list)
	}

	// 空数组 = 清空。
	if _, err := store.SetAI(AIConfig{Personas: []AIPersonaDef{}}, AIConfig{}, actor); err != nil {
		t.Fatalf("清空 personas: %v", err)
	}
	if list, _ := store.AIPersonas(); len(list) != 0 {
		t.Fatalf("空数组应清空: %+v", list)
	}
}

// TestAIOCROverridesRead ai.ocr 整体块读取：未入库零值+默认钳制、入库
// 整体读替、超限钳制、损坏 JSON 报错（调用方回退 env 基线）。
func TestAIOCROverridesRead(t *testing.T) {
	repo := newMemRepo()
	store := newTestStore(repo, &memAudit{})
	// 未入库：disabled + max_image_bytes 回默认。
	ov, _, err := store.AIOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if ov.OCR.Enabled || ov.OCR.ProviderID != "" || ov.OCR.MaxImageBytes != AIOCRMaxBytesDefault {
		t.Fatalf("默认 OCR = %+v, want 零值 + 默认上限", ov.OCR)
	}
	// 入库整体读替 + 超限钳制到 AIOCRMaxBytesLimit。
	repo.rows[KeyAIOCR] = Setting{Key: KeyAIOCR, ValueJSON: `{"enabled":true,"provider_id":"p1","model_id":"m1","max_image_bytes":999999999}`, ValueType: TypeString}
	ov, _, err = store.AIOverrides()
	if err != nil {
		t.Fatal(err)
	}
	want := AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "m1", MaxImageBytes: AIOCRMaxBytesLimit}
	if ov.OCR != want {
		t.Fatalf("OCR = %+v, want %+v（超限钳制）", ov.OCR, want)
	}
	// max_image_bytes<=0 → 回默认。
	repo.rows[KeyAIOCR] = Setting{Key: KeyAIOCR, ValueJSON: `{"enabled":true,"provider_id":"p1","model_id":"m1","max_image_bytes":0}`, ValueType: TypeString}
	if ov, _, _ = store.AIOverrides(); ov.OCR.MaxImageBytes != AIOCRMaxBytesDefault {
		t.Fatalf("<=0 应回默认: %+v", ov.OCR)
	}
	// 损坏 JSON → err。
	repo.rows[KeyAIOCR] = Setting{Key: KeyAIOCR, ValueJSON: `{bad`, ValueType: TypeString}
	if _, _, err := store.AIOverrides(); err == nil {
		t.Fatal("损坏 JSON 应报错")
	}
}

// TestSetAIOCRBlock SetAI 的 ocr 块合并：带载荷 trim+钳制写入并回读；
// 载荷未带（零值块）= 保持现值（同 search 块语义）；<=0 规整默认。
func TestSetAIOCRBlock(t *testing.T) {
	store := newTestStore(newMemRepo(), &memAudit{})
	actor := uuid.New()
	// 带载荷：trim id/model、超限钳制。
	merged, err := store.SetAI(AIConfig{OCR: AIOCRConfig{Enabled: true, ProviderID: " p1 ", ModelID: " m1 ", MaxImageBytes: 1 << 30}}, AIConfig{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	want := AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "m1", MaxImageBytes: AIOCRMaxBytesLimit}
	if merged.OCR != want {
		t.Fatalf("merged.OCR = %+v, want %+v", merged.OCR, want)
	}
	if ov, _, err := store.AIOverrides(); err != nil || ov.OCR != want {
		t.Fatalf("应写入并回读 KeyAIOCR: (%+v, %v)", ov.OCR, err)
	}
	// 载荷未带（零值块）→ 保持现值。
	merged, err = store.SetAI(AIConfig{}, AIConfig{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if merged.OCR != want {
		t.Fatalf("未带 ocr 应保持现值: %+v", merged.OCR)
	}
	// 显式禁用载荷（非零块）生效；max_image_bytes 透传。
	merged, err = store.SetAI(AIConfig{OCR: AIOCRConfig{Enabled: false, MaxImageBytes: 4096}}, AIConfig{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if merged.OCR.Enabled || merged.OCR.MaxImageBytes != 4096 {
		t.Fatalf("显式禁用载荷应生效: %+v", merged.OCR)
	}
	// <=0 回默认。
	merged, err = store.SetAI(AIConfig{OCR: AIOCRConfig{Enabled: true, ProviderID: "p1", ModelID: "m1"}}, AIConfig{}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if merged.OCR.MaxImageBytes != AIOCRMaxBytesDefault {
		t.Fatalf("<=0 应回默认: %+v", merged.OCR)
	}
}

// TestValidateAIPersonas 平台人设列表校验：条数上限 / id 与 name 边界。
func TestValidateAIPersonas(t *testing.T) {
	valid := []AIPersonaDef{{ID: "p1", Name: "写作", SystemPrompt: "你是写作助手"}}
	if err := ValidateAIPersonas(valid); err != nil {
		t.Fatalf("合法列表应通过: %v", err)
	}
	if err := ValidateAIPersonas(nil); err != nil {
		t.Fatalf("nil 列表应通过: %v", err)
	}
	// 条数上限（≤50）。
	over := make([]AIPersonaDef, AIPlatformMaxEntries+1)
	for i := range over {
		over[i] = AIPersonaDef{ID: fmt.Sprintf("p%d", i), Name: "n", SystemPrompt: "p"}
	}
	if err := ValidateAIPersonas(over); err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("超条数应报错: %v", err)
	}
	// id/name 超长。
	if err := ValidateAIPersonas([]AIPersonaDef{{ID: strings.Repeat("i", AIPlatformMaxIDRunes+1), Name: "n", SystemPrompt: "p"}}); err == nil {
		t.Fatal("id 超长应报错")
	}
	if err := ValidateAIPersonas([]AIPersonaDef{{ID: "p1", Name: strings.Repeat("n", AIPlatformMaxNameRunes+1), SystemPrompt: "p"}}); err == nil {
		t.Fatal("name 超长应报错")
	}
}

// TestValidateAIMCPServices ai.mcp 列表校验：条数上限 / id·name·url·
// auth_header 边界。
func TestValidateAIMCPServices(t *testing.T) {
	valid := []AIMCPServiceDef{{ID: "s1", Name: "工具站", URL: "https://mcp.example.com/mcp", Enabled: true}}
	if err := ValidateAIMCPServices(valid); err != nil {
		t.Fatalf("合法列表应通过: %v", err)
	}
	if err := ValidateAIMCPServices(nil); err != nil {
		t.Fatalf("nil 列表应通过: %v", err)
	}
	// 条数上限（≤8）。
	over := make([]AIMCPServiceDef, AIMCPMaxServices+1)
	for i := range over {
		over[i] = AIMCPServiceDef{ID: fmt.Sprintf("s%d", i), Name: "n", URL: "http://x.example.com"}
	}
	if err := ValidateAIMCPServices(over); err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("超条数应报错: %v", err)
	}
	// url 非 http(s) / 重复 id / auth_header 超长。
	if err := ValidateAIMCPServices([]AIMCPServiceDef{{ID: "s1", Name: "n", URL: "ftp://x"}}); err == nil {
		t.Fatal("非 http(s) url 应报错")
	}
	if err := ValidateAIMCPServices([]AIMCPServiceDef{{ID: "s1", Name: "n", URL: "http://a"}, {ID: "s1", Name: "n", URL: "http://b"}}); err == nil {
		t.Fatal("重复 id 应报错")
	}
	long := strings.Repeat("h", AIMCPMaxAuthHeaderRunes+1)
	if err := ValidateAIMCPServices([]AIMCPServiceDef{{ID: "s1", Name: "n", URL: "http://a", AuthHeader: long}}); err == nil {
		t.Fatal("auth_header 超长应报错")
	}
	longURL := "http://x/" + strings.Repeat("p", AIMCPMaxURLRunes)
	if err := ValidateAIMCPServices([]AIMCPServiceDef{{ID: "s1", Name: "n", URL: longURL}}); err == nil {
		t.Fatal("url 超长应报错")
	}
}

// TestAIMCPServicesRoundTrip ai.mcp 读写链：trim 规整、auth_header 留空
// 继承、审计打码、损坏 JSON 容错。
func TestAIMCPServicesRoundTrip(t *testing.T) {
	repo := newMemRepo()
	rec := &memAudit{}
	store := newTestStore(repo, rec)
	actor := uuid.New()
	// 未入库 → 空数组。
	list, err := store.AIMCPServices()
	if err != nil || len(list) != 0 {
		t.Fatalf("未入库应返回空数组: %v %+v", err, list)
	}
	// 写入（trim 规整 + auth 明文入库）。
	in := []AIMCPServiceDef{{ID: " s1 ", Name: " 工具站 ", URL: " https://mcp.example.com/mcp ", AuthHeader: "Authorization: Bearer secret1", Enabled: true}}
	saved, err := store.SetAIMCPServices(in, actor)
	if err != nil {
		t.Fatalf("SetAIMCPServices: %v", err)
	}
	if saved[0].ID != "s1" || saved[0].Name != "工具站" || saved[0].URL != "https://mcp.example.com/mcp" {
		t.Fatalf("应回显规整后的列表: %+v", saved)
	}
	got, _ := store.AIMCPServices()
	if len(got) != 1 || got[0].AuthHeader != "Authorization: Bearer secret1" {
		t.Fatalf("运行时读取须含明文 auth_header: %+v", got)
	}
	// 入库行含明文（运行时要用）；审计载荷打码。
	if !strings.Contains(repo.rows[KeyAIMCPServices].ValueJSON, "secret1") {
		t.Fatalf("入库行应含明文 auth_header: %s", repo.rows[KeyAIMCPServices].ValueJSON)
	}
	last := rec.entries[len(rec.entries)-1]
	if strings.Contains(last.Metadata, "secret1") {
		t.Fatalf("审计不应含明文密钥: %s", last.Metadata)
	}
	if !strings.Contains(last.Metadata, "\"******\"") {
		t.Fatalf("审计应打码 auth_header: %s", last.Metadata)
	}
	// auth_header 留空 = 按 ID 继承现值。
	updated, err := store.SetAIMCPServices([]AIMCPServiceDef{{ID: "s1", Name: "工具站", URL: "https://mcp.example.com/mcp", Enabled: false}}, actor)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated[0].AuthHeader != "Authorization: Bearer secret1" || updated[0].Enabled {
		t.Fatalf("留空应继承 auth_header: %+v", updated)
	}
	// 新 id 无现值可继承 → 空。
	fresh, err := store.SetAIMCPServices([]AIMCPServiceDef{
		{ID: "s1", Name: "工具站", URL: "https://mcp.example.com/mcp", AuthHeader: "Authorization: Bearer secret1", Enabled: true},
		{ID: "s2", Name: "无密钥", URL: "http://localhost:9000/mcp"},
	}, actor)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if fresh[1].AuthHeader != "" {
		t.Fatalf("无现值的 auth_header 应为空: %+v", fresh[1])
	}
	// 非法载荷不落库（现值保留）。
	if _, err := store.SetAIMCPServices([]AIMCPServiceDef{{ID: "bad", Name: "n", URL: "notaurl"}}, actor); err == nil || !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("非法 url 应报 ErrInvalidValue: %v", err)
	}
	if l, _ := store.AIMCPServices(); len(l) != 2 {
		t.Fatalf("非法载荷不应落库: %+v", l)
	}
	// 损坏 JSON → 空数组容错（不阻塞 use_mcp 降级）。
	repo.rows[KeyAIMCPServices] = Setting{Key: KeyAIMCPServices, ValueJSON: `{bad`, ValueType: TypeString}
	if l, err := store.AIMCPServices(); err != nil || len(l) != 0 {
		t.Fatalf("损坏 JSON 应回退空数组: %v %+v", err, l)
	}
}
