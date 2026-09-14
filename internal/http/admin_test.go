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
}

func (f *fakeSettingsService) GetAll() ([]settings.SettingView, error) { return f.views, nil }

func (f *fakeSettingsService) Set(key string, value any, actor uuid.UUID) (any, error) {
	f.lastKey, f.lastVal = key, value
	if f.setErr != nil {
		return nil, f.setErr
	}
	return value, nil
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
	for _, want := range []string{`"key":"upload.max_file_size"`, `"value":1073741824`, `"type":"int"`, `"description"`} {
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
	h := &Handler{stats: &fakeStatsSource{stats: AdminStats{Users: 3, Files: 42, Uploads: 7, Sessions: 11, Shares: 5}}}
	c, w := adminContext(http.MethodGet, "/api/v1/admin/stats", "")
	h.adminStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"users":3`, `"files":42`, `"uploads":7`, `"sessions":11`, `"shares":5`} {
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
