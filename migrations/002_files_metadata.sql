CREATE EXTENSION IF NOT EXISTS ltree;

ALTER TABLE files ALTER COLUMN parent_id DROP NOT NULL;
ALTER TABLE files ADD COLUMN IF NOT EXISTS tree_path LTREE;
ALTER TABLE files ADD COLUMN IF NOT EXISTS team_id UUID;
ALTER TABLE files ADD COLUMN IF NOT EXISTS current_version_id UUID;
ALTER TABLE files ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN IF NOT EXISTS is_public BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE files ADD COLUMN IF NOT EXISTS view_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN IF NOT EXISTS download_count BIGINT NOT NULL DEFAULT 0;

ALTER TABLE files DROP CONSTRAINT IF EXISTS files_parent_id_fkey;
ALTER TABLE files ADD CONSTRAINT files_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES files(id);
ALTER TABLE files DROP CONSTRAINT IF EXISTS files_root_parent_check;
ALTER TABLE files ADD CONSTRAINT files_root_parent_check CHECK (is_root OR parent_id IS NOT NULL);
CREATE INDEX IF NOT EXISTS idx_files_parent_id ON files(parent_id);
CREATE INDEX IF NOT EXISTS idx_files_owner_id ON files(owner_id);
CREATE INDEX IF NOT EXISTS idx_files_team_id ON files(team_id);
CREATE INDEX IF NOT EXISTS idx_files_deleted_at ON files(deleted_at);
CREATE INDEX IF NOT EXISTS idx_files_tree_path ON files USING GIST(tree_path);
DROP INDEX IF EXISTS idx_files_parent_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_files_parent_name_active ON files(parent_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS object_blobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sha256 CHAR(64) NOT NULL UNIQUE,
    storage_key TEXT NOT NULL,
    size BIGINT NOT NULL CHECK (size >= 0),
    mime_type TEXT NOT NULL,
    ref_count BIGINT NOT NULL DEFAULT 0 CHECK (ref_count >= 0),
    status VARCHAR(16) NOT NULL DEFAULT 'created' CHECK (status IN ('created','scanning','available','quarantined','failed','deleting')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS file_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    object_blob_id UUID NOT NULL REFERENCES object_blobs(id),
    content_sha256 CHAR(64) NOT NULL,
    size BIGINT NOT NULL CHECK (size >= 0),
    comment TEXT,
    user_id UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (file_id, version)
);
ALTER TABLE files DROP CONSTRAINT IF EXISTS files_current_version_id_fkey;
ALTER TABLE files ADD CONSTRAINT files_current_version_id_fkey FOREIGN KEY (current_version_id) REFERENCES file_versions(id);
CREATE INDEX IF NOT EXISTS idx_file_versions_file_id ON file_versions(file_id);
CREATE INDEX IF NOT EXISTS idx_object_blobs_status ON object_blobs(status);
