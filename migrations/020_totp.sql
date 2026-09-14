-- 020: 两步验证（TOTP，v2 设计）。
-- user_totp：每用户一行（user_id 为主键）。
-- secret 为 TOTP 共享密钥（Base32，RFC 6238，SHA1/6 位/30 秒），仅服务端
-- 与用户认证器持有。本版本为明文存储：自托管边界内认为数据库与对象存储
-- 同属信任域（与 sessions/api_tokens 等凭据哈希同级保护），应用层加密
-- （如 KMS 信封加密）列入后续迭代。
-- enabled=false 表示 setup 已开始（BeginSetup 落库）但尚未 confirm；
-- confirm 成功置 enabled=true 并写 confirmed_at。
-- recovery_codes 为 JSON 数组文本，存 10 个恢复码的 SHA-256 十六进制哈希
-- （64 字符小写，与 invitations/password_reset_tokens 同一模式）；明文仅
-- confirm 响应返回一次。命中即从数组移除（原子条件更新保证一次性语义）。
-- 禁用（DELETE /api/v1/auth/totp）直接删行：重新启用走全新 setup。
CREATE TABLE IF NOT EXISTS user_totp (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT false,
    recovery_codes TEXT NOT NULL DEFAULT '[]',
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
