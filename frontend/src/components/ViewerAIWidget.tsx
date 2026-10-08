// 查看页悬浮 AI 助理（替换查看页原 AI 摘要按钮行）：
// - 收起态：56px 悬浮球（Sparkles，主色）；pointer 事件手写拖动
//   （位移 >4px 判定为拖动，否则视为点击展开/收起），位置记忆 localStorage
//   （docflow.viewer-ai.pos）；默认 CSS right/bottom 贴窗口右下角（v3.8），
//   双击重置回默认；
// - 弹窗内挂载（FileBrowser 查看弹窗标题行）：默认锚点切到弹窗容器右下角；
// - 展开态卡片（v3.9）：360×520 固有比例（按边界收缩），位置以球锚点经
//   cardPosFor 翻转——空间足够向左上展开（默认右下角形态），不足时向右下
//   展开，并夹取在可用边界（弹窗矩形/视口）内，绝不越界遮出边缘；
//   卡片 = 头部（标题 + 文件名 + 收起）+ Segmented「对话 | 摘要」+ 内容区 +
//   输入区（Cherry Studio 式单行工具栏：联网/思考/清空 + 模型选择 + 发送键
//   最右端，与 AI 助理/AI 创作同款 chat-send-btn）：
//   · 对话：aiChat SSE 流式（系统提示注入当前文件全文截 12000 字），文本类
//     文件的 AI 回答提供消息级「保存为新版本」（围栏代码块优先 →
//     uploadFileVersion 覆盖当前 file_id，自动留版本链）；
//   · 摘要：/ai/summarize 流式，markdown 展示 + 复制。
// - AI 未启用（useAIEnabled=false）不渲染。
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { PointerEvent as ReactPointerEvent } from 'react'
import { App as AntdApp, Button, Input, Popconfirm, Segmented, Select, Tooltip } from 'antd'
import { Check, ChevronDown, Copy, Globe, Brain, RotateCcw, Save, Sparkles, Trash2 } from 'lucide-react'
import AIMarkdown from './AIMarkdown'
import { AIModelOption, AI_MODEL_STORAGE_KEY, AI_THINK_STORAGE_KEY, AI_WEB_STORAGE_KEY, defaultAIModelKey, getAIModels, readAIFlag, writeAIFlag } from './AIAssistant'
import { AIChatThinking, AIToolChain, applyToolResult, toolEntryFrom } from './aichat'
import { CHAT_SEND_ICON, CHAT_STOP_ICON } from './aichat/icons'
import type { AIToolCallEntry } from './aichat'
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

/** 悬浮位置持久化 key（{x,y}：x/y 为悬浮球左上角（视口坐标））。 */
const WIDGET_POS_KEY = 'docflow.viewer-ai.pos'
/** 悬浮球尺寸 / 卡片尺寸（px）。 */
const BALL_SIZE = 44
const CARD_W = 360
const CARD_H = 520
/** 默认边距：球/卡片距边界（px）。 */
const EDGE_MARGIN = 24
/** 卡片与边界的最小安全间距（px）。 */
const CARD_GAP = 8
/** 拖动阈值：位移超过该值判定为拖动（否则视为点击）。 */
const DRAG_THRESHOLD = 4
/** 对话上下文注入的文件全文截断长度。 */
const CONTEXT_TEXT_LIMIT = 12000

/** 可用边界（视口坐标；弹窗内挂载时 = 弹窗容器矩形）。 */
interface HostBounds { x: number; y: number; w: number; h: number }

/** 计算展开卡片位置（翻转向）：以悬浮球右下角为锚，优先向左上展开（球在
 * 右下角的默认形态）；左/上空间不足时向右下展开，最终夹取在边界内。
 * 返回卡片定宽高（按边界收缩），保持 360×520 的固有比例。 */
function cardPosFor(ballX: number, ballY: number, b: HostBounds): { left: number; top: number; width: number; height: number } {
  const w = Math.min(CARD_W, b.w - CARD_GAP * 2)
  const h = Math.min(CARD_H, b.h - CARD_GAP * 2)
  const left = ballX + BALL_SIZE - w >= b.x + CARD_GAP
    ? ballX + BALL_SIZE - w
    : Math.min(ballX + BALL_SIZE + CARD_GAP, b.x + b.w - w - CARD_GAP)
  const top = ballY + BALL_SIZE - h >= b.y + CARD_GAP
    ? ballY + BALL_SIZE - h
    : Math.min(ballY + BALL_SIZE + CARD_GAP, b.y + b.h - h - CARD_GAP)
  return {
    left: Math.min(Math.max(left, b.x + CARD_GAP), b.x + b.w - w - CARD_GAP),
    top: Math.min(Math.max(top, b.y + CARD_GAP), b.y + b.h - h - CARD_GAP),
    width: w,
    height: h,
  }
}

/** 悬浮位置：null = 默认（CSS right/bottom 贴角），否则为拖拽后的自定义
 *  视口坐标（left/top 定位）。双击清除回默认。 */
type CustomPos = { x: number; y: number } | null

/** 读取记忆的自定义位置（null = 使用默认 right/bottom 贴角）。 */
function loadPos(): CustomPos {
  try {
    const raw = window.localStorage.getItem(WIDGET_POS_KEY)
    if (!raw) return null
    const v = JSON.parse(raw) as Partial<{ x: unknown; y: unknown }>
    const x = Number(v.x)
    const y = Number(v.y)
    if (!Number.isFinite(x) || !Number.isFinite(y)) return null
    // 位置超出当前视口 → 视为过期，回默认。
    if (x < 0 || y < 0 || x > window.innerWidth || y > window.innerHeight) return null
    return { x, y }
  } catch {
    return null
  }
}

/** 持久化自定义位置（null = 清除，回默认贴角）。 */
function persistPos(p: CustomPos): void {
  try {
    if (p) window.localStorage.setItem(WIDGET_POS_KEY, JSON.stringify(p))
    else window.localStorage.removeItem(WIDGET_POS_KEY)
  } catch {
    /* ignore */
  }
}

/** 视口内夹取（防拖出屏幕）。 */
function clampToViewport(p: { x: number; y: number }): { x: number; y: number } {
  return {
    x: Math.min(Math.max(p.x, 0), Math.max(0, window.innerWidth - BALL_SIZE)),
    y: Math.min(Math.max(p.y, 0), Math.max(0, window.innerHeight - BALL_SIZE)),
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
  /** 推理思考聚合文本（SSE thinking 增量；折叠区展示，不计入正文）。 */
  thinking?: string
  thinkingStartedAt?: number
  thinkingMS?: number
  /** 外部工具调用（SSE tool/tool_result 生命周期合并）。 */
  toolCalls?: AIToolCallEntry[]
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

  // ---- 悬浮球 / 卡片：位置与拖动（v3.10）----
  // 默认用 CSS right/bottom 贴角（窗口 resize 自动适配，零 JS）；弹窗内挂载
  // 时改贴弹窗容器右下角（JS 定位）；拖拽后切 left/top 自定义；双击重置。
  // 显示时机：挂载后延迟 ~250ms（弹窗动画/DOM 稳定）测量边界完成才显示，
  // 弹窗内不会「先视口右下角再跳到弹窗右下角」。
  const [open, setOpen] = useState(false)
  const [customPos, setCustomPos] = useState<CustomPos>(() => loadPos())
  const [dragging, setDragging] = useState(false)
  const [settled, setSettled] = useState(false)
  const dragRef = useRef<{ startX: number; startY: number; origX: number; origY: number; lastX: number; lastY: number; moved: boolean } | null>(null)
  const suppressClickRef = useRef(false)
  // 当前渲染根元素（球或卡片），用于判定弹窗内挂载与解析边界。
  const hostRef = useRef<HTMLElement | null>(null)
  const [modalRect, setModalRect] = useState<{ left: number; top: number; width: number; height: number } | null>(null)

  // 重测弹窗边界（无弹窗祖先 = null）：open 切换（根元素换位球⇄卡片）与
  // 窗口 resize（弹窗重新居中）时调用；useLayoutEffect 保证首帧前完成。
  const measureModal = () => {
    const modal = hostRef.current?.closest('.ant-modal') as HTMLElement | null
    if (modal) {
      const r = modal.getBoundingClientRect()
      setModalRect({ left: r.left, top: r.top, width: r.width, height: r.height })
    } else {
      setModalRect(null)
    }
  }
  useLayoutEffect(measureModal, [open])

  // 挂载后延迟测量并显示（弹窗动画完毕、DOM 稳定；消除弹窗内跳变）。
  useEffect(() => {
    const timer = window.setTimeout(() => {
      measureModal()
      setSettled(true)
    }, 250)
    return () => window.clearTimeout(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 卡片可用边界（弹窗内挂载 = 弹窗矩形；否则视口）。 */
  const hostBounds: HostBounds = modalRect
    ? { x: modalRect.left, y: modalRect.top, w: modalRect.width, h: modalRect.height }
    : { x: 0, y: 0, w: typeof window === 'undefined' ? CARD_W : window.innerWidth, h: typeof window === 'undefined' ? CARD_H : window.innerHeight }
  /** 边界右下角的默认球锚点（无自定义位置时的等效坐标）。 */
  const defaultAnchor = {
    x: hostBounds.x + hostBounds.w - EDGE_MARGIN - BALL_SIZE,
    y: hostBounds.y + hostBounds.h - EDGE_MARGIN - BALL_SIZE,
  }
  /** 球的锚点坐标：拖拽自定义 > 弹窗内=弹窗右下角 > null（CSS 视口贴角）。 */
  const ballAnchor: { x: number; y: number } | null = customPos
    ?? (modalRect
      ? { x: modalRect.left + modalRect.width - EDGE_MARGIN - BALL_SIZE, y: modalRect.top + modalRect.height - EDGE_MARGIN - BALL_SIZE }
      : null)
  /** 卡片翻转计算用的有效锚点（默认贴角也有等效坐标）。 */
  const anchorResolved = ballAnchor ?? defaultAnchor

  /** 球样式：null = 默认 CSS right/bottom；有值 = left/top 自定义。 */
  const posStyle = ballAnchor
    ? { left: ballAnchor.x, top: ballAnchor.y, right: 'auto', bottom: 'auto' }
    : {}

  /** 卡片位置/尺寸：以球锚点翻转/夹取（定高 520 比例，按边界收缩不越界）。 */
  const cardPos = cardPosFor(anchorResolved.x, anchorResolved.y, hostBounds)
  // 窗口 resize 时重算边界（视口取值直接生效；弹窗内挂载时弹窗矩形随
  // 居中变化，须重测 modalRect）。
  const [resizeTick, setResizeTick] = useState(0)
  useEffect(() => {
    const onResize = () => {
      measureModal()
      setResizeTick((v) => v + 1)
    }
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
  void resizeTick // 仅作 resize 重渲染触发器（读取一次避免未用告警）

  // ---- 拖动（v3.10 重写）：window 级 pointermove/up 监听 ----
  // 两处历史缺陷：① 球内 Sparkles 图标是 e.target，closest('button') 命中
  // 球自身被误判为「子交互控件」直接 return——点在图标上（球的正中央）
  // 无法拖动，表现为「拖不动、要碰运气」；② 仅依赖元素自身的 pointer
  // capture + React 合成事件，快速移动指针脱离元素后事件丢失。改为排除
  // 判定只挡「非自身」的交互控件，move/up 挂 window，拖动全程可靠。
  const moveDragRaw = useCallback((ev: PointerEvent) => {
    const d = dragRef.current
    if (!d) return
    const dx = ev.clientX - d.startX
    const dy = ev.clientY - d.startY
    if (!d.moved && Math.abs(dx) <= DRAG_THRESHOLD && Math.abs(dy) <= DRAG_THRESHOLD) return
    d.moved = true
    const next = clampToViewport({ x: d.origX + dx, y: d.origY + dy })
    d.lastX = next.x
    d.lastY = next.y
    setCustomPos(next)
  }, [])

  const endDragRaw = useCallback(() => {
    const d = dragRef.current
    dragRef.current = null
    setDragging(false)
    window.removeEventListener('pointermove', moveDragRaw)
    window.removeEventListener('pointerup', endDragRaw)
    window.removeEventListener('pointercancel', endDragRaw)
    if (d?.moved) {
      suppressClickRef.current = true
      persistPos({ x: d.lastX, y: d.lastY })
    }
  }, [moveDragRaw])

  const startDrag = (e: ReactPointerEvent<HTMLElement>) => {
    if (e.button !== 0) return
    // 排除非自身的子交互控件（卡片头部的收起按钮等）；球自身是 button，
    // 点在球内图标上时 closest 命中球自身 → 不排除（可拖）。
    const el = e.target as HTMLElement
    const interactive = el.closest('button, a, input, textarea, .ant-segmented')
    if (interactive && interactive !== e.currentTarget) return
    e.preventDefault()
    // 拖拽基准 = 球锚点（卡片头部拖拽同样移动球锚点，卡片随翻转逻辑跟随；
    // 默认贴角无自定义坐标时取边界右下角等效锚点）。
    dragRef.current = {
      startX: e.clientX,
      startY: e.clientY,
      origX: anchorResolved.x,
      origY: anchorResolved.y,
      lastX: anchorResolved.x,
      lastY: anchorResolved.y,
      moved: false,
    }
    try { e.currentTarget.setPointerCapture(e.pointerId) } catch { /* ignore */ }
    window.addEventListener('pointermove', moveDragRaw)
    window.addEventListener('pointerup', endDragRaw)
    window.addEventListener('pointercancel', endDragRaw)
    setDragging(true)
  }

  // 卸载兜底：移除可能残留的 window 拖动监听。
  useEffect(() => () => {
    window.removeEventListener('pointermove', moveDragRaw)
    window.removeEventListener('pointerup', endDragRaw)
    window.removeEventListener('pointercancel', endDragRaw)
  }, [moveDragRaw, endDragRaw])

  /** 双击重置位置（回到默认 CSS right/bottom 贴角）。 */
  const resetPos = () => {
    setCustomPos(null)
    persistPos(null)
  }

  const toggleOpen = () => {
    if (suppressClickRef.current) {
      suppressClickRef.current = false
      return
    }
    setOpen((v) => !v)
  }

  // v3.8：无需 resize handler——默认位置由 CSS right/bottom 自动适配，
  // 自定义位置仅在拖拽时设置（窗口缩小后 clampToViewport 兜底）。

  // ---- 页签 / 对话会话 ----
  const [tab, setTab] = useState<'chat' | 'summary'>('chat')
  const [turns, setTurns] = useState<WidgetTurn[]>([])
  const turnsRef = useRef<WidgetTurn[]>([])
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  // 对话图标开关（v3.4）：联网/思考，localStorage 记忆（与 AI 助手共用键）。
  const [web, setWeb] = useState(() => readAIFlag(AI_WEB_STORAGE_KEY) ?? false)
  const [think, setThink] = useState(() => readAIFlag(AI_THINK_STORAGE_KEY) ?? true)
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
  const [summary, setSummary] = useState<{ loading: boolean; text: string; error: string; thinking?: string; thinkingMS?: number }>({ loading: false, text: '', error: '' })
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
          // v3.4：图标开关随发送携带（联网/思考；模型支持推理才透传）。
          web_search: web,
          think: think && (selected?.reasoning ?? true),
        },
        {
          onDelta: (chunk) => updateTurn(assistantId, (x) => ({ content: x.content + chunk })),
          onThinking: (chunk) => updateTurn(assistantId, (x) => ({
            thinking: (x.thinking ?? '') + chunk,
            thinkingStartedAt: x.thinkingStartedAt ?? Date.now(),
          })),
          onTool: (tool) => updateTurn(assistantId, (x) => ({ toolCalls: [...(x.toolCalls ?? []), toolEntryFrom(tool)] })),
          onToolResult: (r) => updateTurn(assistantId, (x) => ({ toolCalls: applyToolResult(x.toolCalls ?? [], r) })),
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
      updateTurn(assistantId, (x) => (x.thinkingStartedAt ? { thinkingMS: Math.max(0, Date.now() - x.thinkingStartedAt) } : {}))
      updateTurn(assistantId, () => ({ streaming: false }))
      busyRef.current = false
      setBusy(false)
      if (abortRef.current === ac) abortRef.current = null
    }
  }

  /** 生成/重新生成摘要（流式；think 且模型支持时附思考折叠区）。 */
  const runSummary = async () => {
    if (sumBusyRef.current) return
    sumBusyRef.current = true
    setSummary({ loading: true, text: '', error: '' })
    const startedAt = Date.now()
    let hasThinking = false
    const ac = new AbortController()
    sumAbortRef.current = ac
    try {
      await aiSummarizeFileStream(
        fileId,
        (chunk) => setSummary((p) => ({ ...p, text: p.text + chunk })),
        ac.signal,
        (chunk) => {
          hasThinking = true
          setSummary((p) => ({ ...p, thinking: (p.thinking ?? '') + chunk }))
        },
      )
    } catch (err) {
      if (!(err instanceof Error && err.name === 'AbortError')) {
        setSummary((p) => ({ ...p, error: err instanceof Error ? err.message : t(locale, 'aiSummaryFailed') }))
      }
    } finally {
      setSummary((p) => ({ ...p, loading: false, thinkingMS: hasThinking ? Math.max(0, Date.now() - startedAt) : p.thinkingMS }))
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

  return (
    <>
      {open ? (
        <div
          ref={(el) => { hostRef.current = el }}
          className={`viewer-aiw-card${dragging ? ' dragging' : ''}${settled ? '' : ' aiw-pending'}`}
          /* v3.9：卡片恒定定宽高（360×520 固有比例，按边界收缩），位置经
             cardPosFor 翻转/夹取——不再出现接近 1:1 的挤压形态或越界。 */
          style={{ left: cardPos.left, top: cardPos.top, right: 'auto', bottom: 'auto', width: cardPos.width, height: cardPos.height }}
          role="dialog"
          aria-label={zh ? 'AI 助理' : 'AI assistant'}
        >
          {/* 头部：拖动把手 + 标题/文件名 + 收起。 */}
          <div
            className={`viewer-aiw-head${dragging ? ' dragging' : ''}`}
            onPointerDown={startDrag}
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
                      {/* 推理思考折叠区 + 工具调用链（与其他对话场景同款共享组件）。 */}
                      <AIChatThinking text={turn.thinking ?? ''} streaming={turn.streaming} thinkingMS={turn.thinkingMS} zh={zh} />
                      <AIToolChain toolCalls={turn.toolCalls} zh={zh} />
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
                      ) : (
                        // 兜底：流结束但无正文/错误/停止标记——给出可见提示而非空白。
                        <span className="muted">{zh ? '（模型未返回内容）' : '(no content returned)'}</span>
                      )}
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
                <AIChatThinking text={summary.thinking ?? ''} streaming={summary.loading} thinkingMS={summary.thinkingMS} zh={zh} />
                {summary.loading && !summary.text && !summary.thinking && <div className="muted">{t(locale, 'aiSummaryLoading')}</div>}
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
          {/* 底部输入区（v3.9 Cherry Studio 式：输入框 + 单行工具栏，发送/
              停止按钮在工具栏最右端=模型切换右侧，与 AI 助理/创作同款样式）。 */}
          {tab === 'chat' && (
            <div className="chat-input-box viewer-aiw-input">
              <Input.TextArea
                autoSize={{ minRows: 1, maxRows: 4 }}
                value={input}
                placeholder={t(locale, 'aiAssistantPlaceholder')}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                    e.preventDefault()
                    void send(input)
                  }
                }}
              />
              <div className="chat-tools-bar">
                <Tooltip title={zh ? '联网搜索（回答附网络来源）' : 'Web search (with sources)'}>
                  <button type="button" className={`ai-tool-icon${web ? ' on' : ''}`} aria-pressed={web} aria-label={zh ? '联网搜索' : 'Web search'} onClick={() => { const v = !web; setWeb(v); writeAIFlag(AI_WEB_STORAGE_KEY, v) }}>
                    <Globe size={15} strokeWidth={2} aria-hidden="true" />
                  </button>
                </Tooltip>
                <Tooltip title={zh ? '深度思考（所选模型须支持推理）' : 'Deep thinking (requires a reasoning model)'}>
                  <button type="button" className={`ai-tool-icon${think ? ' on' : ''}`} aria-pressed={think} aria-label={zh ? '深度思考' : 'Deep thinking'} onClick={() => { const v = !think; setThink(v); writeAIFlag(AI_THINK_STORAGE_KEY, v) }}>
                    <Brain size={15} strokeWidth={2} aria-hidden="true" />
                  </button>
                </Tooltip>
                <Tooltip title={t(locale, 'aiAssistantClear')}>
                  <Button size="small" type="text" disabled={turns.length === 0} aria-label={t(locale, 'aiAssistantClear')} onClick={() => { abortRef.current?.abort(); applyTurns(() => []) }}>
                    <Trash2 size={13} strokeWidth={2} aria-hidden="true" />
                  </Button>
                </Tooltip>
                <div className="chat-tools-right">
                  {models.length > 0 && (
                    <Select size="small" className="viewer-aiw-model" value={modelKey || undefined} placeholder={zh ? '默认模型' : 'Default model'}
                      onChange={(v) => { setModelKey(v); try { window.localStorage.setItem(AI_MODEL_STORAGE_KEY, v) } catch { /* ignore */ } }}
                      options={models.map((m) => ({ value: m.id, label: `${m.providerName || m.providerId} / ${m.model}` }))} />
                  )}
                </div>
                <span className="chat-send-btn-wrap">
                  {busy ? (
                    <button type="button" className="chat-stop-btn" aria-label={t(locale, 'aiAssistantStop')} title={t(locale, 'aiAssistantStop')} onClick={() => abortRef.current?.abort()}>
                      {CHAT_STOP_ICON}
                    </button>
                  ) : (
                    <button type="button" className="chat-send-btn" disabled={!input.trim()} aria-label={t(locale, 'aiAssistantSend')} title={t(locale, 'aiAssistantSend')} onClick={() => void send(input)}>
                      {CHAT_SEND_ICON}
                    </button>
                  )}
                </span>
              </div>
            </div>
          )}

        </div>
      ) : (
        <button
          type="button"
          ref={(el) => { hostRef.current = el }}
          className={`viewer-aiw-ball${dragging ? ' dragging' : ''}${settled ? '' : ' aiw-pending'}`}
          style={posStyle}
          onPointerDown={startDrag}
          onDoubleClick={resetPos}
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
          <Sparkles size={14} strokeWidth={2} aria-hidden="true" />
        </button>
      )}
    </>
  )
}
