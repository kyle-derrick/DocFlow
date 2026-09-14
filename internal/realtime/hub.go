package realtime

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/docflow/docflow/internal/notify"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// seenTTL 为跨实例消息去重记录（msg_id → 过期时刻）的保留时长：只需
// 覆盖 Redis Pub/Sub 回环送达的窗口，过期后懒清理防 map 无界增长。
const seenTTL = 10 * time.Second

type client struct {
	send chan []byte
}

type Hub struct {
	mu      sync.RWMutex
	clients map[uuid.UUID]map[*client]struct{}
	// broadcaster 为跨实例广播桥（nil 或 Noop 时纯本地分发）。Broadcast
	// 走「本地直发 + Publish 全通道」：发布前先把 msg_id 记入 seen，
	// 订阅回调凭 seen 跳过自己的回环消息，仅分发远端消息到本地连接。
	broadcaster Broadcaster
	seenMu      sync.Mutex
	seen        map[string]time.Time
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[uuid.UUID]map[*client]struct{}),
		seen:    make(map[string]time.Time),
	}
}

// SetBroadcaster 接入跨实例广播桥并订阅其消息；Broadcast 随即对全部
// 实例可见。nil / NoopBroadcaster 时保持单实例行为。
func (h *Hub) SetBroadcaster(b Broadcaster) {
	h.broadcaster = b
	b.Subscribe(func(msg Message) {
		if h.seenBefore(msg.MsgID) {
			return
		}
		h.deliverLocal(msg.UserID, msg.Payload)
	})
}

func (h *Hub) Register(user uuid.UUID, conn *websocket.Conn) func() {
	c := h.addClient(user)
	go func() {
		for msg := range c.send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()
	return func() { h.remove(user, c) }
}

// addClient 登记一个本地连接槽（不依赖真实 websocket 连接，测试可直接
// 观察 c.send 通道）；Register 启动写泵消费该通道。
func (h *Hub) addClient(user uuid.UUID) *client {
	c := &client{send: make(chan []byte, 16)}
	h.mu.Lock()
	if h.clients[user] == nil {
		h.clients[user] = make(map[*client]struct{})
	}
	h.clients[user][c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) remove(user uuid.UUID, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if users := h.clients[user]; users != nil {
		if _, ok := users[c]; ok {
			delete(users, c)
			close(c.send)
		}
		if len(users) == 0 {
			delete(h.clients, user)
		}
	}
}

// Broadcast 分发一条通知：本地直发保证本实例 / 无桥（inprocess）语义
// 不回退；有桥时再 Publish 到共享通道，远端实例经订阅投给各自的本地
// 连接。Publish 前先记 seen，自实例订阅回环凭 msg_id 去重。
func (h *Hub) Broadcast(user uuid.UUID, n notify.Notification) {
	payload, err := json.Marshal(n)
	if err != nil {
		return
	}
	h.deliverLocal(user, payload)
	if h.broadcaster == nil {
		return
	}
	msg := Message{MsgID: uuid.NewString(), UserID: user, Type: n.Type, Payload: payload}
	h.markSeen(msg.MsgID)
	if err := h.broadcaster.Publish(msg); err != nil {
		log.Printf("[realtime] broadcast publish: %v", err)
	}
}

// deliverLocal 把已序列化的通知非阻塞投给该用户的全部本地连接；
// send 缓冲满（慢消费者）视作失效连接异步摘除。
func (h *Hub) deliverLocal(user uuid.UUID, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients[user] {
		select {
		case c.send <- msg:
		default:
			go h.remove(user, c)
		}
	}
}

// markSeen 记录本实例已发布的 msg_id（顺带清理过期项）。
func (h *Hub) markSeen(id string) {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	h.sweepSeenLocked()
	h.seen[id] = time.Now().Add(seenTTL)
}

// seenBefore 原子地检查 msg_id 是否已知：已见返回 true（跳过分发——
// 本实例发布的回环或重复投递），未见则记录并返回 false。
func (h *Hub) seenBefore(id string) bool {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	if exp, ok := h.seen[id]; ok && time.Now().Before(exp) {
		return true
	}
	h.sweepSeenLocked()
	h.seen[id] = time.Now().Add(seenTTL)
	return false
}

func (h *Hub) sweepSeenLocked() {
	now := time.Now()
	for id, exp := range h.seen {
		if now.After(exp) {
			delete(h.seen, id)
		}
	}
}
