package oidc

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// StateTTL 为登录 state 的有效期（10 分钟）：覆盖 IdP 交互与用户停留，
// 过期未消费的条目在后续 Issue 时惰性清理。
const StateTTL = 10 * time.Minute

// stateEntry 记录 state 对应的 PKCE verifier 与过期时间。
type stateEntry struct {
	verifier  string
	expiresAt time.Time
}

// StateStore 为内存版登录 state 存储：map + 互斥锁，TTL 10 分钟，
// Consume 用后即焚（一次性语义，防重放）。单实例部署够用；多实例
// 需粘性会话或换共享存储（与 Ratelimiter 同级别的取舍）。
type StateStore struct {
	mu      sync.Mutex
	entries map[string]stateEntry
	ttl     time.Duration
	now     func() time.Time
}

func NewStateStore() *StateStore {
	return &StateStore{entries: make(map[string]stateEntry), ttl: StateTTL, now: time.Now}
}

// Issue 生成新的 state 与 PKCE verifier 并登记（同时清理过期条目）。
func (s *StateStore) Issue() (state, verifier string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(raw)
	verifier, err = NewVerifier()
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, entry := range s.entries {
		if now.After(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
	s.entries[state] = stateEntry{verifier: verifier, expiresAt: now.Add(s.ttl)}
	return state, verifier, nil
}

// Consume 原子消费 state：命中且未过期返回其 verifier 并删除条目
// （一次性）；未知/已用/过期返回 ok=false。
func (s *StateStore) Consume(state string) (verifier string, ok bool) {
	if state == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[state]
	if !found || s.now().After(entry.expiresAt) {
		delete(s.entries, state)
		return "", false
	}
	delete(s.entries, state)
	return entry.verifier, true
}
