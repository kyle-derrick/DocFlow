// AI 对话共享输入区（@ant-design/x Sender 封装）：
// - autoSize 输入框 + 内嵌发送⇄停止按钮（busy 自动切换，IME 组合中不发送）；
// - # 文件提及浮层（光标前「#」触发；空词=指定根目录文件，否则防抖全文
//   搜索；键盘 ↑↓ 选择、Enter 插入 `文件名` 并回调加引用）；
// - header 槽位：调用方组装开关组/模板/引用 chips（DeepSeek 式一行 pill）。
import { useEffect, useRef, useState } from 'react'
import type { ReactNode, Ref } from 'react'
import { Sender } from '@ant-design/x'
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
    onSend(text)
  }

  return (
    <div className="aic-composer">
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
        loading={busy}
        disabled={disabled}
        onSubmit={() => submit()}
        onCancel={onCancel}
        header={header}
        onKeyDown={(e) => {
          if (e.nativeEvent.isComposing) return // IME 组合中：交输入法处理
          if (mentionOpen) {
            if (e.key === 'ArrowUp' && mentionItems.length > 0) { e.preventDefault(); setMentionActive((i) => (i - 1 + mentionItems.length) % mentionItems.length); return false }
            if (e.key === 'ArrowDown' && mentionItems.length > 0) { e.preventDefault(); setMentionActive((i) => (i + 1) % mentionItems.length); return false }
            if (e.key === 'Enter' && mentionItems.length > 0) { e.preventDefault(); insertMention(mentionItems[mentionActive] ?? mentionItems[0]); return false }
            if (e.key === 'Escape') { e.preventDefault(); setMentionOpen(false); return false }
          }
          return undefined
        }}
      />
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
