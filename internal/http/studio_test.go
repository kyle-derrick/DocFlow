// studio_test.go：/studio/projects 端点测试（载荷校验 400 / 未注入 503 /
// UUID 解析失败分支；DB 交互由 E2E 与真实栈覆盖）。属主隔离语义（非属主
// 404）在 Store 层以 user_id 条件实现，见 internal/studio。
package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/studio"
)

// fakeStudioStore 记录调用并返回可编排结果。
type fakeStudioStore struct {
	listErr   error
	created   *studio.Project
	createErr error
	updated   *studio.Project
	found     bool
	updateErr error
	delFound  bool
	delErr    error
}

func (f *fakeStudioStore) List(user uuid.UUID) ([]studio.Project, error) {
	return nil, f.listErr
}
func (f *fakeStudioStore) Create(user uuid.UUID, p studio.Project) (studio.Project, error) {
	// 与真实 Store 同口径：先走载荷校验（handler 层不做业务校验）。
	if err := studio.ValidateProject(&p); err != nil {
		return studio.Project{}, err
	}
	if f.createErr != nil {
		return studio.Project{}, f.createErr
	}
	if f.created != nil {
		return *f.created, nil
	}
	return p, nil
}
func (f *fakeStudioStore) Update(user, id uuid.UUID, p studio.Project) (studio.Project, bool, error) {
	if err := studio.ValidateProject(&p); err != nil {
		return studio.Project{}, false, err
	}
	if f.updateErr != nil {
		return studio.Project{}, false, f.updateErr
	}
	if !f.found || f.updated == nil {
		return studio.Project{}, false, nil
	}
	return *f.updated, true, nil
}
func (f *fakeStudioStore) Delete(user, id uuid.UUID) (bool, error) {
	return f.delFound, f.delErr
}

// newStudioTestRouter 组装 /studio/projects 测试路由（用户注入 + 可选存储）。
func newStudioTestRouter(actor uuid.UUID, store studioStore) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.studioProjects = store
	withUser := func(handle gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, actor)
			handle(c)
		}
	}
	r := gin.New()
	r.GET("/api/v1/studio/projects", withUser(h.studioProjectsList))
	r.POST("/api/v1/studio/projects", withUser(h.studioProjectsCreate))
	r.PUT("/api/v1/studio/projects/:id", withUser(h.studioProjectsUpdate))
	r.DELETE("/api/v1/studio/projects/:id", withUser(h.studioProjectsDelete))
	return r
}

func studioPayload(name, space, root string) string {
	return `{"name":"` + name + `","space_id":"` + space + `","root_folder_id":"` + root + `","engine":"platform"}`
}

// TestStudioProjectsValidation 载荷校验：缺 name/非法 UUID → 400。
func TestStudioProjectsValidation(t *testing.T) {
	actor := uuid.Must(uuid.NewV7())
	space := uuid.Must(uuid.NewV7())
	root := uuid.Must(uuid.NewV7())
	r := newStudioTestRouter(actor, &fakeStudioStore{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/studio/projects", strings.NewReader(`{"space_id":"`+space.String()+`","root_folder_id":"`+root.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 name 应 400，得 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/studio/projects", strings.NewReader(`{"name":"x","space_id":"not-uuid","root_folder_id":"`+root.String()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 space_id 应 400，得 %d", w.Code)
	}

	// 合法载荷 201（fake 原样返回）。
	req = httptest.NewRequest(http.MethodPost, "/api/v1/studio/projects", strings.NewReader(studioPayload("官网", space.String(), root.String())))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("合法载荷应 201，得 %d（body=%s）", w.Code, w.Body.String())
	}
}

// TestStudioProjectsNotFound 更新/删除非属主或不存在 → 404（fake 返回
// found=false 模拟 Store 的属主过滤结果）。
func TestStudioProjectsNotFound(t *testing.T) {
	actor := uuid.Must(uuid.NewV7())
	space := uuid.Must(uuid.NewV7())
	root := uuid.Must(uuid.NewV7())
	id := uuid.Must(uuid.NewV7())
	r := newStudioTestRouter(actor, &fakeStudioStore{found: false, delFound: false})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/studio/projects/"+id.String(), strings.NewReader(studioPayload("改名", space.String(), root.String())))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("更新不存在应 404，得 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/studio/projects/"+id.String(), nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("删除不存在应 404，得 %d", w.Code)
	}

	// 非法路径 id → parseID 400。
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/studio/projects/not-uuid", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 应 400，得 %d", w.Code)
	}
}

// TestStudioProjectsNoStore 未注入存储（生产恒注入；防装配遗漏）→ 503。
func TestStudioProjectsNoStore(t *testing.T) {
	actor := uuid.Must(uuid.NewV7())
	r := newStudioTestRouter(actor, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/studio/projects", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未注入存储应 503，得 %d", w.Code)
	}
}
