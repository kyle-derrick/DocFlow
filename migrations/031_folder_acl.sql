-- 031: 路径级 ACL（设计 6.5.3/6.5.4 最小落地）。
-- folder_acl 每行一条 (folder, subject, effect, permissions) 规则：
--   - subject：user（单个用户）或 team（folder 所在团队全体成员，用于
--     整体 allow/deny 再用 user 条目覆盖个别成员）；
--   - effect：allow | deny；
--   - permissions：非空数组且 ⊆ {read, write, delete, share}。
-- 求值规则（internal/acl.Resolve）：沿 parent 链自目标文件/目录向上收集
-- 全部条目（由近及远），最近节点优先；同节点内 user 条目优先于 team 条目、
-- 同主体 deny 优先于 allow；无适用条目时回退团队角色判定（files 包注入
-- ACLResolver 接线，无 ACL 行为不变）。
-- 作用域约束（应用层保证）：仅团队作用域目录可设置（HTTP 400 拒绝个人空间），
-- 表侧不重复约束 scope 以便复用。
-- 级联清理：files 行硬删除（回收站彻底删除/团队根级联）时 ON DELETE CASCADE
-- 移除条目；目录软删（回收站）保留 ACL，恢复后继续生效。
CREATE TABLE folder_acl (
    id UUID PRIMARY KEY,
    folder_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    subject_type VARCHAR(8) NOT NULL CHECK (subject_type IN ('user', 'team')),
    subject_id UUID NOT NULL,
    effect VARCHAR(6) NOT NULL CHECK (effect IN ('allow', 'deny')),
    permissions TEXT[] NOT NULL CHECK (array_length(permissions, 1) > 0
        AND permissions <@ ARRAY['read', 'write', 'delete', 'share']::text[]),
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (folder_id, subject_type, subject_id)
);
CREATE INDEX idx_folder_acl_folder_id ON folder_acl (folder_id);
