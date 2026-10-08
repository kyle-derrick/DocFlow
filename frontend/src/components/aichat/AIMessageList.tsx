// AI 对话共享消息流渲染（@ant-design/x：Bubble.List + Think + ThoughtChain）。
// 对话场景（全局 AI 助手 Drawer / 编辑器侧栏）统一气泡：
// - 用户：右侧主色胶囊（含 📎 引用 chips）；
// - AI：左侧无底色全宽 —— 顶部「思考过程」折叠区（Think 组件：流式期间
//   自动展开跟随、完成后自动收起并展示用时，DeepSeek 交互范式）+ 工具
//   调用链（ThoughtChain：running 旋转 → success/error 终态，可展开参数/
//   结果摘要）+ markdown 正文（AIMarkdown）+ 引用来源/网络来源 + 操作行。
import { memo, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Bubble, ThoughtChain } from '@ant-design/x'
import type { BubbleItemType } from '@ant-design/x'
import { Brain, Check, Copy, FileText, Globe, Paperclip, RotateCcw, Sparkles, Wrench } from 'lucide-react'
import { Tooltip } from 'antd'
import AIMarkdown from '../AIMarkdown'
import { useStickyScroll } from './useStickyScroll'
import {
  dfToolLabel, isWriteTool, thinkingTitle,
} from './turns'
import type { AIChatTurnData, AIToolCallEntry, AIWebSource } from './turns'

/** 思考过程折叠区（Think 封装）：流式期间展开跟随、完成后自动收起并
 *  展示用时；用户手动展开/收起后不再被流式状态覆盖。 */
export function AIChatThinking({ text, streaming, thinkingMS, zh }: {
  text: string
  streaming?: boolean
  thinkingMS?: number
  zh: boolean
}) {
  const [expanded, setExpanded] = useState<boolean>(!!streaming)
  const [touched, setTouched] = useState(false)
  useEffect(() => {
    // 流式结束自动收起（仅用户未手动操作过时）。
    if (!streaming && !touched) setExpanded(false)
  }, [streaming, touched])
  if (!text) return null
  return (
    <div className="aic-thinking">
      <Brain size={14} strokeWidth={2} className="aic-thinking-bg" aria-hidden="true" />
      <div className="aic-thinking-body" aria-hidden="false">
        <details open={expanded} onToggle={(e) => { setTouched(true); setExpanded((e.target as HTMLDetailsElement).open) }}>
          <summary className="aic-thinking-summary">
            <span className={`aic-thinking-title${streaming ? ' streaming' : ''}`}>
              {thinkingTitle({ streaming, thinkingMS }, zh)}
            </span>
            {streaming && <span className="aic-thinking-dots" aria-hidden="true"><i /><i /><i /></span>}
          </summary>
          <div className="aic-thinking-text">{text}</div>
        </details>
      </div>
    </div>
  )
}

/** 工具参数/结果摘要展示（<pre> 定宽；无内容返回 null）。 */
function ToolDetail({ entry, zh }: { entry: AIToolCallEntry; zh: boolean }) {
  const parts: Array<{ label: string; text: string }> = []
  if (entry.input) parts.push({ label: zh ? '参数' : 'Input', text: entry.input })
  if (entry.output) parts.push({ label: zh ? '结果' : 'Output', text: entry.output })
  if (entry.errorText) parts.push({ label: zh ? '错误' : 'Error', text: entry.errorText })
  if (parts.length === 0) return null
  return (
    <div className="aic-tool-detail">
      {parts.map((p) => (
        <div key={p.label} className="aic-tool-detail-row">
          <span className="k">{p.label}</span>
          <pre className="v">{p.text}</pre>
        </div>
      ))}
    </div>
  )
}

/** 工具调用链（ThoughtChain 封装）：running 旋转 → success/error 终态；
 *  有参数/结果摘要的条目可展开。df_write_file 附「已保存（自动留版本）」。 */
export function AIToolChain({ toolCalls, zh }: { toolCalls: AIToolCallEntry[] | undefined; zh: boolean }) {
  const items = useMemo(() => (toolCalls ?? []).map((tc, i) => {
    const hasDetail = !!(tc.input || tc.output || tc.errorText)
    const dur = tc.durationMS !== undefined && tc.durationMS > 0
      ? (tc.durationMS >= 1000 ? `${(tc.durationMS / 1000).toFixed(1)}s` : `${tc.durationMS}ms`)
      : ''
    const title = (
      <span className="aic-tool-title">
        {dfToolLabel(tc, zh)}
        {isWriteTool(tc) && <span className="aic-tool-saved">{zh ? ' · 已保存（自动留版本）' : ' · saved (auto versioned)'}</span>}
        {dur && <span className="aic-tool-dur"> · {dur}</span>}
      </span>
    )
    return {
      key: String(i),
      icon: <Wrench size={14} strokeWidth={2} aria-hidden="true" />,
      title,
      status: tc.status === 'running' ? ('loading' as const) : tc.status === 'error' ? ('error' as const) : ('success' as const),
      collapsible: hasDetail,
      content: hasDetail ? <ToolDetail entry={tc} zh={zh} /> : undefined,
    }
  }), [toolCalls, zh])
  if (items.length === 0) return null
  return <ThoughtChain className="aic-toolchain" items={items} />
}

/** 网络来源折叠列表（编号 + 标题超链接；联网开关生效时展示）。 */
export function AIWebSourcesView({ sources, zh }: { sources: AIWebSource[]; zh: boolean }) {
  const [open, setOpen] = useState(false)
  if (sources.length === 0) return null
  return (
    <details className="aic-websources" open={open} onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary><Globe size={12} strokeWidth={2} aria-hidden="true" />{zh ? `网络来源（${sources.length}）` : `Web sources (${sources.length})`}</summary>
      <div className="aic-websources-list">
        {sources.map((s, i) => {
          const inner = (
            <>
              <span className="idx" aria-hidden="true">{i + 1}</span>
              <span className="name">{s.title}</span>
            </>
          )
          return s.url ? (
            <a key={`${i}:${s.url}`} className="aic-websource-link" href={s.url} target="_blank" rel="noopener noreferrer" title={s.title}>
              {inner}
            </a>
          ) : (
            <span key={`${i}:${s.title}`} className="aic-websource-link" title={s.title}>{inner}</span>
          )
        })}
      </div>
    </details>
  )
}

/** 消息操作行：复制 / 重新生成（可选）。 */
function MessageActions({ text, zh, onRegenerate }: { text: string; zh: boolean; onRegenerate?: () => void }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="aic-msg-actions">
      <Tooltip title={copied ? (zh ? '已复制' : 'Copied') : (zh ? '复制' : 'Copy')}>
        <button
          type="button"
          className="aic-msg-action"
          aria-label={zh ? '复制' : 'Copy'}
          onClick={() => {
            void navigator.clipboard.writeText(text).then(() => {
              setCopied(true)
              window.setTimeout(() => setCopied(false), 1500)
            })
          }}
        >
          {copied ? <Check size={13} strokeWidth={2} aria-hidden="true" /> : <Copy size={13} strokeWidth={2} aria-hidden="true" />}
        </button>
      </Tooltip>
      {onRegenerate && (
        <Tooltip title={zh ? '重新生成' : 'Regenerate'}>
          <button type="button" className="aic-msg-action" aria-label={zh ? '重新生成' : 'Regenerate'} onClick={onRegenerate}>
            <RotateCcw size={13} strokeWidth={2} aria-hidden="true" />
          </button>
        </Tooltip>
      )}
    </div>
  )
}

/** AI 气泡内容（除 Bubble 壳外的一切：思考/工具链/正文/来源/操作）。 */
/** 富文本编辑指令中的受保护占位行（[DocFlow-Embed/File/Image …]）在对话
 * 气泡内渲染为可点击卡片链接（点击在独立查看页打开真实内容；应用后由
 * 富文本层还原为原生嵌入节点）。 */
export function humanizeEmbedPlaceholders(text: string): string {
  // v3.7：匹配整行 AND 行内（AI 可能把占位行嵌在段落中间）。
  return text.replace(/\[DocFlow-(Embed|File|Image)([^\]]*)\]/g, (_m, kind: string, rest: string) => {
    const fid = (rest.match(/fileId="([^"]*)"/) || [])[1] ?? ''
    const title = (rest.match(/title="([^"]*)"/) || [])[1] ?? ''
    const kLabel = kind === 'Embed' ? '嵌入' : kind === 'File' ? '文件' : '图片'
    const name = title || fid.slice(0, 8)
    return fid
      ? `**${kLabel}**：[${name}](/view/${fid})`
      : `**${kLabel}**${title ? `：${title}` : ''}`
  })
}

export function AIAssistantMessageBody({ turn, zh, isLast, onRegenerate, extra }: {
  turn: AIChatTurnData
  zh: boolean
  isLast?: boolean
  onRegenerate?: () => void
  /** 宿主附加内容（编辑器侧栏的「应用到文档」等），渲染在操作行之前。 */
  extra?: ReactNode
}) {
  if (turn.error) {
    return (
      <div className="aic-error">
        <div className="aic-error-msg">{turn.error}</div>
      </div>
    )
  }
  const showActions = !turn.streaming && (turn.content || turn.stopped)
  return (
    <div className="aic-msg">
      <AIChatThinking text={turn.thinking ?? ''} streaming={turn.streaming} thinkingMS={turn.thinkingMS} zh={zh} />
      <AIToolChain toolCalls={turn.toolCalls} zh={zh} />
      {turn.content ? (
        <AIMarkdown text={humanizeEmbedPlaceholders(turn.content)} zh={zh} streaming={turn.streaming} />
      ) : turn.streaming ? (
        <span className="aic-generating"><Sparkles size={12} strokeWidth={2} aria-hidden="true" />{zh ? '生成中…' : 'Generating…'}</span>
      ) : turn.stopped ? (
        <span className="aic-stopped">{zh ? '已停止' : 'Stopped'}</span>
      ) : (
        // 兜底：流结束但无正文/错误/停止标记（如推理模型耗尽输出上限返回
        // 空 done）——给出可见提示而非整块空白。
        <span className="aic-stopped muted">{zh ? '（模型未返回内容）' : '(no content returned)'}</span>
      )}
      {turn.streaming && turn.content && <span className="ai-caret" aria-hidden="true" />}
      {turn.stopped && turn.content && <span className="aic-stopped-tag">{zh ? '已停止' : 'Stopped'}</span>}
      {turn.sources && turn.sources.length > 0 && (
        <div className="ai-sources">
          <span className="ai-sources-label muted">{zh ? '引用来源' : 'Sources'}：</span>
          {turn.sources.map((s) => (
            <a key={s.file_id} className="ai-source-link" href={s.url} target="_blank" rel="noopener noreferrer" title={s.name}>
              <FileText size={12} strokeWidth={2} aria-hidden="true" />
              {s.name}
            </a>
          ))}
        </div>
      )}
      <AIWebSourcesView sources={turn.webSources ?? []} zh={zh} />
      {extra}
      {showActions && <MessageActions text={turn.content} zh={zh} onRegenerate={isLast ? onRegenerate : undefined} />}
    </div>
  )
}

/** 用户气泡内容（纯文本 + 📎 引用 chips）。 */
export function AIUserMessageBody({ turn }: { turn: AIChatTurnData }) {
  return (
    <div className="aic-user-msg">
      {turn.content}
      {turn.files && turn.files.length > 0 && (
        <span className="ai-turn-files">
          {turn.files.map((f) => (
            <span key={f.fileId} className="ai-turn-file-chip" title={f.fileName}>
              <Paperclip size={10} strokeWidth={2} aria-hidden="true" />
              <span>{f.fileName}</span>
            </span>
          ))}
        </span>
      )}
    </div>
  )
}

/** 共享消息流（Bubble.List：用户右 / AI 左；贴底跟随 v3.4——用户上滚
 *  阅读历史时不被流式输出强制拉回底部，回到底部自动恢复跟随）。
 *  items 为统一 turn 模型；renderItem 覆盖单条渲染（任务卡等宿主特例）。 */
function AIMessageListImpl({ items, zh, className, autoScroll = true, renderItem }: {
  items: AIChatTurnData[]
  zh: boolean
  className?: string
  autoScroll?: boolean
  /** 单条覆盖渲染（返回 undefined 走默认气泡；docker 任务卡等用）。 */
  renderItem?: (turn: AIChatTurnData, index: number) => ReactNode | undefined
  onRegenerate?: () => void
}) {
  const bubbleItems: BubbleItemType[] = items.map((turn, i) => {
    const override = renderItem?.(turn, i)
    return {
      key: String(turn.id),
      role: turn.role,
      content: override !== undefined
        ? override
        : turn.role === 'user'
          ? <AIUserMessageBody turn={turn} />
          : <AIAssistantMessageBody turn={turn} zh={zh} isLast={i === items.length - 1} />,
    }
  })
  // 贴底跟随（包裹式：div.ref + onScroll；不碰 Bubble.List 的 ref 形状）。
  const last = items.length > 0 ? items[items.length - 1] : null
  const dep = last ? `${last.id}:${(last.content ?? '').length}:${(last.thinking ?? '').length}` : ''
  const sticky = useStickyScroll(dep)
  return (
    <div ref={autoScroll ? sticky.wrapRef : undefined} onScroll={autoScroll ? sticky.onScroll : undefined} className="aic-list-wrap">
      <Bubble.List
        className={className}
        items={bubbleItems}
      role={{
        user: {
          placement: 'end',
          variant: 'filled',
          shape: 'round',
          classNames: { content: 'aic-bubble-user' },
        },
        assistant: {
          placement: 'start',
          variant: 'borderless',
          avatar: (
            <span className="ai-avatar" aria-hidden="true">
              <Sparkles size={13} strokeWidth={2} />
            </span>
          ),
          classNames: { content: 'aic-bubble-ai' },
        },
      }}
      />
    </div>
  )
}

export const AIMessageList = memo(AIMessageListImpl)
