// Package janitor 提供后台定期清理：
//   - 过期且非终态的上传会话置 failed 并删除临时存储对象（tmp/*，幂等）；
//     终态会话超过保留期后删除行；
//   - 过期/已撤销超过 7 天的认证会话行（原生 SQL 删 sessions，不引 auth 模型）；
//   - 过期/已撤销超过 30 天的个人访问令牌行（原生 SQL 删 api_tokens）；
//   - 已读超过 90 天的通知行（原生 SQL 删 notifications，未读不受影响）；
//   - status='deleting' 且 ref_count=0 的 object_blobs 行（SELECT FOR UPDATE
//     行锁复核后）物理删除存储对象与行，与 AddVersion 的复活路径互斥；
//   - 回收站软删除超过 retention.trash_days 的文件复用 files.Store.Purge
//     彻底删除（含关联网页包对象的回调清理）。
//
// 每类清理写审计（janitor.upload / janitor.blob / janitor.trash，metadata 含数量；
// sessions 清理暂仅记日志）；单项错误只记录日志不中断循环。
// clock 与 interval 可注入以便测试。
package janitor

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docflow/docflow/internal/audit"
	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/settings"
	"github.com/docflow/docflow/internal/upload"
	"github.com/google/uuid"
)

// Repo 抽象 janitor 所需的数据访问；生产实现为 GormRepo（GORM/PostgreSQL），
// 测试可用内存实现（模式同 files 包的 repo 抽象）。
type Repo interface {
	// ExpiredActiveSessions 返回过期且非终态（uploading/verifying/scanning）的会话。
	ExpiredActiveSessions(now time.Time, limit int) ([]upload.UploadSession, error)
	// FailSession 将非终态会话置 failed（幂等，仅非终态时生效），返回是否生效。
	FailSession(id uuid.UUID) (bool, error)
	// StaleTerminalSessions 返回终态（available/quarantined/failed）且
	// 终态时间（COALESCE(completed_at, expires_at)）早于 now-retain 的会话。
	StaleTerminalSessions(now time.Time, retain time.Duration, limit int) ([]upload.UploadSession, error)
	// DeleteSessions 删除会话行，返回删除行数。
	DeleteSessions(ids []uuid.UUID) (int, error)
	// DeletingBlobs 返回 status='deleting' 且 ref_count=0 的 blob。
	DeletingBlobs(limit int) ([]files.ObjectBlob, error)
	// DeleteBlobRechecked 行锁复核后删 blob：SELECT FOR UPDATE 锁行并复核
	// status='deleting' 且 ref_count=0，复核通过后在锁内删除物理对象并删行，
	// 返回是否删除（行已复活/被引用/不存在时 false，不执行物理删除）。
	// 与 AddVersion 复活路径（ResurrectBlob 的行更新取行锁）串行化；
	// 生产实现复用 files 包的共享实现。
	DeleteBlobRechecked(id uuid.UUID, deleteObject func(string) error) (bool, error)
	// ExpiredTrashTopLevel 返回软删除超过 retain 的顶层回收站项
	//（父目录未同时处于软删除，避免与父目录重复处理），跨全部用户。
	ExpiredTrashTopLevel(now time.Time, retain time.Duration, limit int) ([]files.File, error)
	// DeleteExpiredSessions 删除 expires_at 或 revoked_at 早于阈值（7 天）的
	// 认证会话行（原生 SQL，不依赖 auth 包模型），返回删除行数。
	DeleteExpiredSessions(now time.Time) (int64, error)
	// DeleteExpiredTokens 删除 expires_at 或 revoked_at 早于阈值（30 天）的
	// 个人访问令牌行（api_tokens，原生 SQL），返回删除行数。
	DeleteExpiredTokens(now time.Time) (int64, error)
	// DeleteOldReadNotifications 删除已读超过保留期（90 天）的通知行
	//（notifications，原生 SQL，read_at 缺失时回退 created_at），返回删除行数。
	DeleteOldReadNotifications(now time.Time, retain time.Duration) (int64, error)
	// DeleteOrphanSearchDocs 删除无对应 files 行的全文索引行
	//（file_search_docs，原生 SQL），返回删除行数。正常路径由外键
	// ON DELETE CASCADE 级联承担，此为竞态/迁移遗留的兜底清理。
	DeleteOrphanSearchDocs() (int64, error)
}

// Purger 抽象回收站彻底删除能力；生产实现为 *files.Store。
type Purger interface {
	Purge(owner, id uuid.UUID) (purged []files.File, deleting []files.ObjectBlob, err error)
	PurgeBlobs(blobs []files.ObjectBlob, deleteObject func(string) error) error
}

// ObjectDeleter 抽象物理对象删除（幂等）；生产实现为 upload.Storage。
type ObjectDeleter interface {
	Delete(key string) error
}

// SettingsProvider 抽象设置热读取；生产实现为 *settings.Store。
type SettingsProvider interface {
	GetInt(key string) (int, error)
}

const (
	// DefaultInterval 默认清理周期。
	DefaultInterval = 10 * time.Minute
	// DefaultTrashDays 回收站默认保留天数（settings 可调）。
	DefaultTrashDays = 30
	// DefaultTerminalSessionRetention 终态上传会话的行保留期。
	DefaultTerminalSessionRetention = 24 * time.Hour
	// sessionRetention 认证会话行的清理阈值：expires_at/revoked_at 早于
	// now-7d 的行删除（DELETE FROM sessions WHERE expires_at < now()-interval '7 days'
	// OR revoked_at < now()-interval '7 days' 的参数化等价形式）。
	sessionRetention = 7 * 24 * time.Hour
	// tokenRetention 个人访问令牌行的清理阈值：expires_at/revoked_at 早于
	// now-30d 的 api_tokens 行删除（永久且未撤销的令牌不受影响）。
	tokenRetention = 30 * 24 * time.Hour
	// notificationRetention 已读通知的清理阈值：已读时间早于 now-90d 的
	// notifications 行删除（未读通知不受影响，避免丢失用户尚未处理的信息）。
	notificationRetention = 90 * 24 * time.Hour
	// defaultBatchLimit 单轮每类清理的批量上限，防止长事务。
	defaultBatchLimit = 500
)

// Janitor 是后台清理任务；须以 goroutine 运行 RunForever。
type Janitor struct {
	repo     Repo
	purger   Purger
	storage  ObjectDeleter
	settings SettingsProvider
	audit    audit.Recorder
	interval time.Duration
	clock    func() time.Time
	// trashDaysFallback 为 settings 读取失败时的回退值（默认 30）。
	trashDaysFallback int
	terminalRetention time.Duration
	batchLimit        int
	logf              func(format string, args ...any)
	runs              atomic.Int64
}

// New 构造 janitor；settingsProvider / recorder 允许为 nil（分别回退默认值与 Nop 审计）。
func New(repo Repo, purger Purger, storage ObjectDeleter, settingsProvider SettingsProvider, recorder audit.Recorder) *Janitor {
	if recorder == nil {
		recorder = audit.NopRecorder{}
	}
	return &Janitor{
		repo: repo, purger: purger, storage: storage, settings: settingsProvider, audit: recorder,
		interval: DefaultInterval, clock: time.Now, trashDaysFallback: DefaultTrashDays,
		terminalRetention: DefaultTerminalSessionRetention, batchLimit: defaultBatchLimit, logf: log.Printf,
	}
}

// SetInterval 设置清理周期（须 > 0 才生效）。
func (j *Janitor) SetInterval(d time.Duration) {
	if d > 0 {
		j.interval = d
	}
}

// SetClock 注入时钟（测试用）。
func (j *Janitor) SetClock(fn func() time.Time) {
	if fn != nil {
		j.clock = fn
	}
}

// SetTrashDaysFallback 设置 settings 读取失败时的回收站保留天数回退值。
func (j *Janitor) SetTrashDaysFallback(days int) {
	if days >= 1 {
		j.trashDaysFallback = days
	}
}

// RunForever 启动即执行一轮清理，随后按 interval 周期执行，直到 ctx 取消。
func (j *Janitor) RunForever(ctx context.Context) {
	j.RunOnce()
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.RunOnce()
		}
	}
}

// RunOnce 执行一轮全部清理；各类清理互相独立，单项错误记日志不中断。
func (j *Janitor) RunOnce() {
	j.runs.Add(1)
	j.sweepUploadSessions()
	j.sweepSessions()
	j.sweepTokens()
	j.sweepNotifications()
	j.sweepDeletingBlobs()
	j.sweepTrash()
	j.sweepSearchOrphans()
}

// sweepSearchOrphans 兜底清理孤儿全文索引行（file_search_docs 无对应
// files 行）。正常路径：文件硬删除时外键 ON DELETE CASCADE 已级联移除
// 索引行；此 sweep 覆盖级联删除失败/迁移前遗留等场景。软删文件的索引
// 保留（恢复后可继续检索），查询侧经 JOIN files ... deleted_at IS NULL
// 排除（见 internal/search）。纯行删除，暂只记日志。
func (j *Janitor) sweepSearchOrphans() {
	n, err := j.repo.DeleteOrphanSearchDocs()
	if err != nil {
		j.logf("janitor: delete orphan search docs: %v", err)
		return
	}
	if n > 0 {
		j.logf("janitor: removed %d orphan search docs", n)
	}
}

// sweepNotifications 清理已读超过保留期（90 天）的通知行。
// 纯行删除（无关联物理对象），暂只记日志（同 sweepSessions/sweepTokens 模式）。
func (j *Janitor) sweepNotifications() {
	n, err := j.repo.DeleteOldReadNotifications(j.clock(), notificationRetention)
	if err != nil {
		j.logf("janitor: delete old read notifications: %v", err)
		return
	}
	if n > 0 {
		j.logf("janitor: removed %d old read notifications", n)
	}
}

// sweepSessions 清理过期/已撤销超过保留期（7 天）的认证会话行。
// 纯行删除（无关联物理对象）；internal/audit 本批次不可改（无 janitor.session
// 动作常量），暂只记日志，后续批次可补审计动作。
func (j *Janitor) sweepSessions() {
	n, err := j.repo.DeleteExpiredSessions(j.clock())
	if err != nil {
		j.logf("janitor: delete expired sessions: %v", err)
		return
	}
	if n > 0 {
		j.logf("janitor: removed %d expired sessions", n)
	}
}

// sweepTokens 清理过期/已撤销超过保留期（30 天）的个人访问令牌行
// （复用 sweepSessions 模式：纯行删除，暂只记日志）。
// expires_at 为 NULL（永久）且未撤销的令牌不会被删除。
func (j *Janitor) sweepTokens() {
	n, err := j.repo.DeleteExpiredTokens(j.clock())
	if err != nil {
		j.logf("janitor: delete expired api tokens: %v", err)
		return
	}
	if n > 0 {
		j.logf("janitor: removed %d expired api tokens", n)
	}
}

// trashDays 热读取 retention.trash_days；读取失败回退默认值。
func (j *Janitor) trashDays() int {
	if j.settings != nil {
		if days, err := j.settings.GetInt(settings.KeyRetentionTrashDays); err == nil && days >= 1 {
			return days
		} else if err != nil {
			j.logf("janitor: read retention.trash_days failed: %v (fallback %d days)", err, j.trashDaysFallback)
		}
	}
	return j.trashDaysFallback
}

// sweepUploadSessions 清理上传会话：过期非终态 → failed + 删 tmp/* 临时对象；
// 终态超保留期 → 删行。
func (j *Janitor) sweepUploadSessions() {
	now := j.clock()
	failed := 0
	if sessions, err := j.repo.ExpiredActiveSessions(now, j.batchLimit); err != nil {
		j.logf("janitor: list expired upload sessions: %v", err)
	} else {
		for _, s := range sessions {
			updated, err := j.repo.FailSession(s.ID)
			if err != nil {
				j.logf("janitor: fail upload session %s: %v", s.ID, err)
				continue
			}
			if !updated {
				continue // 并发窗口内已流转（如恰好 Complete），幂等跳过
			}
			failed++
			// 只清理临时区对象；终态对象（objects/*）由 blob 引用计数管理。
			if strings.HasPrefix(s.StorageKey, "tmp/") {
				if err := j.storage.Delete(s.StorageKey); err != nil {
					j.logf("janitor: delete tmp object %s: %v", s.StorageKey, err)
				}
			}
		}
	}
	removed := 0
	if stale, err := j.repo.StaleTerminalSessions(now, j.terminalRetention, j.batchLimit); err != nil {
		j.logf("janitor: list stale terminal upload sessions: %v", err)
	} else if len(stale) > 0 {
		ids := make([]uuid.UUID, 0, len(stale))
		for _, s := range stale {
			ids = append(ids, s.ID)
		}
		n, err := j.repo.DeleteSessions(ids)
		if err != nil {
			j.logf("janitor: delete stale upload sessions: %v", err)
		}
		removed = n
	}
	if failed+removed > 0 {
		j.record(audit.ActionJanitorUpload, audit.ResourceUpload, "", map[string]any{"failed": failed, "rows_removed": removed})
	}
}

// sweepDeletingBlobs 回收 deleting 且零引用的 blob：行锁复核后删对象删行。
func (j *Janitor) sweepDeletingBlobs() {
	blobs, err := j.repo.DeletingBlobs(j.batchLimit)
	if err != nil {
		j.logf("janitor: list deleting blobs: %v", err)
		return
	}
	deleted := 0
	for _, b := range blobs {
		// 行锁复核：锁内与 AddVersion 的复活路径串行化，复活/仍被引用则跳过。
		ok, err := j.repo.DeleteBlobRechecked(b.ID, j.storage.Delete)
		if err != nil {
			j.logf("janitor: delete blob %s: %v", b.ID, err)
			continue
		}
		if ok {
			deleted++
		}
	}
	if deleted > 0 {
		j.record(audit.ActionJanitorBlob, audit.ResourceBlob, "", map[string]any{"deleted": deleted})
	}
}

// sweepTrash 彻底删除回收站超期项（复用 files.Store.Purge/PurgeBlobs 语义）。
func (j *Janitor) sweepTrash() {
	days := j.trashDays()
	retain := time.Duration(days) * 24 * time.Hour
	items, err := j.repo.ExpiredTrashTopLevel(j.clock(), retain, j.batchLimit)
	if err != nil {
		j.logf("janitor: list expired trash: %v", err)
		return
	}
	purged := 0
	for _, f := range items {
		_, deleting, err := j.purger.Purge(f.OwnerID, f.ID)
		if err != nil {
			// 竞态（用户恰好恢复/手动清空）或存储故障：记日志，下一轮重扫。
			j.logf("janitor: purge trash %s: %v", f.ID, err)
			continue
		}
		purged++
		if len(deleting) > 0 {
			if err := j.purger.PurgeBlobs(deleting, j.storage.Delete); err != nil {
				j.logf("janitor: purge blobs for trash %s: %v", f.ID, err)
			}
		}
	}
	if purged > 0 {
		j.record(audit.ActionJanitorTrash, audit.ResourceFile, "", map[string]any{"files": purged, "trash_days": days})
	}
}

func (j *Janitor) record(action, resourceType, resourceID string, metadata map[string]any) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		raw = []byte("{}")
	}
	_ = j.audit.Record(audit.Entry{Action: action, ResourceType: resourceType, ResourceID: resourceID, Status: audit.StatusSuccess, Metadata: string(raw)})
}
