package janitor

import (
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// activeStatuses / terminalStatuses 与 upload_sessions.status CHECK 约束一致。
var (
	activeStatuses   = []string{"uploading", "verifying", "scanning"}
	terminalStatuses = []string{"available", "quarantined", "failed"}
)

// GormRepo 是 Repo 的 GORM/PostgreSQL 实现。
type GormRepo struct{ db *gorm.DB }

func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

func (g *GormRepo) ExpiredActiveSessions(now time.Time, limit int) ([]upload.UploadSession, error) {
	var out []upload.UploadSession
	err := g.db.Where("expires_at < ? AND status IN ?", now, activeStatuses).
		Order("expires_at").Limit(limit).Find(&out).Error
	return out, err
}

func (g *GormRepo) FailSession(id uuid.UUID) (bool, error) {
	result := g.db.Model(&upload.UploadSession{}).
		Where("id = ? AND status IN ?", id, activeStatuses).
		Update("status", string(upload.StatusFailed))
	return result.RowsAffected > 0, result.Error
}

func (g *GormRepo) StaleTerminalSessions(now time.Time, retain time.Duration, limit int) ([]upload.UploadSession, error) {
	var out []upload.UploadSession
	err := g.db.Where("status IN ? AND COALESCE(completed_at, expires_at) < ?", terminalStatuses, now.Add(-retain)).
		Order("expires_at").Limit(limit).Find(&out).Error
	return out, err
}

func (g *GormRepo) DeleteSessions(ids []uuid.UUID) (int, error) {
	result := g.db.Where("id IN ?", ids).Delete(&upload.UploadSession{})
	return int(result.RowsAffected), result.Error
}

func (g *GormRepo) DeletingBlobs(limit int) ([]files.ObjectBlob, error) {
	var out []files.ObjectBlob
	err := g.db.Where("status = ? AND ref_count = 0", files.BlobStatusDeleting).
		Order("created_at").Limit(limit).Find(&out).Error
	return out, err
}

// DeleteBlobRechecked 复用 files 包的共享实现（单事务「行锁→复核→删对象→删行」），
// 与 files.PurgeBlobs、AddVersion 复活路径保持同一套串行化语义。
func (g *GormRepo) DeleteBlobRechecked(id uuid.UUID, deleteObject func(string) error) (bool, error) {
	return files.DeleteBlobRechecked(g.db, id, deleteObject)
}

func (g *GormRepo) ExpiredTrashTopLevel(now time.Time, retain time.Duration, limit int) ([]files.File, error) {
	var out []files.File
	err := g.db.Where(
		"deleted_at IS NOT NULL AND deleted_at < ? AND is_root = false AND NOT EXISTS (SELECT 1 FROM files p WHERE p.id = files.parent_id AND p.deleted_at IS NOT NULL)",
		now.Add(-retain),
	).Order("deleted_at").Limit(limit).Find(&out).Error
	return out, err
}

// DeleteExpiredSessions 删除 expires_at 或 revoked_at 早于阈值（now-7d）的
// 认证会话行，返回删除行数。原生 SQL 直查 sessions 表（模型在 auth 包，
// 此处不引模型，避免 janitor → auth 依赖）。
func (g *GormRepo) DeleteExpiredSessions(now time.Time) (int64, error) {
	cutoff := now.Add(-sessionRetention)
	result := g.db.Exec("DELETE FROM sessions WHERE expires_at < ? OR revoked_at < ?", cutoff, cutoff)
	return result.RowsAffected, result.Error
}
