-- 045: 目录级快照（仅保存目录树元数据与文件版本指针）。
CREATE TABLE IF NOT EXISTS directory_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    root_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    creator_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_directory_snapshots_root_created ON directory_snapshots(root_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_directory_snapshots_space_created ON directory_snapshots(space_id, created_at DESC);

CREATE TABLE IF NOT EXISTS directory_snapshot_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    snapshot_id UUID NOT NULL REFERENCES directory_snapshots(id) ON DELETE CASCADE,
    relative_path TEXT NOT NULL,
    node_type VARCHAR(16) NOT NULL CHECK (node_type IN ('file', 'folder')),
    name VARCHAR(255) NOT NULL,
    source_file_id UUID REFERENCES files(id) ON DELETE SET NULL,
    source_version_id UUID REFERENCES file_versions(id) ON DELETE RESTRICT,
    content_sha256 CHAR(64),
    size BIGINT NOT NULL DEFAULT 0 CHECK (size >= 0),
    mime_type TEXT NOT NULL DEFAULT '',
    UNIQUE (snapshot_id, relative_path)
);
CREATE INDEX IF NOT EXISTS idx_directory_snapshot_entries_snapshot ON directory_snapshot_entries(snapshot_id);
CREATE INDEX IF NOT EXISTS idx_directory_snapshot_entries_source_version ON directory_snapshot_entries(source_version_id) WHERE source_version_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_directory_snapshot_entries_source_file ON directory_snapshot_entries(source_file_id) WHERE source_file_id IS NOT NULL;
