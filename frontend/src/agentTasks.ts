import { api } from './api'

export interface AgentDiff { path: string; action: string; size: number; sha256: string }
export interface AgentTask { id: string; status: string; prompt: string; image: string; snapshot_id: string; error?: string; created_at: string; workspace_expires_at?: string; apply_diff_hash?: string; applied_at?: string }
export interface AgentApplyResult { path: string; status: string; error?: string }
export interface AgentLog { id: number; task_id: string; stream: string; content: string; created_at: string }

export function probeAgent(): Promise<boolean> {
  return api<{ tasks: AgentTask[] }>('/api/v1/agent-tasks').then(() => true).catch(() => false)
}
/** createAgentTask 可选项：请求级执行引擎与模型意图（优先于平台配置）。 */
export interface CreateAgentTaskOptions {
  /** 执行引擎终值（claude-code/pi/builtin；空/未传 = 跟随平台 agent.harness）。 */
  harness?: string
  /** 模型意图（provider/model；空/未传 = 平台默认——网关侧按平台默认模型执行，仅记录意图）。 */
  model?: { provider_id: string; model_id: string }
}

export function createAgentTask(folderId: string, prompt: string, image = '', timeoutSeconds = 0, opts: CreateAgentTaskOptions = {}): Promise<AgentTask> {
  const body: Record<string, unknown> = { prompt, image, timeout_seconds: timeoutSeconds }
  // 仅在携带具体值时下发（空 = 跟随平台；后端不接受 auto）。
  if (opts.harness) body.harness = opts.harness
  if (opts.model && opts.model.model_id) body.model = opts.model
  return api<AgentTask>(`/api/v1/folders/${folderId}/agent-tasks`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })
}
export function getAgentTask(id: string): Promise<{ task: AgentTask; logs: AgentLog[]; dry_run: boolean; diff: AgentDiff[] }> { return api(`/api/v1/agent-tasks/${id}`) }
export function applyAgentTask(id: string, selectedPaths: string[], expectedSnapshotId: string, expectedDiffHash: string, allowDeletes = false): Promise<{ results: AgentApplyResult[]; status: string; rollback_hint?: string }> { return api(`/api/v1/agent-tasks/${id}/apply`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ selected_paths: selectedPaths, allow_deletes: allowDeletes, expected_snapshot_id: expectedSnapshotId, expected_diff_hash: expectedDiffHash }) }) }
export function discardAgentTask(id: string): Promise<unknown> { return api(`/api/v1/agent-tasks/${id}/discard`, { method: 'POST' }) }
export function getAgentTaskLogs(id: string): Promise<{ logs: AgentLog[] }> { return api(`/api/v1/agent-tasks/${id}/logs`) }
export function cancelAgentTask(id: string): Promise<unknown> { return api(`/api/v1/agent-tasks/${id}/cancel`, { method: 'POST' }) }
export function rollbackAgentTask(id: string): Promise<unknown> { return api(`/api/v1/agent-tasks/${id}/rollback`, { method: 'POST' }) }
