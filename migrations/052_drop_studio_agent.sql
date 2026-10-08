-- 052: 移除 AI 创作空间（Studio）与 Agent 创作舱功能。
-- 用户改为自行使用外部 agent（Claude Code/Codex 等）对接平台 MCP 实现创作，
-- 服务端不再保留项目注册表与沙箱任务表（历史数据随功能一并废弃）。
-- 幂等：DROP TABLE IF EXISTS；外键依赖顺序先日志后主表。
DROP INDEX IF EXISTS idx_agent_task_logs_task_id;
DROP INDEX IF EXISTS idx_agent_tasks_user_status;
DROP INDEX IF EXISTS idx_agent_tasks_root_status;
DROP TABLE IF EXISTS agent_task_logs;
DROP TABLE IF EXISTS agent_tasks;
DROP TABLE IF EXISTS studio_projects;
