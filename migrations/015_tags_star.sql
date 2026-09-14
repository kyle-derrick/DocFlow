-- 015: 标签、收藏与批量操作支撑。
-- tags：用户维度标签（v1.0 设计 3.2.8 的实现取舍：标签属于创建者 user_id，
-- 每用户内唯一 (user_id, name)，避免全局命名空间抢占；name 经服务端 NFC
-- 归一并限 64 rune、拒绝控制字符）。
CREATE TABLE IF NOT EXISTS tags (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
CREATE INDEX IF NOT EXISTS idx_tags_user_id ON tags(user_id);

-- file_tags：标签-文件多对多关联（v1.0 设计 3.2.9）。
-- 复合主键 (tag_id, file_id) 保证幂等；两端均级联删除：
-- 标签删除即解除全部关联；文件被 purge 硬删时自动清理关联
--（软删除不清理，恢复后标签保持）。
CREATE TABLE IF NOT EXISTS file_tags (
    tag_id UUID NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tag_id, file_id)
);
-- 按文件反查其标签 / 按标签 JOIN 文件列表。
CREATE INDEX IF NOT EXISTS idx_file_tags_file_id ON file_tags(file_id);

-- files.is_starred：收藏标记（v1.0 设计 3.2.1 is_starred，行级布尔）。
-- 实现取舍：团队文件为行级共享星标（不引入 per-user 星标表），切换星标
-- 仅要求读权限（authorizeFileAccess），与设计的用户维度收藏语义在
-- v1.0 简化为共享标记。
ALTER TABLE files ADD COLUMN IF NOT EXISTS is_starred BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_files_starred_active ON files(is_starred) WHERE deleted_at IS NULL;
