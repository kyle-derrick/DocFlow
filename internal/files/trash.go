package files

import (
	"errors"
	"log"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrParentDeleted 恢复时原父目录仍处于软删除状态。
	ErrParentDeleted = errors.New("parent folder is deleted")
	// ErrNotDeleted 彻底删除只允许作用于软删除的文件。
	ErrNotDeleted = errors.New("file is not deleted")
)

// trashRepo 抽象回收站操作所需的数据访问。
// 生产实现为 gormTrashRepo（在事务内执行），测试可用内存实现
// 验证恢复冲突与彻底删除的引用计数语义。
type trashRepo interface {
	// GetAny 返回文件行（不论归属与是否软删除）；不存在返回 ErrNotFound。
	// 用户授权由调用方经 authorize 回调在事务内完成（个人 owner / 团队
	// CanWrite·CanDelete，见 Store.Restore/Purge）。
	GetAny(id uuid.UUID) (File, error)
	// ParentAlive 判断父目录是否存在且未软删除。
	ParentAlive(parent uuid.UUID) (bool, error)
	// HasActiveSibling 判断同一父目录下是否存在同名活跃文件（排除 exclude）。
	HasActiveSibling(parent *uuid.UUID, name string, exclude uuid.UUID) (bool, error)
	// Undelete 清除软删除标记；并发下唯一索引冲突返回 ErrConflict。
	Undelete(id uuid.UUID) error
	// Descendants 返回 root 及其全部后代（含软删除）的 ID；
	// 生产实现以 FOR UPDATE 锁定后代行，与 AddVersion 的 GetFileForUpdate
	// 串行化（防止 Purge 期间对后代追加版本导致引用计数错乱）。
	Descendants(root uuid.UUID) ([]uuid.UUID, error)
	// FilesByIDs 返回指定 ID 的文件行。
	FilesByIDs(ids []uuid.UUID) ([]File, error)
	// WebpkgPublicIDs 返回挂在这些文件上的 web_packages.public_id
	//（须在删除文件行之前读取：web_packages.file_id 对 files.id 级联删除）。
	WebpkgPublicIDs(fileIDs []uuid.UUID) ([]string, error)
	// BlobRefsForFiles 返回这些文件通过 file_versions 引用的 blob 及引用条数
	//（同一文件的多个版本可指向同一 blob，需按版本数递减计数）。
	BlobRefsForFiles(fileIDs []uuid.UUID) ([]BlobRef, error)
	// ClearCurrentVersions 清空这些文件的 current_version_id
	//（须先于 DeleteVersions：files.current_version_id 外键指向 file_versions）。
	ClearCurrentVersions(fileIDs []uuid.UUID) error
	// DeleteVersions 删除这些文件的 file_versions。
	DeleteVersions(fileIDs []uuid.UUID) error
	// DeleteFiles 硬删除这些文件行。
	DeleteFiles(ids []uuid.UUID) error
	// CountBlobRefs 统计仍引用该 blob 的 file_versions 数量。
	CountBlobRefs(blobID uuid.UUID) (int64, error)
	// GetBlob 返回 blob；不存在返回 ErrNotFound。
	GetBlob(id uuid.UUID) (ObjectBlob, error)
	// DecrementBlobBy 将引用计数递减 n。
	DecrementBlobBy(id uuid.UUID, n int64) error
	// ZeroBlobAndMarkDeleting 将引用计数清零并标记 deleting（仅在没有其他引用时调用）。
	ZeroBlobAndMarkDeleting(id uuid.UUID) error
	// DeleteBlobRechecked 单事务内「SELECT FOR UPDATE 锁行 → 复核
	// status='deleting' 且 ref_count=0 → 锁内删物理对象 → 删行」，返回是否删除。
	// 复核不通过（已被 AddVersion 复活/仍被引用/不存在）返回 false 且不删对象，
	// 与 ResurrectBlob 的行更新取行锁互斥。
	DeleteBlobRechecked(id uuid.UUID, deleteObject func(storageKey string) error) (bool, error)
}

// BlobRef 表示待处理 blob 及本次删除移除的引用条数。
type BlobRef struct {
	BlobID uuid.UUID
	Count  int64
}

// restoreLogic 恢复软删除文件：
// 原父目录已被删除或名称冲突时返回对应错误，不静默改名/移动。
// authorize 在行锁内复核用户对该文件的权限（个人 owner / 团队 CanWrite）。
func restoreLogic(r trashRepo, id uuid.UUID, authorize func(File) error) (File, error) {
	f, err := r.GetAny(id)
	if err != nil {
		return File{}, err
	}
	if err := authorize(f); err != nil {
		return File{}, err
	}
	if f.DeletedAt == nil {
		return File{}, ErrNotDeleted
	}
	if f.IsRoot {
		return File{}, ErrRoot
	}
	if f.ParentID != nil {
		alive, err := r.ParentAlive(*f.ParentID)
		if err != nil {
			return File{}, err
		}
		if !alive {
			return File{}, ErrParentDeleted
		}
	}
	conflict, err := r.HasActiveSibling(f.ParentID, f.Name, f.ID)
	if err != nil {
		return File{}, err
	}
	if conflict {
		return File{}, ErrConflict
	}
	if err := r.Undelete(f.ID); err != nil {
		return File{}, err
	}
	f.DeletedAt = nil
	return f, nil
}

// purgeLogic 彻底删除一个软删除的文件（硬删除）：
// 处理其全部后代、删除 file_versions 引用并递减 object_blobs 引用计数；
// 引用计数归零的 blob 标记 deleting 交由调用方删除物理对象，
// 仍被引用的对象只递减计数、绝不删除。根目录不可删除。
// authorize 在行锁内复核用户对该文件的权限（个人 owner / 团队 CanDelete）。
// 返回被删文件、待物理删除的 blob 与关联网页包的对象前缀
// （webpkg/<public_id>，供事务提交后清理，见 Store.Purge）。
func purgeLogic(r trashRepo, id uuid.UUID, authorize func(File) error) (purged []File, deleting []ObjectBlob, webpkgPrefixes []string, err error) {
	f, err := r.GetAny(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := authorize(f); err != nil {
		return nil, nil, nil, err
	}
	if f.DeletedAt == nil {
		return nil, nil, nil, ErrNotDeleted
	}
	if f.IsRoot {
		return nil, nil, nil, ErrRoot
	}
	ids, err := r.Descendants(f.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	// purge 前读取关联网页包前缀：web_packages.file_id 对 files.id 级联删除，
	// 文件行删除后 public_id 不可再查。
	pids, err := r.WebpkgPublicIDs(ids)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, pid := range pids {
		webpkgPrefixes = append(webpkgPrefixes, "webpkg/"+pid)
	}
	if purged, err = r.FilesByIDs(ids); err != nil {
		return nil, nil, nil, err
	}
	blobIDs, err := r.BlobRefsForFiles(ids)
	if err != nil {
		return nil, nil, nil, err
	}
	// FK 顺序：先清空 files.current_version_id（外键指向 file_versions），
	// 再删 file_versions，最后删文件行。
	if err = r.ClearCurrentVersions(ids); err != nil {
		return nil, nil, nil, err
	}
	if err = r.DeleteVersions(ids); err != nil {
		return nil, nil, nil, err
	}
	if err = r.DeleteFiles(ids); err != nil {
		return nil, nil, nil, err
	}
	for _, ref := range blobIDs {
		blob, err := r.GetBlob(ref.BlobID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, nil, nil, err
		}
		remaining, err := r.CountBlobRefs(ref.BlobID)
		if err != nil {
			return nil, nil, nil, err
		}
		if remaining > 0 {
			// 仍有其他版本引用：按本次删除的版本数递减计数，不删除对象。
			if err = r.DecrementBlobBy(ref.BlobID, ref.Count); err != nil {
				return nil, nil, nil, err
			}
			continue
		}
		// 引用计数归零：清零计数并标记 deleting，物理对象由调用方删除。
		if err = r.ZeroBlobAndMarkDeleting(ref.BlobID); err != nil {
			return nil, nil, nil, err
		}
		blob.RefCount = 0
		blob.Status = BlobStatusDeleting
		deleting = append(deleting, blob)
	}
	return purged, deleting, webpkgPrefixes, nil
}

// purgeBlobsLogic 删除已标记 deleting 且引用计数归零的 blob 的物理对象与行记录。
// 逐 blob 独立事务「SELECT FOR UPDATE 复核 status='deleting' 且 ref_count=0 →
// 锁内删物理对象 → 删行」：复核不过（期间被 AddVersion 复活/仍被引用）直接跳过，
// 绝不删除仍可能被引用的对象；单个 blob 失败回滚可安全重试。
func purgeBlobsLogic(r trashRepo, blobs []ObjectBlob, deleteObject func(storageKey string) error) error {
	for _, b := range blobs {
		if b.Status != BlobStatusDeleting || b.RefCount != 0 {
			continue
		}
		if _, err := r.DeleteBlobRechecked(b.ID, deleteObject); err != nil {
			return err
		}
	}
	return nil
}

// ListTrash 列出用户默认空间的软删除文件。
func (s *Store) ListTrash(user uuid.UUID, limit int) ([]File, error) {
	defaultSpace := s.defaultSpaceID(user)
	return s.ListTrashSpace(user, defaultSpace, limit)
}

// ListTrashSpace 列出指定空间（统一空间模型）的顶层软删除项：先验证
// 用户对空间的读权限（文件行 owner 或在册成员），再按 space_id 限定。
func (s *Store) ListTrashSpace(user, spaceID uuid.UUID, limit int) ([]File, error) {
	if limit <= 0 {
		limit = 100
	}
	if spaceID == uuid.Nil {
		return nil, ErrNotFound
	}
	// 授权：文件行 owner 命中即可；否则要求空间在册成员。
	var ownerHit int64
	if err := s.db.Model(&File{}).
		Where("space_id = ? AND owner_id = ? AND deleted_at IS NOT NULL AND is_root = false", spaceID, user).
		Limit(1).Count(&ownerHit).Error; err != nil {
		return nil, err
	}
	if ownerHit == 0 {
		if s.spaceReader == nil {
			return nil, ErrForbidden
		}
		ok, err := s.spaceReader(user, spaceID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrForbidden
		}
	}
	q := s.db.Where("deleted_at IS NOT NULL AND is_root = false AND NOT EXISTS (SELECT 1 FROM files p WHERE p.id = files.parent_id AND p.deleted_at IS NOT NULL)")
	q = q.Where("space_id = ?", spaceID)
	var out []File
	err := q.Order("deleted_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

// Restore 恢复软删除文件；冲突时返回 ErrParentDeleted/ErrConflict（409），不静默改名。
// 权限：文件行 owner 或空间成员写权限（authorizeFileWrite）。
func (s *Store) Restore(user, id uuid.UUID) (File, error) {
	authorize := func(f File) error { return authorizeFileWrite(f, user, s.spaceWriter, s.acl) }
	var f File
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		f, e = restoreLogic(&gormTrashRepo{tx: tx}, id, authorize)
		return e
	})
	if err != nil {
		return File{}, err
	}
	return f, nil
}

// Purge 彻底删除（硬删除）软删除文件及其全部后代，并按引用计数处理 object_blobs。
// 权限：文件行 owner 或空间成员 CanDelete（authorizeSpaceDelete）。
// 返回被删除的文件与引用计数归零（待物理删除）的 blob。
// 事务提交后经注入的 webpkg 清理回调（SetWebpkgCleaner）删除关联网页包的
// webpkg/<public_id>/ 前缀对象（best-effort）；HTTP purge 与 janitor sweepTrash
// 均经本方法，两路清理统一生效（janitor 走不做用户判定的 PurgeSystem）。
func (s *Store) Purge(user, id uuid.UUID) (purged []File, deleting []ObjectBlob, err error) {
	authorize := func(f File) error { return authorizeSpaceDelete(f, user, s.spaceDeleter, s.acl) }
	return s.purgeWithAuthorize(id, authorize)
}

// PurgeSystem 系统级彻底删除（janitor 回收站超期清理）：不做用户权限判定。
func (s *Store) PurgeSystem(id uuid.UUID) (purged []File, deleting []ObjectBlob, err error) {
	return s.purgeWithAuthorize(id, func(File) error { return nil })
}

func (s *Store) purgeWithAuthorize(id uuid.UUID, authorize func(File) error) (purged []File, deleting []ObjectBlob, err error) {
	var webpkgPrefixes []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		purged, deleting, webpkgPrefixes, e = purgeLogic(&gormTrashRepo{tx: tx}, id, authorize)
		return e
	})
	if err != nil {
		return nil, nil, err
	}
	s.cleanupWebpkgObjects(webpkgPrefixes)
	return purged, deleting, nil
}

// cleanupWebpkgObjects best-effort 清理网页包对象前缀；失败仅记日志
// （孤儿前缀不影响数据一致性，可由存储巡检兜底）。
func (s *Store) cleanupWebpkgObjects(prefixes []string) {
	if s.webpkgCleaner == nil {
		return
	}
	for _, prefix := range prefixes {
		if err := s.webpkgCleaner(prefix); err != nil {
			log.Printf("[files] cleanup webpkg objects %s: %v", prefix, err)
		}
	}
}

// PurgeBlobs 删除已标记 deleting 的 blob 对应的物理对象并删除其行记录。
func (s *Store) PurgeBlobs(blobs []ObjectBlob, deleteObject func(storageKey string) error) error {
	return purgeBlobsLogic(&gormTrashRepo{tx: s.db}, blobs, deleteObject)
}

// PurgeSpace 整空间彻底删除（管理端「已解散 → 彻底删除」）：在同一事务内
// 物理删除该空间全部文件（含根目录与软删除项）、file_versions，并按引用
// 计数处理 object_blobs；返回待物理删除的 blob（调用方经 PurgeBlobs 删除，
// janitor sweepDeletingBlobs 兜底重试）。与 Purge 的差异：按 space_id 全集
// 处理、不做用户授权、允许根目录；关联网页包对象前缀同样 best-effort 清理。
func (s *Store) PurgeSpace(spaceID uuid.UUID) (deleting []ObjectBlob, err error) {
	var webpkgPrefixes []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		r := &gormTrashRepo{tx: tx}
		var ids []uuid.UUID
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Model(&File{}).Where("space_id = ?", spaceID).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		pids, err := r.WebpkgPublicIDs(ids)
		if err != nil {
			return err
		}
		for _, pid := range pids {
			webpkgPrefixes = append(webpkgPrefixes, "webpkg/"+pid)
		}
		refs, err := r.BlobRefsForFiles(ids)
		if err != nil {
			return err
		}
		// FK 顺序同 purgeLogic：先清 current_version_id → 删版本 → 删文件行。
		if err := r.ClearCurrentVersions(ids); err != nil {
			return err
		}
		if err := r.DeleteVersions(ids); err != nil {
			return err
		}
		if err := r.DeleteFiles(ids); err != nil {
			return err
		}
		for _, ref := range refs {
			blob, err := r.GetBlob(ref.BlobID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return err
			}
			remaining, err := r.CountBlobRefs(ref.BlobID)
			if err != nil {
				return err
			}
			if remaining > 0 {
				if err := r.DecrementBlobBy(ref.BlobID, ref.Count); err != nil {
					return err
				}
				continue
			}
			if err := r.ZeroBlobAndMarkDeleting(ref.BlobID); err != nil {
				return err
			}
			blob.RefCount = 0
			blob.Status = BlobStatusDeleting
			deleting = append(deleting, blob)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.cleanupWebpkgObjects(webpkgPrefixes)
	return deleting, nil
}
