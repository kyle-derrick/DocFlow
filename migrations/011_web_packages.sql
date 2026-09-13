-- 011: 网页包（zip）安全预览。
-- web_packages 记录文件（当前版本为 zip）解包后的静态站点索引：
--   - file_id 唯一（网页包挂在 file 的当前版本）；
--   - public_id 为 43 字符 URL-safe 随机串（防枚举），解包对象存储于
--     webpkg/<public_id>/ 前缀下，经 GET /content/<public_id>/<path> 提供；
--   - status：extracting（解包中）| ready（可预览）| failed（IO 失败）|
--     blocked（未通过安全校验，如 Zip Slip/符号链接/超限/缺 index.html）。
CREATE TABLE IF NOT EXISTS web_packages (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id UUID NOT NULL UNIQUE REFERENCES files(id) ON DELETE CASCADE,
    public_id CHAR(43) NOT NULL UNIQUE,
    entry_count INT NOT NULL DEFAULT 0,
    total_size BIGINT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'extracting' CHECK (status IN ('extracting', 'ready', 'failed', 'blocked')),
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
