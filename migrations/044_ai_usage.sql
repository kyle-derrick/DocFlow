-- 044: AI 用量记录表（AI 能力第一版）。
-- 每次对话/摘要补全记一行：user/provider/model/tokens/ms/created_at，
-- 管理面板按用户聚合统计（GET /api/v1/admin/ai/usage，日期过滤）。
-- 无外键（users 软删/清洗不级联用量明细），user_id 仅索引。
CREATE TABLE IF NOT EXISTS ai_usage (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    provider_id varchar(64) NOT NULL,
    model varchar(128) NOT NULL,
    prompt_tokens integer NOT NULL DEFAULT 0,
    completion_tokens integer NOT NULL DEFAULT 0,
    duration_ms bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ai_usage_user_created ON ai_usage(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ai_usage_created ON ai_usage(created_at DESC);
