package files

import (
	"errors"
	"log"

	"github.com/google/uuid"
	"gorm.io/gorm"
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
	// GetAny 返回 owner 名下的文件行（不论是否软删除）；不存在返回 ErrNotFound。
	GetAny(owner, id uuid.UUID) (File, error)
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
func restoreLogic(r trashRepo, owner, id uuid.UUID) (File, error) {
	f, err := r.GetAny(owner, id)
	if err != nil {
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
// 返回被删文件、待物理删除的 blob 与关联网页包的对象前缀
// （webpkg/<public_id>，供事务提交后清理，见 Store.Purge）。
func purgeLogic(r trashRepo, owner, id uuid.UUID) (purged []File, deleting []ObjectBlob, webpkgPrefixes []string, err error) {
	f, err := r.GetAny(owner, id)
	if err != nil {
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

// ListTrash 列出当前用户软删除的文件（仅顶层删除项：父目录也在回收站中的后代不重复出现）。
func (s *Store) ListTrash(owner uuid.UUID, limit int) ([]File, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []File
	err := s.db.Where(
		"owner_id = ? AND deleted_at IS NOT NULL AND is_root = false AND NOT EXISTS (SELECT 1 FROM files p WHERE p.id = files.parent_id AND p.deleted_at IS NOT NULL)",
		owner,
	).Order("deleted_at DESC, id").Limit(limit).Find(&out).Error
	return out, err
}

// Restore 恢复软删除文件；冲突时返回 ErrParentDeleted/ErrConflict（409），不静默改名。
func (s *Store) Restore(owner, id uuid.UUID) (File, error) {
	var f File
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		f, e = restoreLogic(&gormTrashRepo{tx: tx}, owner, id)
		return e
	})
	if err != nil {
		return File{}, err
	}
	return f, nil
}

// Purge 彻底删除（硬删除）软删除文件及其全部后代，并按引用计数处理 object_blobs。
// 返回被删除的文件与引用计数归零（待物理删除）的 blob。
// 事务提交后经注入的 webpkg 清理回调（SetWebpkgCleaner）删除关联网页包的
// webpkg/<public_id>/ 前缀对象（best-effort）；HTTP purge 与 janitor sweepTrash
// 均经本方法，两路清理统一生效。
func (s *Store) Purge(owner, id uuid.UUID) (purged []File, deleting []ObjectBlob, err error) {
	var webpkgPrefixes []string
	err = s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		purged, deleting, webpkgPrefixes, e = purgeLogic(&gormTrashRepo{tx: tx}, owner, id)
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
