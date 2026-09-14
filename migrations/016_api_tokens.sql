-- 016: 个人访问令牌（PAT，v1.0 设计 3.2.12 / 8.2）。
-- api_tokens：用户自助创建的 Bearer 凭证（供脚本/CI 集成），无 session
-- 语义（不支持 refresh）。明文 token 为 `dfpat_` + 32 字节随机数的 URL-safe
-- base64（43 字符），仅创建响应返回一次；库中只存 SHA-256 十六进制哈希
-- （64 字符小写，与 invitations/password_reset_tokens 同一模式）。
-- prefix 为明文前 14 字符（`dfpat_` + 随机体前 8 字符）：认证按 prefix
-- 索引定位候选行，再比对全量哈希；长度 = 6 + 8（VARCHAR(14) 恰好容纳，
-- 任务描述中的 VARCHAR(8) 不足以存储完整定位前缀，按 14 实现）。
-- expires_at 可空（NULL = 永久）；revoked_at 置位即撤销，认证立即拒绝；
-- 过期/撤销超 30 天的行由 janitor 清理（sweepTokens）。
CREATE TABLE IF NOT EXISTS api_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(100) NOT NULL,
    token_hash CHAR(64) NOT NULL UNIQUE,
    prefix VARCHAR(14) NOT NULL,
    last_used_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 认证路径按 prefix 定位候选行。
CREATE INDEX IF NOT EXISTS idx_api_tokens_prefix ON api_tokens(prefix);
-- 设置页按属主列举未撤销令牌。
CREATE INDEX IF NOT EXISTS idx_api_tokens_user_active ON api_tokens(user_id) WHERE revoked_at IS NULL;
-- janitor 过期/撤销清理扫描。
CREATE INDEX IF NOT EXISTS idx_api_tokens_expires ON api_tokens(expires_at);
