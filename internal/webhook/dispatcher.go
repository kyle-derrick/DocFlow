package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	// SignatureHeader 为签名头名，值为 sha256=<hex hmac>。
	SignatureHeader = "X-DocFlow-Signature"
	// DeliveryTimeout 单次投递 HTTP 超时。
	DeliveryTimeout = 10 * time.Second
	// DefaultMaxConsecutiveFailures 连续失败自动禁用阈值。
	DefaultMaxConsecutiveFailures = 10
)

// DefaultRetryBackoff 为内部重试退避序列（3 次：1s/5s/25s）；
// 总尝试次数 = 1 + len(backoff) = 4。
var DefaultRetryBackoff = []time.Duration{time.Second, 5 * time.Second, 25 * time.Second}

// Delivery 为 task:webhook-delivery 的队列载荷：通知分发回调组装，
// 投递侧反解后定位 hook 并执行投递。ResourceID 为 nil 表示无关联资源。
type Delivery struct {
	HookID     uuid.UUID  `json:"hook_id"`
	UserID     uuid.UUID  `json:"user_id"`
	Event      string     `json:"event"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	ResourceID *uuid.UUID `json:"resource_id,omitempty"`
}

// MarshalDelivery 序列化投递载荷（notify 接线的入队侧使用）。
func MarshalDelivery(hookID, userID uuid.UUID, event, title, body string, resourceID *uuid.UUID) ([]byte, error) {
	return json.Marshal(Delivery{HookID: hookID, UserID: userID, Event: event, Title: title, Body: body, ResourceID: resourceID})
}

// outboundPayload 为 POST 到回调 URL 的 JSON 载荷形状
// {event, timestamp, data}；data 为通知内容（与站内通知一致）。
type outboundPayload struct {
	Event     string       `json:"event"`
	Timestamp time.Time    `json:"timestamp"`
	Data      outboundData `json:"data"`
}

type outboundData struct {
	UserID     uuid.UUID  `json:"user_id"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	ResourceID *uuid.UUID `json:"resource_id"`
}

// Sign 计算签名头值：sha256=<hex(HMAC-SHA256(secret, body))>，
// body 为实际发送的 JSON 字节（接收方用创建时下发的一次性 secret 验签）。
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Dispatcher 执行 webhook 投递：反解队列载荷 → 定位 hook（已删除/已禁用
// 静默跳过）→ POST 签名载荷（超时 10s，内部重试 3 次退避）→ 记账
// last_status/last_delivered_at/failure_count（成功清零），连续失败达阈值
// 自动禁用并记日志。
type Dispatcher struct {
	store       Store
	client      *http.Client
	now         func() time.Time
	backoff     []time.Duration
	maxFailures int
	logf        func(format string, args ...any)
}

func NewDispatcher(store Store) *Dispatcher {
	return &Dispatcher{
		store:       store,
		client:      &http.Client{Timeout: DeliveryTimeout},
		now:         time.Now,
		backoff:     DefaultRetryBackoff,
		maxFailures: DefaultMaxConsecutiveFailures,
		logf:        log.Printf,
	}
}

// SetHTTPClient 注入 HTTP 客户端（测试用 Transport 替身；超时仍以注入值为准）。
func (d *Dispatcher) SetHTTPClient(client *http.Client) {
	if client != nil {
		d.client = client
	}
}

// SetRetryBackoff 注入重试退避序列（测试用毫秒级序列）。
func (d *Dispatcher) SetRetryBackoff(backoff []time.Duration) {
	if len(backoff) > 0 {
		d.backoff = backoff
	}
}

// SetClock 注入时钟（测试用）。
func (d *Dispatcher) SetClock(fn func() time.Time) {
	if fn != nil {
		d.now = fn
	}
}

// DeliverTask 适配 tasks 的投递处理函数：恒返回 nil（重试已在投递内部
// 完成，不让 inprocess/redis 层再触发一次队列级重试）；载荷非法、hook
// 已删除/禁用均静默跳过（记日志）。
func (d *Dispatcher) DeliverTask(ctx context.Context, raw []byte) error {
	var dl Delivery
	if err := json.Unmarshal(raw, &dl); err != nil {
		d.logf("[webhook] invalid delivery payload: %v", err)
		return nil
	}
	hook, err := d.store.Get(dl.HookID)
	if errors.Is(err, ErrNotFound) {
		return nil // hook 已删除：静默丢弃
	}
	if err != nil {
		d.logf("[webhook] load hook %s: %v", dl.HookID, err)
		return nil
	}
	if !hook.Enabled {
		d.logf("[webhook] skip disabled hook %s (event %s)", hook.ID, dl.Event)
		return nil
	}
	d.deliver(ctx, hook, dl)
	return nil
}

// deliver 执行单次事件投递：构造签名载荷并按退避重试，最后记账。
func (d *Dispatcher) deliver(ctx context.Context, hook Webhook, dl Delivery) {
	body, err := json.Marshal(outboundPayload{
		Event:     dl.Event,
		Timestamp: d.now().UTC(),
		Data:      outboundData{UserID: dl.UserID, Title: dl.Title, Body: dl.Body, ResourceID: dl.ResourceID},
	})
	if err != nil {
		d.logf("[webhook] marshal payload for hook %s: %v", hook.ID, err)
		return
	}
	signature := Sign(hook.Secret, body)
	status, cause := d.attempt(ctx, hook.URL, body, signature)
	if cause == nil {
		if err := d.store.RecordSuccess(hook.ID, status, d.now().UTC()); err != nil {
			d.logf("[webhook] record success for hook %s: %v", hook.ID, err)
		}
		return
	}
	// ctx 取消（进程退出/worker 关闭）打断退避等待：不记账，避免污染
	// 连续失败计数（下一次事件会重新投递）。
	if errors.Is(cause, context.Canceled) {
		d.logf("[webhook] delivery for hook %s aborted: %v", hook.ID, cause)
		return
	}
	// 全部尝试失败：递增连续失败计数；达阈值自动禁用（邮件/日志提示——
	// 记日志，用户可在设置页重新启用；secret 仍在，验签不受影响）。
	failures, err := d.store.RecordFailure(hook.ID, status, d.now().UTC())
	if err != nil {
		d.logf("[webhook] record failure for hook %s: %v", hook.ID, err)
		return
	}
	if failures >= d.maxFailures {
		if _, err := d.store.SetEnabled(hook.UserID, hook.ID, false, d.now().UTC()); err != nil {
			d.logf("[webhook] auto-disable hook %s: %v", hook.ID, err)
			return
		}
		d.logf("[webhook] hook %s auto-disabled after %d consecutive failures (last status %d: %v)", hook.ID, failures, status, cause)
	}
}

// attempt 依次尝试 1+len(backoff) 次：2xx 即成功返回；非 2xx 或传输错误
// 按退避等待后重试。返回最终 HTTP 状态码（传输失败为 0）与失败原因。
func (d *Dispatcher) attempt(ctx context.Context, url string, body []byte, signature string) (int, error) {
	attempts := 1 + len(d.backoff)
	var status int
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			// 退避等待可被 ctx 取消打断（进程退出/worker 关闭时放弃本次投递）。
			select {
			case <-time.After(d.backoff[i-1]):
			case <-ctx.Done():
				return status, ctx.Err()
			}
		}
		code, err := d.post(ctx, url, body, signature)
		status, lastErr = code, err
		if err == nil && code >= 200 && code < 300 {
			return code, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("unexpected http status %d", code)
		}
	}
	return status, lastErr
}

// post 发送单次签名 POST；读取并丢弃（有限长度的）响应体后返回状态码。
func (d *Dispatcher) post(ctx context.Context, url string, body []byte, signature string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, signature)
	req.Header.Set("User-Agent", "DocFlow-Webhook/1.1")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}
