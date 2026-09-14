-- 022: 分享密码保护与水印（设计 6.6.2/6.17、US-003）。
-- password_hash 为 SHA-256(password || share_id) 的十六进制小写（64 字符）：
-- 以 share_id 充当盐，防止同密码跨分享的彩虹表比对；明文密码不落库、
-- 不回传（创建/详情响应仅返回 has_password 布尔标记）。
ALTER TABLE shares ADD COLUMN IF NOT EXISTS password_hash CHAR(64);

-- 密码校验通过后发放的公开访问会话（设计 3.2.20）：HttpOnly cookie 只存
-- 随机值本身，库中仅保存 SHA-256(share_id || random) 哈希（session_hash）；
-- 有效期 1 小时（应用层写入 expires_at），过期后由 janitor 清理。
CREATE TABLE IF NOT EXISTS share_access_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    share_id UUID NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    session_hash CHAR(64) NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_share_access_sessions_share_id ON share_access_sessions(share_id);

-- 水印：默认开启；watermark_text 为自定义模板（支持 {email}/{date}/{name}
-- 占位符），NULL 表示渲染时使用系统默认模板（settings share.watermark_text）。
ALTER TABLE shares ADD COLUMN IF NOT EXISTS watermark_enabled BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE shares ADD COLUMN IF NOT EXISTS watermark_text TEXT;
