package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/redis/go-redis/v9"
)

// notifyChannel 为跨实例通知广播的 Redis Pub/Sub 频道。
const notifyChannel = "docflow:notify"

// RedisBroadcaster 基于 Redis Pub/Sub 的跨实例广播桥（QUEUE_DRIVER=redis
// 时启用）：连接参数与 asynq 队列共用（REDIS_ADDR/REDIS_PASSWORD，无独立
// 配置；连通性由启动时的 PingRedis 一并校验）。Publish 即 PUBLISH；
// 每个 Subscribe 一条独立 SUBSCRIBE 连接 + 投递 goroutine，cancel 关闭
// 连接并停止投递。消息经 Hub 的 seen 去重后仅分发本地连接。
type RedisBroadcaster struct {
	client *redis.Client
	wg     sync.WaitGroup
}

var _ Broadcaster = (*RedisBroadcaster)(nil)

// NewRedisBroadcaster 构造桥（惰性拨号，不建立连接）。
func NewRedisBroadcaster(addr, password string) *RedisBroadcaster {
	return &RedisBroadcaster{
		client: redis.NewClient(&redis.Options{Addr: addr, Password: password}),
	}
}

func (r *RedisBroadcaster) Publish(msg Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal notify message: %w", err)
	}
	return r.client.Publish(context.Background(), notifyChannel, payload).Err()
}

// Subscribe 注册回调并同步等待 SUBSCRIBE 确认（确认前的消息不投递，
// 排除「先 Publish 后完成订阅」的丢消息窗口）；返回的 cancel 注销回调。
func (r *RedisBroadcaster) Subscribe(handler func(Message)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	pubsub := r.client.Subscribe(ctx, notifyChannel)
	// Receive 阻塞至订阅确认（subscribe ack）；失败仅记日志——本实例
	// 退化为不收远端消息（前端仍有 15s 轮询兜底），不影响其他实例。
	if _, err := pubsub.Receive(ctx); err != nil {
		log.Printf("[realtime] subscribe %s: %v", notifyChannel, err)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for m := range pubsub.Channel() {
			var msg Message
			if err := json.Unmarshal([]byte(m.Payload), &msg); err != nil {
				log.Printf("[realtime] decode notify message: %v", err)
				continue
			}
			handler(msg)
		}
	}()
	return func() {
		cancel()
		_ = pubsub.Close()
	}
}

// Close 释放底层 Redis 连接并等待投递 goroutine 退出（client.Close 会
// 一并关闭活跃 PubSub，Channel 循环随之结束；进程退出时调用）。
func (r *RedisBroadcaster) Close() error {
	err := r.client.Close()
	r.wg.Wait()
	return err
}
