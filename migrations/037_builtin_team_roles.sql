-- 037: 团队角色内置化（五级 owner/admin/member_share/member/guest，删除自定义角色）。
-- 存量数据迁移到最接近的内置级：
--   自定义角色（roles.permissions JSON）：
--     admin 勾选            -> admin
--     write 勾选 + share 勾选 -> member_share
--     write 勾选（无 share）  -> member
--     仅 read               -> guest
--     其余/兜底             -> member_share
--   旧系统角色：editor -> member_share；viewer -> guest；owner 保持不变。
-- 随后删除 team_members.role_id 与 roles 表（自定义角色 CRUD API 已删除），
-- folder_acl 中 subject_type='role' 的条目一并清除（主体类型不再存在）。
UPDATE team_members tm
SET role = COALESCE(
    (SELECT CASE
        WHEN COALESCE((r.permissions ->> 'admin')::boolean, false) THEN 'admin'
        WHEN COALESCE((r.permissions ->> 'write')::boolean, false) THEN
            CASE WHEN COALESCE((r.permissions ->> 'share')::boolean, false)
                 THEN 'member_share' ELSE 'member' END
        WHEN COALESCE((r.permissions ->> 'read')::boolean, false) THEN 'guest'
        ELSE 'member_share'
    END
    FROM roles r
    WHERE r.id = tm.role_id),
    'member_share')
WHERE tm.role = 'custom' AND tm.role_id IS NOT NULL;

UPDATE team_members SET role = 'member_share' WHERE role = 'editor';
UPDATE team_members SET role = 'guest' WHERE role = 'viewer';

-- 路径级 ACL：自定义角色主体类型随之移除（后端白名单仅剩 user/team）。
DELETE FROM folder_acl WHERE subject_type = 'role';

ALTER TABLE team_members DROP CONSTRAINT IF EXISTS team_members_role_id_custom_check;
ALTER TABLE team_members DROP CONSTRAINT IF EXISTS team_members_role_check;
DROP INDEX IF EXISTS idx_team_members_role_id;
ALTER TABLE team_members DROP COLUMN IF EXISTS role_id;
ALTER TABLE team_members ADD CONSTRAINT team_members_role_check
    CHECK (role IN ('owner', 'admin', 'member_share', 'member', 'guest'));

DROP TABLE IF EXISTS roles;
