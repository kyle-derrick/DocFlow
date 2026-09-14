package janitor

import (
	"github.com/docflow/docflow/internal/settings"
	"time"
)

type auditRepo interface {
	DeleteOldAuditLogs(time.Time, time.Duration) (int64, error)
}

func (j *Janitor) sweepAuditLogs(r auditRepo) {
	days := 90
	if j.settings != nil {
		if n, e := j.settings.GetInt(settings.KeyAuditRetentionDays); e == nil && n > 0 {
			days = n
		}
	}
	if n, e := r.DeleteOldAuditLogs(j.clock(), time.Duration(days)*24*time.Hour); e != nil {
		j.logf("janitor: delete audit logs: %v", e)
	} else if n > 0 {
		j.logf("janitor: removed %d audit logs", n)
	}
}
