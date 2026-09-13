-- 009: 文件版本管理（v1.0）——上传链路支持「覆盖为新版本」。
-- 会话携带 target_file_id 时，Complete 不再创建新 File，
-- 而是向目标文件追加 FileVersion 并按 MAX_VERSIONS_PER_FILE 裁剪历史版本。
-- 可空列：常规上传会话不受影响。
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS target_file_id UUID REFERENCES files(id);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_target_file_id ON upload_sessions(target_file_id) WHERE target_file_id IS NOT NULL;
