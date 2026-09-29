-- 051: Studio 项目注册表（服务端化，studio_projects）。
-- 项目 = 空间 + 根目录 + 执行引擎的绑定（此前仅存浏览器 localStorage，换浏览
-- 器/清存储即丢入口）。字段与前端 StudioProject 一一对应；id 服务端生成；
-- 属主维度（user_id 级联清理），不做 (user_id, name) 唯一约束（允许重名，
-- 由前端列表按 updated_at 排序呈现）；space/folder 为弱引用快照（space_name/
-- folder_path 创建时记录用于展示，不追随重命名——与既有展示兜底逻辑一致）。
CREATE TABLE IF NOT EXISTS studio_projects (
    id             uuid        NOT NULL,
    user_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name           text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    space_id       uuid        NOT NULL,
    root_folder_id uuid        NOT NULL,
    space_name     text        NOT NULL DEFAULT '',
    folder_path    text        NOT NULL DEFAULT '',
    engine         text        NOT NULL DEFAULT 'platform' CHECK (engine IN ('platform', 'docker')),
    harness        text        NOT NULL DEFAULT '' CHECK (char_length(harness) <= 32),
    model          text        NOT NULL DEFAULT '' CHECK (char_length(model) <= 160),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_studio_projects_user_updated ON studio_projects (user_id, updated_at DESC);
