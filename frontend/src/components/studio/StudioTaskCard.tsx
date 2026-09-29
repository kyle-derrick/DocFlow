// Studio docker 任务卡（Trae 式内联 agent 输出）：在对话流内展示完整任务
// 生命周期——状态徽标 + 实时日志流（stdout/stderr/system 按序滚动）+ 终态
// 文件变更清单（新增/修改/删除着色，可展开行内操作）+ apply/discard/rollback
// 操作按钮。替代旧「跳独立评审面板」的交互（v3.6）。
import { useCallback, useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button, Tooltip } from 'antd'
import { Bot, Check, ChevronDown, ChevronRight, FileDiff, FileMinus, FilePlus, RotateCcw, Trash2 } from 'lucide-react'
import { applyAgentTask, cancelAgentTask, discardAgentTask, getAgentTask, rollbackAgentTask } from '../../agentTasks'
import type { AgentDiff, AgentTask } from '../../agentTasks'

const STATUS: Record<string, { label: string; cls: string }> = {
  queued: { label: '排队中', cls: 'wait' },
  running: { label: '运行中', cls: 'run' },
  succeeded: { label: '待确认', cls: 'ok' },
  applied: { label: '已写入', cls: 'ok' },
  failed: { label: '失败', cls: 'bad' },
  cancelled: { label: '已取消', cls: 'bad' },
  discarded: { label: '已丢弃', cls: 'bad' },
  rolled_back: { label: '已回滚', cls: 'bad' },
}
const isTerminal = (s: string) => s !== 'queued' && s !== 'running'
const isPending = (s: string) => s === 'succeeded'

interface TaskLogRow {
  ID: number
  Stream: string
  Content: string
  CreatedAt: string
}

export default function StudioTaskCard({ zh, taskId, prompt, task, onApplied }: {
  zh: boolean
  taskId: string
  prompt: string
  /** 父层任务状态（5s 轮询同步；undefined = 不在列表中，自行拉一次）。 */
  task?: AgentTask
  onApplied: () => void
  onOpenInTab?: (f: { id: string; name: string }) => void
}) {
  const { message } = AntdApp.useApp()
  const [status, setStatus] = useState(task?.status ?? 'queued')
  const [error, setError] = useState(task?.error ?? '')
  const [logs, setLogs] = useState<TaskLogRow[]>([])
  const [diff, setDiff] = useState<AgentDiff[] | null>(null)
  const [snapshotId, setSnapshotId] = useState(task?.snapshot_id ?? '')
  const [diffHash, setDiffHash] = useState('')
  const [logsOpen, setLogsOpen] = useState(true)
  const [busy, setBusy] = useState(false)
  const logsRef = useRef<HTMLDivElement | null>(null)

  // 父层任务状态同步（轮询驱动）。
  useEffect(() => { if (task) { setStatus(task.status); setError(task.error ?? '') } }, [task])

  // 拉取任务详情（含日志 + diff）：运行中 3s 轮询，终态一次即止。
  useEffect(() => {
    let alive = true
    const fetchAll = async () => {
      try {
        const res = await getAgentTask(taskId)
        if (!alive) return
        setStatus(res.task.status)
        setError(res.task.error ?? '')
        setSnapshotId(res.task.snapshot_id)
        setDiffHash(res.task.apply_diff_hash ?? '')
        setLogs((res.logs ?? []) as unknown as TaskLogRow[])
        if (isTerminal(res.task.status) && res.diff) setDiff(res.diff)
      } catch { /* 竞态忽略 */ }
    }
    void fetchAll()
    if (!isTerminal(status)) {
      const timer = window.setInterval(fetchAll, 3000)
      return () => { alive = false; window.clearInterval(timer) }
    }
    return () => { alive = false }
  }, [taskId, status])

  // 日志滚动贴底。
  useEffect(() => {
    if (logsOpen && logsRef.current) logsRef.current.scrollTop = logsRef.current.scrollHeight
  }, [logs, logsOpen])

  const act = useCallback(async (label: string, fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    try {
      await fn()
      message.success(label)
      onApplied()
      const res = await getAgentTask(taskId)
      setStatus(res.task.status)
    } catch (e) {
      message.error(e instanceof Error ? e.message : '操作失败')
    } finally { setBusy(false) }
  }, [busy, message, onApplied, taskId])

  const meta = STATUS[status] ?? { label: status, cls: 'wait' }
  const added = diff?.filter((d) => d.action === 'A') ?? []
  const modified = diff?.filter((d) => d.action === 'M') ?? []
  const deleted = diff?.filter((d) => d.action === 'D') ?? []

  return (
    <div className={`studio-taskcard stc-${meta.cls}`}>
      {/* 头部：图标 + 状态 + prompt 摘要。 */}
      <div className="studio-taskcard-head">
        <Bot size={14} strokeWidth={2} aria-hidden="true" />
        <span>{zh ? '创作任务' : 'Task'}</span>
        <span className={`studio-badge studio-badge-${meta.cls}`}>{zh ? meta.label : status}</span>
        {error && <span className="stc-error" title={error}>{error.slice(0, 60)}</span>}
      </div>
      <div className="studio-taskcard-prompt" title={prompt}>{prompt}</div>

      {/* 日志流（运行中自动展开 + 贴底滚动；终态可折叠）。 */}
      <div className="stc-section">
        <button type="button" className="stc-section-toggle" onClick={() => setLogsOpen((v) => !v)}>
          {logsOpen ? <ChevronDown size={12} aria-hidden="true" /> : <ChevronRight size={12} aria-hidden="true" />}
          {zh ? `日志${logs.length > 0 ? `（${logs.length}）` : ''}` : `Logs${logs.length ? ` (${logs.length})` : ''}`}
        </button>
        {logsOpen && (
          <div className="stc-logs" ref={logsRef}>
            {logs.length === 0 && <span className="muted">{isTerminal(status) ? (zh ? '（无日志）' : '(no logs)') : (zh ? '等待容器启动…' : 'Waiting for container…')}</span>}
            {logs.map((l) => (
              <div key={l.ID} className={`stc-log stc-log-${l.Stream}`}>
                <span className="stc-log-stream">{l.Stream}</span>
                <span className="stc-log-text">{l.Content}</span>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* 文件变更清单（终态 + 有 diff 时展示）。 */}
      {isTerminal(status) && diff !== null && diff.length > 0 && (
        <div className="stc-section">
          <div className="stc-section-title">
            <FileDiff size={12} aria-hidden="true" />
            {zh ? `文件变更（${diff.length}）` : `Changes (${diff.length})`}
          </div>
          <div className="stc-diff">
            {added.map((d) => (
              <div key={d.path} className="stc-diff-row stc-diff-add" title={d.path}>
                <FilePlus size={12} aria-hidden="true" />
                <span className="stc-diff-path">{d.path}</span>
                <span className="stc-diff-size muted">{(d.size / 1024).toFixed(1)}K</span>
              </div>
            ))}
            {modified.map((d) => (
              <div key={d.path} className="stc-diff-row stc-diff-mod" title={d.path}>
                <FileDiff size={12} aria-hidden="true" />
                <span className="stc-diff-path">{d.path}</span>
                <span className="stc-diff-size muted">{(d.size / 1024).toFixed(1)}K</span>
              </div>
            ))}
            {deleted.map((d) => (
              <div key={d.path} className="stc-diff-row stc-diff-del" title={d.path}>
                <FileMinus size={12} aria-hidden="true" />
                <span className="stc-diff-path">{d.path}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* 操作按钮（按状态显示）。 */}
      <div className="stc-actions">
        {status === 'running' && (
          <Button size="small" danger loading={busy} onClick={() => void act(zh ? '已取消' : 'Cancelled', () => cancelAgentTask(taskId))}>
            <Trash2 size={12} aria-hidden="true" />{zh ? '取消任务' : 'Cancel'}
          </Button>
        )}
        {isPending(status) && diff && diff.length > 0 && (
          <>
            <Button size="small" type="primary" loading={busy}
              onClick={() => void act(zh ? '已写入平台' : 'Applied', () => applyAgentTask(taskId, diff.map((d) => d.path), snapshotId, diffHash, true))}>
              <Check size={12} aria-hidden="true" />{zh ? `写入 ${diff.length} 个文件` : `Apply ${diff.length} file(s)`}
            </Button>
            <Button size="small" danger loading={busy} onClick={() => void act(zh ? '已丢弃' : 'Discarded', () => discardAgentTask(taskId))}>
              <Trash2 size={12} aria-hidden="true" />{zh ? '丢弃' : 'Discard'}
            </Button>
          </>
        )}
        {isPending(status) && (!diff || diff.length === 0) && (
          <Button size="small" loading={busy} onClick={() => void act(zh ? '已丢弃' : 'Discarded', () => discardAgentTask(taskId))}>
            <Trash2 size={12} aria-hidden="true" />{zh ? '丢弃（无产物）' : 'Discard (no output)'}
          </Button>
        )}
        {status === 'applied' && (
          <Tooltip title={zh ? '回滚到写入前状态' : 'Roll back to pre-apply state'}>
            <Button size="small" loading={busy} onClick={() => void act(zh ? '已回滚' : 'Rolled back', () => rollbackAgentTask(taskId))}>
              <RotateCcw size={12} aria-hidden="true" />{zh ? '回滚' : 'Rollback'}
            </Button>
          </Tooltip>
        )}
        {status === 'applied' && diff && diff.length > 0 && (
          <span className="muted stc-hint">{zh ? `已写入 ${diff.length} 个文件到平台目录` : `${diff.length} file(s) applied`}</span>
        )}
      </div>
    </div>
  )
}
