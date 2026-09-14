package webhook

import (
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/notify"
)

const (
	// secretPrefix 为签名密钥明文前缀（UI 识别用，同 dfpat_ 模式）。
	secretPrefix = "whsec_"
	// secretRandomBytes 为随机体长度（32B → URL-safe base64 43 字符）。
	secretRandomBytes = 32
	// maxURLLength 回调 URL 长度上限。
	maxURLLength = 2048
)

// Service 提供 webhook 的注册/列举/启停/删除（HTTP 层使用）与按事件检索
// （通知分发回调使用）。secret 明文仅在 Create 返回一次。
type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service { return &Service{store: store, now: time.Now} }

// SetClock 注入时钟（测试用，幂等）。
func (s *Service) SetClock(fn func() time.Time) {
	if fn != nil {
		s.now = fn
	}
}

// ValidateURL 校验回调 URL：必须为 http(s) 方案、host 非空、不得携带
// userinfo（凭据不应出现在 URL 中）；localhost/内网地址允许（自托管
// 部署场景，不强制公网）。SSRF 面经投递侧缓解：secret 签名使伪造回调
// 可被接收方验证、有限重试（3 次退避）与连续失败自动禁用限制放大行为。
func ValidateURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxURLLength {
		return ErrInvalidURL
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return ErrInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidURL
	}
	if u.User != nil || u.Host == "" {
		return ErrInvalidURL
	}
	return nil
}

// ValidateEvents 校验事件列表：非空且全部在 notify 白名单内；去重并保持
// 首次出现顺序。
func ValidateEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return nil, ErrInvalidEvents
	}
	seen := make(map[string]struct{}, len(events))
	out := make([]string, 0, len(events))
	for _, e := range events {
		if !notify.ValidEventType(e) {
			return nil, ErrInvalidEvents
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out, nil
}

// newSecret 生成 whsec_ + 32B 随机数的 URL-safe base64（43 字符）明文。
func newSecret() (string, error) {
	buf := make([]byte, secretRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Create 注册 webhook：URL 与事件校验通过后生成 secret（明文仅本次返回，
// 库中存于 secret 列，取舍见 migration 018 注释）。(user_id,url) 重复返回
// ErrDuplicateURL。
func (s *Service) Create(owner uuid.UUID, rawURL string, events []string) (Webhook, string, error) {
	if err := ValidateURL(rawURL); err != nil {
		return Webhook{}, "", err
	}
	normalized, err := ValidateEvents(events)
	if err != nil {
		return Webhook{}, "", err
	}
	secret, err := newSecret()
	if err != nil {
		return Webhook{}, "", err
	}
	now := s.now().UTC()
	w := Webhook{
		ID:        uuid.New(),
		UserID:    owner,
		URL:       strings.TrimSpace(rawURL),
		Events:    normalized,
		Secret:    secret,
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.Create(w); err != nil {
		return Webhook{}, "", err
	}
	return w, secret, nil
}

// List 见 Store.List（含已禁用，供设置页展示状态）。
func (s *Service) List(owner uuid.UUID) ([]Webhook, error) { return s.store.List(owner) }

// Update 启停本人的 webhook（非属主/不存在返回 ErrNotFound）。
func (s *Service) Update(owner, id uuid.UUID, enabled bool) (Webhook, error) {
	found, err := s.store.SetEnabled(owner, id, enabled, s.now().UTC())
	if err != nil {
		return Webhook{}, err
	}
	if !found {
		return Webhook{}, ErrNotFound
	}
	return s.store.Get(id)
}

// Delete 删除本人的 webhook（非属主/不存在返回 ErrNotFound）。
func (s *Service) Delete(owner, id uuid.UUID) error {
	found, err := s.store.Delete(owner, id)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// HooksForEvent 返回该用户启用且订阅了 event 的 webhook（notify 出站
// 接线使用）；未知事件类型返回空（防御）。
func (s *Service) HooksForEvent(owner uuid.UUID, event string) ([]Webhook, error) {
	if !notify.ValidEventType(event) {
		return nil, nil
	}
	return s.store.ListForEvent(owner, event)
}
