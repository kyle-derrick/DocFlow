package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func init() { gin.SetMode(gin.TestMode) }

func TestRateLimiterFixedWindow(t *testing.T) {
	l := NewRateLimiter(3)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base
	l.SetNow(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		ok, retry := l.Allow("k")
		if !ok || retry != 0 {
			t.Fatalf("request %d should pass, got ok=%v retry=%v", i+1, ok, retry)
		}
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("4th request in window must be rejected")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry = %v, want (0, 1m]", retry)
	}
	// 不同 key 互不影响。
	if ok2, _ := l.Allow("other"); !ok2 {
		t.Fatal("different key must have its own bucket")
	}
	// 窗口滚动后恢复。
	now = base.Add(time.Minute)
	if ok3, _ := l.Allow("k"); !ok3 {
		t.Fatal("new window must reset the bucket")
	}
}

func TestAPILimiterReturns429WithRetryAfter(t *testing.T) {
	l := NewRateLimiter(2)
	user := uuid.New()
	router := gin.New()
	router.POST("/api/v1/ping", func(c *gin.Context) {
		c.Set(auth.UserIDContextKey, user)
		c.Next()
	}, apiLimiter(l), func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})
	do := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ping", nil)
		req.RemoteAddr = ip + ":12345"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	if w := do("10.0.0.1"); w.Code != http.StatusOK {
		t.Fatalf("first request: %d", w.Code)
	}
	if w := do("10.0.0.1"); w.Code != http.StatusOK {
		t.Fatalf("second request: %d", w.Code)
	}
	w := do("10.0.0.1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: want 429, got %d", w.Code)
	}
	retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || retry < 1 {
		t.Fatalf("Retry-After = %q, want positive integer", w.Header().Get("Retry-After"))
	}
	// 同一用户不同 IP 独立计数。
	if w := do("10.0.0.2"); w.Code != http.StatusOK {
		t.Fatalf("different IP for same user: %d", w.Code)
	}
}

func TestLoginLimiterStricterAndBodyReplay(t *testing.T) {
	l := NewRateLimiter(2)
	router := gin.New()
	router.POST("/api/v1/auth/login", loginLimiter(l), func(c *gin.Context) {
		var request struct {
			Email string `json:"email"`
		}
		if c.ShouldBindJSON(&request) != nil {
			c.String(http.StatusBadRequest, "bad")
			return
		}
		c.String(http.StatusOK, request.Email)
	})
	post := func(ip, email string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"email": email, "password": "x"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":9999"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	email := "alice@example.com"
	for i := 0; i < 2; i++ {
		w := post("203.0.113.1", email)
		if w.Code != http.StatusOK || w.Body.String() != email {
			t.Fatalf("request %d: code=%d body=%q (limiter must not consume body)", i+1, w.Code, w.Body.String())
		}
	}
	w := post("203.0.113.1", email)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd login attempt: want 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must include Retry-After")
	}
	// 同 IP 换邮箱不受同一桶限制（按邮箱前缀分桶）。
	if w := post("203.0.113.1", "bob@example.com"); w.Code != http.StatusOK {
		t.Fatalf("different email same IP: %d", w.Code)
	}
	// 同邮箱换 IP 也不受限制。
	if w := post("203.0.113.2", email); w.Code != http.StatusOK {
		t.Fatalf("same email different IP: %d", w.Code)
	}
}

func TestLoginLimiterKeyHashesEmailPrefix(t *testing.T) {
	a := loginLimiterKey("1.2.3.4", "Alice@Example.com")
	b := loginLimiterKey("1.2.3.4", "alice@example.com")
	if a != b {
		t.Fatal("key must be case-insensitive on email prefix")
	}
	if strings.Contains(a, "alice") {
		t.Fatal("key must not embed the plaintext email prefix")
	}
	if loginLimiterKey("1.2.3.4", "alice@example.com") == loginLimiterKey("1.2.3.4", "bob@example.com") {
		t.Fatal("different prefixes must map to different keys")
	}
	if loginLimiterKey("1.2.3.4", "alice@example.com") == loginLimiterKey("5.6.7.8", "alice@example.com") {
		t.Fatal("different IPs must map to different keys")
	}
	// 无 @ 的输入退化为整串参与哈希，不 panic。
	if loginLimiterKey("1.2.3.4", "plainname") == "" {
		t.Fatal("key must not be empty")
	}
}
