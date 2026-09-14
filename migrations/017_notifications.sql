-- 017: 站内通知与通知偏好（v1.0 设计 3.2.x）。
-- notifications：站内通知（user 维度，本人可见）。type 为事件类型
-- （v1.0 范围：upload.completed / upload.quarantined / share.accessed /
-- file.updated），resource_id 指向关联资源（文件 ID 等，可空）。
-- 已读通知超过 90 天由 janitor 清理（sweepNotifications）。
CREATE TABLE IF NOT EXISTS notifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type VARCHAR(32) NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    resource_id UUID,
    is_read BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at TIMESTAMPTZ
);
-- 铃铛未读数与列表分页（user_id, is_read, created_at DESC）。
CREATE INDEX IF NOT EXISTS idx_notifications_user_unread
    ON notifications(user_id, is_read, created_at DESC);

-- user_notification_preferences：通知偏好（无记录 = 默认开启）。
-- PK (user_id, event_type) 支持 upsert（enabled 覆盖更新）。
CREATE TABLE IF NOT EXISTS user_notification_preferences (
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_type VARCHAR(32) NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT true,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, event_type)
);
