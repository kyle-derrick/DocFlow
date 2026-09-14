-- 023: 文件访问事件（分享访问统计，设计 6.6.3/3.2.20）。
-- 公开分享下载/预览成功时写入：action ∈ download|preview。
-- ip_hash = SHA-256(盐 || ip)：实例级静态盐（ACCESS_SALT，缺省由 JWT secret
-- 派生），明文 IP 不落库；ip_prefix 为展示用脱敏前缀（IPv4 形如 203.0.*）。
-- user_agent 入库前截断 512 字节。事件由 janitor 按 retention.access_events_days
--（默认 90 天）清理。
CREATE TABLE IF NOT EXISTS file_access_events (
    id BIGSERIAL PRIMARY KEY,
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    share_id UUID REFERENCES shares(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    action VARCHAR(16) NOT NULL CHECK (action IN ('download', 'preview')),
    ip_hash CHAR(64) NOT NULL,
    ip_prefix VARCHAR(24) NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_file_access_events_share_created ON file_access_events(share_id, created_at);
CREATE INDEX IF NOT EXISTS idx_file_access_events_file_created ON file_access_events(file_id, created_at);
