package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RateLimiter 是内存实现的固定窗口限流器（每分钟 N 次）。
type RateLimiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	now       func() time.Time
	buckets   map[string]windowCount
	lastSweep time.Time
}

type windowCount struct {
	start time.Time
	count int
}

func NewRateLimiter(limitPerMinute int) *RateLimiter {
	now := time.Now
	return &RateLimiter{limit: limitPerMinute, window: time.Minute, now: now, buckets: make(map[string]windowCount), lastSweep: now()}
}

// SetNow 注入时钟，便于测试。
func (l *RateLimiter) SetNow(now func() time.Time) { l.now = now }

// Allow 返回 (allowed, retryAfter)。retryAfter 为当前窗口剩余时间。
// 每个窗口周期清理一次过期桶，避免内存无限增长。
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.limit <= 0 {
		return true, 0
	}
	if now.Sub(l.lastSweep) >= l.window {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		l.buckets[key] = windowCount{start: now, count: 1}
		return true, 0
	}
	if b.count >= l.limit {
		return false, l.window - now.Sub(b.start)
	}
	b.count++
	l.buckets[key] = b
	return true, 0
}

// loginLimiterKey 计算登录限流 key：IP + 邮箱前缀哈希（sha256 前 16 hex）。
// 不存储明文邮箱，仅取 @ 前的前缀参与哈希。
func loginLimiterKey(ip, email string) string {
	prefix := email
	if at := strings.IndexByte(email, '@'); at > 0 {
		prefix = email[:at]
	}
	sum := sha256.Sum256([]byte(strings.ToLower(prefix)))
	return ip + "|" + hex.EncodeToString(sum[:8])
}

// apiLimiter 为所有 /api/v1 认证接口提供按 user_id+IP 的基础限流。
// 需挂在 auth.RequireAccessToken 之后，以读取 user_id。
func apiLimiter(l *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.ClientIP()
		if value, exists := c.Get(auth.UserIDContextKey); exists {
			if id, ok := value.(uuid.UUID); ok {
				key = id.String() + "|" + key
			}
		}
		if ok, retry := l.Allow(key); !ok {
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}

// publicLimiter 为公开分享接口提供按 IP 的独立限流（PUBLIC_RATE_LIMIT_PER_MIN）。
// 公开接口不要求认证、不设置任何 cookie。
func publicLimiter(l *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if ok, retry := l.Allow(c.ClientIP()); !ok {
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}

// loginLimiter 为登录接口提供更严格限流（按 IP+邮箱前缀哈希）。
// 预读 body 计算 key 后重置 Body，登录 handler 可再次绑定 JSON。
func loginLimiter(l *RateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal(body, &request)
		key := loginLimiterKey(c.ClientIP(), request.Email)
		if ok, retry := l.Allow(key); !ok {
			c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}
