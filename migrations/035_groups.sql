-- 035: 用户组（groups / group_members，仅 admin 管理）。
-- 用户组是管理端的组织维度（人员归属），与团队（teams，协作空间）互补：
-- 组不挂文件空间、不参与文件权限判定，仅用于人员分组管理。
-- 审计资源类型 'group'：audit_logs.resource_type 为自由文本列（migration 005，
-- 无 CHECK 约束），无需变更。

-- 用户组：name 全局唯一；created_by 为创建该组的管理员（仅审计/展示用途，
-- 不参与权限判定——组的管理权限由 /admin 路由组的 admin 角色保证）。
CREATE TABLE IF NOT EXISTS groups (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 组成员：组与用户多对多。删除组级联清空成员关系（组不删用户）；
-- 用户行删除时级联清理其组关系（本项目用户仅软禁用、不物理删除，此为兜底）。
CREATE TABLE IF NOT EXISTS group_members (
    group_id UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_group_members_user_id ON group_members(user_id);
