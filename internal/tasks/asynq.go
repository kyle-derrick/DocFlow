package tasks

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/docflow/docflow/internal/metrics"
)

// RedisAsynq 基于 asynq 的队列驱动（QUEUE_DRIVER=redis）。Client 与 Server
// 分开构造：Enqueuer 侧仅持 asynq.Client 负责入队（Web 请求路径使用）；
// 消费端由 NewAsynqServer + NewMux 在 main 中构造并启动（每个后端实例
// 既是生产者也是消费者，任意实例可处理任意任务）。
type RedisAsynq struct {
	client *asynq.Client
}

var _ Enqueuer = (*RedisAsynq)(nil)

// NewRedisAsynq 构造入队客户端（不建立长连接，入队时惰性拨号）；
// 启动连通性校验见 PingRedis。
func NewRedisAsynq(redisAddr, redisPassword string) *RedisAsynq {
	return &RedisAsynq{client: asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddr, Password: redisPassword})}
}

func (r *RedisAsynq) EnqueueCompleteUpload(sessionID uuid.UUID) error {
	payload, err := marshalCompleteUploadPayload(sessionID)
	if err != nil {
		return err
	}
	if _, err := r.client.Enqueue(asynq.NewTask(TaskTypeCompleteUpload, payload)); err != nil {
		return err
	}
	metrics.IncQueueEnqueued(TaskTypeCompleteUpload, metrics.QueueDriverRedis)
	return nil
}

func (r *RedisAsynq) EnqueueExtractWebpkg(fileID uuid.UUID) error {
	payload, err := marshalExtractWebpkgPayload(fileID)
	if err != nil {
		return err
	}
	if _, err := r.client.Enqueue(asynq.NewTask(TaskTypeExtractWebpkg, payload)); err != nil {
		return err
	}
	metrics.IncQueueEnqueued(TaskTypeExtractWebpkg, metrics.QueueDriverRedis)
	return nil
}

func (r *RedisAsynq) Driver() string { return metrics.QueueDriverRedis }

// Close 释放入队客户端连接（进程退出时由 main 调用）。
func (r *RedisAsynq) Close() error { return r.client.Close() }

// NewAsynqServer 构造 asynq 消费端（worker）：concurrency 为本实例并行
// 处理任务数（QUEUE_CONCURRENCY，默认 5）。由调用方负责 Start/Shutdown。
func NewAsynqServer(redisAddr, redisPassword string, concurrency int) *asynq.Server {
	if concurrency < 1 {
		concurrency = 1
	}
	return asynq.NewServer(asynq.RedisClientOpt{Addr: redisAddr, Password: redisPassword}, asynq.Config{Concurrency: concurrency})
}

// NewMux 注册两类任务的 asynq 处理器：与 InProcess 驱动共用同一 TaskFunc
// 实现，行为一致；处理结果计入 docflow_queue_processed_total{type,status}，
// 瞬时错误原样返回交由 asynq 重试。
func NewMux(completeUpload, extractWebpkg TaskFunc) *asynq.ServeMux {
	mux := asynq.NewServeMux()
	mux.HandleFunc(TaskTypeCompleteUpload, wrapTaskFunc(TaskTypeCompleteUpload, completeUpload))
	mux.HandleFunc(TaskTypeExtractWebpkg, wrapTaskFunc(TaskTypeExtractWebpkg, extractWebpkg))
	return mux
}

// wrapTaskFunc 适配 TaskFunc 为 asynq 处理器（反序列化载荷 + 指标计数）。
// 载荷非法属永久失败，包装 asynq.SkipRetry 避免无谓重试。
func wrapTaskFunc(taskType string, fn TaskFunc) func(context.Context, *asynq.Task) error {
	return func(ctx context.Context, t *asynq.Task) error {
		if fn == nil {
			return fmt.Errorf("no handler registered for %s: %w", taskType, asynq.SkipRetry)
		}
		id, err := decodePayload(taskType, t.Payload())
		if err != nil {
			metrics.IncQueueProcessed(taskType, metrics.QueueStatusFailed)
			log.Printf("[tasks:redis] %s invalid payload: %v", taskType, err)
			return fmt.Errorf("%s: %w", err.Error(), asynq.SkipRetry)
		}
		if err := fn(ctx, id); err != nil {
			metrics.IncQueueProcessed(taskType, metrics.QueueStatusFailed)
			return err
		}
		metrics.IncQueueProcessed(taskType, metrics.QueueStatusSuccess)
		return nil
	}
}

// PingRedis 在启用 redis 驱动时做启动连通性校验（fail fast）：
// 拨号 + PING，失败由调用方 fatal。
func PingRedis(ctx context.Context, redisAddr, redisPassword string) error {
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, Password: redisPassword, DialTimeout: 5 * time.Second})
	defer func() { _ = rdb.Close() }()
	return rdb.Ping(ctx).Err()
}
