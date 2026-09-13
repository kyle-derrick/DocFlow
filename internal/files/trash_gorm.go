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
		if err := g.tx.Model(&File{}).Where("parent_id IN ?", frontier).Pluck("id", &next).Error; err != nil {
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

func (g *gormTrashRepo) DeleteBlobRowIfUnreferenced(id uuid.UUID) (bool, error) {
	result := g.tx.Where("id = ? AND ref_count = 0 AND status = ?", id, BlobStatusDeleting).Delete(&ObjectBlob{})
	return result.RowsAffected > 0, result.Error
}
