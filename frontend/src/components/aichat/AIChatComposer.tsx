// AI 对话共享输入区（@ant-design/x Sender 封装）：
// - autoSize 输入框 + 内嵌发送⇄停止按钮（busy 自动切换，IME 组合中不发送）；
// - # 文件提及浮层（光标前「#」触发；空词=指定根目录文件，否则防抖全文
//   搜索；键盘 ↑↓ 选择、Enter 插入 `文件名` 并回调加引用）；
// - header 槽位：调用方组装开关组/模板/引用 chips（DeepSeek 式一行 pill）。
import { useEffect, useRef, useState } from 'react'
import type { ReactNode, Ref } from 'react'
import { Sender } from '@ant-design/x'
import { CHAT_SEND_ICON, CHAT_STOP_ICON } from './icons'
import { FileText } from 'lucide-react'
import { listFiles, searchFiles } from '../../api'

/** Sender 实例结构类型（@ant-design/x 未从入口导出 SenderRef；仅用到的
 *  focus 与 textarea 句柄子集）。 */
export interface AISenderInstance {
  focus: () => void
  resizableTextArea?: { textArea: HTMLTextAreaElement }
}

/** 解析 # 文件提及：取光标前最近一个「#」，且其后到光标无空格/换行才算有效。 */
export function detectMention(text: string, caret: number): { idx: number; query: string } | null {
  const idx = text.slice(0, caret).lastIndexOf('#')
  if (idx < 0) return null
  const query = text.slice(idx + 1, caret)
  return /[\s]/.test(query) ? null : { idx, query }
}

export interface AIChatComposerProps {
  zh: boolean
  value: string
  onChange: (v: string) => void
  onSend: (text: string) => void
  /** busy：内嵌发送按钮自动变停止（onCancel）。 */
  busy?: boolean
  onCancel?: () => void
  disabled?: boolean
  placeholder?: string
  /** Sender header（开关组/模板 chips/引用 chips 由调用方组装）。 */
  header?: ReactNode
  /** #提及候选根目录（null/缺省 = 禁用提及）。 */
  mentionRoot?: string | null
  /** 选中提及项回调（宿主把文件加入引用 chips）。 */
  onMentionPick?: (f: { id: string; name: string }) => void
  /** 外部聚焦信号（递增触发 focus）。 */
  focusSignal?: number
  senderRef?: Ref<AISenderInstance>
  className?: string
}

export default function AIChatComposer({
  zh, value, onChange, onSend, busy, onCancel, disabled, placeholder, header,
  mentionRoot = null, onMentionPick, focusSignal, senderRef, className,
}: AIChatComposerProps) {
  const localRef = useRef<AISenderInstance | null>(null)
  const [mentionOpen, setMentionOpen] = useState(false)
  const [mentionQuery, setMentionQuery] = useState('')
  const [mentionItems, setMentionItems] = useState<Array<{ id: string; name: string }>>([])
  const [mentionActive, setMentionActive] = useState(0)
  const [mentionLoading, setMentionLoading] = useState(false)
  const mentionOn = mentionRoot !== null && !!onMentionPick

  // v3.7 输入历史（↑/↓ 切换，Cherry Studio/终端式）：会话级内存数组，
  // 发送后追加；↑ 回溯、↓ 前进、Esc 清空回当前输入。
  const historyRef = useRef<string[]>([])
  const historyIdxRef = useRef(-1)
  const draftRef = useRef('')
  const pushHistory = (text: string) => {
    if (!text.trim()) return
    const h = historyRef.current
    if (h[h.length - 1] !== text) h.push(text)
    if (h.length > 50) h.shift() // 上限 50 条
    historyIdxRef.current = -1
    draftRef.current = ''
  }
  const navigateHistory = (dir: 1 | -1): string | null => {
    const h = historyRef.current
    if (h.length === 0) return null
    let idx = historyIdxRef.current
    if (dir === -1) { // ↑ 回溯
      if (idx === -1) { draftRef.current = value; idx = h.length } // 保存当前草稿
      idx = Math.max(0, idx - 1)
    } else { // ↓ 前进
      if (idx === -1) return null
      idx = Math.min(h.length, idx + 1)
      if (idx === h.length) { // 回到草稿
        historyIdxRef.current = -1
        return draftRef.current
      }
    }
    historyIdxRef.current = idx
    return h[idx] ?? null
  }

  // 外部聚焦信号（欢迎卡「让 AI 生成」等）。
  useEffect(() => {
    if (focusSignal && focusSignal > 0) localRef.current?.focus()
  }, [focusSignal])

  // #提及候选：空词 = 指定根目录文件；否则 250ms 防抖全文搜索（过滤目录）。
  useEffect(() => {
    if (!mentionOpen || !mentionOn) return
    let alive = true
    const q = mentionQuery.trim()
    setMentionLoading(true)
    const run = (p: Promise<Array<{ id: string; name: string; type?: string }>>) => {
      void p
        .then((items) => {
          if (!alive) return
          setMentionItems(items.filter((x) => x.type !== 'folder').slice(0, 20).map((x) => ({ id: x.id, name: x.name })))
          setMentionActive(0)
        })
        .catch(() => { if (alive) { setMentionItems([]); setMentionActive(0) } })
        .finally(() => { if (alive) setMentionLoading(false) })
    }
    if (!q) {
      run(listFiles(mentionRoot, { limit: 20 }))
      return () => { alive = false }
    }
    const timer = window.setTimeout(() => run(searchFiles(q, 20)), 250)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
  }, [mentionOpen, mentionQuery, mentionRoot, mentionOn])

  /** 选中提及项：把「#词」替换为 `文件名`，光标落在反引号后。 */
  const insertMention = (f: { id: string; name: string }) => {
    const ta = localRef.current?.resizableTextArea?.textArea ?? null
    const caret = ta ? ta.selectionStart : value.length
    const det = detectMention(value, caret)
    const next = det ? `${value.slice(0, det.idx)}\`${f.name}\`${value.slice(caret)}` : `${value} \`${f.name}\``
    onChange(next)
    setMentionOpen(false)
    setMentionQuery('')
    onMentionPick?.(f)
    const pos = det ? det.idx + f.name.length + 2 : next.length
    requestAnimationFrame(() => {
      ta?.focus()
      ta?.setSelectionRange(pos, pos)
    })
  }

  const submit = () => {
    const text = value.trim()
    if (!text || busy || disabled) return
    pushHistory(text)
    onSend(text)
  }

  return (
    <div className="chat-input-box">
      <Sender
        ref={(r) => {
          localRef.current = (r ?? null) as unknown as AISenderInstance | null
          if (typeof senderRef === 'function') senderRef(localRef.current)
          else if (senderRef && typeof senderRef === 'object') (senderRef as { current: AISenderInstance | null }).current = localRef.current
        }}
        className={className ? `aic-sender ${className}` : 'aic-sender'}
        value={value}
        onChange={(v, event) => {
          if (mentionOn) {
            const el = event?.target as HTMLTextAreaElement | undefined
            const caret = el?.selectionStart ?? v.length
            const det = detectMention(v, caret)
            setMentionOpen(!!det)
            setMentionQuery(det?.query ?? '')
          }
          onChange(v)
        }}
        placeholder={placeholder}
        autoSize={{ minRows: 1, maxRows: 6 }}
        /* v3.9：内置发送按钮保留挂载、经 CSS 隐藏（.chat-input-box 规则同时
            覆盖 v1/v2 类名）——不能用 suffix={false}：@ant-design/x v2 的
            submitDisabled 状态仅由内置 SendButton 的 effect 驱动，卸载内置
            按钮会导致 Enter 永远无法提交。发送/停止按钮只在 chat-tools-bar
            最右端，Cherry Studio 式。 */
        disabled={disabled}
        onSubmit={() => submit()}
        onCancel={onCancel}
        /* v3.7：header prop 从 Sender 内部移到框外底部（chat-tools-bar）。 */
        onKeyDown={(e) => {
          if (e.nativeEvent.isComposing) return // IME 组合中：交输入法处理
          // v3.7 输入历史导航：光标在首行时 ↑ 回溯、末行时 ↓ 前进（终端式）。
          if (!mentionOpen && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
            const ta = localRef.current?.resizableTextArea?.textArea
            if (ta) {
              const atFirstLine = e.key === 'ArrowUp' && ta.selectionStart === 0
              const atLastLine = e.key === 'ArrowDown' && ta.selectionStart >= value.length
              if (atFirstLine || atLastLine) {
                const next = navigateHistory(e.key === 'ArrowUp' ? -1 : 1)
                if (next !== null) {
                  e.preventDefault()
                  onChange(next)
                  return false
                }
              }
            }
          }
          if (mentionOpen) {
            if (e.key === 'ArrowUp' && mentionItems.length > 0) { e.preventDefault(); setMentionActive((i) => (i - 1 + mentionItems.length) % mentionItems.length); return false }
            if (e.key === 'ArrowDown' && mentionItems.length > 0) { e.preventDefault(); setMentionActive((i) => (i + 1) % mentionItems.length); return false }
            if (e.key === 'Enter' && mentionItems.length > 0) { e.preventDefault(); insertMention(mentionItems[mentionActive] ?? mentionItems[0]); return false }
            if (e.key === 'Escape') { e.preventDefault(); setMentionOpen(false); return false }
          }
          return undefined
        }}
      />
      {/* v3.7 Cherry Studio 式：工具栏在输入框内底部（header prop 内容
          渲染为单行工具栏 + 发送/停止按钮最右端）。 */}
      <div className="chat-tools-bar">
        {header}
        <span className="chat-send-btn-wrap">
          {busy ? (
            <button type="button" className="chat-stop-btn" aria-label={zh ? '停止生成' : 'Stop'} title={zh ? '停止生成' : 'Stop'} onClick={onCancel}>
              {CHAT_STOP_ICON}
            </button>
          ) : (
            <button type="button" className="chat-send-btn" disabled={!value.trim() || disabled} aria-label={zh ? '发送' : 'Send'} title={zh ? '发送（Enter）' : 'Send (Enter)'} onClick={submit}>
              {CHAT_SEND_ICON}
            </button>
          )}
        </span>
      </div>
      {mentionOpen && (
        <div className="aic-mention-panel">
          {mentionLoading && <div className="aic-mention-state muted">{zh ? '搜索中…' : 'Searching…'}</div>}
          {!mentionLoading && mentionItems.length === 0 && <div className="aic-mention-state muted">{zh ? '没有匹配的文件' : 'No matching files'}</div>}
          {mentionItems.map((item, i) => (
            <button key={item.id} type="button" className={`aic-mention-item${i === mentionActive ? ' active' : ''}`}
              onMouseDown={(e) => e.preventDefault()} onMouseEnter={() => setMentionActive(i)} onClick={() => insertMention(item)}>
              <FileText size={13} strokeWidth={2} aria-hidden="true" />
              <span className="name" title={item.name}>{item.name}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
