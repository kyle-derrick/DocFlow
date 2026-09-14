package http

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/files"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeDashboardSource 是 dashboardSource 的内存实现（聚合单测用）。
type fakeDashboardSource struct {
	summary DashboardSummary
	err     error
	// lastUser 记录最后一次查询的用户（校验 owner 维度透传）。
	lastUser uuid.UUID
}

func (f *fakeDashboardSource) Dashboard(user uuid.UUID, now time.Time) (DashboardSummary, error) {
	f.lastUser = user
	if f.err != nil {
		return DashboardSummary{}, f.err
	}
	return f.summary, nil
}

// fakeRoleLookup 是 auth.RoleLookup 的内存实现。
type fakeRoleLookup struct {
	role string
	err  error
}

func (f fakeRoleLookup) Role(id uuid.UUID) (string, error) { return f.role, f.err }

func TestDashboardPersonal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := uuid.New()
	updated := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	fake := &fakeDashboardSource{summary: DashboardSummary{
		Files:        12,
		StorageBytes: 2048,
		TeamFiles:    3,
		Shares:       4,
		Uploads7d:    7,
		RecentFiles: []files.File{
			{ID: uuid.New(), Name: "a.txt", UpdatedAt: updated},
		},
	}}
	// 非 admin：无全局统计节。
	h := &Handler{dashboard: fake, roles: fakeRoleLookup{role: "user"}}
	c, w := adminContext(http.MethodGet, "/api/v1/dashboard", "")
	c.Set("user_id", user)
	h.dashboardStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if fake.lastUser != user {
		t.Fatalf("dashboard queried user %s, want %s", fake.lastUser, user)
	}
	body := w.Body.String()
	for _, want := range []string{`"files":12`, `"storage_bytes":2048`, `"team_files":3`, `"shares":4`, `"uploads_7d":7`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body %s must contain %s", body, want)
		}
	}
	if strings.Contains(body, `"admin"`) {
		t.Fatalf("non-admin body must not contain admin section: %s", body)
	}
	if !strings.Contains(body, `"name":"a.txt"`) || !strings.Contains(body, updated.Format(time.RFC3339Nano)) {
		t.Fatalf("body %s must contain recent file name/updated_at", body)
	}
}

func TestDashboardAdminSection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := uuid.New()
	h := &Handler{
		dashboard: &fakeDashboardSource{},
		roles:     fakeRoleLookup{role: auth.RoleAdmin},
		stats:     &fakeStatsSource{stats: AdminStats{Users: 9, Tokens: 2}},
	}
	c, w := adminContext(http.MethodGet, "/api/v1/dashboard", "")
	c.Set("user_id", user)
	h.dashboardStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"admin"`) || !strings.Contains(body, `"users":9`) || !strings.Contains(body, `"tokens":2`) {
		t.Fatalf("admin body must embed global stats: %s", body)
	}
}

func TestDashboardRoleLookupFailureDegrades(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := uuid.New()
	// 角色查询失败：静默降级为个人视图（200，无 admin 节）。
	h := &Handler{
		dashboard: &fakeDashboardSource{},
		roles:     fakeRoleLookup{err: errors.New("db down")},
	}
	c, w := adminContext(http.MethodGet, "/api/v1/dashboard", "")
	c.Set("user_id", user)
	h.dashboardStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"admin"`) {
		t.Fatalf("role lookup failure must degrade to personal view: %s", w.Body.String())
	}
}

func TestDashboardErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	user := uuid.New()
	cases := []struct {
		name   string
		h      *Handler
		status int
	}{
		{name: "unconfigured", h: &Handler{}, status: http.StatusInternalServerError},
		{name: "aggregate error", h: &Handler{dashboard: &fakeDashboardSource{err: errors.New("db down")}}, status: http.StatusInternalServerError},
		{name: "admin stats unconfigured", h: &Handler{
			dashboard: &fakeDashboardSource{},
			roles:     fakeRoleLookup{role: auth.RoleAdmin},
		}, status: http.StatusInternalServerError},
		{name: "admin stats error", h: &Handler{
			dashboard: &fakeDashboardSource{},
			roles:     fakeRoleLookup{role: auth.RoleAdmin},
			stats:     &fakeStatsSource{err: errors.New("db down")},
		}, status: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, w := adminContext(http.MethodGet, "/api/v1/dashboard", "")
			c.Set("user_id", user)
			tc.h.dashboardStats(c)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.status, w.Body.String())
			}
		})
	}
}
