-- 042: 换绑邮箱验证码（账号安全，v2.4 整改项 14）。
-- 流程：验证密码 → 请求换绑（本表落一行 6 位数字验证码的哈希 + 新邮箱，
-- 邮件投递验证码）→ 提交验证码确认（原子消费 + 更新 users.email）。
-- code_hash = SHA-256(user_id || ":" || code)，10 分钟有效、一次性
-- （used_at 原子条件更新）；同用户重复请求多行并存，消费时任一未用未过期
-- 行命中即生效（后请求的行优先，created_at 倒序）。
CREATE TABLE IF NOT EXISTS email_change_codes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    new_email TEXT NOT NULL,
    code_hash CHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_email_change_codes_user ON email_change_codes(user_id, created_at DESC);
