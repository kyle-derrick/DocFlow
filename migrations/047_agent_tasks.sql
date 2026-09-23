-- 047: Docker Agent 创作舱 v1（默认由 agent.enabled=false 关闭）
CREATE TABLE IF NOT EXISTS agent_tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    space_id UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    root_folder_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    snapshot_id UUID NOT NULL REFERENCES directory_snapshots(id) ON DELETE RESTRICT,
    image TEXT NOT NULL,
    status VARCHAR(16) NOT NULL CHECK (status IN ('queued','running','succeeded','failed','cancelled','rolled_back','applied')),
    prompt TEXT NOT NULL,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    error TEXT,
    workspace_path TEXT,
    workspace_expires_at TIMESTAMPTZ,
    diff_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    apply_result_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    apply_diff_hash TEXT NOT NULL DEFAULT '',
    applied_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_user_created ON agent_tasks(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_tasks_status ON agent_tasks(status);
CREATE TABLE IF NOT EXISTS agent_task_logs (
    id BIGSERIAL PRIMARY KEY,
    task_id UUID NOT NULL REFERENCES agent_tasks(id) ON DELETE CASCADE,
    stream VARCHAR(16) NOT NULL,
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_task_logs_task_created ON agent_task_logs(task_id, created_at);
