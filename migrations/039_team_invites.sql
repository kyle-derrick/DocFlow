-- 039: 团队邀请（v1.7.1 成员管理完善）。
-- 邮箱邀请：owner/admin 向邮箱发出入队邀请，生成一次性 token（仅存
-- SHA-256 哈希，明文只在创建响应/复制链接时可见一次），受邀者登录后经
-- POST /api/v1/team-invites/join/:token 接受（邮箱须匹配，7 天有效期）。
CREATE TABLE IF NOT EXISTS team_invites (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    email VARCHAR(320) NOT NULL,
    role VARCHAR(16) NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member_share', 'member', 'guest')),
    token_hash CHAR(64) NOT NULL UNIQUE,
    invited_by UUID REFERENCES users(id) ON DELETE SET NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_team_invites_team_id ON team_invites(team_id);
CREATE INDEX IF NOT EXISTS idx_team_invites_email ON team_invites(email);
