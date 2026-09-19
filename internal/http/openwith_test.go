package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/auth"
)

// fakeOpenWithStore 为 openWithStore 的内存实现（多用户隔离由 key 保证）。
type fakeOpenWithStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]map[string]auth.OpenWithPreference
}

func newFakeOpenWithStore() *fakeOpenWithStore {
	return &fakeOpenWithStore{rows: map[uuid.UUID]map[string]auth.OpenWithPreference{}}
}

func (f *fakeOpenWithStore) ListOpenWith(userID uuid.UUID) ([]auth.OpenWithPreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := make([]auth.OpenWithPreference, 0, len(f.rows[userID]))
	for _, p := range f.rows[userID] {
		rows = append(rows, p)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Ext < rows[j].Ext })
	return rows, nil
}

func (f *fakeOpenWithStore) SetOpenWith(userID uuid.UUID, ext, opener string) (auth.OpenWithPreference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[userID] == nil {
		f.rows[userID] = map[string]auth.OpenWithPreference{}
	}
	p := auth.OpenWithPreference{Ext: ext, Opener: opener}
	f.rows[userID][ext] = p
	return p, nil
}

func (f *fakeOpenWithStore) DeleteOpenWith(userID uuid.UUID, ext string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows[userID], ext)
	return nil
}

// openWithEnv 构造 /me/open-with 测试路由（actor 注入 user_id，模式同
// me_test.go）。
func openWithEnv(t *testing.T) (*gin.Engine, *fakeOpenWithStore, uuid.UUID, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newFakeOpenWithStore()
	h := NewHandler(nil, nil, nil, nil, nil, nil, nil, false, "", 0)
	h.openWith = store
	alice, bob := uuid.New(), uuid.New()
	router := gin.New()
	withUser := func(user uuid.UUID, handler gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(auth.UserIDContextKey, user)
			handler(c)
		}
	}
	router.GET("/api/v1/me/open-with", withUser(alice, h.listOpenWith))
	router.PUT("/api/v1/me/open-with", withUser(alice, h.updateOpenWith))
	router.DELETE("/api/v1/me/open-with", withUser(alice, h.deleteOpenWith))
	// bob 复用同一 handler 实例（隔离断言用）。
	router.GET("/bob/me/open-with", withUser(bob, h.listOpenWith))
	router.PUT("/bob/me/open-with", withUser(bob, h.updateOpenWith))
	return router, store, alice, bob
}

func openWithCall(router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestOpenWithUpsertGetDelete(t *testing.T) {
	router, _, _, _ := openWithEnv(t)
	// 初始为空列表。
	w := openWithCall(router, http.MethodGet, "/api/v1/me/open-with", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"open_with":[]`) {
		t.Fatalf("initial list: %d %s", w.Code, w.Body.String())
	}
	// upsert（ext 规范化：去点、小写）。
	w = openWithCall(router, http.MethodPut, "/api/v1/me/open-with", `{"ext":".MD","opener":"markdown"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	var saved auth.OpenWithPreference
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.Ext != "md" || saved.Opener != "markdown" {
		t.Fatalf("saved = %v err=%v", saved, err)
	}
	w = openWithCall(router, http.MethodPut, "/api/v1/me/open-with", `{"ext":"drawio","opener":"drawio"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put 2: %d", w.Code)
	}
	// 覆盖更新同一 ext。
	w = openWithCall(router, http.MethodPut, "/api/v1/me/open-with", `{"ext":"md","opener":"code"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("overwrite: %d", w.Code)
	}
	// 列表（按 ext 排序）。
	w = openWithCall(router, http.MethodGet, "/api/v1/me/open-with", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var out struct {
		OpenWith []auth.OpenWithPreference `json:"open_with"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.OpenWith) != 2 || out.OpenWith[0].Ext != "drawio" || out.OpenWith[1].Ext != "md" || out.OpenWith[1].Opener != "code" {
		t.Fatalf("list = %+v", out.OpenWith)
	}
	// delete（幂等）。
	w = openWithCall(router, http.MethodDelete, "/api/v1/me/open-with?ext=md", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	w = openWithCall(router, http.MethodDelete, "/api/v1/me/open-with?ext=md", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete again: %d", w.Code)
	}
	w = openWithCall(router, http.MethodGet, "/api/v1/me/open-with", "")
	if !strings.Contains(w.Body.String(), `"ext":"drawio"`) || strings.Contains(w.Body.String(), `"ext":"md"`) {
		t.Fatalf("after delete: %s", w.Body.String())
	}
}

func TestOpenWithValidation400(t *testing.T) {
	router, _, _, _ := openWithEnv(t)
	cases := []struct {
		name string
		body string
	}{
		{"bad-opener", `{"ext":"md","opener":"vscode"}`},
		{"empty-opener", `{"ext":"md","opener":""}`},
		{"bad-ext-chars", `{"ext":"m/d","opener":"text"}`},
		{"empty-ext", `{"ext":"","opener":"text"}`},
		{"too-long-ext", `{"ext":"` + strings.Repeat("a", 17) + `","opener":"text"}`},
		{"dot-only-ext", `{"ext":".","opener":"text"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := openWithCall(router, http.MethodPut, "/api/v1/me/open-with", tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("put %s: %d %s", tc.body, w.Code, w.Body.String())
			}
		})
	}
	// DELETE 非法 ext → 400。
	if w := openWithCall(router, http.MethodDelete, "/api/v1/me/open-with?ext=", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("delete empty ext: %d", w.Code)
	}
	if w := openWithCall(router, http.MethodDelete, "/api/v1/me/open-with?ext=a%20b", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("delete bad ext: %d", w.Code)
	}
}

func TestOpenWithUserIsolation(t *testing.T) {
	router, _, _, _ := openWithEnv(t)
	if w := openWithCall(router, http.MethodPut, "/bob/me/open-with", `{"ext":"md","opener":"code"}`); w.Code != http.StatusOK {
		t.Fatalf("bob put: %d", w.Code)
	}
	if w := openWithCall(router, http.MethodPut, "/api/v1/me/open-with", `{"ext":"md","opener":"markdown"}`); w.Code != http.StatusOK {
		t.Fatalf("alice put: %d", w.Code)
	}
	w := openWithCall(router, http.MethodGet, "/api/v1/me/open-with", "")
	if !strings.Contains(w.Body.String(), `"opener":"markdown"`) || strings.Contains(w.Body.String(), `"opener":"code"`) {
		t.Fatalf("alice list polluted: %s", w.Body.String())
	}
	w = openWithCall(router, http.MethodGet, "/bob/me/open-with", "")
	if !strings.Contains(w.Body.String(), `"opener":"code"`) || strings.Contains(w.Body.String(), `"opener":"markdown"`) {
		t.Fatalf("bob list polluted: %s", w.Body.String())
	}
}

// NormalizeOpenWithExt / ValidateOpenWithOpener 单元矩阵。
func TestOpenWithNormalizeMatrix(t *testing.T) {
	for ext, want := range map[string]string{
		"md": "md", ".MD": "md", " Md ": "md", "docx": "docx", "drawio": "drawio",
	} {
		got, err := auth.NormalizeOpenWithExt(ext)
		if err != nil || got != want {
			t.Fatalf("NormalizeOpenWithExt(%q) = %q, %v; want %q", ext, got, err, want)
		}
	}
	for _, ext := range []string{"", ".", "a.b", "m/d", "md x", strings.Repeat("a", 17), ".png.exe"} {
		if _, err := auth.NormalizeOpenWithExt(ext); err == nil {
			t.Fatalf("NormalizeOpenWithExt(%q) must fail", ext)
		}
	}
	for _, opener := range []string{"office", "drawio", "excalidraw", "text", "markdown", "code", "web", "default"} {
		if err := auth.ValidateOpenWithOpener(opener); err != nil {
			t.Fatalf("opener %q must be valid", opener)
		}
	}
	// 复合编码（v:/e:/v:+e:）。
	for _, opener := range []string{
		"v:raw", "v:office", "v:drawio", "v:excalidraw", "v:xmind", "v:richtext",
		"e:text", "e:office", "e:drawio", "e:excalidraw", "e:richtext", "e:none",
		"v:raw+e:text", "v:office+e:office", "e:text+v:raw",
	} {
		if err := auth.ValidateOpenWithOpener(opener); err != nil {
			t.Fatalf("opener %q must be valid", opener)
		}
	}
	for _, opener := range []string{
		"", "vscode", "Office", "browser",
		"v:", "e:", "v:raw+e:text+e:none", "v:raw+v:office", "e:text+e:office",
		"v:code", "e:code", "v:web", "e:web", "v:default", "raw", "e:none+",
	} {
		if err := auth.ValidateOpenWithOpener(opener); err == nil {
			t.Fatalf("opener %q must be invalid", opener)
		}
	}
}
