// 查看页悬浮 AI 助理（替换查看页原 AI 摘要按钮行）：
// - 收起态：40px 悬浮球（Sparkles，主色）；pointer 事件手写拖动
//   （位移 >4px 判定为拖动，否则视为点击展开/收起），位置记忆 localStorage
//   （docflow.viewer-ai.pos），始终约束在可用边界内（resize 重夹取）；
// - 边缘吸附：拖动结束时距左/右边界 <24px 时吸附贴边——悬浮球收成半嵌
//   边缘的圆点（一半露出边界外），点击弹出完整悬浮球/卡片（脱离吸附态）；
// - 弹窗内挂载：组件渲染点（FileBrowser 查看弹窗标题行 headExtra）在
//   .ant-modal-container 内时，可用边界自动切换为弹窗容器矩形（悬浮球/
//   卡片始终在弹窗内，z-index 高于弹窗）；独立查看页（ViewerPage）无弹窗
//   祖先 → 边界为视口；
// - 展开态：360×520（按边界收缩）卡片 = 头部（标题 + 文件名 + 收起按钮 +
//   拖动把手）+ Segmented「对话 | 摘要」+ 内容区：
//   · 对话：精简版多轮对话（aiChat SSE 流式；模型选择器 = 默认模型 + 下拉，
//     复用 AIAssistant 的 getAIModels/AI_MODEL_STORAGE_KEY），系统提示注入
//     当前文件上下文（文件名 + 全文截 12000 字，fetchFileText 现取缓存），
//     气泡经 AIMarkdown 渲染；文本类文件的 AI 回答提供消息级「保存为新版本」
//     （回复中围栏代码块优先/整段兜底 → uploadFileVersion 覆盖当前 file_id，
//     自动留版本链可在历史版本回退；成功经 onSaved 通知查看页重新拉取预览）；
//     非文本类（pdf/office 等）禁用保存按钮 + Tooltip 说明；
//   · 摘要：/ai/summarize 流式（同 AISummary 链路），markdown 展示 + 复制。
// - AI 未启用（useAIEnabled=false）不渲染。
import { useEffect, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'
import { App as AntdApp, Button, Input, Popconfirm, Segmented, Select, Tooltip } from 'antd'
import { Check, ChevronDown, Copy, RotateCcw, Save, Send, Sparkles, Square, Trash2 } from 'lucide-react'
import AIMarkdown from './AIMarkdown'
import { AIModelOption, AI_MODEL_STORAGE_KEY, defaultAIModelKey, getAIModels } from './AIAssistant'
import {
  AIMessage,
  aiChat,
  aiSummarizeFileStream,
  fetchFileText,
  isTextLike,
  uploadFileVersion,
} from '../api'
import { useAIEnabled } from '../aiFeature'
import { t, useLocale } from '../i18n'

/** 悬浮位置持久化 key（{x,y,snap}：x/y 为悬浮球/卡片左上角（视口坐标），
 *  snap = 吸附边（'left' | 'right' | null，半嵌圆点态）。 */
const WIDGET_POS_KEY = 'docflow.viewer-ai.pos'
/** 悬浮球尺寸 / 卡片尺寸 / 边界内边距（px）。 */
const BALL_SIZE = 40
const CARD_W = 360
const CARD_H = 520
const VIEW_MARGIN = 8
/** 拖动阈值：位移超过该值判定为拖动（否则视为点击）。 */
const DRAG_THRESHOLD = 4
/** 边缘吸附阈值：拖动结束时球心距左/右边界小于该值吸附贴边。 */
const EDGE_SNAP = 24
/** 对话上下文注入的文件全文截断长度。 */
const CONTEXT_TEXT_LIMIT = 12000

/** 悬浮位置。 */
interface DragPos {
  x: number
  y: number
}

/** 可用边界（视口或弹窗容器矩形；坐标一律为视口坐标系）。 */
interface Bounds {
  left: number
  top: number
  width: number
  height: number
}

/** 读取记忆的悬浮位置（非法/缺失返回 null；snap 宽松校验）。 */
function loadPos(): (DragPos & { snap: 'left' | 'right' | null }) | null {
  try {
    const raw = window.localStorage.getItem(WIDGET_POS_KEY)
    if (!raw) return null
    const v = JSON.parse(raw) as Partial<DragPos & { snap: unknown }>
    const x = Number(v.x)
    const y = Number(v.y)
    if (!Number.isFinite(x) || !Number.isFinite(y)) return null
    return { x, y, snap: v.snap === 'left' || v.snap === 'right' ? v.snap : null }
  } catch {
    return null
  }
}

/** 持久化悬浮位置与吸附边（失败静默）。 */
function persistPos(p: DragPos & { snap: 'left' | 'right' | null }): void {
  try {
    window.localStorage.setItem(WIDGET_POS_KEY, JSON.stringify(p))
  } catch {
    /* ignore */
  }
}

/** 视口边界。 */
function viewportBounds(): Bounds {
  return { left: 0, top: 0, width: window.innerWidth, height: window.innerHeight }
}

/** 位置夹取进边界（按当前元素尺寸；边界过小时贴边界内缘）。 */
function clampPos(p: DragPos, w: number, h: number, b: Bounds): DragPos {
  return {
    x: Math.min(Math.max(p.x, b.left + VIEW_MARGIN), Math.max(b.left + VIEW_MARGIN, b.left + b.width - w - VIEW_MARGIN)),
    y: Math.min(Math.max(p.y, b.top + VIEW_MARGIN), Math.max(b.top + VIEW_MARGIN, b.top + b.height - h - VIEW_MARGIN)),
  }
}

/** 吸附态 x 坐标（半嵌：球一半露出边界外）。 */
function snappedX(side: 'left' | 'right', b: Bounds): number {
  return side === 'left' ? b.left - BALL_SIZE / 2 : b.left + b.width - BALL_SIZE / 2
}

/** 吸附态仅夹取 y（x 半嵌边界外，不参与夹取）。 */
function clampSnappedY(y: number, b: Bounds): number {
  return Math.min(Math.max(y, b.top + VIEW_MARGIN), Math.max(b.top + VIEW_MARGIN, b.top + b.height - BALL_SIZE - VIEW_MARGIN))
}

/** 卡片实际尺寸（按边界收缩：窄/矮视口或弹窗不超出）。 */
function cardSize(b: Bounds): { w: number; h: number } {
  return {
    w: Math.min(CARD_W, Math.max(160, b.width - VIEW_MARGIN * 2)),
    h: Math.min(CARD_H, Math.max(200, b.height - VIEW_MARGIN * 2)),
  }
}

/** 对话消息（会话内自增 id：流式回调按 id 定位更新）。 */
interface WidgetTurn {
  id: number
  role: 'user' | 'assistant'
  content: string
  streaming?: boolean
  stopped?: boolean
  error?: string
}

/** 从 AI 回答提取保存内容：优先最长的围栏代码块（```).+?```），无代码块时整段兜底。 */
function extractSaveContent(reply: string): string {
  const blocks: string[] = []
  const re = /```[\w+-]*\r?\n([\s\S]*?)```/g
  let m: RegExpExecArray | null
  while ((m = re.exec(reply)) !== null) blocks.push(m[1])
  if (blocks.length > 0) {
    return blocks.reduce((a, b) => (b.length > a.length ? b : a)).replace(/\s+$/, '')
  }
  return reply.trim()
}

/** 查看页悬浮 AI 助理（fileId/fileName 必填；onSaved = 保存新版本后的刷新回调）。 */
export default function ViewerAIWidget({
  fileId,
  fileName,
  onSaved,
}: {
  fileId: string
  fileName: string
  onSaved?: () => void
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const aiOn = useAIEnabled()
  const { message } = AntdApp.useApp()
  const textLike = isTextLike(fileName, '')

  // ---- 悬浮球 / 卡片：位置与拖动（边界 = 视口或弹窗容器）----
  const [open, setOpen] = useState(false)
  // 隐藏锚点：挂在渲染点（弹窗标题行内），closest 向上探测是否在
  // .ant-modal-container（antd v6 弹窗内容盒）内——在则边界为弹窗矩形。
  const anchorRef = useRef<HTMLSpanElement | null>(null)
  const boundsRef = useRef<Bounds>(viewportBounds())
  const readBounds = (): Bounds => {
    const host = anchorRef.current?.closest<HTMLElement>('.ant-modal-container, .ant-modal-content')
    let next: Bounds | null = null
    if (host) {
      const r = host.getBoundingClientRect()
      if (r.width > 0 && r.height > 0) next = { left: r.left, top: r.top, width: r.width, height: r.height }
    }
    boundsRef.current = next ?? viewportBounds()
    return boundsRef.current
  }
  const [pos, setPos] = useState<DragPos>(() => {
    const stored = loadPos()
    if (stored) return { x: stored.x, y: stored.y }
    return { x: window.innerWidth - BALL_SIZE - 24, y: window.innerHeight - BALL_SIZE - 24 }
  })
  const posRef = useRef(pos)
  // 吸附边（'left' | 'right' | null）：非空 = 半嵌圆点态（仅收起态球）。
  const [snap, setSnap] = useState<'left' | 'right' | null>(() => loadPos()?.snap ?? null)
  const [dragging, setDragging] = useState(false)
  const dragRef = useRef<{ startX: number; startY: number; origX: number; origY: number; moved: boolean; w: number; h: number } | null>(null)
  const suppressClickRef = useRef(false)

  const applyPos = (p: DragPos) => {
    posRef.current = p
    setPos(p)
  }

  // 挂载后按记忆/默认位置初始化（锚点此时已渲染，可探测弹窗边界）；
  // 初始化前悬浮球隐藏，避免按视口默认位置在弹窗外闪现一帧。
  const [inited, setInited] = useState(false)
  useEffect(() => {
    const b = readBounds()
    const stored = loadPos()
    if (stored?.snap) {
      applyPos({ x: snappedX(stored.snap, b), y: clampSnappedY(stored.y, b) })
    } else if (stored) {
      applyPos(clampPos(stored, BALL_SIZE, BALL_SIZE, b))
    } else {
      applyPos({ x: b.left + b.width - BALL_SIZE - 24, y: b.top + b.height - BALL_SIZE - 24 })
    }
    setInited(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const startDrag = (e: ReactPointerEvent<HTMLElement>) => {
    if (e.button !== 0) return
    e.preventDefault()
    const b = readBounds()
    const size = open ? cardSize(b) : { w: BALL_SIZE, h: BALL_SIZE }
    dragRef.current = {
      startX: e.clientX,
      startY: e.clientY,
      origX: posRef.current.x,
      origY: posRef.current.y,
      moved: false,
      w: size.w,
      h: size.h,
    }
    try {
      e.currentTarget.setPointerCapture(e.pointerId)
    } catch {
      /* ignore */
    }
    setDragging(true)
  }

  const moveDrag = (e: ReactPointerEvent<HTMLElement>) => {
    const d = dragRef.current
    if (!d) return
    const dx = e.clientX - d.startX
    const dy = e.clientY - d.startY
    if (!d.moved && Math.abs(dx) <= DRAG_THRESHOLD && Math.abs(dy) <= DRAG_THRESHOLD) return
    d.moved = true
    applyPos(clampPos({ x: d.origX + dx, y: d.origY + dy }, d.w, d.h, boundsRef.current))
  }

  const endDrag = () => {
    const d = dragRef.current
    dragRef.current = null
    setDragging(false)
    if (!d) return
    if (d.moved) {
      suppressClickRef.current = true
      // 边缘吸附：仅收起态球（卡片展开时保持完整在界内）；距边 <24px
      // 吸附为半嵌圆点（x 一半露出边界外，y 保持）。
      const b = boundsRef.current
      let side: 'left' | 'right' | null = null
      if (!open) {
        if (posRef.current.x - b.left < EDGE_SNAP) side = 'left'
        else if (b.left + b.width - (posRef.current.x + d.w) < EDGE_SNAP) side = 'right'
      }
      setSnap(side)
      if (side) applyPos({ x: snappedX(side, b), y: clampSnappedY(posRef.current.y, b) })
      persistPos({ ...posRef.current, snap: side })
    }
  }

  const toggleOpen = () => {
    const next = !open
    const b = readBounds()
    if (next) {
      // 展开：脱离吸附态，回到边界内完整可见。
      setSnap(null)
      const size = cardSize(b)
      applyPos(clampPos(posRef.current, size.w, size.h, b))
    } else {
      applyPos(clampPos(posRef.current, BALL_SIZE, BALL_SIZE, b))
    }
    setOpen(next)
    persistPos({ ...posRef.current, snap: null })
  }

  // 边界尺寸变化（视口 resize）：按当前形态（含吸附态）重新夹取位置。
  useEffect(() => {
    const onResize = () => {
      const b = readBounds()
      if (snap && !open) {
        applyPos({ x: snappedX(snap, b), y: clampSnappedY(posRef.current.y, b) })
        return
      }
      const size = open ? cardSize(b) : { w: BALL_SIZE, h: BALL_SIZE }
      applyPos(clampPos(posRef.current, size.w, size.h, b))
    }
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, snap])

  // ---- 页签 / 对话会话 ----
  const [tab, setTab] = useState<'chat' | 'summary'>('chat')
  const [turns, setTurns] = useState<WidgetTurn[]>([])
  const turnsRef = useRef<WidgetTurn[]>([])
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)
  const abortRef = useRef<AbortController | null>(null)
  const seqRef = useRef(0)
  const listRef = useRef<HTMLDivElement | null>(null)
  const [copiedId, setCopiedId] = useState<number | null>(null)
  // 模型选择（默认模型 + 下拉，选择记忆与全局助手共用同一 localStorage key）。
  const [models, setModels] = useState<AIModelOption[]>([])
  const [modelKey, setModelKey] = useState(() => {
    try {
      return window.localStorage.getItem(AI_MODEL_STORAGE_KEY) ?? ''
    } catch {
      return ''
    }
  })
  // 文件上下文全文缓存（首次发送时现取；保存新版本后以新内容覆盖）。
  const fileTextRef = useRef<string | null>(null)

  // 「保存为新版本」状态（消息级一次性按钮）。
  const [savingId, setSavingId] = useState<number | null>(null)
  const [savedIds, setSavedIds] = useState<Set<number>>(() => new Set())

  // ---- 摘要（/ai/summarize 流式）----
  const [summary, setSummary] = useState<{ loading: boolean; text: string; error: string }>({ loading: false, text: '', error: '' })
  const sumBusyRef = useRef(false)
  const sumAbortRef = useRef<AbortController | null>(null)

  const applyTurns = (fn: (prev: WidgetTurn[]) => WidgetTurn[]) => {
    setTurns((prev) => {
      const next = fn(prev)
      turnsRef.current = next
      return next
    })
  }

  const updateTurn = (id: number, patch: (turn: WidgetTurn) => Partial<WidgetTurn>) => {
    applyTurns((prev) => prev.map((x) => (x.id === id ? { ...x, ...patch(x) } : x)))
  }

  /** 文件上下文系统提示（文本类：文件名 + 全文截 12000 字；非文本：仅文件名）。 */
  const ensureFileContext = async (): Promise<string> => {
    if (!textLike) {
      return zh
        ? `用户正在 DocFlow 查看文件「${fileName}」（非文本类文件，无法提供全文，仅可就文件名对话）。`
        : `The user is viewing "${fileName}" in DocFlow (non-text file; no full text available).`
    }
    if (fileTextRef.current === null) {
      try {
        fileTextRef.current = await fetchFileText(fileId)
      } catch {
        fileTextRef.current = ''
      }
    }
    const full = fileTextRef.current ?? ''
    const truncated = full.length > CONTEXT_TEXT_LIMIT
    return zh
      ? `用户正在 DocFlow 查看文件「${fileName}」。文件全文如下${truncated ? `（超过 ${CONTEXT_TEXT_LIMIT} 字已截断）` : ''}：\n\n${full.slice(0, CONTEXT_TEXT_LIMIT)}`
      : `The user is viewing "${fileName}" in DocFlow. Full text below${truncated ? ` (truncated to ${CONTEXT_TEXT_LIMIT} chars)` : ''}:\n\n${full.slice(0, CONTEXT_TEXT_LIMIT)}`
  }

  /** 发送一轮对话（多轮历史 + 文件上下文 system 前置）。 */
  const send = async (question: string) => {
    const text = question.trim()
    if (!text || busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setInput('')
    const assistantId = ++seqRef.current
    const history: AIMessage[] = turnsRef.current
      .filter((x) => !x.error && x.content)
      .map((x) => ({ role: x.role, content: x.content }))
    applyTurns((p) => [
      ...p,
      { id: ++seqRef.current, role: 'user', content: text },
      { id: assistantId, role: 'assistant', content: '', streaming: true },
    ])
    const ac = new AbortController()
    abortRef.current = ac
    try {
      const sys = await ensureFileContext()
      const selected = models.find((m) => m.id === modelKey) ?? null
      await aiChat(
        {
          messages: [{ role: 'system', content: sys }, ...history, { role: 'user', content: text }],
          providerId: selected?.providerId || undefined,
          model: selected ? { providerId: selected.providerId, modelId: selected.model } : undefined,
        },
        {
          onDelta: (chunk) => updateTurn(assistantId, (x) => ({ content: x.content + chunk })),
          onDone: () => { /* 用量等元信息不展示 */ },
        },
        ac.signal,
      )
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') {
        updateTurn(assistantId, () => ({ stopped: true }))
      } else {
        updateTurn(assistantId, () => ({ error: err instanceof Error ? err.message : t(locale, 'aiAssistantErr') }))
      }
    } finally {
      updateTurn(assistantId, () => ({ streaming: false }))
      busyRef.current = false
      setBusy(false)
      if (abortRef.current === ac) abortRef.current = null
    }
  }

  /** 生成/重新生成摘要（流式）。 */
  const runSummary = async () => {
    if (sumBusyRef.current) return
    sumBusyRef.current = true
    setSummary({ loading: true, text: '', error: '' })
    const ac = new AbortController()
    sumAbortRef.current = ac
    try {
      await aiSummarizeFileStream(
        fileId,
        (chunk) => setSummary((p) => ({ ...p, text: p.text + chunk })),
        ac.signal,
      )
    } catch (err) {
      if (!(err instanceof Error && err.name === 'AbortError')) {
        setSummary((p) => ({ ...p, error: err instanceof Error ? err.message : t(locale, 'aiSummaryFailed') }))
      }
    } finally {
      setSummary((p) => ({ ...p, loading: false }))
      sumBusyRef.current = false
      if (sumAbortRef.current === ac) sumAbortRef.current = null
    }
  }

  // 打开且切到「摘要」页签时自动生成一次（无结果且无错误时）。
  useEffect(() => {
    if (open && tab === 'summary' && !summary.text && !summary.error && !summary.loading) void runSummary()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, tab])

  // 展开时拉取可选模型列表（失败/未配置返回空 → 选择器隐藏）。
  useEffect(() => {
    if (!aiOn || !open) return
    void getAIModels().then((list) => {
      setModels(list)
      setModelKey((cur) => {
        if (list.length === 0) return cur
        if (cur && list.some((m) => m.id === cur)) return cur
        const dkey = defaultAIModelKey()
        return dkey && list.some((m) => m.id === dkey) ? dkey : cur
      })
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [aiOn, open])

  // 文件切换（同页复用组件时）：清空会话/摘要/上下文缓存。
  useEffect(() => {
    turnsRef.current = []
    setTurns([])
    setSummary({ loading: false, text: '', error: '' })
    fileTextRef.current = null
    setSavedIds(new Set())
  }, [fileId])

  // 新回合/流式更新时滚动到底部。
  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [turns, summary.text])

  /** 复制回答（短暂 ✓ 反馈）。 */
  const copyTurn = (turn: WidgetTurn) => {
    void navigator.clipboard.writeText(turn.content)
    setCopiedId(turn.id)
    window.setTimeout(() => setCopiedId((cur) => (cur === turn.id ? null : cur)), 1200)
  }

  /** 保存为新版本：提取代码块/全文 → uploadFileVersion 覆盖当前 file_id
   * （自动留版本链，可在历史版本回退）→ onSaved 通知查看页刷新预览。 */
  const saveAsVersion = async (turn: WidgetTurn) => {
    if (!textLike || savingId !== null) return
    const content = extractSaveContent(turn.content)
    if (!content) return
    setSavingId(turn.id)
    try {
      await uploadFileVersion(new File([content], fileName, { type: 'text/plain' }), fileId, () => {})
      fileTextRef.current = content
      setSavedIds((prev) => new Set(prev).add(turn.id))
      void message.success(zh ? '已保存为新版本，可在历史版本回退' : 'Saved as a new version; restore from version history if needed')
      onSaved?.()
    } catch (err) {
      void message.error(err instanceof Error ? err.message : zh ? '保存失败' : 'Failed to save')
    } finally {
      setSavingId(null)
    }
  }

  // AI 未启用：不渲染（hooks 已全部声明完毕）。
  if (!aiOn) return null

  /** 文本类文件的「保存为新版本」按钮（Popconfirm 确认；已保存变 ✓ 禁用）。 */
  const saveButton = (turn: WidgetTurn) => {
    const saved = savedIds.has(turn.id)
    const btn = (
      <Button
        size="small"
        type="text"
        className="viewer-aiw-act"
        loading={savingId === turn.id}
        disabled={saved || savingId !== null && savingId !== turn.id}
        aria-label={zh ? '保存为新版本' : 'Save as new version'}
      >
        {saved ? <Check size={13} strokeWidth={2} aria-hidden="true" /> : <Save size={13} strokeWidth={2} aria-hidden="true" />}
      </Button>
    )
    if (saved) {
      return (
        <Tooltip title={zh ? '已保存为新版本' : 'Saved as a new version'}>{btn}</Tooltip>
      )
    }
    return (
      <Popconfirm
        title={zh ? '保存为新版本？' : 'Save as a new version?'}
        description={zh ? '将覆盖当前文件内容，原内容自动留存，可在历史版本回退。' : 'This overwrites the file; the previous content is kept in version history.'}
        okText={zh ? '保存' : 'Save'}
        cancelText={zh ? '取消' : 'Cancel'}
        onConfirm={() => void saveAsVersion(turn)}
      >
        {btn}
      </Popconfirm>
    )
  }

  // 卡片尺寸（按当前边界收缩；弹窗内挂载时随弹窗大小）。
  const card = cardSize(boundsRef.current)

  return (
    <>
      {/* 边界探测锚点（display:none 不参与布局，仅 closest 向上找弹窗容器）。 */}
      <span ref={anchorRef} className="viewer-aiw-anchor" aria-hidden="true" />
      {open ? (
        <div
          className={`viewer-aiw-card${dragging ? ' dragging' : ''}`}
          style={{ left: pos.x, top: pos.y, width: card.w, height: card.h }}
          role="dialog"
          aria-label={zh ? 'AI 助理' : 'AI assistant'}
        >
          {/* 头部：拖动把手 + 标题/文件名 + 收起。 */}
          <div
            className={`viewer-aiw-head${dragging ? ' dragging' : ''}`}
            onPointerDown={startDrag}
            onPointerMove={moveDrag}
            onPointerUp={endDrag}
            onPointerCancel={endDrag}
          >
            <span className="viewer-aiw-title">
              <Sparkles size={14} strokeWidth={2} aria-hidden="true" />
              {zh ? 'AI 助理' : 'AI Assistant'}
            </span>
            <span className="viewer-aiw-file" title={fileName}>{fileName}</span>
            <Button
              type="text"
              size="small"
              aria-label={t(locale, 'close')}
              title={t(locale, 'close')}
              onClick={toggleOpen}
            >
              <ChevronDown size={14} strokeWidth={2} aria-hidden="true" />
            </Button>
          </div>
          <div className="viewer-aiw-tabs">
            <Segmented
              block
              size="small"
              value={tab}
              onChange={(v) => setTab(v as 'chat' | 'summary')}
              options={[
                { label: zh ? '对话' : 'Chat', value: 'chat' },
                { label: t(locale, 'aiSummary'), value: 'summary' },
              ]}
            />
          </div>
          {/* 内容区（对话/摘要共用滚动容器）。 */}
          <div className="viewer-aiw-body" ref={listRef}>
            {tab === 'chat' ? (
              <>
                {turns.length === 0 && (
                  <div className="viewer-aiw-empty muted">
                    {zh
                      ? `已注入「${fileName}」全文上下文，可直接提问、总结或让 AI 修改后保存为新版本。`
                      : `Context from "${fileName}" is injected. Ask, summarize, or let the AI revise and save a new version.`}
                  </div>
                )}
                {turns.map((turn) =>
                  turn.role === 'user' ? (
                    <div key={turn.id} className="viewer-aiw-bubble user">{turn.content}</div>
                  ) : (
                    <div key={turn.id} className="viewer-aiw-bubble assistant">
                      {turn.error ? (
                        <div className="error-text">{turn.error}</div>
                      ) : turn.content ? (
                        <>
                          <AIMarkdown text={turn.content} zh={zh} streaming={turn.streaming} />
                          {turn.streaming && <span className="ai-caret" aria-hidden="true" />}
                        </>
                      ) : turn.streaming ? (
                        <span className="muted">{t(locale, 'aiAssistantGenerating')}</span>
                      ) : turn.stopped ? (
                        <span className="muted">{t(locale, 'aiAssistantStopped')}</span>
                      ) : null}
                      {turn.stopped && turn.content && <span className="ai-stopped-tag">{t(locale, 'aiAssistantStopped')}</span>}
                      {!turn.streaming && !turn.error && turn.content && (
                        <div className="viewer-aiw-msg-actions">
                          <Tooltip title={copiedId === turn.id ? t(locale, 'aiAssistantCopied') : t(locale, 'aiAssistantCopy')}>
                            <Button size="small" type="text" className="viewer-aiw-act" aria-label={t(locale, 'aiAssistantCopy')} onClick={() => copyTurn(turn)}>
                              {copiedId === turn.id ? <Check size={13} strokeWidth={2} aria-hidden="true" /> : <Copy size={13} strokeWidth={2} aria-hidden="true" />}
                            </Button>
                          </Tooltip>
                          {textLike ? (
                            saveButton(turn)
                          ) : (
                            <Tooltip title={zh ? '仅文本类文件支持保存修改' : 'Only text-like files support saving changes'}>
                              <Button size="small" type="text" className="viewer-aiw-act" disabled aria-label={zh ? '保存为新版本' : 'Save as new version'}>
                                <Save size={13} strokeWidth={2} aria-hidden="true" />
                              </Button>
                            </Tooltip>
                          )}
                        </div>
                      )}
                    </div>
                  ),
                )}
              </>
            ) : (
              <>
                {summary.loading && !summary.text && <div className="muted">{t(locale, 'aiSummaryLoading')}</div>}
                {summary.text && (
                  <AIMarkdown text={summary.text} zh={zh} streaming={summary.loading} />
                )}
                {summary.loading && summary.text && <span className="ai-caret" aria-hidden="true" />}
                {summary.error && <div className="error-text">{summary.error}</div>}
                {!summary.loading && summary.text && (
                  <div className="viewer-aiw-sum-actions">
                    <Button size="small" onClick={() => void runSummary()}>
                      <RotateCcw size={12} strokeWidth={2} aria-hidden="true" />
                      {zh ? '重新生成' : 'Regenerate'}
                    </Button>
                    <Button
                      size="small"
                      onClick={() => {
                        void navigator.clipboard.writeText(summary.text)
                        void message.success(t(locale, 'aiAssistantCopied'))
                      }}
                    >
                      <Copy size={12} strokeWidth={2} aria-hidden="true" />
                      {t(locale, 'aiAssistantCopy')}
                    </Button>
                  </div>
                )}
              </>
            )}
          </div>
          {/* 底部：模型选择 + 清空（对话页签）/ 输入行。 */}
          {tab === 'chat' && (
            <div className="viewer-aiw-input">
              <div className="viewer-aiw-input-row">
                {models.length > 0 && (
                  <Select
                    size="small"
                    className="viewer-aiw-model"
                    value={modelKey || undefined}
                    placeholder={zh ? '默认模型' : 'Default model'}
                    onChange={(v) => {
                      setModelKey(v)
                      try {
                        window.localStorage.setItem(AI_MODEL_STORAGE_KEY, v)
                      } catch {
                        /* ignore */
                      }
                    }}
                    options={models.map((m) => ({ value: m.id, label: `${m.providerName || m.providerId} / ${m.model}` }))}
                  />
                )}
                <span className="viewer-aiw-input-spacer" />
                <Tooltip title={t(locale, 'aiAssistantClear')}>
                  <Button size="small" type="text" className="viewer-aiw-act" disabled={turns.length === 0} aria-label={t(locale, 'aiAssistantClear')} onClick={() => { abortRef.current?.abort(); applyTurns(() => []) }}>
                    <Trash2 size={13} strokeWidth={2} aria-hidden="true" />
                  </Button>
                </Tooltip>
              </div>
              <div className="viewer-aiw-input-row">
                <Input.TextArea
                  autoSize={{ minRows: 1, maxRows: 4 }}
                  value={input}
                  placeholder={t(locale, 'aiAssistantPlaceholder')}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={(e) => {
                    // Enter 发送 / Shift+Enter 换行；输入法组合中 Enter 不发送。
                    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                      e.preventDefault()
                      void send(input)
                    }
                  }}
                />
                {busy ? (
                  <Button
                    className="ai-stop-btn"
                    shape="circle"
                    size="small"
                    aria-label={t(locale, 'aiAssistantStop')}
                    title={t(locale, 'aiAssistantStop')}
                    onClick={() => abortRef.current?.abort()}
                  >
                    <Square size={10} fill="currentColor" strokeWidth={0} aria-hidden="true" />
                  </Button>
                ) : (
                  <Button
                    className="ai-send-btn"
                    type="primary"
                    shape="circle"
                    size="small"
                    disabled={!input.trim()}
                    aria-label={t(locale, 'aiAssistantSend')}
                    title={t(locale, 'aiAssistantSend')}
                    onClick={() => void send(input)}
                  >
                    <Send size={13} strokeWidth={2} aria-hidden="true" />
                  </Button>
                )}
              </div>
            </div>
          )}
        </div>
      ) : (
        <button
          type="button"
          className={`viewer-aiw-ball${dragging ? ' dragging' : ''}${snap ? ' snapped' : ''}`}
          style={{ left: pos.x, top: pos.y, visibility: inited ? undefined : 'hidden' }}
          onPointerDown={startDrag}
          onPointerMove={moveDrag}
          onPointerUp={endDrag}
          onPointerCancel={endDrag}
          onClick={() => {
            if (suppressClickRef.current) {
              suppressClickRef.current = false
              return
            }
            toggleOpen()
          }}
          aria-label={zh ? 'AI 助理' : 'AI assistant'}
          title={zh ? 'AI 助理：摘要 / 对话 / 修改' : 'AI assistant: summary / chat / edit'}
        >
          <Sparkles size={16} strokeWidth={2} aria-hidden="true" />
        </button>
      )}
    </>
  )
}
