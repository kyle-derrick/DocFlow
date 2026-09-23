package files

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// 批量操作单项错误码（per-item results 的 error_code，机器可读）。
const (
	BatchCodeNotFound      = "NOT_FOUND"
	BatchCodeForbidden     = "FORBIDDEN"
	BatchCodeRoot          = "ROOT"
	BatchCodeNameConflict  = "NAME_CONFLICT"
	BatchCodeInvalidTarget = "INVALID_TARGET"
	BatchCodeParentDeleted = "PARENT_DELETED"
	BatchCodeNotDeleted    = "NOT_DELETED"
	BatchCodeDepthLimit    = "DEPTH_LIMIT"
	BatchCodeQuota         = "SPACE_QUOTA_EXCEEDED"
	BatchCodeInternal      = "INTERNAL"
)

// ErrMoveTarget 批量移动目标非法（目标为自身或其后代）。
var ErrMoveTarget = errors.New("move target folder is invalid")

// BatchItemResult 批量操作单项结果；ok=false 时 error_code 说明原因。
type BatchItemResult struct {
	ID        uuid.UUID `json:"id"`
	OK        bool      `json:"ok"`
	ErrorCode string    `json:"error_code,omitempty"`
}

// batchErrorCode 把 Store 错误映射为批量结果错误码（未知错误归 INTERNAL，
// 单项失败不影响其余项继续执行——部分成功语义）。
func batchErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound):
		return BatchCodeNotFound
	case errors.Is(err, ErrForbidden):
		return BatchCodeForbidden
	case errors.Is(err, ErrRoot):
		return BatchCodeRoot
	case errors.Is(err, ErrConflict):
		return BatchCodeNameConflict
	case errors.Is(err, ErrMoveTarget):
		return BatchCodeInvalidTarget
	case errors.Is(err, ErrParentDeleted):
		return BatchCodeParentDeleted
	case errors.Is(err, ErrNotDeleted):
		return BatchCodeNotDeleted
	case errors.Is(err, ErrFolderDepth):
		return BatchCodeDepthLimit
	case errors.Is(err, ErrQuotaExceeded):
		return BatchCodeQuota
	default:
		return BatchCodeInternal
	}
}

// authorizeBatchWrite 判定 user 能否变更（移动/移入回收站）单个文件：
// 文件行 owner 短路；其余要求空间成员写权限（owner/admin/member_share/
// member，guest 403；非成员归一为 ErrNotFound，不泄露存在性）。
func (s *Store) authorizeBatchWrite(f File, user uuid.UUID) error {
	if f.OwnerID == user {
		return nil
	}
	if s.spaceWriter == nil {
		return ErrForbidden
	}
	ok, err := s.spaceWriter(user, f.SpaceID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// moveItemDecision 移动单项的纯逻辑判定：根目录不可移动；
// 目标为文件自身（同 id）拒绝。名称冲突与后代判定依赖数据访问，由调用方完成。
func moveItemDecision(item, target File) error {
	if item.IsRoot {
		return ErrRoot
	}
	if item.ID == target.ID {
		return ErrMoveTarget
	}
	return nil
}

// isWithin 沿 parent 链向上判断 candidate 是否等于 ancestor 或位于其子树内
// （用于阻止把目录移动到自身/后代下）。环/超深路径防御：上限 1000 层。
func (s *Store) isWithin(candidate, ancestor uuid.UUID) (bool, error) {
	cur := candidate
	for i := 0; i < 1000; i++ {
		if cur == ancestor {
			return true, nil
		}
		var f File
		if err := s.db.Select("id", "parent_id").Where("id = ?", cur).First(&f).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, err
		}
		if f.ParentID == nil {
			return false, nil
		}
		cur = *f.ParentID
	}
	return false, nil
}

// BatchMove 把多个文件/目录移动到 target 目录下，逐项执行、逐项收集结果：
// 整体不因单项失败回滚（部分成功语义），冲突项（目标目录存在同名活跃项）
// 跳过并记 NAME_CONFLICT。目标目录整体校验失败（不存在/无写权限）时
// 整个请求失败（返回错误，HTTP 层映射 404/403）。
// 跨空间移动继承目标空间（space_id 更新）并校验目标空间配额（超限
// SPACE_QUOTA_EXCEEDED）。
// MoveRename atomically moves one item and assigns its destination name. All authorization,
// cycle, depth, quota, and conflict checks happen before the single transaction update.
func (s *Store) MoveRename(user, id, target uuid.UUID, name string) (File, error) {
	t, err := authorizeParentFolder(s, user, target, s.spaceWriter, s.acl)
	if err != nil {
		return File{}, err
	}
	n, err := NormalizeName(name)
	if err != nil {
		return File{}, err
	}
	var f File
	if err = s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	if err = moveItemDecision(f, t); err != nil {
		return File{}, err
	}
	if inside, e := s.isWithin(t.ID, f.ID); e != nil {
		return File{}, e
	} else if inside {
		return File{}, ErrMoveTarget
	}
	if err = s.authorizeBatchWrite(f, user); err != nil {
		return File{}, err
	}
	if err = authorizeFileWrite(f, user, s.spaceWriter, s.acl); err != nil {
		return File{}, err
	}
	var conflict int64
	if err = s.db.Model(&File{}).Where("parent_id = ? AND lower(name) = lower(?) AND deleted_at IS NULL AND id <> ?", t.ID, n, id).Count(&conflict).Error; err != nil {
		return File{}, err
	}
	if conflict > 0 {
		return File{}, ErrConflict
	}
	result := s.db.Model(&File{}).Where("id = ? AND deleted_at IS NULL", id).Updates(map[string]any{"parent_id": t.ID, "space_id": t.SpaceID, "name": n})
	if result.Error != nil {
		if strings.Contains(strings.ToLower(result.Error.Error()), "unique") {
			return File{}, ErrConflict
		}
		return File{}, result.Error
	}
	if result.RowsAffected != 1 {
		return File{}, ErrNotFound
	}
	f.ParentID, f.SpaceID, f.Name = &t.ID, t.SpaceID, n
	return f, nil
}

func (s *Store) BatchMove(user uuid.UUID, ids []uuid.UUID, target uuid.UUID) ([]BatchItemResult, error) {
	t, err := authorizeParentFolder(s, user, target, s.spaceWriter, s.acl)
	if err != nil {
		return nil, err
	}
	results := make([]BatchItemResult, 0, len(ids))
	for _, id := range ids {
		r := BatchItemResult{ID: id, OK: true}
		if err := s.moveOne(user, id, t); err != nil {
			r.OK = false
			r.ErrorCode = batchErrorCode(err)
		}
		results = append(results, r)
	}
	return results, nil
}

// moveOne 执行单个移动：读权限归属校验（写级）、目标合法性、同名冲突
// 检查后更新 parent_id 并继承目标空间；跨空间移动前校验目标空间配额。
func (s *Store) moveOne(user, id uuid.UUID, target File) error {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := moveItemDecision(f, target); err != nil {
		return err
	}
	// 目标位于待移动目录自身子树内 → 拒绝（会形成环）。
	if inside, err := s.isWithin(target.ID, f.ID); err != nil {
		return err
	} else if inside {
		return ErrMoveTarget
	}
	if err := s.authorizeBatchWrite(f, user); err != nil {
		return err
	}
	// 深度校验（folder.max_depth）：目标深度 + 待移动子树高度不得超过上限
	//（仅目录参与：子树高度>1；文件自身高度恒为 1）。
	targetDepth, derr := s.folderDepthOf(target.ID)
	if derr != nil {
		return derr
	}
	if f.Type == "folder" {
		height, herr := s.folderSubtreeHeight(f.ID)
		if herr != nil {
			return herr
		}
		if verr := validateMoveDepth(targetDepth, height, s.effectiveMaxFolderDepth()); verr != nil {
			return verr
		}
	} else if verr := validateMoveDepth(targetDepth, 1, s.effectiveMaxFolderDepth()); verr != nil {
		return verr
	}
	var conflict int64
	if err := s.db.Model(&File{}).
		Where("parent_id = ? AND lower(name) = lower(?) AND deleted_at IS NULL AND id <> ?", target.ID, f.Name, f.ID).
		Count(&conflict).Error; err != nil {
		return err
	}
	if conflict > 0 {
		return ErrConflict
	}
	// 跨空间移动：目标空间配额校验（子树当前版本字节合计；软删计入的
	// 配额口径以未软删近似——移动不复制软删项）。
	if f.SpaceID != target.SpaceID {
		var bytes int64
		if f.Type == "folder" {
			b, serr := s.subtreeBytes(f.ID)
			if serr != nil && !errors.Is(serr, ErrNotFound) {
				return serr
			}
			bytes = b
		} else {
			bytes = s.fileSize(f.ID)
		}
		if err := s.checkSpaceQuota(target.SpaceID, bytes); err != nil {
			return err
		}
	}
	result := s.db.Model(&File{}).Where("id = ? AND deleted_at IS NULL", id).
		Updates(map[string]any{"parent_id": target.ID, "space_id": target.SpaceID})
	if result.Error != nil {
		// 并发窗口内目标目录出现同名项：唯一部分索引兜底为名称冲突。
		if strings.Contains(strings.ToLower(result.Error.Error()), "unique") {
			return ErrConflict
		}
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// BatchTrash 批量软删除（移入回收站）；根目录跳过并记 ROOT。
// 单项语义与单文件 Delete 一致（子树随之隐藏，回收站列表去重展示），
// 但额外支持团队文件（成员写权限）。
func (s *Store) BatchTrash(user uuid.UUID, ids []uuid.UUID) []BatchItemResult {
	results := make([]BatchItemResult, 0, len(ids))
	for _, id := range ids {
		r := BatchItemResult{ID: id, OK: true}
		if err := s.trashOne(user, id); err != nil {
			r.OK = false
			r.ErrorCode = batchErrorCode(err)
		}
		results = append(results, r)
	}
	return results
}

func (s *Store) trashOne(user, id uuid.UUID) error {
	var f File
	if err := s.db.Where("id = ? AND deleted_at IS NULL", id).First(&f).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		return err
	}
	if f.IsRoot {
		return ErrRoot
	}
	if err := s.authorizeBatchWrite(f, user); err != nil {
		return err
	}
	return s.db.Model(&File{}).Where("id = ? AND deleted_at IS NULL", id).
		Update("deleted_at", gorm.Expr("CURRENT_TIMESTAMP")).Error
}

// BatchRestore 批量恢复，逐项复用 Restore 语义（原父目录被删/名称冲突
// 返回对应错误码，不静默改名）；单项失败不影响其余项。
func (s *Store) BatchRestore(user uuid.UUID, ids []uuid.UUID) []BatchItemResult {
	results := make([]BatchItemResult, 0, len(ids))
	for _, id := range ids {
		r := BatchItemResult{ID: id, OK: true}
		if _, err := s.Restore(user, id); err != nil {
			r.OK = false
			r.ErrorCode = batchErrorCode(err)
		}
		results = append(results, r)
	}
	return results
}
