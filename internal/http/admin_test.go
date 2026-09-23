package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/settings"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeSettingsService 是 settingsService 的内存实现。
type fakeSettingsService struct {
	views   []settings.SettingView
	setErr  error
	lastKey string
	lastVal any
	// intKeys / intErr 支撑 GetInt（batch.max_items 热读取路径的测试）。
	intKeys map[string]int
	intErr  map[string]error
	// boolKeys / boolErr 支撑 GetBool（collab.enabled 热读取路径的测试）；
	// 未显式配置的键回退内置定义默认值（与 *settings.Store 行为一致）。
	boolKeys map[string]bool
	boolErr  map[string]error
	// personas / skills 支撑平台人设/技能端点的测试（可配置读写内存）。
	personas []settings.AIPersonaDef
	skills   []settings.AISkillDef
}

func (f *fakeSettingsService) GetAll() ([]settings.SettingView, error) { return f.views, nil }

func (f *fakeSettingsService) Set(key string, value any, actor uuid.UUID) (any, error) {
	f.lastKey, f.lastVal = key, value
	if f.setErr != nil {
		return nil, f.setErr
	}
	return value, nil
}

func (f *fakeSettingsService) GetInt(key string) (int, error) {
	if err, ok := f.intErr[key]; ok {
		return 0, err
	}
	return f.intKeys[key], nil
}

// GetIntDefined 补齐 settingsService 接口：intKeys 显式配置的键返回
// (值, true)；未配置的键返回 (0, false)（与 *settings.Store 的「未入库」
// 语义一致，供防爆破参数「settings 优先、回落 env」路径测试）。
func (f *fakeSettingsService) GetIntDefined(key string) (int, bool) {
	n, ok := f.intKeys[key]
	return n, ok
}

// GetBool 补齐 settingsService 接口：显式配置优先，否则回退内置定义的
// 默认值（如 collab.enabled 缺省 true）。
func (f *fakeSettingsService) GetBool(key string) (bool, error) {
	if err, ok := f.boolErr[key]; ok {
		return false, err
	}
	if v, ok := f.boolKeys[key]; ok {
		return v, nil
	}
	d, err := settings.DefinitionByKey(key)
	if err != nil {
		return false, err
	}
	b, ok := d.Default.(bool)
	if !ok {
		return false, errors.New("settings key is not bool")
	}
	return b, nil
}

// SMTPOverrides 补齐 settingsService 接口（SMTP 运行时覆盖；测试场景恒空）。
func (f *fakeSettingsService) SMTPOverrides() (settings.SMTPOverride, error) {
	return settings.SMTPOverride{}, nil
}

// SetSMTP 补齐 settingsService 接口（SMTP 运行时保存；测试场景原样返回）。
func (f *fakeSettingsService) SetSMTP(in, env settings.SMTPSettings, actor uuid.UUID) (settings.SMTPSettings, error) {
	return in, nil
}

// AIOverrides 补齐 settingsService 接口（AI 运行时覆盖；测试场景恒默认）。
func (f *fakeSettingsService) AIOverrides() (settings.AIConfig, bool, error) {
	return settings.DefaultAIConfig(), false, nil
}

// personas/skills 支撑平台人设/技能端点的测试（可配置读写内存）。
func (f *fakeSettingsService) AIPersonas() ([]settings.AIPersonaDef, error) {
	if f.personas == nil {
		return []settings.AIPersonaDef{}, nil
	}
	return f.personas, nil
}

func (f *fakeSettingsService) SetAIPersonas(list []settings.AIPersonaDef, _ uuid.UUID) ([]settings.AIPersonaDef, error) {
	if err := settings.ValidateAIPersonas(list); err != nil {
		return nil, err
	}
	f.personas = list
	return list, nil
}

func (f *fakeSettingsService) AISkills() ([]settings.AISkillDef, error) {
	if f.skills == nil {
		return []settings.AISkillDef{}, nil
	}
	return f.skills, nil
}

func (f *fakeSettingsService) SetAISkills(list []settings.AISkillDef, _ uuid.UUID) ([]settings.AISkillDef, error) {
	if err := settings.ValidateAISkills(list); err != nil {
		return nil, err
	}
	f.skills = list
	return list, nil
}

// SetAI 补齐 settingsService 接口（AI 运行时保存；测试场景原样返回）。
func (f *fakeSettingsService) SetAI(in, env settings.AIConfig, actor uuid.UUID) (settings.AIConfig, error) {
	return in, nil
}

type fakeStatsSource struct {
	stats AdminStats
	err   error
}

func (f *fakeStatsSource) Stats() (AdminStats, error) { return f.stats, f.err }

func adminContext(method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

func TestListAdminSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{settings: &fakeSettingsService{views: []settings.SettingView{
		{Key: settings.KeyUploadMaxFileSize, Value: int64(1 << 30), Type: settings.TypeInt, Description: "单文件上传大小上限（字节）", Default: int64(1 << 30)},
	}}}
	c, w := adminContext(http.MethodGet, "/api/v1/admin/settings", "")
	h.listAdminSettings(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"key":"upload.max_file_size"`, `"value":1073741824`, `"type":"int"`, `"description"`, `"effect"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	// 服务未配置时 500（不 panic）。
	h = &Handler{}
	c, w = adminContext(http.MethodGet, "/api/v1/admin/settings", "")
	h.listAdminSettings(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unconfigured status = %d, want 500", w.Code)
	}
}

// TestListAdminSettingsSecrets 凭据状态（G6）：响应附 secrets 只读探针
// （env 非空 = configured），不回显任何密钥值。
func TestListAdminSettingsSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("JWT_SECRET", "unit-test-jwt-secret-0123456789abcdef")
	t.Setenv("SMTP_PASS", "")
	t.Setenv("S3_SECRET_KEY", "")
	t.Setenv("ONLYOFFICE_JWT_SECRET", "")
	h := &Handler{settings: &fakeSettingsService{views: []settings.SettingView{}}}
	c, w := adminContext(http.MethodGet, "/api/v1/admin/settings", "")
	h.listAdminSettings(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"secrets"`, `"jwt_secret":true`, `"smtp_password":false`, `"s3_secret_key":false`, `"onlyoffice_jwt_secret":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	// 绝不回显密钥值。
	if strings.Contains(body, "unit-test-jwt-secret-0123456789abcdef") {
		t.Fatal("secrets must never echo values")
	}
}

func TestUpdateAdminSetting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := uuid.New()
	cases := []struct {
		name    string
		body    string
		setErr  error
		status  int
		wantVal any
	}{
		{name: "ok", body: `{"value":10}`, status: http.StatusOK, wantVal: float64(10)},
		{name: "unknown key", body: `{"value":1}`, setErr: settings.ErrUnknownKey, status: http.StatusNotFound},
		{name: "bad type", body: `{"value":"x"}`, setErr: settings.ErrInvalidType, status: http.StatusBadRequest},
		{name: "out of range", body: `{"value":0}`, setErr: settings.ErrInvalidValue, status: http.StatusBadRequest},
		{name: "missing value", body: `{}`, setErr: settings.ErrInvalidType, status: http.StatusBadRequest},
		{name: "invalid json", body: `not-json`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSettingsService{setErr: tc.setErr}
			h := &Handler{settings: fake}
			c, w := adminContext(http.MethodPut, "/api/v1/admin/settings/upload.max_versions_per_file", tc.body)
			c.Params = gin.Params{{Key: "key", Value: "upload.max_versions_per_file"}}
			c.Set("user_id", admin)
			h.updateAdminSetting(c)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.status, w.Body.String())
			}
			if tc.status == http.StatusOK {
				if fake.lastKey != "upload.max_versions_per_file" || fake.lastVal != tc.wantVal {
					t.Fatalf("Set called with %s=%v", fake.lastKey, fake.lastVal)
				}
				if !strings.Contains(w.Body.String(), `"value":10`) {
					t.Fatalf("body = %s, want normalized value echoed", w.Body.String())
				}
			}
		})
	}
}

func TestAdminStats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{stats: &fakeStatsSource{stats: AdminStats{Users: 3, Files: 42, Uploads: 7, Sessions: 11, Shares: 5, Tokens: 2}}}
	c, w := adminContext(http.MethodGet, "/api/v1/admin/stats", "")
	h.adminStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"users":3`, `"files":42`, `"uploads":7`, `"sessions":11`, `"shares":5`, `"tokens":2`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	// 统计源故障 500；未配置 500。
	h = &Handler{stats: &fakeStatsSource{err: errors.New("db down")}}
	c, w = adminContext(http.MethodGet, "/api/v1/admin/stats", "")
	h.adminStats(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("error status = %d, want 500", w.Code)
	}
	h = &Handler{}
	c, w = adminContext(http.MethodGet, "/api/v1/admin/stats", "")
	h.adminStats(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unconfigured status = %d, want 500", w.Code)
	}
}
