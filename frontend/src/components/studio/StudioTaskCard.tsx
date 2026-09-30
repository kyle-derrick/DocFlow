// Agent 对话式输出（v3.8，Cherry Studio 式）：docker 任务在对话流中渲染为
// 普通 AI 消息——思考折叠区（容器日志）→ 正文（任务摘要/错误）→ 工具链
// （文件变更清单）→ 操作按钮（写入/丢弃/回滚），全部内联，无边框卡片。
import { useCallback, useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button, Tooltip } from 'antd'
import { Check, FileDiff, FileMinus, FilePlus, RotateCcw, Trash2 } from 'lucide-react'
import { AIChatThinking } from '../aichat'
import { applyAgentTask, cancelAgentTask, discardAgentTask, getAgentTask, rollbackAgentTask } from '../../agentTasks'
import type { AgentDiff, AgentTask } from '../../agentTasks'

const isTerminal = (s: string) => s !== 'queued' && s !== 'running'
const isPending = (s: string) => s === 'succeeded'

const STATUS_LABEL: Record<string, string> = {
  queued: '排队中', running: '执行中', succeeded: '待确认', applied: '已写入',
  failed: '失败', cancelled: '已取消', discarded: '已丢弃', rolled_back: '已回滚',
}

export default function StudioTaskCard({ zh, taskId, task }: {
  zh: boolean
  taskId: string
  task?: AgentTask
  onApplied: () => void
}) {
  const { message } = AntdApp.useApp()
  const [status, setStatus] = useState(task?.status ?? 'queued')
  const [error, setError] = useState(task?.error ?? '')
  const [logs, setLogs] = useState<string[]>([])
  const [diff, setDiff] = useState<AgentDiff[] | null>(null)
  const [snapshotId, setSnapshotId] = useState(task?.snapshot_id ?? '')
  const [diffHash, setDiffHash] = useState('')
  const [busy, setBusy] = useState(false)
  const aliveRef = useRef(true)

  useEffect(() => { if (task) { setStatus(task.status); setError(task.error ?? '') } }, [task])

  // 轮询任务详情（含日志+diff）。
  useEffect(() => {
    aliveRef.current = true
    const poll = async () => {
      try {
        const res = await getAgentTask(taskId)
        if (!aliveRef.current) return
        setStatus(res.task.status)
        setError(res.task.error ?? '')
        setSnapshotId(res.task.snapshot_id)
        setDiffHash(res.task.apply_diff_hash ?? '')
        setLogs((res.logs ?? []).map((l) =>
          l.stream === 'system' ? `⚙ ${l.content}` : l.stream === 'stderr' ? `▸ ${l.content}` : l.content
        ))
        if (isTerminal(res.task.status) && res.diff) setDiff(res.diff)
      } catch { /* 竞态忽略 */ }
    }
    void poll()
    if (!isTerminal(status)) {
      const timer = window.setInterval(poll, 3000)
      return () => { aliveRef.current = false; window.clearInterval(timer) }
    }
    return () => { aliveRef.current = false }
  }, [taskId, status])

  const act = useCallback(async (label: string, fn: () => Promise<unknown>) => {
    if (busy) return
    setBusy(true)
    try {
      await fn()
      message.success(label)
      const res = await getAgentTask(taskId)
      setStatus(res.task.status)
    } catch (e) {
      message.error(e instanceof Error ? e.message : '操作失败')
    } finally { setBusy(false) }
  }, [busy, message, taskId])

  const meta = STATUS_LABEL[status] ?? status
  const added = diff?.filter((d) => d.action === 'A') ?? []
  const modified = diff?.filter((d) => d.action === 'M') ?? []
  const deleted = diff?.filter((d) => d.action === 'D') ?? []
  const thinkingText = logs.join('\n')

  // 对话式渲染：无卡片边框，与普通 AI 消息同视觉。
  return (
    <div className="aic-msg stc-msg">
      {/* 思考折叠区：容器日志（runner 进度/AI 轮次）。 */}
      <AIChatThinking
        text={thinkingText}
        streaming={!isTerminal(status)}
        zh={zh}
      />

      {/* 正文：任务状态/摘要/错误。 */}
      {error ? (
        <div className="aic-error">
          <div className="aic-error-msg">{error}</div>
        </div>
      ) : (
        <div className="stc-summary">
          {isPending(status) && diff && diff.length > 0
            ? (zh ? `任务完成，产出 ${diff.length} 个文件变更。确认后写入平台目录。` : `Done. ${diff.length} file change(s) ready to apply.`)
            : isTerminal(status)
              ? (zh ? `任务已完成${diff && diff.length > 0 ? `（${diff.length} 个变更）` : '（无产物变更）'}。` : `Task completed.`)
              : (zh ? `${meta}…` : `${meta}…`)
          }
        </div>
      )}

      {/* 文件变更（可折叠列表，非卡片区块）。 */}
      {isTerminal(status) && diff && diff.length > 0 && (
        <div className="stc-changes">
          <div className="stc-changes-title">
            <FileDiff size={12} aria-hidden="true" />
            {zh ? `文件变更（${diff.length}）` : `Changes (${diff.length})`}
          </div>
          <div className="stc-diff">
            {added.map((d) => (
              <div key={d.path} className="stc-diff-row stc-diff-add" title={d.path}>
                <FilePlus size={12} aria-hidden="true" />
                <span className="stc-diff-path">{d.path}</span>
              </div>
            ))}
            {modified.map((d) => (
              <div key={d.path} className="stc-diff-row stc-diff-mod" title={d.path}>
                <FileDiff size={12} aria-hidden="true" />
                <span className="stc-diff-path">{d.path}</span>
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

      {/* 操作按钮（消息级操作行，与复制/重试同排）。 */}
      <div className="aic-msg-actions stc-actions">
        {status === 'running' && (
          <Button size="small" danger loading={busy} onClick={() => void act(zh ? '已取消' : 'Cancelled', () => cancelAgentTask(taskId))}>
            <Trash2 size={12} aria-hidden="true" />{zh ? '取消' : 'Cancel'}
          </Button>
        )}
        {isPending(status) && diff && diff.length > 0 && (
          <>
            <Button size="small" type="primary" loading={busy}
              onClick={() => void act(zh ? '已写入平台' : 'Applied', () => applyAgentTask(taskId, diff.map((d) => d.path), snapshotId, diffHash, true))}>
              <Check size={12} aria-hidden="true" />{zh ? `写入 ${diff.length} 个文件` : `Apply ${diff.length}`}
            </Button>
            <Button size="small" danger loading={busy} onClick={() => void act(zh ? '已丢弃' : 'Discarded', () => discardAgentTask(taskId))}>
              <Trash2 size={12} aria-hidden="true" />{zh ? '丢弃' : 'Discard'}
            </Button>
          </>
        )}
        {isPending(status) && (!diff || diff.length === 0) && (
          <Button size="small" loading={busy} onClick={() => void act(zh ? '已丢弃' : 'Discarded', () => discardAgentTask(taskId))}>
            <Trash2 size={12} aria-hidden="true" />{zh ? '丢弃（无产物）' : 'Discard'}
          </Button>
        )}
        {status === 'applied' && (
          <Tooltip title={zh ? '回滚到写入前' : 'Roll back'}>
            <Button size="small" loading={busy} onClick={() => void act(zh ? '已回滚' : 'Rolled back', () => rollbackAgentTask(taskId))}>
              <RotateCcw size={12} aria-hidden="true" />{zh ? '回滚' : 'Rollback'}
            </Button>
          </Tooltip>
        )}
      </div>
    </div>
  )
}
