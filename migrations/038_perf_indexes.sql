-- 038: 性能补索引（files 列表/检索链路核对后仅补缺口）。
-- 文件列表（parent_id + deleted_at IS NULL）已由 002 的部分唯一索引
-- idx_files_parent_name_active（parent_id 前缀，WHERE deleted_at IS NULL）覆盖，
-- 不再重复建 files(parent_id, deleted_at)。
-- 标签过滤（/files?tag_id= 与 /teams/:id/files?tag_id= 的
-- EXISTS (SELECT 1 FROM file_tags ft WHERE ft.file_id = files.id AND ft.tag_id = ?)）
-- 此前仅有 file_id 单列索引：按 tag 反查文件需 (tag_id, file_id) 复合索引。
CREATE INDEX IF NOT EXISTS idx_file_tags_tag_file ON file_tags(tag_id, file_id);
-- 团队空间目录列举（team_id + parent_id + deleted_at IS NULL）：
-- idx_files_team_id 仅 team_id 单列，补 (team_id, parent_id) 复合。
CREATE INDEX IF NOT EXISTS idx_files_team_parent ON files(team_id, parent_id) WHERE deleted_at IS NULL;
