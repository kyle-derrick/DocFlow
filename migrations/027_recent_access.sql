-- 027: recent access compatibility migration.
ALTER TABLE files ADD COLUMN IF NOT EXISTS last_access_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_files_last_access ON files(owner_id, last_access_at DESC) WHERE deleted_at IS NULL;
