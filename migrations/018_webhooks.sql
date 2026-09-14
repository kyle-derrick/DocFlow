-- 018: Webhook 通知渠道（v1.1）。
-- webhooks：用户自助注册的出站回调 URL，站内通知事件（与 notify 相同四类：
-- upload.completed / upload.quarantined / share.accessed / file.updated）发生时
-- POST {event,timestamp,data} JSON 并附 X-DocFlow-Signature HMAC 签名头。
-- URL 的 http(s) 方案/主机等格式校验在应用层执行（内网/localhost 允许，
-- 自托管场景）；events 白名单校验同样在应用层（notify.EventTypes）。
-- 投递语义：超时 10s、内部重试 3 次（1s/5s/25s 退避，经 task:webhook-delivery
-- 队列异步执行）；last_status 为最近一次投递 HTTP 状态码（0=传输层失败，NULL=从未
-- 投递），failure_count 连续失败计数（成功清零），连续 10 次失败自动 enabled=false。
-- URL 唯一性：(user_id, url)——同一用户不重复注册同一回调地址。
-- 设计取舍：secret 以明文存储（设计文档原为 secret_hash CHAR(64)）——
-- 出站 HMAC 签名必须持有明文密钥，只存哈希无法构造签名（与入站认证的
-- API token 不同，secret 不用于服务端验证，仅用于对请求体签名）；
-- 自托管单机场景接受明文落库，泄露面即数据库本身。
CREATE TABLE IF NOT EXISTS webhooks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    events TEXT[] NOT NULL CHECK (array_length(events, 1) > 0),
    secret TEXT NOT NULL,
    last_status SMALLINT,
    last_delivered_at TIMESTAMPTZ,
    failure_count INT NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, url)
);
-- 通知分发时按属主检索（events @> ARRAY[事件类型] 走本索引 + 过滤）。
CREATE INDEX IF NOT EXISTS idx_webhooks_user ON webhooks(user_id);
