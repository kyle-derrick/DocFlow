CREATE TABLE IF NOT EXISTS document_comments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    version_id UUID NOT NULL REFERENCES file_versions(id) ON DELETE CASCADE,
    parent_id UUID REFERENCES document_comments(id) ON DELETE CASCADE,
    anchor_from INTEGER NOT NULL CHECK (anchor_from >= 0),
    anchor_to INTEGER NOT NULL CHECK (anchor_to >= anchor_from),
    quote TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL CHECK (char_length(body) BETWEEN 1 AND 4000),
    author_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status VARCHAR(16) NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (anchor_to <= 1000000)
);
CREATE INDEX IF NOT EXISTS idx_document_comments_file_created ON document_comments(file_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_document_comments_parent ON document_comments(parent_id);
