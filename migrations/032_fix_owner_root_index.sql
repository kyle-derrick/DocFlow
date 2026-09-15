-- 032: 修复个人根目录唯一索引的作用域。
-- 001 的 idx_files_owner_root 仅按 owner_id 对 is_root 行去重：团队根目录
-- （同 owner、team_id 非空）插入时与该用户的个人根目录冲突（23505，
-- create team 误报 409 team name already exists，运行时才会暴露）。
-- team 维度唯一性已由 idx_files_team_root(team_id) WHERE is_root 保证；
-- owner 维度唯一性限定个人空间（team_id IS NULL）。
DROP INDEX IF EXISTS idx_files_owner_root;
CREATE UNIQUE INDEX idx_files_owner_root ON files(owner_id) WHERE is_root AND team_id IS NULL;
