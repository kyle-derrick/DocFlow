package janitor

import (
	"github.com/docflow/docflow/internal/settings"
	"time"
)

type auditRepo interface {
	DeleteOldAuditLogs(time.Time, time.Duration) (int64, error)
}

// defaultAuditRetentionDays 为 settings 读取失败时的回退保留天数。
const defaultAuditRetentionDays = 90

// sweepAuditLogs 清理超过保留期（audit.retention_days）的审计日志；
// 0 = 永久保留（跳过清理）。janitor 周期运行，等效「每日删过期」且更及时。
func (j *Janitor) sweepAuditLogs(r auditRepo) {
	days := defaultAuditRetentionDays
	if j.settings != nil {
		if n, e := j.settings.GetInt(settings.KeyAuditRetentionDays); e == nil {
			if n <= 0 {
				return // 0 = 永久保留
			}
			days = n
		}
	}
	if n, e := r.DeleteOldAuditLogs(j.clock(), time.Duration(days)*24*time.Hour); e != nil {
		j.logf("janitor: delete audit logs: %v", e)
	} else if n > 0 {
		j.logf("janitor: removed %d audit logs older than %d days", n, days)
	}
}
