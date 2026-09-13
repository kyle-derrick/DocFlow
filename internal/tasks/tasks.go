// Package tasks 提供可选的后台任务队列（设计文档：多实例横向扩展——
// 无本地状态、任意实例可处理）。默认 inprocess（进程内 goroutine 直接
// 执行，零外部依赖）；QUEUE_DRIVER=redis 时切换为 asynq（Redis 队列）：
// Web 实例入队，任意实例的 worker 消费。两种驱动共用同一组处理实现
// （TaskFunc），行为完全一致；单测不依赖真实 Redis。
package tasks

import (
	"context"
	"encoding/json"
	"log"

	"github.com/google/uuid"

	"github.com/docflow/docflow/internal/metrics"
)

// TaskType 任务类型常量：既是 asynq 的 typename，也是 Prometheus 指标的
// type 标签值（低基数，仅此两个）。
const (
	// TaskTypeCompleteUpload 上传补完（verify→scan→落库，幂等）。
	TaskTypeCompleteUpload = "task:complete-upload"
	// TaskTypeExtractWebpkg 网页包自动解包（zip 候选判定在处理侧）。
	TaskTypeExtractWebpkg = "task:extract-webpkg"
)

// Enqueuer 后台任务入队接口：Web 侧（tus 自动完成、上传完成钩子）调用。
// 实现决定任务在何处执行：
//   - InProcess：当前进程 goroutine 内联执行（默认驱动，行为与原内联
//     goroutine 一致）；
//   - RedisAsynq：写入 Redis（asynq），由任意实例的 asynq worker 执行。
type Enqueuer interface {
	// EnqueueCompleteUpload 入队「上传补完」；Complete 对终态幂等，
	// 重复投递安全。
	EnqueueCompleteUpload(sessionID uuid.UUID) error
	// EnqueueExtractWebpkg 入队「网页包自动解包」。
	EnqueueExtractWebpkg(fileID uuid.UUID) error
	// Driver 返回驱动名（inprocess|redis），供日志与指标标签使用。
	Driver() string
}

// TaskFunc 为单个任务的处理函数：inprocess 与 asynq mux 共用同一实现，
// 保证两种驱动行为一致。返回非 nil error 时：
//   - redis 驱动：asynq 按默认策略重试（瞬时故障自愈；终态错误由处理
//     函数自行归零返回避免无谓重试）；
//   - inprocess 驱动：仅记日志（与原内联 goroutine 一致，不重试）。
type TaskFunc func(ctx context.Context, id uuid.UUID) error

// completeUploadPayload / extractWebpkgPayload 为任务 JSON 载荷
// （asynq 载荷即其序列化形式；inprocess 不经序列化直接传参）。
type completeUploadPayload struct {
	SessionID uuid.UUID `json:"session_id"`
}

type extractWebpkgPayload struct {
	FileID uuid.UUID `json:"file_id"`
}

// marshalCompleteUploadPayload 序列化 complete-upload 载荷。
func marshalCompleteUploadPayload(sessionID uuid.UUID) ([]byte, error) {
	return json.Marshal(completeUploadPayload{SessionID: sessionID})
}

// marshalExtractWebpkgPayload 序列化 extract-webpkg 载荷。
func marshalExtractWebpkgPayload(fileID uuid.UUID) ([]byte, error) {
	return json.Marshal(extractWebpkgPayload{FileID: fileID})
}

// decodePayload 按任务类型反序列化载荷，返回目标 ID。
func decodePayload(taskType string, payload []byte) (uuid.UUID, error) {
	switch taskType {
	case TaskTypeCompleteUpload:
		var p completeUploadPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return uuid.Nil, err
		}
		return p.SessionID, nil
	case TaskTypeExtractWebpkg:
		var p extractWebpkgPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return uuid.Nil, err
		}
		return p.FileID, nil
	}
	return uuid.Nil, errUnknownTaskType(taskType)
}

// InProcess 进程内队列（默认驱动）：入队即启动 goroutine 执行任务，
// panic recover 防止拖垮进程；入队本身无外部依赖、不返回错误。
type InProcess struct {
	completeUpload TaskFunc
	extractWebpkg  TaskFunc
}

var _ Enqueuer = (*InProcess)(nil)

// NewInProcess 构造进程内驱动；两个处理函数与 redis 驱动共用
// （CompleteUploadHandler / ExtractWebpkgHandler 产出）。
func NewInProcess(completeUpload, extractWebpkg TaskFunc) *InProcess {
	return &InProcess{completeUpload: completeUpload, extractWebpkg: extractWebpkg}
}

func (p *InProcess) EnqueueCompleteUpload(sessionID uuid.UUID) error {
	metrics.IncQueueEnqueued(TaskTypeCompleteUpload, metrics.QueueDriverInProcess)
	p.goRun(TaskTypeCompleteUpload, p.completeUpload, sessionID)
	return nil
}

func (p *InProcess) EnqueueExtractWebpkg(fileID uuid.UUID) error {
	metrics.IncQueueEnqueued(TaskTypeExtractWebpkg, metrics.QueueDriverInProcess)
	p.goRun(TaskTypeExtractWebpkg, p.extractWebpkg, fileID)
	return nil
}

func (p *InProcess) Driver() string { return metrics.QueueDriverInProcess }

// goRun 在新 goroutine 中执行任务，统一 recover / 计数 / 日志。
func (p *InProcess) goRun(taskType string, fn TaskFunc, id uuid.UUID) {
	if fn == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				metrics.IncQueueProcessed(taskType, metrics.QueueStatusFailed)
				log.Printf("[tasks:%s] %s panicked: %v", p.Driver(), taskType, r)
			}
		}()
		if err := fn(context.Background(), id); err != nil {
			metrics.IncQueueProcessed(taskType, metrics.QueueStatusFailed)
			log.Printf("[tasks:%s] %s %s failed: %v", p.Driver(), taskType, id, err)
			return
		}
		metrics.IncQueueProcessed(taskType, metrics.QueueStatusSuccess)
	}()
}
