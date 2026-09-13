-- 012: ONLYOFFICE 回调幂等持久化。
-- onlyoffice_callbacks 记录已受理的 DocumentServer 保存回调幂等键
-- (file_id, document_key, callback_url)（原进程内内存表迁移而来）：
--   - 回调验签通过后、处理前先 INSERT（ON CONFLICT DO NOTHING）抢占幂等键，
--     冲突即视为已处理直接回 {"error":0}，不重复建版本（跨重启/多实例生效）；
--   - status 存回调原始状态（如 "2"/"6"），result 存处理结果（0 成功 / 1 失败）；
--   - 处理失败时回滚（DELETE）该记录，允许 DocumentServer 重试。
CREATE TABLE IF NOT EXISTS onlyoffice_callbacks (
    id BIGSERIAL PRIMARY KEY,
    file_id UUID NOT NULL,
    document_key VARCHAR(255) NOT NULL,
    callback_url TEXT NOT NULL,
    status VARCHAR(16) NOT NULL,
    result VARCHAR(8) NOT NULL CHECK (result IN ('0', '1')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_onlyoffice_callbacks_idem UNIQUE (file_id, document_key, callback_url)
);
CREATE INDEX IF NOT EXISTS idx_onlyoffice_callbacks_file ON onlyoffice_callbacks(file_id);
