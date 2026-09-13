-- 010: 系统设置与管理员角色。
-- users.role：内置角色 user（默认）/ admin，RequireRole 中间件据此鉴权；
-- system_settings：运行时可调的非密钥配置（密钥类如 JWT/S3/SMTP 凭据一律走环境变量，
-- 不入库、不可经管理 API 读写）。value_json 存标量 JSON，value_type 标注类型供校验。
ALTER TABLE users ADD COLUMN IF NOT EXISTS role VARCHAR(16) NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin'));

CREATE TABLE IF NOT EXISTS system_settings (
    key TEXT PRIMARY KEY,
    value_json JSONB NOT NULL,
    value_type VARCHAR(16) NOT NULL CHECK (value_type IN ('bool', 'int', 'string')),
    description TEXT NOT NULL DEFAULT '',
    updated_by UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
