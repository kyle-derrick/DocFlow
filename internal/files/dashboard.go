package files

import (
	"github.com/google/uuid"
)

// 个人空间聚合的统一过滤条件：owner 维度、未软删、非根目录、仅文件
// （目录不计入「我的文件数」与存储占用；团队文件单列，见 TeamFileCount）。
const personalFileScope = "owner_id = ? AND scope_type = 'personal' AND deleted_at IS NULL AND is_root = false AND type = 'file'"

// PersonalDashboard 个人仪表盘（owner 维度）聚合结果。
type PersonalDashboard struct {
	// FileCount 个人空间未软删文件数（不含目录与根目录）。
	FileCount int64
	// StorageBytes 个人空间文件当前版本总占用：file_versions.size 经
	// files.current_version_id JOIN 求和（软删排除；历史版本与团队文件不计入）。
	StorageBytes int64
	// TeamFileCount 当前用户可访问团队空间的未软删文件数（在册成员实时
	// 判定，EXISTS team_members，与检索/分享的权限口径一致）。
	TeamFileCount int64
	// RecentFiles 个人空间最近更新的文件（updated_at 倒序）。
	RecentFiles []File
}

// PersonalDashboardStats 聚合个人仪表盘数据（v1.1 仪表盘后端聚合查询）。
// recentLimit 为最近文件条数（<=0 时取 5）；查询失败返回首个错误。
func (s *Store) PersonalDashboardStats(owner uuid.UUID, recentLimit int) (PersonalDashboard, error) {
	if recentLimit <= 0 {
		recentLimit = 5
	}
	var out PersonalDashboard
	if err := s.db.Model(&File{}).
		Where(personalFileScope, owner).
		Count(&out.FileCount).Error; err != nil {
		return PersonalDashboard{}, err
	}
	// 存储占用：当前版本 → file_versions.size 求和；COALESCE 保证空集为 0。
	if err := s.db.Raw(
		"SELECT COALESCE(SUM(fv.size), 0) FROM files f "+
			"JOIN file_versions fv ON fv.id = f.current_version_id "+
			"WHERE f.owner_id = ? AND f.scope_type = 'personal' AND f.deleted_at IS NULL "+
			"AND f.is_root = false AND f.type = 'file'", owner,
	).Scan(&out.StorageBytes).Error; err != nil {
		return PersonalDashboard{}, err
	}
	// 团队空间文件：在册成员可访问的团队文件（成员变动实时生效）。
	if err := s.db.Model(&File{}).
		Where("scope_type = 'team' AND team_id IS NOT NULL AND deleted_at IS NULL "+
			"AND is_root = false AND type = 'file' "+
			"AND EXISTS (SELECT 1 FROM team_members tm WHERE tm.team_id = files.team_id AND tm.user_id = ?)", owner).
		Count(&out.TeamFileCount).Error; err != nil {
		return PersonalDashboard{}, err
	}
	if err := s.db.
		Where(personalFileScope+" AND current_version_id IS NOT NULL", owner).
		Where("EXISTS (SELECT 1 FROM file_versions fv JOIN object_blobs ob ON ob.id = fv.object_blob_id WHERE fv.id = files.current_version_id AND fv.file_id = files.id AND ob.status = ?)", BlobStatusAvailable).
		Order("updated_at DESC, id DESC").
		Limit(recentLimit).
		Find(&out.RecentFiles).Error; err != nil {
		return PersonalDashboard{}, err
	}
	return out, nil
}
