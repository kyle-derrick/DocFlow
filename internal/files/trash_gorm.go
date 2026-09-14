package files

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// gormTrashRepo 是 trashRepo 的 GORM/PostgreSQL 实现；
// 由 Store 在事务内构造，保证彻底删除的原子性。
type gormTrashRepo struct{ tx *gorm.DB }

func (g *gormTrashRepo) GetAny(owner, id uuid.UUID) (File, error) {
	var f File
	err := g.tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND owner_id = ?", id, owner).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return File{}, ErrNotFound
	}
	return f, err
}

func (g *gormTrashRepo) ParentAlive(parent uuid.UUID) (bool, error) {
	var count int64
	err := g.tx.Model(&File{}).Where("id = ? AND deleted_at IS NULL", parent).Count(&count).Error
	return count > 0, err
}

func (g *gormTrashRepo) HasActiveSibling(parent *uuid.UUID, name string, exclude uuid.UUID) (bool, error) {
	q := g.tx.Model(&File{}).Where("lower(name) = lower(?) AND deleted_at IS NULL AND id <> ?", name, exclude)
	if parent == nil {
		q = q.Where("parent_id IS NULL")
	} else {
		q = q.Where("parent_id = ?", *parent)
	}
	var count int64
	err := q.Count(&count).Error
	return count > 0, err
}

func (g *gormTrashRepo) Undelete(id uuid.UUID) error {
	err := g.tx.Model(&File{}).Where("id = ?", id).Update("deleted_at", nil).Error
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		// 并发窗口内的同名恢复：按名称冲突处理。
		return ErrConflict
	}
	return err
}

func (g *gormTrashRepo) Descendants(root uuid.UUID) ([]uuid.UUID, error) {
	all := []uuid.UUID{root}
	frontier := []uuid.UUID{root}
	seen := map[uuid.UUID]bool{root: true}
	for len(frontier) > 0 {
		var next []uuid.UUID
		// FOR UPDATE 锁定后代文件行：与 AddVersion 的 GetFileForUpdate 串行化，
		// 防止 Purge 期间并发追加版本/递增引用计数造成引用错乱。
		if err := g.tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Model(&File{}).Where("parent_id IN ?", frontier).Pluck("id", &next).Error; err != nil {
			return nil, err
		}
		frontier = frontier[:0]
		for _, id := range next {
			if !seen[id] {
				seen[id] = true
				all = append(all, id)
				frontier = append(frontier, id)
			}
		}
	}
	return all, nil
}

func (g *gormTrashRepo) FilesByIDs(ids []uuid.UUID) ([]File, error) {
	var out []File
	err := g.tx.Where("id IN ?", ids).Find(&out).Error
	return out, err
}

// WebpkgPublicIDs 读取挂在这些文件上的 web_packages.public_id
// （原生表查询，避免 files → webpkg 的反向依赖）。文件行仍在（级联删除前）时调用。
func (g *gormTrashRepo) WebpkgPublicIDs(fileIDs []uuid.UUID) ([]string, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	var pids []string
	err := g.tx.Table("web_packages").Where("file_id IN ?", fileIDs).Pluck("public_id", &pids).Error
	return pids, err
}

func (g *gormTrashRepo) BlobRefsForFiles(fileIDs []uuid.UUID) ([]BlobRef, error) {
	var refs []BlobRef
	err := g.tx.Model(&FileVersion{}).
		Select("object_blob_id AS blob_id, COUNT(*) AS count").
		Where("file_id IN ?", fileIDs).
		Group("object_blob_id").
		Scan(&refs).Error
	return refs, err
}

func (g *gormTrashRepo) DeleteVersions(fileIDs []uuid.UUID) error {
	return g.tx.Where("file_id IN ?", fileIDs).Delete(&FileVersion{}).Error
}

func (g *gormTrashRepo) ClearCurrentVersions(fileIDs []uuid.UUID) error {
	return g.tx.Model(&File{}).Where("id IN ? AND current_version_id IS NOT NULL", fileIDs).Update("current_version_id", nil).Error
}

func (g *gormTrashRepo) DeleteFiles(ids []uuid.UUID) error {
	return g.tx.Where("id IN ?", ids).Delete(&File{}).Error
}

func (g *gormTrashRepo) CountBlobRefs(blobID uuid.UUID) (int64, error) {
	var count int64
	err := g.tx.Model(&FileVersion{}).Where("object_blob_id = ?", blobID).Count(&count).Error
	return count, err
}

func (g *gormTrashRepo) GetBlob(id uuid.UUID) (ObjectBlob, error) {
	var b ObjectBlob
	err := g.tx.Where("id = ?", id).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ObjectBlob{}, ErrNotFound
	}
	return b, err
}

func (g *gormTrashRepo) DecrementBlobBy(id uuid.UUID, n int64) error {
	return g.tx.Model(&ObjectBlob{}).Where("id = ?", id).
		UpdateColumn("ref_count", gorm.Expr("GREATEST(ref_count - ?, 0)", n)).Error
}

func (g *gormTrashRepo) ZeroBlobAndMarkDeleting(id uuid.UUID) error {
	return g.tx.Model(&ObjectBlob{}).Where("id = ?", id).
		Updates(map[string]any{"ref_count": 0, "status": BlobStatusDeleting}).Error
}

// DeleteBlobRechecked 复用共享实现（见包级 DeleteBlobRechecked）。
func (g *gormTrashRepo) DeleteBlobRechecked(id uuid.UUID, deleteObject func(storageKey string) error) (bool, error) {
	return DeleteBlobRechecked(g.tx, id, deleteObject)
}

// DeleteBlobRechecked 单事务内「SELECT FOR UPDATE 锁行 → 复核 status='deleting'
// 且 ref_count=0 → 锁内删存储对象 → 删行」，失败回滚可安全重试。
// 复核不通过（行已被 AddVersion 复活为 available/仍被引用/不存在）直接跳过，
// 不触碰物理对象。files.PurgeBlobs 与 janitor 的 blob 回收共用本实现；
// 行锁与 AddVersion 复活路径（ResurrectBlob 的行更新取行锁）串行化，
// 先后取得锁的一方胜出，不会删除仍被引用（或已复活）的对象。
func DeleteBlobRechecked(db *gorm.DB, id uuid.UUID, deleteObject func(storageKey string) error) (bool, error) {
	var deleted bool
	err := db.Transaction(func(tx *gorm.DB) error {
		var blob ObjectBlob
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND ref_count = 0", id, BlobStatusDeleting).
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
		result := tx.Where("id = ?", blob.ID).Delete(&ObjectBlob{})
		deleted = result.RowsAffected > 0
		return result.Error
	})
	return deleted, err
}
