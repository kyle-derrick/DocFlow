package files

import (
	"github.com/google/uuid"
)

// PersonalDashboard 个人仪表盘（统一空间模型）聚合结果。
type PersonalDashboard struct {
	// FileCount 默认空间未软删文件数（不含目录与根目录）。
	FileCount int64
	// StorageBytes 默认空间文件当前版本总占用（软删排除，展示口径；
	// 配额口径见 UsedSpaceStorage）。
	StorageBytes int64
	// SpaceFileCount 用户可访问的其他空间（默认空间之外）未软删文件数
	//（在册成员实时判定，EXISTS space_members/space_group_members）。
	SpaceFileCount int64
	// RecentFiles 用户可访问的全部空间文件（updated_at 倒序）。
	RecentFiles []File
}

// defaultSpaceID 返回用户默认空间 ID（无默认空间时返回 uuid.Nil）。
func (s *Store) defaultSpaceID(owner uuid.UUID) uuid.UUID {
	// 标量扫描经 string 中转再 Parse：Raw().Scan 直接扫 uuid.UUID（[16]byte
	// 数组）在 Postgres 驱动下报 converting string to uint8。
	var idStr string
	if err := s.db.Raw("SELECT id FROM spaces WHERE owner_id = ? AND is_default AND deleted_at IS NULL", owner).Scan(&idStr).Error; err != nil || idStr == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// PersonalDashboardStats 聚合个人仪表盘数据（v1.1 仪表盘后端聚合查询）。
// recentLimit 为最近文件条数（<=0 时取 5）；查询失败返回首个错误。
func (s *Store) PersonalDashboardStats(owner uuid.UUID, recentLimit int) (PersonalDashboard, error) {
	if recentLimit <= 0 {
		recentLimit = 5
	}
	var out PersonalDashboard
	defaultSpace := s.defaultSpaceID(owner)
	if defaultSpace != uuid.Nil {
		if err := s.db.Model(&File{}).
			Where("space_id = ? AND deleted_at IS NULL AND is_root = false AND type = 'file'", defaultSpace).
			Count(&out.FileCount).Error; err != nil {
			return PersonalDashboard{}, err
		}
		if err := s.db.Raw(
			"SELECT COALESCE(SUM(fv.size), 0) FROM files f "+
				"JOIN file_versions fv ON fv.id = f.current_version_id "+
				"WHERE f.space_id = ? AND f.deleted_at IS NULL "+
				"AND f.is_root = false AND f.type = 'file'", defaultSpace,
		).Scan(&out.StorageBytes).Error; err != nil {
			return PersonalDashboard{}, err
		}
	}
	// 其他空间文件：在册成员（直接或经用户组）可访问、排除默认空间。
	memberCond := `space_id <> ? AND deleted_at IS NULL AND is_root = false AND type = 'file' AND (
		EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = files.space_id AND sm.user_id = ?)
		OR EXISTS (SELECT 1 FROM space_group_members sgm
		           JOIN group_members gm ON gm.group_id = sgm.group_id
		           WHERE sgm.space_id = files.space_id AND gm.user_id = ?))`
	if err := s.db.Model(&File{}).
		Where(memberCond, defaultSpace, owner, owner).
		Count(&out.SpaceFileCount).Error; err != nil {
		return PersonalDashboard{}, err
	}
	cond, args := readableScopeSQL(owner)
	if err := s.db.Model(&File{}).
		Where("deleted_at IS NULL AND is_root = false AND type = 'file' AND current_version_id IS NOT NULL AND "+cond, args...).
		Where("EXISTS (SELECT 1 FROM file_versions fv JOIN object_blobs ob ON ob.id = fv.object_blob_id WHERE fv.id = files.current_version_id AND fv.file_id = files.id AND ob.status = ?)", BlobStatusAvailable).
		Order("updated_at DESC, id DESC").
		Limit(recentLimit).
		Find(&out.RecentFiles).Error; err != nil {
		return PersonalDashboard{}, err
	}
	return out, nil
}
