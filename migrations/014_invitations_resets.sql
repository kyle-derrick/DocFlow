-- 014: 邀请制认证与密码重置一次性令牌。
-- invitations：管理员邀请（v1.0 仅邀请制注册）。email 小写归一；
-- 明文 token（32 字节随机数的 URL-safe base64）不落库，仅保存其 SHA-256
-- 十六进制哈希（64 字符小写），与 shares.token_hash 同一模式。
-- 撤销（DELETE /api/v1/admin/invitations/:id）直接删行：实体无 revoked_at，
-- 删除后 token 即不可解析。
CREATE TABLE IF NOT EXISTS invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(320) NOT NULL,
    invited_by UUID REFERENCES users(id) ON DELETE SET NULL,
    role VARCHAR(16) NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    token_hash CHAR(64) NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 幂等创建探测：未过期未接受的同邮箱邀请。
CREATE INDEX IF NOT EXISTS idx_invitations_email_active ON invitations(email) WHERE accepted_at IS NULL;
-- 过期清理（janitor/运维按 expires_at 扫描）。
CREATE INDEX IF NOT EXISTS idx_invitations_expires ON invitations(expires_at);
CREATE INDEX IF NOT EXISTS idx_invitations_created ON invitations(created_at DESC);

-- password_reset_tokens：密码重置一次性令牌（默认 30 分钟有效）。
-- used_at 原子标记（条件 UPDATE ... WHERE used_at IS NULL）保证一次性语义；
-- token_hash 同为 SHA-256 hex CHAR(64) 唯一。
CREATE TABLE IF NOT EXISTS password_reset_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash CHAR(64) NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_user ON password_reset_tokens(user_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_expires ON password_reset_tokens(expires_at);
