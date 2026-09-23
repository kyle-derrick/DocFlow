CREATE TABLE IF NOT EXISTS webdav_tokens (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name varchar(100) NOT NULL,
    token_hash char(64) NOT NULL UNIQUE,
    last_used_at timestamptz,
    expires_at timestamptz,
    created_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS idx_webdav_tokens_user ON webdav_tokens(user_id);
