package search

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GormRepo 是 Repo 的 GORM/PostgreSQL 实现（file_search_docs，migration 019）。
type GormRepo struct{ db *gorm.DB }

func NewGormRepo(db *gorm.DB) *GormRepo { return &GormRepo{db: db} }

// accessibleScopeSQL 返回「user 可读」过滤 SQL 片段与参数（统一空间模型）：
// owner 命中，或用户为文档所在空间的在册成员（直接成员或经用户组，EXISTS
// 子查询实时判定）。与 files.readableScopeSQL 同一判定模式。
func accessibleScopeSQL(alias string, user uuid.UUID) (string, []any) {
	return "(" + alias + ".owner_id = ? OR EXISTS (SELECT 1 FROM space_members sm WHERE sm.space_id = " + alias + ".space_id AND sm.user_id = ?)" +
		" OR EXISTS (SELECT 1 FROM space_group_members sgm JOIN group_members gm ON gm.group_id = sgm.group_id WHERE sgm.space_id = " + alias + ".space_id AND gm.user_id = ?))", []any{user, user, user}
}

// UpsertDoc upsert 索引文档：按 file_id 冲突更新全部字段（version_id/owner_id/
// space_id/name/content/updated_at）。content 为空串落 NULL（tsv 退化为名称向量，
// 由生成列自动维护）。PostgreSQL 14+ 对生成列的 INSERT 须显式列清单（不含 tsv），
// 原生 SQL 直写。
func (g *GormRepo) UpsertDoc(d Doc) error {
	return g.db.Exec(`INSERT INTO file_search_docs (file_id, version_id, owner_id, space_id, name, content, updated_at)
VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), now())
ON CONFLICT (file_id) DO UPDATE SET
  version_id = EXCLUDED.version_id, owner_id = EXCLUDED.owner_id, space_id = EXCLUDED.space_id,
  name = EXCLUDED.name, content = EXCLUDED.content, updated_at = now()`,
		d.FileID, d.VersionID, d.OwnerID, d.SpaceID, d.Name, d.Content).Error
}

// RemoveDoc 删除 fileID 的索引文档（幂等）。
func (g *GormRepo) RemoveDoc(fileID uuid.UUID) error {
	return g.db.Exec("DELETE FROM file_search_docs WHERE file_id = ?", fileID).Error
}

// QueryDocs 检索 user 可读且命中 q 的文件：
//   - 匹配：f.name ILIKE %q%（大小写不敏感，%_\ 已转义）OR tsv @@
//     plainto_tsquery('simple', q)（中文整句单 token，内容检索对英文有效）；
//   - 名称取 files 行实时值（rename 后索引重建前仍准确），软删/根目录排除；
//   - snippet：名称命中返回名称；内容命中返回 ts_headline（'simple'，
//     高亮标记 [[..]]，前端纯文本渲染后自行高亮，避免 HTML 注入）；
//   - 排序：名称命中优先，其次 updated_at 倒序，id 稳定次序。
func (g *GormRepo) QueryDocs(user uuid.UUID, opts QueryOptions) ([]Result, error) {
	pattern := LikePattern(opts.Q)
	cond, condArgs := accessibleScopeSQL("d", user)
	where := "(f.name ILIKE ? OR d.tsv @@ plainto_tsquery('simple', ?)) AND " + cond
	// 占位符按 SQL 文本顺序绑定：SELECT 片段（snippet 的 ILIKE 与
	// plainto_tsquery）在 WHERE 之前出现，须先提供 pattern/Q 再接 WHERE 的
	// pattern/Q，最后是访问范围、可选过滤与 ORDER BY/LIMIT。
	args := []any{pattern, opts.Q} // SELECT: CASE WHEN f.name ILIKE ? / snippet plainto_tsquery(?)
	args = append(args, pattern, opts.Q)
	args = append(args, condArgs...)
	if opts.TagID != nil {
		where += " AND EXISTS (SELECT 1 FROM file_tags ft WHERE ft.file_id = d.file_id AND ft.tag_id = ?)"
		args = append(args, *opts.TagID)
	}
	if opts.Starred != nil {
		where += " AND f.is_starred = ?"
		args = append(args, *opts.Starred)
	}
	args = append(args, pattern, opts.Limit)
	var out []Result
	err := g.db.Raw(`SELECT d.file_id AS id, f.name, f.type, f.parent_id, f.updated_at,
  CASE WHEN f.name ILIKE ? THEN f.name
       WHEN d.content IS NULL THEN ''
       ELSE ts_headline('simple', d.content, plainto_tsquery('simple', ?), 'MaxWords=20, MinWords=5, StartSel=[[, StopSel=]]')
  END AS snippet
FROM file_search_docs d
JOIN files f ON f.id = d.file_id AND f.deleted_at IS NULL AND f.is_root = false
WHERE `+where+`
ORDER BY (f.name ILIKE ?) DESC, f.updated_at DESC, d.file_id
LIMIT ?`, args...).Scan(&out).Error
	if out == nil {
		out = []Result{}
	}
	return out, err
}

// DeleteOrphanDocs 删除无对应 files 行的孤儿索引（janitor 兜底；正常路径由
// 外键 ON DELETE CASCADE 级联承担），返回删除行数。
func (g *GormRepo) DeleteOrphanDocs() (int64, error) {
	result := g.db.Exec("DELETE FROM file_search_docs WHERE file_id NOT IN (SELECT id FROM files)")
	return result.RowsAffected, result.Error
}

// 确保 GormRepo 满足 Repo（编译期约束）。
var _ Repo = (*GormRepo)(nil)
