-- 033: 管理页面 HTTPS 运行时切换的持久化状态（单行表，id 恒为 1）。
-- mode: http（明文，本地验证默认）| auto（域名 + ACME 自动签发）
--       | internal（域名/IP + Caddy 内部 CA 自签）。
-- 状态经 Caddy admin API（POST /load）热下发到入口，本表仅记录偏好，
-- caddy 容器单独重启后由 backend 启动时重新下发。
CREATE TABLE IF NOT EXISTS tls_state (
    id         integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    mode       text    NOT NULL DEFAULT 'http',
    domain     text    NOT NULL DEFAULT '',
    updated_by uuid,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO tls_state (id, mode, domain) VALUES (1, 'http', '')
    ON CONFLICT (id) DO NOTHING;
