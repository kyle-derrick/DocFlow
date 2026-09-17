-- 034: 用户「默认打开方式」偏好（按扩展名记录）。
-- 每用户每扩展名一行（upsert 语义）；opener 为前端打开器标识枚举
-- （office/drawio/excalidraw/text/markdown/code/web/default），枚举校验在
-- 服务层完成。删除用户时级联清理。
CREATE TABLE IF NOT EXISTS user_open_with (
    user_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ext        VARCHAR(16) NOT NULL,
    opener     VARCHAR(32) NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, ext)
);
