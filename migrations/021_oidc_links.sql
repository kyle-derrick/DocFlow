-- 021: OIDC 单点登录（v2 设计）。
-- oidc_links：IdP 身份（sub，issuer 内稳定标识）→ DocFlow 用户的关联。
-- 首次 SSO 登录时按 email 匹配既有用户（或自动开户，OIDC_AUTO_PROVISION）
-- 后写入本表；此后 IdP 侧 email 变更仍可凭 sub 登录。
-- sub 为主键：单 issuer 部署下即全局唯一；多 IdP 预留时须扩为
-- (issuer, sub) 复合键（本版本不拆）。
CREATE TABLE IF NOT EXISTS oidc_links (
    sub TEXT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issuer TEXT NOT NULL,
    linked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_oidc_links_user_id ON oidc_links(user_id);
