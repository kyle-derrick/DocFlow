-- 025: asynchronous thumbnail generation contract; original blobs are never used as thumbnails.
CREATE TABLE IF NOT EXISTS thumbnail_jobs (
    id UUID PRIMARY KEY,
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    status VARCHAR(16) NOT NULL CHECK (status IN ('pending','processing','ready','unsupported','failed')),
    thumbnail_url TEXT,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(file_id)
);
CREATE INDEX IF NOT EXISTS idx_thumbnail_jobs_status ON thumbnail_jobs(status);
