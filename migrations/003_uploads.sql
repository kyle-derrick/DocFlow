CREATE TABLE IF NOT EXISTS upload_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id UUID NOT NULL REFERENCES files(id),
    tus_id TEXT NOT NULL UNIQUE,
    name VARCHAR(255) NOT NULL,
    storage_key TEXT NOT NULL,
    "offset" BIGINT NOT NULL DEFAULT 0 CHECK ("offset" >= 0),
    size BIGINT NOT NULL CHECK (size >= 0),
    expected_sha256 CHAR(64),
    status VARCHAR(16) NOT NULL DEFAULT 'uploading' CHECK (status IN ('uploading','verifying','scanning','available','quarantined','failed')),
    expires_at TIMESTAMPTZ NOT NULL,
    retry_count INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_user_status ON upload_sessions(user_id, status);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_expires_at ON upload_sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_parent_id ON upload_sessions(parent_id);
