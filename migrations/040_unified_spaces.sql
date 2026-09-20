-- 040: 统一空间模型（spaces 取代 teams；个人/团队双轨合并为唯一容器「空间」）。
-- 本迁移为清库重建基线的一部分：不做存量数据迁移，直接 DROP 旧结构后按新模型重建
-- （测试数据可清库，见部署说明 docker compose down + 清 postgres 卷）。
--
-- 模型要点：
--   - 每个用户注册即自动获得一个默认空间「{name}的空间」（is_default，可改名不可删除）；
--   - 角色五级沿用：owner/admin/member_share/member/guest；
--   - 用户组与直接成员并存时权限取最高（space_members ∪ space_group_members）；
--   - 配额挂空间：spaces.quota_bytes（0=不限），admin 全局默认 space.default_quota；
--   - files.team_id → space_id（所有文件必须属于某空间），scope_type（personal/team）列移除。

-- 1) 删除团队模型旧结构（顺序先删引用方）。
DROP TABLE IF EXISTS share_teams;
DROP TABLE IF EXISTS team_invites;
DROP TABLE IF EXISTS team_members;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS teams;

-- folder_acl 主体类型 team → space。
ALTER TABLE folder_acl DROP CONSTRAINT IF EXISTS folder_acl_subject_type_check;
UPDATE folder_acl SET subject_type = 'space' WHERE subject_type = 'team';
ALTER TABLE folder_acl ADD CONSTRAINT folder_acl_subject_type_check
    CHECK (subject_type IN ('user', 'space'));

-- 2) 空间模型新表。
CREATE TABLE spaces (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    quota_bytes BIGINT NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    is_default BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_spaces_owner_id ON spaces(owner_id);
CREATE INDEX idx_spaces_active ON spaces(id) WHERE deleted_at IS NULL;
-- 每个用户至多一个未删除的默认空间。
CREATE UNIQUE INDEX idx_spaces_default_per_user ON spaces(owner_id) WHERE is_default AND deleted_at IS NULL;

-- 直接成员：五级内置角色（owner 仅经所有权转让产生）。
CREATE TABLE space_members (
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role VARCHAR(16) NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member_share', 'member', 'guest')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, user_id)
);
CREATE INDEX idx_space_members_user_id ON space_members(user_id);

-- 用户组成员：可授予角色不含 owner（所有权仅经转让产生）。
CREATE TABLE space_group_members (
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    group_id UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    role VARCHAR(16) NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member_share', 'member', 'guest')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, group_id)
);
CREATE INDEX idx_space_group_members_group_id ON space_group_members(group_id);

-- 空间邮箱邀请（语义同原 team_invites，token 仅存 SHA-256 哈希）。
CREATE TABLE space_invites (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    email VARCHAR(320) NOT NULL,
    role VARCHAR(16) NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member_share', 'member', 'guest')),
    token_hash CHAR(64) NOT NULL UNIQUE,
    invited_by UUID REFERENCES users(id) ON DELETE SET NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_space_invites_space_id ON space_invites(space_id);
CREATE INDEX idx_space_invites_email ON space_invites(email);

-- 私有分享授权：share_teams → share_spaces。
CREATE TABLE share_spaces (
    share_id UUID NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (share_id, space_id)
);
CREATE INDEX idx_share_spaces_space_id ON share_spaces(space_id);

-- 3) files：team_id → space_id（统一空间，所有文件挂空间）；scope_type 移除。
DROP INDEX IF EXISTS idx_files_team_id;
DROP INDEX IF EXISTS idx_files_team_root;
DROP INDEX IF EXISTS idx_files_owner_root;
ALTER TABLE files DROP CONSTRAINT IF EXISTS files_team_id_fkey;
ALTER TABLE files RENAME COLUMN team_id TO space_id;
ALTER TABLE files DROP COLUMN IF EXISTS scope_type;
ALTER TABLE files ALTER COLUMN space_id SET NOT NULL;
ALTER TABLE files ADD CONSTRAINT files_space_id_fkey
    FOREIGN KEY (space_id) REFERENCES spaces(id) ON DELETE CASCADE;
-- 每个空间至多一个根目录。
CREATE UNIQUE INDEX idx_files_space_root ON files(space_id) WHERE is_root;
CREATE INDEX idx_files_space_id ON files(space_id) WHERE deleted_at IS NULL;

-- 4) 全文检索索引文档：team_id → space_id。
ALTER TABLE file_search_docs RENAME COLUMN team_id TO space_id;
