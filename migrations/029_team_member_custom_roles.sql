-- 029: 自定义角色与权限继承（设计 6.5.2/6.5.5 最小落地）。
-- team_members 增加 role_id 可空外键引用 roles(id)：现有 role 字符串保留
-- （系统角色 owner/editor/viewer），自定义角色以 role='custom' + role_id 表示。
-- 权限继承 = 团队成员身份：团队内所有文件/目录适用同一权限集，无子目录或
-- 路径级覆盖（设计 6.5.3/6.5.4 的路径级继承为后续扩展，本期纯查询实现）。
ALTER TABLE team_members ADD COLUMN IF NOT EXISTS role_id UUID REFERENCES roles(id) ON DELETE RESTRICT;

-- role 取值放宽为含 'custom'；自定义角色必须同时携带 role_id（系统角色必须为 NULL）。
ALTER TABLE team_members DROP CONSTRAINT IF EXISTS team_members_role_check;
ALTER TABLE team_members ADD CONSTRAINT team_members_role_check
    CHECK (role IN ('owner', 'editor', 'viewer', 'custom'));
ALTER TABLE team_members ADD CONSTRAINT team_members_role_id_custom_check
    CHECK ((role_id IS NULL) != (role = 'custom'));

CREATE INDEX IF NOT EXISTS idx_team_members_role_id ON team_members(role_id) WHERE role_id IS NOT NULL;
