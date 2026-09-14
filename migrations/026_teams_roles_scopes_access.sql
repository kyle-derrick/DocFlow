-- 026: team lifecycle, custom role metadata, PAT scopes and recent access.
ALTER TABLE teams ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_teams_active ON teams(id) WHERE deleted_at IS NULL;
CREATE TABLE IF NOT EXISTS roles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name VARCHAR(64) NOT NULL,
    permissions JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(team_id, name)
);
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scopes JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE files ADD COLUMN IF NOT EXISTS last_access_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_files_last_access ON files(owner_id, last_access_at DESC) WHERE deleted_at IS NULL;
