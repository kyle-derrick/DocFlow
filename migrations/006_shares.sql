-- 006: 公开链接分享（MVP 仅支持 type=file 的文件分享；share_users/share_teams 私有分享不在本次范围）。
-- 明文 token（32 字节随机数的 URL-safe base64）不落库，仅保存其 SHA-256 十六进制哈希（64 字符小写）。
CREATE TABLE IF NOT EXISTS shares (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    token_hash CHAR(64) NOT NULL UNIQUE,
    permission VARCHAR(16) NOT NULL DEFAULT 'view' CHECK (permission IN ('view', 'download')),
    expires_at TIMESTAMPTZ,
    max_downloads INTEGER CHECK (max_downloads IS NULL OR max_downloads >= 0),
    download_count INTEGER NOT NULL DEFAULT 0 CHECK (download_count >= 0),
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_shares_owner_created ON shares(owner_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_shares_file_id ON shares(file_id);
-- files.is_public（002 已加列）为冗余辅助字段，由应用层维护：文件存在有效公开分享时为 true。
