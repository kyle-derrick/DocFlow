-- 049: 用户个人 AI 配置（双轨制：个人自备 Provider + 平台 Provider）。
-- 每用户一行（整块 JSON upsert 语义）；prefs 结构与校验在服务层完成
--（internal/auth/aiprefs.go）：providers（≤8，openai_compatible/anthropic）
-- + default_models（chat/summary/edit/embedding）+ personas（≤20）+
-- prefer_personal。api_key 为密钥：入库明文供运行时直连使用，任何读路径
-- 均以掩码回显（api_key_configured），绝不返回明文。删除用户时级联清理。
CREATE TABLE IF NOT EXISTS user_ai_prefs (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    prefs      JSONB       NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id)
);
