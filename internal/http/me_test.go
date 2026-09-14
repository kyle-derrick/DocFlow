package http

import (
	"net/http"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeUsage 为 storageUsage 的内存实现（/me 的 storage.used 注入）。
type fakeUsage struct {
	used int64
	err  error
}

func (f fakeUsage) UsedStorage(uuid.UUID) (int64, error) { return f.used, f.err }

// UpdateProfile 覆盖 fakeUserDirectory 默认实现：同步更新内存账号记录
// （空串清空文本字段、language/timezone 空串跳过），供 PATCH /me 响应断言。
func (f *fakeAccount) UpdateProfile(id uuid.UUID, update auth.ProfileUpdate) error {
	user, ok := f.accounts[id]
	if !ok {
		return auth.ErrUserNotFound
	}
	apply := func(dst **string, src *string) {
		if src == nil {
			return
		}
		trimmed := strings.TrimSpace(*src)
		if trimmed == "" {
			*dst = nil
			return
		}
		*dst = &trimmed
	}
	apply(&user.Nickname, update.Nickname)
	apply(&user.Department, update.Department)
	apply(&user.Position, update.Position)
	apply(&user.Phone, update.Phone)
	apply(&user.Bio, update.Bio)
	if update.Language != nil {
		if v := strings.TrimSpace(*update.Language); v != "" {
			user.Language = v
		}
	}
	if update.Timezone != nil {
		if v := strings.TrimSpace(*update.Timezone); v != "" {
			user.Timezone = v
		}
	}
	f.accounts[id] = user
	if f.profiles == nil {
		f.profiles = make(map[uuid.UUID]auth.ProfileUpdate)
	}
	f.profiles[id] = update
	return nil
}

// meTestEnv 构造挂好用户目录与用量源的 /me 测试环境；actor 注入 user_id
// 上下文绕过 Bearer（聚焦业务语义，模式同 totp_test.go）。
func meTestEnv(t *testing.T, user auth.User, usage fakeUsage) (*gin.Engine, *fakeAccount) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	account := newFakeAccount()
	account.accounts[user.ID] = user
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.users = account
	h.usage = usage
	router := gin.New()
	withUser := func(handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, user.ID)
			handler(c)
		}
	}
	router.GET("/api/v1/me", withUser(h.me))
	router.PATCH("/api/v1/me", withUser(h.updateMe))
	return router, account
}

// GET /me：返回 id/username/email/role/status、storage{used,quota} 与档案
// 字段（quota 10GiB 默认；软删计入的 used 由用量源给出）。
func TestGetMe(t *testing.T) {
	nickname := "爱丽丝"
	user := auth.User{
		ID: uuid.New(), Username: "alice", Email: "alice@example.com",
		Status: auth.StatusActive, Role: auth.RoleUser,
		StorageQuota: auth.DefaultStorageQuota, Nickname: &nickname,
		Language: "zh-CN", Timezone: "Asia/Shanghai",
	}
	router, _ := meTestEnv(t, user, fakeUsage{used: 1 << 20})
	w := callJSON(router, http.MethodGet, "/api/v1/me", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`"username":"alice"`, `"email":"alice@example.com"`, `"role":"user"`,
		`"used":1048576`, `"quota":10737418240`,
		`"nickname":"爱丽丝"`, `"language":"zh-CN"`, `"timezone":"Asia/Shanghai"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
	if strings.Contains(body, "password") {
		t.Fatalf("body = %s must not contain password hash", body)
	}
}

// PATCH /me 档案校验矩阵：language 白名单、timezone 非空、各字段长度上限、
// 合法更新成功并回显完整视图。
func TestUpdateMeValidationMatrix(t *testing.T) {
	user := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", Status: auth.StatusActive, StorageQuota: auth.DefaultStorageQuota, Language: "zh-CN", Timezone: "Asia/Shanghai"}

	cases := []struct {
		name string
		body string
		code int
	}{
		{"invalid-language", `{"language":"fr-FR"}`, http.StatusBadRequest},
		{"empty-timezone", `{"timezone":""}`, http.StatusBadRequest},
		{"too-long-timezone", `{"timezone":"` + strings.Repeat("x", 65) + `"}`, http.StatusBadRequest},
		{"too-long-nickname", `{"nickname":"` + strings.Repeat("昵", 65) + `"}`, http.StatusBadRequest},
		{"too-long-bio", `{"bio":"` + strings.Repeat("字", 513) + `"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, _ := meTestEnv(t, user, fakeUsage{})
			w := callJSON(router, http.MethodPatch, "/api/v1/me", tc.body)
			if w.Code != tc.code {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.code, w.Body.String())
			}
		})
	}

	// 合法更新：全部档案字段 + 清空（空串）语义。
	router, account := meTestEnv(t, user, fakeUsage{used: 42})
	w := callJSON(router, http.MethodPatch, "/api/v1/me",
		`{"nickname":"Alice","department":"工程部","position":"工程师","phone":"13800000000","bio":"个人简介","language":"en-US","timezone":"UTC"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"nickname":"Alice"`, `"language":"en-US"`, `"timezone":"UTC"`, `"used":42`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
	update, ok := account.profiles[user.ID]
	if !ok {
		t.Fatal("profile update must be recorded")
	}
	if update.Nickname == nil || *update.Nickname != "Alice" || update.Language == nil || *update.Language != "en-US" {
		t.Fatalf("recorded update = %+v", update)
	}
}

// auth.ProfileUpdate.Validate 单元矩阵（C21a）：合法/各非法分支。
func TestProfileUpdateValidate(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name    string
		update  auth.ProfileUpdate
		wantErr bool
	}{
		{"empty", auth.ProfileUpdate{}, false},
		{"valid-full", auth.ProfileUpdate{Nickname: str("Alice"), Department: str("Dev"), Position: str("Eng"), Phone: str("123"), Bio: str("hi"), Language: str("zh-CN"), Timezone: str("Asia/Shanghai")}, false},
		{"empty-strings-allowed", auth.ProfileUpdate{Nickname: str(""), Language: str("")}, false},
		{"language-not-whitelisted", auth.ProfileUpdate{Language: str("ja-JP")}, true},
		{"timezone-space-length1-passes-store-trims", auth.ProfileUpdate{Timezone: str(" ")}, false},
		{"nickname-too-long", auth.ProfileUpdate{Nickname: str(strings.Repeat("a", 65))}, true},
		{"department-too-long", auth.ProfileUpdate{Department: str(strings.Repeat("a", 129))}, true},
		{"phone-too-long", auth.ProfileUpdate{Phone: str(strings.Repeat("1", 33))}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.update.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
