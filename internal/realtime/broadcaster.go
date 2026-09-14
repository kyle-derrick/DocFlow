package realtime

import (
	"encoding/json"

	"github.com/google/uuid"
)

// Message 为跨实例广播的线上信封（JSON）：msg_id 用于回环去重——发布
// 实例先把 msg_id 记入本地 seen 集合（TTL 10s），订阅回调收到已知
// msg_id（即本实例发布的消息经 Pub/Sub 回环）时跳过；user_id 定位
// 接收方本地连接集合；type 为通知事件类型（排查用）；payload 为完整
// notify.Notification JSON，与单实例直发格式一致，客户端无感。
type Message struct {
	MsgID   string          `json:"msg_id"`
	UserID  uuid.UUID       `json:"user_id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Broadcaster 为 Hub 的跨实例广播桥。Publish 把消息发布到所有实例共享
// 的通道（Redis Pub/Sub）；Subscribe 注册回调，通道消息（含本实例自己
// 发布的回环）统一交给回调，由 Hub 做 seen 去重后仅分发本地连接。
// 返回的 cancel 注销回调。
type Broadcaster interface {
	Publish(msg Message) error
	Subscribe(handler func(Message)) (cancel func())
}

// NoopBroadcaster 单实例（QUEUE_DRIVER=inprocess）桥实现：发布即丢弃、
// 订阅不投递，Hub 保持纯本地分发行为。
type NoopBroadcaster struct{}

func (NoopBroadcaster) Publish(Message) error { return nil }

func (NoopBroadcaster) Subscribe(func(Message)) func() { return func() {} }
