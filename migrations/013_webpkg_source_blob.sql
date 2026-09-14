-- 013: web_packages 版本失效校验。
-- source_blob_sha256 记录解包时源文件当前版本的 blob sha256（可空，旧数据不回填）：
--   - ExtractForFile 解包时写入当前解包源（成功与失败均记录）；
--   - Resolve 比对包记录与文件当前版本 blob sha，不一致（版本已更替）或为空
--     （migration 013 前的旧行）时视为不存在，防止继续提供旧版本内容；
--   - AutoExtract 发现「已有 zip 包但新版本非 zip 候选」时将行置 blocked
--     （error='superseded by non-archive version'）。
ALTER TABLE web_packages ADD COLUMN IF NOT EXISTS source_blob_sha256 CHAR(64);
