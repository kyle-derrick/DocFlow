-- 036: 多文件打包分享（批量分享 = 一个目录式链接）。
-- shares.is_bundle 标记打包分享：file_id 指向打包条目的公共父目录（锚点，
-- 目录式访问语义复用目录分享的 tree/raw/zip 链路）；实际可见条目由
-- share_files 连接表限定——锚点目录的其余子项不对外暴露。
ALTER TABLE shares ADD COLUMN IF NOT EXISTS is_bundle BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS share_files (
    share_id UUID NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (share_id, file_id)
);
CREATE INDEX IF NOT EXISTS idx_share_files_file_id ON share_files(file_id);
