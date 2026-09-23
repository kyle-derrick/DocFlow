-- 050: AI 记忆（用户维度长期偏好，ai_memory）。
-- v1 仅手动维护（kind='manual'，POST /api/v1/ai/memory 本人添加；'auto'
-- 为自动提取预留值，当前版本不写入）；/ai/chat 的 include_memory=true 时
-- 取该用户最近 20 条拼入 system 上下文（「以下是用户的长期偏好记忆…」）。
-- content ≤2000 字符由服务层校验（auth.ValidateAIMemory），DB 侧同步
-- CHECK 兜底；删除用户级联清理（ON DELETE CASCADE）。
CREATE TABLE IF NOT EXISTS ai_memory (
    id         uuid        NOT NULL,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text        NOT NULL CHECK (kind IN ('manual', 'auto')),
    content    text        NOT NULL CHECK (char_length(content) <= 2000),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_ai_memory_user_created ON ai_memory (user_id, created_at DESC);
