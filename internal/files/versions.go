package files

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrInvalidTarget 覆盖目标不是文件（如目录）。
	ErrInvalidTarget = errors.New("target must be a file")
	// ErrNotFileVersion 版本不属于该文件，不允许指向。
	ErrNotFileVersion = errors.New("version does not belong to this file")
	// ErrBlobUnavailable 同 sha256 的 blob 处于不可复用状态（如 quarantined），
	// 且 sha256 唯一约束阻止新建行，上传须失败。
	ErrBlobUnavailable = errors.New("blob content is unavailable")
)

// defaultMaxVersions 每文件默认保留的版本数上限（可经 SetMaxVersions / MAX_VERSIONS_PER_FILE 覆盖）。
const defaultMaxVersions = 5

// VersionDetail 是版本列表项：FileVersion 不可变字段 + 关联 blob 的实时状态摘要。
type VersionDetail struct {
	ID        uuid.UUID `gorm:"column:id" json:"id"`
	Version   int       `gorm:"column:version" json:"version"`
	Size      int64     `gorm:"column:size" json:"size"`
	SHA256    string    `gorm:"column:sha256" json:"sha256"`
	MimeType  string    `gorm:"column:mime_type" json:"mime_type"`
	Status    string    `gorm:"column:status" json:"status"`
	Comment   *string   `gorm:"column:comment" json:"comment"`
	UserID    uuid.UUID `gorm:"column:user_id" json:"user_id"`
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

// versionsRepo 抽象版本管理所需的数据访问。
// 生产实现为 gormVersionsRepo（在事务内执行，文件行 FOR UPDATE 锁串行化版本号分配）；
// 测试可用内存实现验证引用计数与保留策略语义（模式同 trashRepo）。
type versionsRepo interface {
	// GetFileForUpdate 锁定并返回未删除的 type='file' 行；不存在返回 ErrNotFound。
	GetFileForUpdate(id uuid.UUID) (File, error)
	// GetBlobBySHA 按 sha256 返回 blob（不限状态，供复活判定）；不存在返回 ErrNotFound。
	GetBlobBySHA(sha256 string) (ObjectBlob, error)
	// IncrementBlobRef 引用计数 +1。
	IncrementBlobRef(id uuid.UUID) error
	// ResurrectBlob 将 deleting 且 ref_count=0 的 blob 复活为 available/ref_count=1；
	// 返回 false 表示已被 janitor 删除（应走新建分支）。行更新即取行锁，
	// 与 janitor 的“锁行→复核→删对象→删行”串行化。
	ResurrectBlob(id uuid.UUID) (bool, error)
	// CreateBlob 新建 blob 行（调用方负责设置 RefCount/Status）。
	CreateBlob(b ObjectBlob) error
	// NextVersionNumber 返回 MAX(version)+1（无版本时为 1）。
	NextVersionNumber(fileID uuid.UUID) (int, error)
	// CreateVersion 新建版本行；(file_id, version) 唯一冲突由底层报错。
	CreateVersion(v FileVersion) error
	// SetCurrentVersion 更新 files.current_version_id。
	SetCurrentVersion(fileID, versionID uuid.UUID) error
	// GetVersion 返回版本行；不存在返回 ErrNotFound。
	GetVersion(id uuid.UUID) (FileVersion, error)
	// ListVersionsDesc 按版本号倒序返回该文件全部版本。
	ListVersionsDesc(fileID uuid.UUID) ([]FileVersion, error)
	// DeleteVersion 删除版本行（仅裁剪路径使用；版本本身不可变）。
	DeleteVersion(id uuid.UUID) error
	// DecrementBlobRef 引用计数 -1（下限 0）。
	DecrementBlobRef(id uuid.UUID) error
	// MarkBlobDeletingIfZero 在引用计数已归零时置 status=deleting，返回是否生效。
	MarkBlobDeletingIfZero(id uuid.UUID) (bool, error)
}

// addVersionLogic 向文件追加新版本（事务内的纯逻辑）：
//   - 同 sha256 的 available blob 复用（ref_count+1）；
//   - 同 sha256 的 blob 处于 deleting 且 ref_count=0（裁剪后待 janitor 回收）时复活该行
//     （sha256 唯一约束阻止另起新行；复活经行更新取行锁，与 janitor 串行化）；
//   - 未命中则新建 blob（ref_count=1）。
//
// 版本号取 MAX(version)+1（文件行锁已串行化并发追加，(file_id, version) 唯一约束兜底）；
// 最后更新 current_version_id。返回新版本与是否新建了 blob
// （复用/复活时调用方应清理冗余的物理对象）。
func addVersionLogic(r versionsRepo, fileID uuid.UUID, storageKey, sha256 string, size int64, mimeType string, userID uuid.UUID) (FileVersion, bool, error) {
	if _, err := r.GetFileForUpdate(fileID); err != nil {
		return FileVersion{}, false, err
	}
	blob, err := r.GetBlobBySHA(sha256)
	newBlob := false
	switch {
	case err == nil && blob.Status == BlobStatusAvailable:
		if err := r.IncrementBlobRef(blob.ID); err != nil {
			return FileVersion{}, false, err
		}
	case err == nil && blob.Status == BlobStatusDeleting && blob.RefCount == 0:
		// 裁剪遗留的待回收行：复活复用；若 janitor 恰好已删行则退回新建。
		resurrected, rerr := r.ResurrectBlob(blob.ID)
		if rerr != nil {
			return FileVersion{}, false, rerr
		}
		if !resurrected {
			blob = ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: BlobStatusAvailable}
			if cerr := r.CreateBlob(blob); cerr != nil {
				return FileVersion{}, false, cerr
			}
			newBlob = true
		}
	case errors.Is(err, ErrNotFound):
		// 内容去重未命中：新建物理对象行。
		blob = ObjectBlob{ID: uuid.New(), SHA256: sha256, StorageKey: storageKey, Size: size, MimeType: mimeType, RefCount: 1, Status: BlobStatusAvailable}
		if cerr := r.CreateBlob(blob); cerr != nil {
			return FileVersion{}, false, cerr
		}
		newBlob = true
	case err == nil:
		// quarantined/failed 等状态：不可复用，且 sha256 唯一约束阻止新建。
		return FileVersion{}, false, ErrBlobUnavailable
	default:
		return FileVersion{}, false, err
	}
	next, err := r.NextVersionNumber(fileID)
	if err != nil {
		return FileVersion{}, false, err
	}
	version := FileVersion{ID: uuid.New(), FileID: fileID, Version: next, ObjectBlobID: blob.ID, ContentSHA256: sha256, Size: size, UserID: userID}
	if err := r.CreateVersion(version); err != nil {
		return FileVersion{}, false, err
	}
	if err := r.SetCurrentVersion(fileID, version.ID); err != nil {
		return FileVersion{}, false, err
	}
	return version, newBlob, nil
}

// setCurrentVersionLogic 将 current_version_id 指向该文件的既有版本（回滚）。
// 仅允许指向本文件版本（否则 ErrNotFileVersion）；版本行不可变，只改指针。
func setCurrentVersionLogic(r versionsRepo, fileID, versionID uuid.UUID) (FileVersion, error) {
	if _, err := r.GetFileForUpdate(fileID); err != nil {
		return FileVersion{}, err
	}
	version, err := r.GetVersion(versionID)
	if err != nil {
		return FileVersion{}, err
	}
	if version.FileID != fileID {
		return FileVersion{}, ErrNotFileVersion
	}
	if err := r.SetCurrentVersion(fileID, versionID); err != nil {
		return FileVersion{}, err
	}
	return version, nil
}

// pruneVersionsLogic 保留最新 keep 个版本，裁掉其余（current_version 指向的版本永不移除）。
// 被裁版本解除 blob 引用（ref_count-1）；计数归零的 blob 置 status=deleting——
// 物理删除延后：本函数只标状态，由后台清理（janitor）对每行在单事务内执行
// “行锁→复核 status=deleting 且 ref_count=0→删存储对象→删行”（复用 trash.go
// PurgeBlobs 的两步语义，失败可安全重试）。行锁与 AddVersion 的复活路径互斥，
// 先后取得锁的一方胜出，不会删除仍被引用（或已复活）的对象。
// 仍被其他版本/文件引用的对象只递减计数，绝不标记或删除。
// keep<=0 时仅保护 current。返回被裁版本数。
func pruneVersionsLogic(r versionsRepo, fileID uuid.UUID, keep int) (int, error) {
	f, err := r.GetFileForUpdate(fileID)
	if err != nil {
		return 0, err
	}
	versions, err := r.ListVersionsDesc(fileID)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for i, v := range versions {
		if i < keep {
			continue // 最新 keep 个保留
		}
		if f.CurrentVersionID != nil && v.ID == *f.CurrentVersionID {
			continue // 回滚后 current 可能落在保留窗口外：永不裁剪
		}
		if err := r.DeleteVersion(v.ID); err != nil {
			return pruned, err
		}
		if err := r.DecrementBlobRef(v.ObjectBlobID); err != nil {
			return pruned, err
		}
		if _, err := r.MarkBlobDeletingIfZero(v.ObjectBlobID); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

// authorizeFileWrite 判定 user 能否修改 file（追加版本/回滚）：
// 个人文件仅 owner；团队文件要求成员写权限（owner/editor，viewer 403）。
// 个人文件非 owner 统一 ErrNotFound（不泄露存在性），团队越权返回 ErrForbidden。
func authorizeFileWrite(f File, user uuid.UUID, canWriteTeam TeamWriter) error {
	if f.OwnerID == user {
		return nil
	}
	teamID := teamScope(f)
	if teamID == nil {
		return ErrNotFound
	}
	if canWriteTeam == nil {
		return ErrForbidden
	}
	ok, err := canWriteTeam(user, *teamID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// gormVersionsRepo 是 versionsRepo 的 GORM/PostgreSQL 实现，
// 由 Store 在事务内构造。
type gormVersionsRepo struct{ tx *gorm.DB }

func (g *gormVersionsRepo) GetFileForUpdate(id uuid.UUID) (File, error) {
	var f File
	err := g.tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND type = 'file' AND deleted_at IS NULL", id).First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return File{}, ErrNotFound
	}
	return f, err
}

func (g *gormVersionsRepo) GetBlobBySHA(sha256 string) (ObjectBlob, error) {
	var b ObjectBlob
	err := g.tx.Where("sha256 = ?", sha256).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ObjectBlob{}, ErrNotFound
	}
	return b, err
}

func (g *gormVersionsRepo) IncrementBlobRef(id uuid.UUID) error {
	return g.tx.Model(&ObjectBlob{}).Where("id = ?", id).
		UpdateColumn("ref_count", gorm.Expr("ref_count + 1")).Error
}

func (g *gormVersionsRepo) ResurrectBlob(id uuid.UUID) (bool, error) {
	result := g.tx.Model(&ObjectBlob{}).
		Where("id = ? AND status = ? AND ref_count = 0", id, BlobStatusDeleting).
		Updates(map[string]any{"status": BlobStatusAvailable, "ref_count": 1})
	return result.RowsAffected > 0, result.Error
}

func (g *gormVersionsRepo) CreateBlob(b ObjectBlob) error {
	return g.tx.Create(&b).Error
}

func (g *gormVersionsRepo) NextVersionNumber(fileID uuid.UUID) (int, error) {
	var next int
	err := g.tx.Model(&FileVersion{}).Where("file_id = ?", fileID).
		Select("COALESCE(MAX(version), 0) + 1").Scan(&next).Error
	return next, err
}

func (g *gormVersionsRepo) CreateVersion(v FileVersion) error {
	return g.tx.Create(&v).Error
}

func (g *gormVersionsRepo) SetCurrentVersion(fileID, versionID uuid.UUID) error {
	return g.tx.Model(&File{}).Where("id = ?", fileID).Update("current_version_id", versionID).Error
}

func (g *gormVersionsRepo) GetVersion(id uuid.UUID) (FileVersion, error) {
	var v FileVersion
	err := g.tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", id).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return FileVersion{}, ErrNotFound
	}
	return v, err
}

func (g *gormVersionsRepo) ListVersionsDesc(fileID uuid.UUID) ([]FileVersion, error) {
	var out []FileVersion
	err := g.tx.Where("file_id = ?", fileID).Order("version DESC").Find(&out).Error
	return out, err
}

func (g *gormVersionsRepo) DeleteVersion(id uuid.UUID) error {
	return g.tx.Where("id = ?", id).Delete(&FileVersion{}).Error
}

func (g *gormVersionsRepo) DecrementBlobRef(id uuid.UUID) error {
	return g.tx.Model(&ObjectBlob{}).Where("id = ?", id).
		UpdateColumn("ref_count", gorm.Expr("GREATEST(ref_count - 1, 0)")).Error
}

func (g *gormVersionsRepo) MarkBlobDeletingIfZero(id uuid.UUID) (bool, error) {
	result := g.tx.Model(&ObjectBlob{}).
		Where("id = ? AND ref_count = 0", id).
		Update("status", BlobStatusDeleting)
	return result.RowsAffected > 0, result.Error
}

// SetMaxVersions 设置每文件保留版本数上限（幂等；n<1 时保持默认 5）。
func (s *Store) SetMaxVersions(n int) {
	if n >= 1 {
		s.maxVersions = n
	}
}

// SetMaxVersionsProvider 注入版本保留数的运行时提供器（幂等；nil 不覆盖）：
// 提供器在每次 Prune 前热读取（如 system_settings 的 upload.max_versions_per_file），
// 返回非正值（含读取失败时的回退哨兵 0/-1）时回退 SetMaxVersions 的静态值。
func (s *Store) SetMaxVersionsProvider(fn func() int) {
	if fn != nil {
		s.maxVersionsFn = fn
	}
}

// effectiveMaxVersions 返回当前生效的版本保留数：提供器优先，异常回退静态值。
func (s *Store) effectiveMaxVersions() int {
	if s.maxVersionsFn != nil {
		if n := s.maxVersionsFn(); n >= 1 {
			return n
		}
	}
	return s.maxVersions
}

// AddVersion 为文件追加新版本并设为 current（事务）。
// blob 复用/新建与版本号分配见 addVersionLogic；返回新版本及是否新建了 blob。
func (s *Store) AddVersion(f File, storageKey, sha256 string, size int64, mimeType string, userID uuid.UUID) (FileVersion, bool, error) {
	var version FileVersion
	var newBlob bool
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		version, newBlob, e = addVersionLogic(&gormVersionsRepo{tx: tx}, f.ID, storageKey, sha256, size, mimeType, userID)
		return e
	})
	if err != nil {
		return FileVersion{}, false, err
	}
	return version, newBlob, nil
}

// GetVersionBlob 返回属于 fileID 的指定版本及其关联 blob（文件须未删除）。
// 不做用户鉴权：供 ONLYOFFICE 签名下载等内部集成使用，授权由签名 token 保证
// （token 绑定 file_id+version_id，伪造的版本参数在签名比对时即被拒绝）。
func (s *Store) GetVersionBlob(fileID, versionID uuid.UUID) (FileVersion, ObjectBlob, error) {
	if _, err := s.GetFileByID(fileID); err != nil {
		return FileVersion{}, ObjectBlob{}, err
	}
	var version FileVersion
	if err := s.db.Where("id = ? AND file_id = ?", versionID, fileID).First(&version).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return FileVersion{}, ObjectBlob{}, ErrNotFound
		}
		return FileVersion{}, ObjectBlob{}, err
	}
	var blob ObjectBlob
	if err := s.db.Where("id = ?", version.ObjectBlobID).First(&blob).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return FileVersion{}, ObjectBlob{}, ErrNotFound
		}
		return FileVersion{}, ObjectBlob{}, err
	}
	return version, blob, nil
}

// ListVersions 返回 user 可读文件的版本列表（含 blob status/size/sha256/mime，按版本号倒序）。
// 读权限与 Get/下载/预览一致：个人文件 owner、团队文件任意在册成员。
func (s *Store) ListVersions(user, fileID uuid.UUID) ([]VersionDetail, error) {
	f, err := s.Get(user, fileID)
	if err != nil {
		return nil, err
	}
	if f.Type != "file" {
		return nil, ErrInvalidTarget
	}
	var out []VersionDetail
	err = s.db.Table("file_versions").
		Select("file_versions.id, file_versions.version, file_versions.size, file_versions.content_sha256 AS sha256, file_versions.comment, file_versions.user_id, file_versions.created_at, object_blobs.mime_type, object_blobs.status").
		Joins("JOIN object_blobs ON object_blobs.id = file_versions.object_blob_id").
		Where("file_versions.file_id = ?", fileID).
		Order("file_versions.version DESC").
		Scan(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetCurrentVersion 将文件 current_version 指向 versionID（版本回滚）。
// 权限：个人文件 owner、团队文件 CanWrite（authorizeFileWrite）；
// 仅允许指向该文件的版本（ErrNotFileVersion）。返回更新后的文件（供 ETag）与新当前版本。
func (s *Store) SetCurrentVersion(user, fileID, versionID uuid.UUID) (File, FileVersion, error) {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", fileID).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, FileVersion{}, ErrNotFound
		}
		return File{}, FileVersion{}, err
	}
	if err := authorizeFileWrite(f, user, s.teamWriter); err != nil {
		return File{}, FileVersion{}, err
	}
	var version FileVersion
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		version, e = setCurrentVersionLogic(&gormVersionsRepo{tx: tx}, fileID, versionID)
		return e
	})
	if err != nil {
		return File{}, FileVersion{}, err
	}
	// 回读文件行：gorm Update 已刷新 updated_at，ETag 需要准确值。
	if err := s.db.Where("id = ?", fileID).First(&f).Error; err != nil {
		return File{}, FileVersion{}, err
	}
	return f, version, nil
}

// PruneVersions 裁剪文件历史版本，保留最新 keep 个（current 指向的版本受保护）。
// 返回被裁数量；blob 物理删除延后至 janitor（见 pruneVersionsLogic）。
func (s *Store) PruneVersions(fileID uuid.UUID, keep int) (int, error) {
	var pruned int
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var e error
		pruned, e = pruneVersionsLogic(&gormVersionsRepo{tx: tx}, fileID, keep)
		return e
	})
	if err != nil {
		return 0, err
	}
	return pruned, nil
}

// ValidateReplaceTarget 校验 user 可将上传内容作为新版本写入 target：
// 必须存在、未删除且 type='file'；个人文件要求 owner，团队文件要求 CanWrite。
// 返回文件行（会话沿用其现有名称与父目录）。
func (s *Store) ValidateReplaceTarget(user, target uuid.UUID) (File, error) {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", target).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	if f.Type != "file" {
		return File{}, ErrInvalidTarget
	}
	if err := authorizeFileWrite(f, user, s.teamWriter); err != nil {
		return File{}, err
	}
	return f, nil
}

// ReplaceFileVersion 上传 Complete 的「覆盖为新版本」落库钩子：
// 重新校验写权限（会话期间成员/删除状态可能变化）后 AddVersion，
// 再按当前生效的版本保留数（settings 热读取，读失败回退 config 值）裁剪历史。
// 返回是否新建了 blob（复用去重时调用方应删除冗余物理对象）。
func (s *Store) ReplaceFileVersion(user, fileID uuid.UUID, storageKey, sha256 string, size int64, mimeType string) (bool, error) {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", fileID).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, ErrNotFound
		}
		return false, err
	}
	if err := authorizeFileWrite(f, user, s.teamWriter); err != nil {
		return false, err
	}
	_, newBlob, err := s.AddVersion(f, storageKey, sha256, size, mimeType, user)
	if err != nil {
		return false, err
	}
	if _, err := s.PruneVersions(fileID, s.effectiveMaxVersions()); err != nil {
		return newBlob, err
	}
	return newBlob, nil
}
