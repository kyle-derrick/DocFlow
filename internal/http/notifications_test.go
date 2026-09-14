package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/docflow/docflow/internal/notify"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// notificationsTestEnv 构造注入内存通知服务的 Handler。
type notificationsTestEnv struct {
	h    *Handler
	svc  *notify.Service
	repo *notify.MemoryStore
}

func newNotificationsTestEnv(t *testing.T) *notificationsTestEnv {
	t.Helper()
	store := notify.NewMemoryStore()
	svc := notify.NewService(store, notify.NewMemoryPreferenceRepo())
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	h.SetNotifications(svc)
	return &notificationsTestEnv{h: h, svc: svc, repo: store}
}

// callNotifications 以 uid 身份调用通知端点（绕过认证中间件，聚焦本人维度语义）。
func callNotifications(h *Handler, method, path, body string, uid uuid.UUID) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, uid)
			handler(c)
		}
	}
	router.GET("/api/v1/notifications", withUser(h.listNotifications))
	router.POST("/api/v1/notifications/:id/read", withUser(h.markNotificationRead))
	router.POST("/api/v1/notifications/read-all", withUser(h.markAllNotificationsRead))
	router.GET("/api/v1/notification-preferences", withUser(h.listNotificationPreferences))
	router.PUT("/api/v1/notification-preferences/:type", withUser(h.updateNotificationPreference))
	w := httptest.NewRecorder()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	router.ServeHTTP(w, req)
	return w
}

func seedNotification(t *testing.T, repo *notify.MemoryStore, user uuid.UUID, title string, at time.Time) notify.Notification {
	t.Helper()
	n := notify.Notification{ID: uuid.New(), UserID: user, Type: notify.EventUploadCompleted, Title: title, CreatedAt: at}
	if err := repo.Create(n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 列表：本人维度、created_at 倒序、附 unread_count；unread_only 过滤；
// 非法 cursor 400；非法 unread_only 400。
func TestListNotificationsEndpoint(t *testing.T) {
	env := newNotificationsTestEnv(t)
	uid, other := uuid.New(), uuid.New()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mineOld := seedNotification(t, env.repo, uid, "older", base)
	seedNotification(t, env.repo, uid, "newer", base.Add(time.Minute))
	seedNotification(t, env.repo, other, "foreign", base.Add(2*time.Minute))

	w := callNotifications(env.h, http.MethodGet, "/api/v1/notifications", "", uid)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"unread_count":2`) {
		t.Fatalf("unread_count missing: %s", body)
	}
	if !strings.Contains(body, "newer") || !strings.Contains(body, "older") || strings.Contains(body, "foreign") {
		t.Fatalf("list must contain only own notifications: %s", body)
	}
	if strings.Index(body, "newer") > strings.Index(body, "older") {
		t.Fatalf("list must be created_at desc: %s", body)
	}

	// 单条已读后 unread_only 只剩未读。
	if ok, err := env.repo.MarkRead(uid, mineOld.ID); err != nil || !ok {
		t.Fatal("seed mark read failed")
	}
	w = callNotifications(env.h, http.MethodGet, "/api/v1/notifications?unread_only=true", "", uid)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "older") {
		t.Fatalf("unread_only must exclude read items: %d %s", w.Code, w.Body.String())
	}

	// 非法参数。
	if w := callNotifications(env.h, http.MethodGet, "/api/v1/notifications?cursor=zzz", "", uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d, want 400", w.Code)
	}
	if w := callNotifications(env.h, http.MethodGet, "/api/v1/notifications?limit=0", "", uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, want 400", w.Code)
	}
}

// 单条已读：本人 204 幂等；他人通知 404；不存在 404。
func TestMarkNotificationReadOwnership(t *testing.T) {
	env := newNotificationsTestEnv(t)
	uid, other := uuid.New(), uuid.New()
	n := seedNotification(t, env.repo, uid, "mine", time.Now())

	if w := callNotifications(env.h, http.MethodPost, "/api/v1/notifications/"+n.ID.String()+"/read", "", other); w.Code != http.StatusNotFound {
		t.Fatalf("foreign notification status = %d, want 404", w.Code)
	}
	if w := callNotifications(env.h, http.MethodPost, "/api/v1/notifications/"+uuid.New().String()+"/read", "", uid); w.Code != http.StatusNotFound {
		t.Fatalf("nonexistent status = %d, want 404", w.Code)
	}
	if w := callNotifications(env.h, http.MethodPost, "/api/v1/notifications/"+n.ID.String()+"/read", "", uid); w.Code != http.StatusNoContent {
		t.Fatalf("own status = %d, want 204", w.Code)
	}
	// 幂等：重复标记仍 204。
	if w := callNotifications(env.h, http.MethodPost, "/api/v1/notifications/"+n.ID.String()+"/read", "", uid); w.Code != http.StatusNoContent {
		t.Fatalf("repeat status = %d, want 204", w.Code)
	}
}

// 全部已读：清零本人未读，不影响他人。
func TestMarkAllNotificationsRead(t *testing.T) {
	env := newNotificationsTestEnv(t)
	uid, other := uuid.New(), uuid.New()
	seedNotification(t, env.repo, uid, "a", time.Now())
	seedNotification(t, env.repo, uid, "b", time.Now().Add(time.Second))
	seedNotification(t, env.repo, other, "c", time.Now())

	if w := callNotifications(env.h, http.MethodPost, "/api/v1/notifications/read-all", "", uid); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if n, _ := env.svc.CountUnread(uid); n != 0 {
		t.Fatalf("uid unread = %d, want 0", n)
	}
	if n, _ := env.svc.CountUnread(other); n != 1 {
		t.Fatalf("other unread = %d, want 1 (read-all is per-user)", n)
	}
}

// 偏好端点：列表含全部事件类型（默认开启）；更新生效；未知类型 400；
// 缺省 body 400。
func TestNotificationPreferencesEndpoints(t *testing.T) {
	env := newNotificationsTestEnv(t)
	uid := uuid.New()

	w := callNotifications(env.h, http.MethodGet, "/api/v1/notification-preferences", "", uid)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, et := range notify.EventTypes {
		if !strings.Contains(body, et) {
			t.Fatalf("preferences must contain %s: %s", et, body)
		}
	}

	w = callNotifications(env.h, http.MethodPut, "/api/v1/notification-preferences/share.accessed", `{"enabled":false}`, uid)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("update status = %d body = %s", w.Code, w.Body.String())
	}
	if enabled, _ := env.svc.PreferenceEnabled(uid, notify.EventShareAccessed); enabled {
		t.Fatal("preference must be disabled after update")
	}

	if w := callNotifications(env.h, http.MethodPut, "/api/v1/notification-preferences/not.an.event", `{"enabled":true}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown type status = %d, want 400", w.Code)
	}
	if w := callNotifications(env.h, http.MethodPut, "/api/v1/notification-preferences/share.accessed", `{"enabled":"yes"}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid body status = %d, want 400", w.Code)
	}
	if w := callNotifications(env.h, http.MethodPut, "/api/v1/notification-preferences/share.accessed", `{}`, uid); w.Code != http.StatusBadRequest {
		t.Fatalf("missing enabled status = %d, want 400", w.Code)
	}
}

// 未注入通知服务时端点 503（生产恒注入；此处仅防御性语义）。
func TestNotificationsNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", time.Hour)
	router := gin.New()
	router.GET("/api/v1/notifications", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, uuid.New())
		h.listNotifications(c)
	})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
