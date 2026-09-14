package realtime

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/docflow/docflow/internal/notify"
	"github.com/google/uuid"
)

// fakeBroadcaster 模拟 Redis Pub/Sub 语义：Publish 同步扇出给全部订阅者
// （含发布实例自身的订阅回调，即回环），用于验证 Hub 的跨实例分发与
// msg_id seen 去重。
type fakeBroadcaster struct {
	mu   sync.Mutex
	subs []fakeSub
}

type fakeSub struct {
	id int
	fn func(Message)
}

func (f *fakeBroadcaster) Publish(msg Message) error {
	f.mu.Lock()
	subs := make([]fakeSub, len(f.subs))
	copy(subs, f.subs)
	f.mu.Unlock()
	for _, s := range subs {
		s.fn(msg)
	}
	return nil
}

func (f *fakeBroadcaster) Subscribe(fn func(Message)) func() {
	f.mu.Lock()
	id := len(f.subs)
	f.subs = append(f.subs, fakeSub{id: id, fn: fn})
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, s := range f.subs {
			if s.id == id {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				return
			}
		}
	}
}

func recv(t *testing.T, ch chan []byte) []byte {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

func assertNoExtra(t *testing.T, name string, chs ...chan []byte) {
	t.Helper()
	for _, ch := range chs {
		select {
		case extra := <-ch:
			t.Fatalf("%s received unexpected duplicate message: %s", name, extra)
		default:
		}
	}
}

// TestHubCrossInstanceBroadcast：两个 Hub 共享同一桥（模拟 Redis Pub/Sub
// 含回环）——A 实例 Broadcast 后：A 本地连接恰好收到一条（本地直发，
// 回环被 seen 去重），B 实例同用户本地连接恰好收到一条（远端分发），
// 其他用户连接不收。
func TestHubCrossInstanceBroadcast(t *testing.T) {
	bridge := &fakeBroadcaster{}
	hubA := NewHub()
	hubA.SetBroadcaster(bridge)
	hubB := NewHub()
	hubB.SetBroadcaster(bridge)

	user := uuid.New()
	other := uuid.New()
	clientA := hubA.addClient(user)
	clientB := hubB.addClient(user)
	clientOther := hubB.addClient(other)

	n := notify.Notification{ID: uuid.New(), Type: notify.EventUploadCompleted, Title: "上传完成", Body: "文件已就绪"}
	hubA.Broadcast(user, n)

	var gotB notify.Notification
	if err := json.Unmarshal(recv(t, clientB.send), &gotB); err != nil {
		t.Fatalf("hubB message decode: %v", err)
	}
	if gotB.ID != n.ID || gotB.Type != n.Type {
		t.Fatalf("hubB got wrong notification: %+v", gotB)
	}

	var gotA notify.Notification
	if err := json.Unmarshal(recv(t, clientA.send), &gotA); err != nil {
		t.Fatalf("hubA message decode: %v", err)
	}
	if gotA.ID != n.ID {
		t.Fatalf("hubA got wrong notification: %+v", gotA)
	}

	// 各连接恰好一条：无回环重复、无跨用户串扰。
	time.Sleep(50 * time.Millisecond)
	assertNoExtra(t, "hubA", clientA.send)
	assertNoExtra(t, "hubB", clientB.send)
	assertNoExtra(t, "other user", clientOther.send)
}

// TestHubSeenDedupRedelivery：同一 msg_id 重复投递（模拟重复送达）只分发
// 一次——首次未见则投递并记 seen，后续命中 seen 跳过。
func TestHubSeenDedupRedelivery(t *testing.T) {
	bridge := &fakeBroadcaster{}
	hub := NewHub()
	hub.SetBroadcaster(bridge)

	user := uuid.New()
	c := hub.addClient(user)

	msg := Message{MsgID: uuid.NewString(), UserID: user, Type: "x", Payload: json.RawMessage(`{"type":"x","title":"t"}`)}
	if err := bridge.Publish(msg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	recv(t, c.send)

	if err := bridge.Publish(msg); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	assertNoExtra(t, "redelivered", c.send)
}

// TestHubNoopBroadcaster：inprocess（无 Redis）时 Noop 桥保持纯本地
// 分发行为——广播仅达本实例连接，Publish 为空操作。
func TestHubNoopBroadcaster(t *testing.T) {
	hub := NewHub()
	hub.SetBroadcaster(NoopBroadcaster{})
	user := uuid.New()
	c := hub.addClient(user)

	n := notify.Notification{ID: uuid.New(), Type: notify.EventFileUpdated, Title: "t", Body: "b"}
	hub.Broadcast(user, n)

	var got notify.Notification
	if err := json.Unmarshal(recv(t, c.send), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != n.ID {
		t.Fatalf("got wrong notification: %+v", got)
	}
	assertNoExtra(t, "noop", c.send)
}

// TestMessageEnvelopeJSON：跨实例信封序列化为 {msg_id,user_id,type,payload}
// 并可无损往返（RedisBroadcaster 的线上格式契约）。
func TestMessageEnvelopeJSON(t *testing.T) {
	user := uuid.New()
	m := Message{MsgID: "m-1", UserID: user, Type: notify.EventQuotaWarning, Payload: json.RawMessage(`{"id":"abc","type":"quota.warning"}`)}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"msg_id"`, `"user_id"`, `"type"`, `"payload"`} {
		if !bytes.Contains(data, []byte(key)) {
			t.Fatalf("envelope missing field %s: %s", key, data)
		}
	}
	var back Message
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.MsgID != m.MsgID || back.UserID != user || back.Type != m.Type || !bytes.Equal(back.Payload, m.Payload) {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}
