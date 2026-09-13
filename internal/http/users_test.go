package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// fakeUserDirectory 是 userDirectory 的内存实现，记录 Lookup 调用参数供断言。
type fakeUserDirectory struct {
	lookupQ     string
	lookupLimit int
	results     []auth.User
	lookupErr   error
	names       map[uuid.UUID]string
}

func (f *fakeUserDirectory) FindActiveByEmail(string) (auth.User, error) {
	return auth.User{}, errors.New("not found")
}

func (f *fakeUserDirectory) Lookup(q string, limit int) ([]auth.User, error) {
	f.lookupQ, f.lookupLimit = q, limit
	return f.results, f.lookupErr
}

func (f *fakeUserDirectory) Username(id uuid.UUID) (string, error) {
	if n, ok := f.names[id]; ok {
		return n, nil
	}
	return "", errors.New("user not found")
}

func lookupContext(query string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/users/lookup?q="+url.QueryEscape(query), nil)
	return c, w
}

// 空 q（缺失或纯空白）返回 400。
func TestLookupUsersEmptyQueryRejected(t *testing.T) {
	h := &Handler{users: &fakeUserDirectory{}}
	for _, q := range []string{"", "   "} {
		c, w := lookupContext(q)
		h.lookupUsers(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("q=%q status = %d, want 400", q, w.Code)
		}
	}
}

// 响应只含 id 与 username（绝不返回 email），查询原样透传给目录层。
func TestLookupUsersReturnsIDAndUsernameOnly(t *testing.T) {
	alice := auth.User{ID: uuid.New(), Username: "alice", Email: "alice@example.com", PasswordHash: "x"}
	bob := auth.User{ID: uuid.New(), Username: "bob", Email: "bob@example.com", PasswordHash: "y"}
	fake := &fakeUserDirectory{results: []auth.User{alice, bob}}
	h := &Handler{users: fake}

	c, w := lookupContext("ali")
	h.lookupUsers(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"username":"alice"`) || !strings.Contains(body, `"username":"bob"`) {
		t.Fatalf("body = %s, want usernames alice/bob", body)
	}
	for _, forbidden := range []string{"email", "alice@example.com", "password", "status"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("body %q must not contain %q (enumeration/privacy)", body, forbidden)
		}
	}
	if fake.lookupQ != "ali" {
		t.Fatalf("lookup q = %q, want ali", fake.lookupQ)
	}
	if fake.lookupLimit != 10 {
		t.Fatalf("lookup limit = %d, want 10", fake.lookupLimit)
	}
}

// 无匹配返回空数组（非 null），目录层错误返回 500。
func TestLookupUsersEmptyAndError(t *testing.T) {
	h := &Handler{users: &fakeUserDirectory{results: nil}}
	c, w := lookupContext("zzz")
	h.lookupUsers(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty result body = %q, want []", w.Body.String())
	}

	h = &Handler{users: &fakeUserDirectory{lookupErr: errors.New("db down")}}
	c, w = lookupContext("a")
	h.lookupUsers(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("error status = %d, want 500", w.Code)
	}
}
