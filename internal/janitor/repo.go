package janitor

import (
	"errors"
	"time"

	"github.com/docflow/docflow/internal/files"
	"github.com/docflow/docflow/internal/upload"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// DeleteBlobRechecked 单事务内「SELECT FOR UPDATE 锁行 → 复核 status='deleting'
// 且 ref_count=0 → 删存储对象 → 删行」，失败回滚可安全重试。
// 复核不通过（行已复活为 available/仍被引用/已删除）直接跳过，不触碰物理对象。
func (g *GormRepo) DeleteBlobRechecked(id uuid.UUID, deleteObject func(string) error) (bool, error) {
	var deleted bool
	err := g.db.Transaction(func(tx *gorm.DB) error {
		var blob files.ObjectBlob
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND ref_count = 0", id, files.BlobStatusDeleting).
			First(&blob).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := deleteObject(blob.StorageKey); err != nil {
			return err
		}
		result := tx.Where("id = ?", blob.ID).Delete(&files.ObjectBlob{})
		deleted = result.RowsAffected > 0
		return result.Error
	})
	return deleted, err
}

func (g *GormRepo) ExpiredTrashTopLevel(now time.Time, retain time.Duration, limit int) ([]files.File, error) {
	var out []files.File
	err := g.db.Where(
		"deleted_at IS NOT NULL AND deleted_at < ? AND is_root = false AND NOT EXISTS (SELECT 1 FROM files p WHERE p.id = files.parent_id AND p.deleted_at IS NOT NULL)",
		now.Add(-retain),
	).Order("deleted_at").Limit(limit).Find(&out).Error
	return out, err
}
