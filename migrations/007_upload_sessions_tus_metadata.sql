-- tus 1.0.0 creation 扩展：保存客户端上传时的原始 Upload-Metadata 头，供 HEAD 回显。
ALTER TABLE upload_sessions ADD COLUMN IF NOT EXISTS metadata TEXT NOT NULL DEFAULT '';
