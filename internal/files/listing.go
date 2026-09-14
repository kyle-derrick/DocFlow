package files

import (
	"strings"

	"github.com/google/uuid"
)

// SortOptions 目录/检索列举的排序参数；字段经白名单校验（见 SortClause）。
type SortOptions struct {
	// Sort 排序键：name（缺省）| updated_at | size（取当前版本大小）。
	Sort string
	// Order 方向：asc（缺省）| desc。
	Order string
}

// sortColumns 排序键白名单 → SQL 列表达式。size 无独立列，经当前版本
// 子查询取 file_versions.size（无当前版本时为 NULL，PostgreSQL 默认
// NULLS LAST 升序/NULLS FIRST 降序——与「无版本排最后」的直觉不符时
// 由前端默认按 name 排序规避）。
var sortColumns = map[string]string{
	"name":       "lower(name)",
	"updated_at": "updated_at",
	"size":       "(SELECT fv.size FROM file_versions fv WHERE fv.id = files.current_version_id)",
}

// SortClause 校验并返回 ORDER BY 子句；非法键/方向返回 ok=false（HTTP 层 400）。
// 追加 id 作稳定次序键，避免同值行分页抖动。
func SortClause(sort, order string) (string, bool) {
	col, ok := sortColumns[sort]
	if !ok {
		return "", false
	}
	dir := "ASC"
	if strings.EqualFold(order, "desc") {
		dir = "DESC"
	} else if order != "" && !strings.EqualFold(order, "asc") {
		return "", false
	}
	return col + " " + dir + ", id " + dir, true
}

// SearchOptions 跨目录检索（tag / starred 过滤）参数。
type SearchOptions struct {
	// TagID 限定带该标签的文件（EXISTS file_tags）。
	TagID *uuid.UUID
	// Starred 限定收藏状态；nil 表示不过滤。
	Starred *bool
	SortOptions
	Limit int
}

// readableScopeSQL 返回「user 可读」过滤 SQL 片段与参数：
// 个人文件 owner 命中；团队文件（scope_type='team'）要求在册成员
// （EXISTS team_members，成员变动实时生效，与 share 包同模式）。
func readableScopeSQL(user uuid.UUID) (string, []any) {
	return "(owner_id = ? OR (scope_type = 'team' AND team_id IS NOT NULL AND " +
		"EXISTS (SELECT 1 FROM team_members tm WHERE tm.team_id = files.team_id AND tm.user_id = ?)))", []any{user, user}
}

// SearchAccessible 跨目录列出 user 可读且命中过滤条件的文件
// （个人 + 团队，含目录本身；不含根目录与软删除项）。
// tag_id 的归属校验由调用方（HTTP 层经 tagging 服务）完成。
func (s *Store) SearchAccessible(user uuid.UUID, opts SearchOptions) ([]File, error) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Sort == "" {
		opts.Sort = "name"
	}
	clause, ok := SortClause(opts.Sort, opts.Order)
	if !ok {
		clause = "lower(name) ASC, id ASC"
	}
	cond, args := readableScopeSQL(user)
	q := s.db.Where("deleted_at IS NULL AND is_root = false AND "+cond, args...)
	if opts.TagID != nil {
		q = q.Where("EXISTS (SELECT 1 FROM file_tags ft WHERE ft.file_id = files.id AND ft.tag_id = ?)", *opts.TagID)
	}
	if opts.Starred != nil {
		q = q.Where("is_starred = ?", *opts.Starred)
	}
	var out []File
	err := q.Order(clause).Limit(opts.Limit).Find(&out).Error
	return out, err
}

// SetStarred 切换收藏标记并返回更新后的元数据。
// 权限取舍：is_starred 为行级布尔（团队文件共享星标，不引入 per-user 表），
// 放宽为读权限即可切换（authorizeFileAccess：个人 owner、团队任意在册成员）；
// 用 UpdateColumn 不触碰 updated_at/ETag（收藏是视图状态，不算内容变更）。
func (s *Store) SetStarred(user, id uuid.UUID, starred bool) (File, error) {
	f, err := s.Get(user, id)
	if err != nil {
		return File{}, err
	}
	if err := s.db.Model(&File{}).Where("id = ? AND deleted_at IS NULL", id).
		UpdateColumn("is_starred", starred).Error; err != nil {
		return File{}, err
	}
	f.IsStarred = starred
	return f, nil
}
