// AI 对话共享数据模型（纯逻辑，无 React 依赖；各对话场景共用）：
// - 工具调用生命周期条目（SSE event:tool → running / event:tool_result →
//   success|error，按 server+tool 顺序配对合并）；
// - 推理思考（thinking）增量随 turn 聚合，附起止时间供折叠标题展示用时；
// - 网络来源宽松归一化（原 AIAssistant 内实现迁出，双向复用）。
import type { AIToolCall, AIToolResult } from '../../api'

/** 工具调用条目状态：running=执行中 / success=成功 / error=失败。 */
export type AIToolStatus = 'running' | 'success' | 'error'

/** 消息上的工具调用条目（SSE tool + tool_result 两事件合并后的展示形态）。 */
export interface AIToolCallEntry {
  /** 展示文本（后端原始 label；df_* 平台工具经 dfToolLabel 映射双语）。 */
  label: string
  server?: string
  tool?: string
  /** 参数摘要（执行前 tool 事件附带，JSON 字符串）。 */
  input?: string
  /** 结果摘要（执行后 tool_result 附带）。 */
  output?: string
  /** 失败原因（tool_result ok=false）。 */
  errorText?: string
  /** 执行耗时 ms（tool_result 附带）。 */
  durationMS?: number
  status: AIToolStatus
}

/** SSE tool 事件（执行前）→ running 条目。 */
export function toolEntryFrom(call: AIToolCall): AIToolCallEntry {
  return {
    label: call.label || [call.server, call.tool].filter(Boolean).join(' / '),
    server: call.server,
    tool: call.tool,
    input: call.input,
    status: 'running',
  }
}

/**
 * SSE tool_result 事件合并进条目列表：定位最后一个 running 且 server+tool
 * 匹配的条目（多工具同名时按顺序消费；无匹配时回落最后一个 running），
 * 写入终态/耗时/结果摘要。返回新数组（不可变更新）。
 */
export function applyToolResult(entries: AIToolCallEntry[], r: AIToolResult): AIToolCallEntry[] {
  if (entries.length === 0) return entries
  let idx = -1
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i]
    if (e.status !== 'running') continue
    if (e.server === r.server && e.tool === r.tool) { idx = i; break }
    if (idx < 0) idx = i // 回退：最后一个 running
  }
  if (idx < 0) return entries
  const next = [...entries]
  next[idx] = {
    ...next[idx],
    status: r.ok ? 'success' : 'error',
    output: r.output,
    errorText: r.error,
    durationMS: r.duration_ms,
  }
  return next
}

// ---------- df_* 平台文件工具双语映射（server=docflow） ----------

const DF_TOOL_LABELS: Record<string, { zh: string; en: string }> = {
  df_list_dir: { zh: '列目录', en: 'List directory' },
  df_read_file: { zh: '读取文件', en: 'Read file' },
  df_write_file: { zh: '写入文件', en: 'Write file' },
  df_mkdir: { zh: '新建目录', en: 'Create folder' },
  df_search: { zh: '搜索文件', en: 'Search files' },
}

/** 平台内置文件工具的 server 标识（后端固定 docflow）。 */
export const DF_SERVER = 'docflow'

/** 工具条目展示文本：server=docflow 的 df_* 工具映射为「平台 / 列目录」式
 *  双语；其余透传后端 label（「服务名 / 工具名」）。 */
export function dfToolLabel(tc: AIToolCallEntry, zh: boolean): string {
  if (tc.server === DF_SERVER && tc.tool) {
    const mapped = DF_TOOL_LABELS[tc.tool]
    if (mapped) return `${zh ? '平台' : 'Platform'} / ${zh ? mapped.zh : mapped.en}`
  }
  return tc.label
}

/** df_write_file 落盘后自动留版本（条目标题附注）。 */
export function isWriteTool(tc: AIToolCallEntry): boolean {
  return tc.server === DF_SERVER && tc.tool === 'df_write_file'
}

// ---------- 网络来源（原 AIAssistant 实现迁出） ----------

export interface AIWebSource {
  title: string
  url: string
}

/** 宽松归一化网络来源数组（非数组/空元素静默跳过，至多 20 条）。 */
export function normalizeWebSources(raw: unknown): AIWebSource[] {
  if (!Array.isArray(raw)) return []
  const out: AIWebSource[] = []
  for (const item of raw) {
    if (out.length >= 20) break
    if (typeof item === 'string') {
      const s = item.trim()
      if (s) out.push({ title: s, url: '' })
      continue
    }
    if (!item || typeof item !== 'object') continue
    const e = item as Record<string, unknown>
    const title = String(e.title ?? e.name ?? '').trim()
    const url = String(e.url ?? e.link ?? '').trim()
    if (title || url) out.push({ title: title || url, url })
  }
  return out
}

// ---------- 对话消息（turn）统一展示模型 ----------

import type { AISource } from '../../api'

/** 引用文件（随对话发送 fileIds；用户气泡 📎 chips 展示）。 */
export interface AIAttachFile {
  fileId: string
  fileName: string
}

/** 对话消息统一展示模型（AI 助手 / 编辑器侧栏共用渲染）。 */
export interface AIChatTurnData {
  /** 会话内自增 ID：流式回调按 id 定位更新。 */
  id: number | string
  role: 'user' | 'assistant'
  content: string
  streaming?: boolean
  /** 用户主动停止（保留已生成内容，不视为错误）。 */
  stopped?: boolean
  error?: string
  /** 推理思考聚合文本（SSE thinking 增量；不计入 content）。 */
  thinking?: string
  /** 首个思考增量的时间戳（ms；折叠标题展示「已深度思考（用时 Ns）」）。 */
  thinkingStartedAt?: number
  /** 思考结束耗时（ms；流结束后写入）。 */
  thinkingMS?: number
  /** 用户回合引用的文件（chips 展示）。 */
  files?: AIAttachFile[]
  sources?: AISource[]
  webSources?: AIWebSource[]
  toolCalls?: AIToolCallEntry[]
}

/** 思考折叠标题：流式中「思考中…」；完成后「已深度思考（用时 N s）」。 */
export function thinkingTitle(turn: Pick<AIChatTurnData, 'streaming' | 'thinkingMS'>, zh: boolean): string {
  if (turn.streaming) return zh ? '思考中…' : 'Thinking…'
  const ms = turn.thinkingMS
  if (ms === undefined || ms <= 0) return zh ? '已深度思考' : 'Thought'
  const sec = ms >= 1000 ? `${(ms / 1000).toFixed(1)} ${zh ? '秒' : 's'}` : `${ms} ms`
  return zh ? `已深度思考（用时 ${sec}）` : `Thought for ${sec}`
}
