package files

import (
	"errors"

	"github.com/google/uuid"
)

// ErrQuotaExceeded 表示空间存储配额超限（统一空间模型）：上传建会话
// （Start/StartReplace）、跨空间复制/移动时由配额校验返回，HTTP 层映射
// 413 {error, code: SPACE_QUOTA_EXCEEDED}。
var ErrQuotaExceeded = errors.New("space storage quota exceeded")

// UsedStorage 返回用户默认空间的当前版本总占用（/me 与 MCP 缺省空间口径）；
// 无默认空间时为 0。统一空间模型下「个人用量」= 默认空间用量。
func (s *Store) UsedStorage(owner uuid.UUID) (int64, error) {
	spaceID := s.defaultSpaceID(owner)
	if spaceID == uuid.Nil {
		return 0, nil
	}
	return s.UsedSpaceStorage(spaceID)
}

// DefaultSpaceRoot 返回用户默认空间根目录（MCP 缺省空间入口）；无默认
// 空间时返回 ErrNotFound。
func (s *Store) DefaultSpaceRoot(user uuid.UUID) (File, error) {
	spaceID := s.defaultSpaceID(user)
	if spaceID == uuid.Nil {
		return File{}, ErrNotFound
	}
	return s.SpaceRoot(spaceID)
}

// UsedSpaceStorage 返回空间当前版本总占用（字节）：SUM(file_versions.size)
// 经 files.current_version_id JOIN。软删（回收站）文件计入——底层对象在
// 彻底清理（purge/janitor）前仍占用存储，防止以「删除至回收站」绕过配额。
func (s *Store) UsedSpaceStorage(spaceID uuid.UUID) (int64, error) {
	var out int64
	err := s.db.Raw(
		"SELECT COALESCE(SUM(fv.size), 0) FROM files f "+
			"JOIN file_versions fv ON fv.id = f.current_version_id "+
			"WHERE f.space_id = ? "+
			"AND f.is_root = false AND f.type = 'file'", spaceID,
	).Scan(&out).Error
	return out, err
}

// QuotaExceeded 判定追加 size 字节后是否超出配额（纯函数，供单测矩阵）：
// quota<=0 视为不限；used+size 溢出按超限处理（fail closed）。
func QuotaExceeded(used, quota, size int64) bool {
	if quota <= 0 || size <= 0 {
		return false
	}
	total := used + size
	if total < 0 { // int64 溢出
		return true
	}
	return total > quota
}

// checkSpaceQuota 校验空间追加 add 字节后是否超限（统一空间模型的核心
// 判定：读 spaces.quota_bytes 与 UsedSpaceStorage 后求值）。
// 配额读取失败返回原始错误（调用方按 500 处理，不误放行）。
func (s *Store) checkSpaceQuota(spaceID uuid.UUID, add int64) error {
	var quota int64
	if err := s.db.Raw("SELECT quota_bytes FROM spaces WHERE id = ? AND deleted_at IS NULL", spaceID).Scan(&quota).Error; err != nil {
		return err
	}
	used, err := s.UsedSpaceStorage(spaceID)
	if err != nil {
		return err
	}
	if QuotaExceeded(used, quota, add) {
		return ErrQuotaExceeded
	}
	return nil
}

// CheckUploadQuota 为 upload.Service 的配额校验回调（main 注入）：按上传
// 目标父目录定位空间后校验空间配额（user 参数保留以兼容注入签名；统一
// 空间模型下配额按空间判定）。parent 无效或读取失败返回错误（不误放行）。
func (s *Store) CheckUploadQuota(user, parent uuid.UUID, size int64) error {
	_ = user
	// 标量扫描经 string 中转再 Parse：Raw().Scan 直接扫 uuid.UUID（[16]byte
	// 数组）在 Postgres 驱动下报 converting string to uint8。
	var spaceIDStr string
	if err := s.db.Raw("SELECT space_id FROM files WHERE id = ? AND deleted_at IS NULL", parent).Scan(&spaceIDStr).Error; err != nil {
		return err
	}
	if spaceIDStr == "" {
		return ErrNotFound
	}
	spaceID, err := uuid.Parse(spaceIDStr)
	if err != nil {
		return err
	}
	return s.checkSpaceQuota(spaceID, size)
}

// subtreeBytes 返回目录子树内全部未软删文件的当前版本字节合计
// （跨空间复制/移动的配额校验用；递归 CTE 展开）。
func (s *Store) subtreeBytes(root uuid.UUID) (int64, error) {
	var out int64
	err := s.db.Raw(`
		WITH RECURSIVE tree AS (
			SELECT id FROM files WHERE id = ?
			UNION ALL
			SELECT f.id FROM files f JOIN tree t ON f.parent_id = t.id WHERE f.deleted_at IS NULL
		)
		SELECT COALESCE(SUM(fv.size), 0) FROM files f
		JOIN tree ON tree.id = f.id
		JOIN file_versions fv ON fv.id = f.current_version_id
		WHERE f.type = 'file' AND f.deleted_at IS NULL`, root).Scan(&out).Error
	return out, err
}

// fileSize 返回文件当前版本字节（无当前版本为 0；单文件复制的配额校验用）。
func (s *Store) fileSize(id uuid.UUID) int64 {
	var size int64
	if err := s.db.Raw("SELECT COALESCE(fv.size, 0) FROM files f JOIN file_versions fv ON fv.id = f.current_version_id WHERE f.id = ?", id).Scan(&size).Error; err != nil {
		return 0
	}
	return size
}
