package files

import (
	"errors"

	"github.com/google/uuid"
)

// ErrQuotaExceeded 表示属主存储配额超限（C3，设计 3.2.1/6.3.1/6.12.4）：
// 上传建会话（Start/StartReplace）时由配额校验返回，HTTP 层映射
// 403 {error, code: QUOTA_EXCEEDED}。
var ErrQuotaExceeded = errors.New("storage quota exceeded")

// UsedStorage 返回 owner 个人空间当前版本总占用（字节）：SUM(file_versions.size)
// 经 files.current_version_id JOIN。与 PersonalDashboardStats.StorageBytes 的
// 口径差异：软删（回收站）文件计入——底层对象在彻底清理（purge/janitor）
// 前仍占用存储，防止用户以「删除至回收站」绕过配额；目录与团队文件不计入
// （团队空间不占个人配额）。
func (s *Store) UsedStorage(owner uuid.UUID) (int64, error) {
	var out int64
	err := s.db.Raw(
		"SELECT COALESCE(SUM(fv.size), 0) FROM files f "+
			"JOIN file_versions fv ON fv.id = f.current_version_id "+
			"WHERE f.owner_id = ? AND f.scope_type = 'personal' "+
			"AND f.is_root = false AND f.type = 'file'", owner,
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

// CheckUploadQuota 为 upload.Service 的配额校验回调（main 注入）：读取属主
// 配额（users.storage_quota）与已用（UsedStorage，软删计入）后判定。
// 配额读取失败返回原始错误（调用方按 500 处理，不误放行）。
func (s *Store) CheckUploadQuota(owner uuid.UUID, size int64) error {
	var quota int64
	if err := s.db.Raw("SELECT storage_quota FROM users WHERE id = ?", owner).Scan(&quota).Error; err != nil {
		return err
	}
	used, err := s.UsedStorage(owner)
	if err != nil {
		return err
	}
	if QuotaExceeded(used, quota, size) {
		return ErrQuotaExceeded
	}
	return nil
}
