-- 008: 团队与私有分享授权（设计文档 3.2.2/3.2.3/3.2.7/3.2.14）。
-- 权限判定本期以查询实现（casbin_rule 表保留，后续可切换 Casbin 适配器）。

-- 团队：name 全局唯一，owner_id 为团队创建者（保留全部管理权）。
CREATE TABLE IF NOT EXISTS teams (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_teams_owner_id ON teams(owner_id);

-- 团队成员：owner 管理成员，editor 可写团队空间，viewer 只读；创建团队时自动写入 owner 成员。
CREATE TABLE IF NOT EXISTS team_members (
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role VARCHAR(16) NOT NULL DEFAULT 'viewer' CHECK (role IN ('owner', 'editor', 'viewer')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_team_members_user_id ON team_members(user_id);

-- 私有分享：visibility 区分公开链接与私有分享；私有分享不生成公开 token，
-- token_hash 允许为空（PostgreSQL UNIQUE 约束允许多个 NULL，公开 token 仍全局唯一）。
ALTER TABLE shares ADD COLUMN IF NOT EXISTS visibility VARCHAR(16) NOT NULL DEFAULT 'public' CHECK (visibility IN ('public', 'private'));
ALTER TABLE shares ALTER COLUMN token_hash DROP NOT NULL;

-- 私有分享显式授权：指定用户 / 指定团队（团队成员实时判定，不在此冗余快照）。
CREATE TABLE IF NOT EXISTS share_users (
    share_id UUID NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (share_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_share_users_user_id ON share_users(user_id);

CREATE TABLE IF NOT EXISTS share_teams (
    share_id UUID NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (share_id, team_id)
);
CREATE INDEX IF NOT EXISTS idx_share_teams_team_id ON share_teams(team_id);

-- 团队根目录复用 files 表（scope_type='team'、team_id 非空、is_root=true、owner_id=团队创建者）；
-- files.team_id 由全量索引改为部分索引（仅团队文件），并保证每个团队至多一个根目录。
DROP INDEX IF EXISTS idx_files_team_id;
CREATE INDEX IF NOT EXISTS idx_files_team_id ON files(team_id) WHERE team_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_files_team_root ON files(team_id) WHERE is_root;
