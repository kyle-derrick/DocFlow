-- 043: 发布前性能复盘（C1 EXPLAIN）补索引。
-- 文件目录列举的 updated_at 排序（FilesPage 排序切换 / 默认「最近修改」
-- 场景）此前仅有 (space_id, parent_id) 过滤索引（038 idx_files_team_parent），
-- 无排序列：EXPLAIN 实测 5000 行目录 Seq Scan + top-N sort（5.2ms，
-- LIMIT 200）。补 (space_id, parent_id, updated_at DESC, id DESC) 部分索引后
-- 优化器可走索引序扫描提前终止（id DESC 为 SortClause 稳定次序键，须一并
-- 入索引否则退化为 incremental sort 兜底）。
-- name 排序已由 idx_files_parent_name_active（parent_id 前缀）覆盖；
-- size 排序经 file_versions 相关子查询（pkey point lookup，无可优化索引），
-- 深表场景需数据模型冗余，列入低优先项不改。
CREATE INDEX IF NOT EXISTS idx_files_space_parent_updated
    ON files(space_id, parent_id, updated_at DESC, id DESC)
    WHERE deleted_at IS NULL;
