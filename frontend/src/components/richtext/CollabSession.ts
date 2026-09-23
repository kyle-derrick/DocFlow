// DocFlow 富文本多人实时协作会话（.dfrt/.dfdoc，Tiptap + prosemirror-collab）。
// - 传输：GET /api/v1/collab/:fileId/ws（升级为 WebSocket；JWT 经
//   Sec-WebSocket-Protocol 的 bearer 子协议携带，与通知 WS 同一校验方式）；
// - 消息协议（JSON 信封）：
//   出站 {type:"join",name,color} / {type:"steps",steps,clientID,version} /
//        {type:"presence",selection:{anchor,head}} /
//        {type:"snapshot",doc,version,target}（leader 应答 sync-request：
//        doc=editor.getJSON()、version=本端已应用的最新权威版本，落后会被
//        服务端判 stale 并重发 sync-request，需再次上报直至追平） /
//        {type:"ping"}
//   入站 {type:"init",version,participants}（空房首成员） /
//        {type:"init-doc",doc,version,participants}（非空房加入者首条消息：
//        doc 为 leader 实时文档、version 为对应权威版本，作为编辑器基座） /
//        {type:"steps",version,steps,clientIDs} /
//        {type:"presence",connId,userId,name,color,selection,leader} /
//        {type:"leave",connId} / {type:"sync-begin"}（有 pending 加入者，
//        暂停非 leader 的 steps 提交） / {type:"sync-request",target}（要求
//        leader 上报实时快照） / {type:"sync-end",version}（恢复发送） /
//        {type:"error",message}
//   非致命 error：message 为 "syncing"（快照同步中非 leader steps 被暂缓，
//   保持 sendable 待 sync-end 补发）或 "stale snapshot"（leader 上报快照版本
//   落后，服务端已重发 sync-request）时仅提示、不断开会话。
// - 断线重连：5s/10s/20s 退避后固定 20s 持续重试；宿主可通过 maxFailedAttempts
//   限定连续失败次数（超出后停止重试并回调 onFatal，编辑器回退单机模式）；
// - 纯函数（collabColorFor/encode/decodeCollabMessage/mergeParticipants 等）
//   均导出，便于复用与编译期校验；远程光标为独立 ProseMirror 插件
//  （connId → 彩色竖线 + 名字标签，位置随 tr.mapping 映射）。
// 未做边界：断线期间他人 steps 无法追赶（重连后按版本差异提示刷新）；
// 同一用户多实例（同 userId 多连接）房间内不互通光标（connId 各自独立）。

import { Extension } from '@tiptap/core'
import { Plugin, PluginKey } from '@tiptap/pm/state'
import type { EditorState } from '@tiptap/pm/state'
import { Decoration, DecorationSet } from '@tiptap/pm/view'
import type { EditorView } from '@tiptap/pm/view'
import { collab as pmCollab } from '@tiptap/pm/collab'

/** 协作连接状态：connecting（含退避等待）| connected | offline。 */
export type CollabStatus = 'connecting' | 'connected' | 'offline'

/** 选区位置（与 ProseMirror Selection 的 anchor/head 对应）。 */
export interface CollabSelection {
  anchor: number
  head: number
}

/** 房间内一位参与者（connId 为服务端连接标识，leader 为保存协调者）。 */
export interface CollabParticipant {
  connId: string
  userId?: string
  name: string
  color: string
  leader?: boolean
  selection?: CollabSelection | null
}

/** 出站消息（JSON 信封，见文件头注释）。 */
export type CollabClientMessage =
  | { type: 'join'; name: string; color: string }
  | { type: 'steps'; steps: unknown[]; clientID: number; version: number }
  | { type: 'presence'; selection: CollabSelection | null }
  | { type: 'snapshot'; doc: unknown; version: number; target: string }
  | { type: 'ping' }

/** 入站消息（JSON 信封，见文件头注释）。 */
export type CollabServerMessage =
  | { type: 'init'; version: number; participants: CollabParticipant[] }
  | { type: 'init-doc'; doc: unknown; version: number; participants: CollabParticipant[] }
  | { type: 'steps'; version: number; steps: unknown[]; clientIDs: number[] }
  | { type: 'presence'; connId: string; userId?: string; name: string; color: string; selection: CollabSelection | null; leader?: boolean }
  | { type: 'leave'; connId: string }
  | { type: 'sync-begin' }
  | { type: 'sync-request'; target: string }
  | { type: 'sync-end'; version: number }
  | { type: 'error'; message: string }

/** 宿主页快照（leader 自动保存 / 协作离线提示用）。 */
export interface CollabSnapshot {
  status: CollabStatus
  participants: CollabParticipant[]
  selfConnId: string | null
  leaderConnId: string | null
  /** 提示文案（服务端 error / 版本不一致建议刷新等；空串为无）。 */
  message: string
}

// ---- 纯函数（导出供复用；tsc 编译期即覆盖） ----

/** 协作头像/光标调色板（8 色，暗色/亮色主题下均可用）。 */
const COLLAB_PALETTE = ['#f43f5e', '#f59e0b', '#10b981', '#3b82f6', '#8b5cf6', '#ec4899', '#14b8a6', '#84cc16']

/** 按 seed（userId）哈希固定分配调色板颜色：同一用户跨会话颜色稳定。 */
export function collabColorFor(seed: string): string {
  let hash = 0
  for (let i = 0; i < seed.length; i++) hash = (hash * 31 + seed.charCodeAt(i)) >>> 0
  return COLLAB_PALETTE[hash % COLLAB_PALETTE.length]
}

/** 生成随机 32 位协作 clientID（prosemirror-collab 数字客户端标识）。 */
export function randomCollabClientID(): number {
  return Math.floor(Math.random() * 0x7fffffff)
}

/** 头像首字（去空白后取首个字符的大写，空名回退 '?'）。 */
export function avatarCharOf(name: string): string {
  const first = name.trim().charAt(0)
  return first ? first.toUpperCase() : '?'
}

/** 协作 WS 地址（同源 ws/wss，fileId 编码入路径）。 */
export function collabWsUrl(fileId: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/api/v1/collab/${encodeURIComponent(fileId)}/ws`
}

function isSelection(v: unknown): v is CollabSelection {
  if (!v || typeof v !== 'object') return false
  const s = v as Record<string, unknown>
  return typeof s.anchor === 'number' && typeof s.head === 'number' && s.anchor >= 0 && s.head >= 0
}

function coerceParticipant(v: unknown): CollabParticipant | null {
  if (!v || typeof v !== 'object') return null
  const p = v as Record<string, unknown>
  if (typeof p.connId !== 'string' || typeof p.name !== 'string') return null
  return {
    connId: p.connId,
    userId: typeof p.userId === 'string' ? p.userId : undefined,
    name: p.name,
    color: typeof p.color === 'string' ? p.color : collabColorFor(p.userId && typeof p.userId === 'string' ? p.userId : p.connId),
    leader: p.leader === true,
    selection: isSelection(p.selection) ? p.selection : null,
  }
}

/** 出站消息编码（JSON 串）。 */
export function encodeCollabMessage(msg: CollabClientMessage): string {
  return JSON.stringify(msg)
}

/** 入站消息解码与校验：非法 JSON / 缺字段 / 未知类型返回 null（不抛错）。 */
export function decodeCollabMessage(raw: string): CollabServerMessage | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return null
  }
  if (!parsed || typeof parsed !== 'object') return null
  const m = parsed as Record<string, unknown>
  switch (m.type) {
    case 'init': {
      if (typeof m.version !== 'number' || !Array.isArray(m.participants)) return null
      const participants = m.participants.map(coerceParticipant).filter((p): p is CollabParticipant => p !== null)
      return { type: 'init', version: m.version, participants }
    }
    case 'init-doc': {
      if (!m.doc || typeof m.doc !== 'object' || typeof m.version !== 'number' || !Array.isArray(m.participants)) return null
      const participants = m.participants.map(coerceParticipant).filter((p): p is CollabParticipant => p !== null)
      return { type: 'init-doc', doc: m.doc, version: m.version, participants }
    }
    case 'steps': {
      if (typeof m.version !== 'number' || !Array.isArray(m.steps) || !Array.isArray(m.clientIDs)) return null
      const clientIDs = m.clientIDs.filter((id): id is number => typeof id === 'number')
      return { type: 'steps', version: m.version, steps: m.steps, clientIDs }
    }
    case 'sync-begin':
      return { type: 'sync-begin' }
    case 'sync-request':
      if (typeof m.target !== 'string') return null
      return { type: 'sync-request', target: m.target }
    case 'sync-end':
      if (typeof m.version !== 'number') return null
      return { type: 'sync-end', version: m.version }
    case 'presence': {
      const participant = coerceParticipant(m)
      if (!participant) return null
      return { type: 'presence', connId: participant.connId, userId: participant.userId, name: participant.name, color: participant.color, selection: participant.selection ?? null, leader: participant.leader }
    }
    case 'leave':
      if (typeof m.connId !== 'string') return null
      return { type: 'leave', connId: m.connId }
    case 'error':
      return { type: 'error', message: typeof m.message === 'string' ? m.message : 'collab error' }
    default:
      return null
  }
}

/** participants 归并：按 connId upsert（incoming 覆盖同名字段），保持既有顺序、新增追加。 */
export function mergeParticipants(base: CollabParticipant[], incoming: CollabParticipant[]): CollabParticipant[] {
  if (incoming.length === 0) return base.slice()
  const map = new Map<string, CollabParticipant>(base.map((p) => [p.connId, { ...p }]))
  for (const p of incoming) {
    const prev = map.get(p.connId)
    map.set(p.connId, prev ? { ...prev, ...p } : { ...p })
  }
  return Array.from(map.values())
}

/** participants 移除：按 connId 集合过滤（leave 用）。 */
export function dropParticipants(list: CollabParticipant[], connIds: string[]): CollabParticipant[] {
  if (connIds.length === 0) return list.slice()
  const drop = new Set(connIds)
  return list.filter((p) => !drop.has(p.connId))
}

// ---- 远程光标插件（Decoration.widget：彩色竖线 + 名字标签） ----

/** 一条远程光标（connId 绑定，anchor/head 为远端文档位置）。 */
export interface RemoteCaretEntry {
  connId: string
  name: string
  color: string
  anchor: number
  head: number
}

interface RemoteCaretsPluginState {
  carets: Map<string, RemoteCaretEntry>
  decos: DecorationSet
}

type RemoteCaretMeta =
  | { set: RemoteCaretEntry }
  | { remove: string }
  | { clear: true }

/** 远程光标插件状态键（dispatch meta 更新 / 读 getState 用）。 */
export const remoteCaretsKey = new PluginKey<RemoteCaretsPluginState>('docflowRemoteCarets')

function buildCaretDecorations(carets: Map<string, RemoteCaretEntry>, doc: EditorState['doc']): DecorationSet {
  const decos: Decoration[] = []
  carets.forEach((entry) => {
    const max = doc.content.size
    const pos = Math.min(Math.max(entry.head, 0), max)
    decos.push(
      Decoration.widget(
        pos,
        () => {
          const caret = document.createElement('span')
          caret.className = 'rich-text-remote-caret'
          caret.style.borderColor = entry.color
          caret.dataset.connId = entry.connId
          const label = document.createElement('span')
          label.className = 'rich-text-remote-caret-label'
          label.style.background = entry.color
          label.textContent = entry.name || '?'
          caret.appendChild(label)
          return caret
        },
        { side: -10, key: `remote-caret-${entry.connId}` },
      ),
    )
  })
  return DecorationSet.create(doc, decos)
}

/**
 * 远程光标插件：state 维护 connId → {anchor,head,name,color}，DecorationSet
 * 随本地/远端 steps 的 tr.mapping.map 自动更新位置；widget 渲染彩色竖线与
 * 名字标签（DOM 内联样式，class rich-text-remote-caret）。
 */
export function createRemoteCaretsPlugin(): Plugin<RemoteCaretsPluginState> {
  return new Plugin<RemoteCaretsPluginState>({
    key: remoteCaretsKey,
    state: {
      init: () => ({ carets: new Map(), decos: DecorationSet.empty }),
      apply: (tr, prev) => {
        const meta = tr.getMeta(remoteCaretsKey) as RemoteCaretMeta | undefined
        if (meta) {
          const carets = new Map(prev.carets)
          if ('set' in meta) carets.set(meta.set.connId, meta.set)
          else if ('remove' in meta) carets.delete(meta.remove)
          else carets.clear()
          return { carets, decos: buildCaretDecorations(carets, tr.doc) }
        }
        if (tr.docChanged && prev.carets.size > 0) {
          return { carets: prev.carets, decos: prev.decos.map(tr.mapping, tr.doc) }
        }
        return prev
      },
    },
    props: {
      decorations(state: EditorState) {
        return remoteCaretsKey.getState(state)?.decos ?? DecorationSet.empty
      },
    },
  })
}

/** 更新/新增一条远程光标（位置负值视为远端已失效 → 移除）。 */
export function setRemoteCaret(view: EditorView, entry: RemoteCaretEntry): void {
  if (entry.anchor < 0 || entry.head < 0) {
    removeRemoteCaret(view, entry.connId)
    return
  }
  view.dispatch(view.state.tr.setMeta(remoteCaretsKey, { set: entry }))
}

/** 移除一条远程光标（对端离开/失效）。 */
export function removeRemoteCaret(view: EditorView, connId: string): void {
  view.dispatch(view.state.tr.setMeta(remoteCaretsKey, { remove: connId }))
}

/** 清空全部远程光标（协作会话结束/回退单机时）。 */
export function clearRemoteCarets(view: EditorView): void {
  view.dispatch(view.state.tr.setMeta(remoteCaretsKey, { clear: true }))
}

/**
 * 协作扩展：注册 prosemirror-collab 的 collab({clientID,version}) 插件与
 * 远程光标插件（Extension.addProseMirrorPlugins，随编辑器实例生命周期）。
 * version 为编辑器基座对应的权威版本（空房首成员为 0，非空房加入者为
 * init-doc 的 version），保证 sendableSteps/receiveTransaction 的版本计数
 * 与服务端权威序列对齐。
 */
export function createCollabExtension(clientID: number, version: number): Extension {
  return Extension.create({
    name: 'docflowCollab',
    addProseMirrorPlugins() {
      return [pmCollab({ clientID, version }), createRemoteCaretsPlugin()]
    },
  })
}

// ---- 协作会话 ----

export interface CollabSessionOptions {
  /** WS 地址（collabWsUrl 生成）。 */
  url: string
  /** JWT access token；或经 tokenProvider 每次连接时动态取（页面刷新后
   * token 异步恢复完成前快照为 null 会导致 401 死循环——「协作连接中…」
   * 卡死的根因，故优先用 provider）。 */
  token?: string | null
  tokenProvider?: () => string | null
  /** 本端用户 ID（识别 participants 中的自己）。 */
  userId: string | null
  /** 展示名与分配色（join 携带）。 */
  name: string
  color: string
  onStatus?: (status: CollabStatus) => void
  /** participants 变化（init/init-doc/presence/leave 归并后全量快照）。 */
  onParticipants?: (participants: CollabParticipant[]) => void
  onPresence?: (participant: CollabParticipant) => void
  onLeave?: (connId: string) => void
  /** 进房 init（空房首成员；version 为服务端当前版本）——构造期回调，
   * 供 bootstrap 落编辑器基座（重连时宿主可与 lastServerVersion 比较漂移）。 */
  onInit?: (version: number, participants: CollabParticipant[]) => void
  /** 进房 init-doc（非空房加入者首条消息，构造期回调）：doc 为 leader 实时
   *  文档、version 为对应权威版本，宿主以此为编辑器基座。 */
  onInitDoc?: (doc: unknown, version: number, participants: CollabParticipant[]) => void
  onError?: (message: string) => void
  /** 彻底失败（error 信封 / 连续重连超限）：此后不再重试，宿主应回退单机。 */
  onFatal?: (reason: string) => void
  /** 连续连接失败次数上限（超出后 onFatal 并停止）；默认无限持续重试。 */
  maxFailedAttempts?: number
}

/** steps 应用回调（attachStepsSink 注入；编辑器就绪后才可应用）。 */
export type CollabStepsSink = (version: number, steps: unknown[], clientIDs: number[]) => void

/** sync 流程回调（attachHandlers 注入；编辑器就绪后才可处理）。 */
export interface CollabSyncHandlers {
  /** 有 pending 加入者：非 leader 暂停 steps 发送（由 sendsHeld 表达）。 */
  onSyncBegin?: () => void
  /** 快照同步完成（version 为当前权威版本）：恢复发送并补发 sendable。 */
  onSyncEnd?: (version: number) => void
  /** 要求本端（leader）上报实时快照（target 回填到 snapshot.target）。 */
  onSyncRequest?: (target: string) => void
}

/** 非致命 error 集合：'syncing'（同步中非 leader steps 被暂缓）与
 * 'stale snapshot'（快照版本落后，服务端已重发 sync-request）仅经 onError
 * 提示，不触发 onFatal、不断开会话；其余 error 均视为致命。 */
const NON_FATAL_COLLAB_ERRORS: ReadonlySet<string> = new Set(['syncing', 'stale snapshot'])

/** 重连退避序列：5s → 10s → 20s → 之后固定 20s。 */
const RETRY_BACKOFF_MS = [5_000, 10_000, 20_000]

/** 心跳 ping 间隔。 */
const PING_INTERVAL_MS = 25_000

/**
 * 协作会话：单条 WebSocket 的连接/join/收发/心跳/重连/成员维护。
 * 所有回调均在事件回调线程同步触发；close() 后不再产生任何回调。
 */
export class CollabSession {
  private opts: CollabSessionOptions
  private ws: WebSocket | null = null
  private participants: CollabParticipant[] = []
  private status: CollabStatus = 'offline'
  private selfConnId: string | null = null
  private stopped = false
  private failedAttempts = 0
  private retryTimer: number | null = null
  private pingTimer: number | null = null
  /** steps 发送暂停标志：sync-begin 置 true、sync-end 置 false（初始 false，
   * 重连后复位 false）。true 期间非 leader 不发送 steps（留 sendable 待
   * sync-end 后补发）；leader 不受影响（服务端放行其冲账 steps）。 */
  sendsHeld = false
  /** 编辑器侧 steps 应用回调（attachStepsSink 注入；未注入时 steps 入队）。 */
  private stepsSink: CollabStepsSink | null = null
  /** attach 前收到的 steps（按到达顺序排干，避免编辑器未建好时丢 steps）。 */
  private queuedSteps: Array<{ version: number; steps: unknown[]; clientIDs: number[] }> = []
  /** 编辑器侧 sync 流程回调（attachHandlers 注入）。 */
  private syncHandlers: CollabSyncHandlers | null = null
  /** 最近一次已知服务端权威版本（init/init-doc/steps/sync-end 推进）；
   * init/init-doc 在回调之后才更新，重连回调内仍可读到上次连接的值。 */
  private serverVersion: number | null = null

  constructor(options: CollabSessionOptions) {
    this.opts = options
  }

  /** 开始连接（join 在 onopen 后自动发送）。 */
  connect(): void {
    this.stopped = false
    this.setStatus('connecting')
    this.open()
  }

  /** 主动断开（宿主回退单机/组件卸载用）：停止重试与心跳，静默关闭。 */
  close(): void {
    this.stopped = true
    if (this.retryTimer !== null) {
      window.clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
    this.stopPing()
    if (this.ws) {
      const ws = this.ws
      this.ws = null
      try {
        ws.close()
      } catch {
        /* 已关闭 */
      }
    }
    this.setStatus('offline')
  }

  get currentStatus(): CollabStatus {
    return this.status
  }

  get currentParticipants(): CollabParticipant[] {
    return this.participants
  }

  /** 本端 connId（init/presence 中按 userId 匹配；未知为 null）。 */
  get currentSelfConnId(): string | null {
    return this.selfConnId
  }

  get leaderConnId(): string | null {
    return this.participants.find((p) => p.leader)?.connId ?? null
  }

  /** 最近一次已知服务端权威版本（未知为 null）；重连后 init 前仍保留上次
   * 连接的值，供宿主比较版本漂移。 */
  get lastServerVersion(): number | null {
    return this.serverVersion
  }

  /** 注入编辑器侧 steps 应用回调（编辑器就绪后调用）：未注入期间收到的
   * steps 先入内部队列，注入后按到达顺序排干。 */
  attachStepsSink(fn: CollabStepsSink): void {
    this.stepsSink = fn
    if (this.queuedSteps.length === 0) return
    const queued = this.queuedSteps
    this.queuedSteps = []
    for (const item of queued) fn(item.version, item.steps, item.clientIDs)
  }

  /** 注入编辑器侧 sync 流程回调（编辑器就绪后调用）；未注入期间 sync 消息
   * 仅维护 sendsHeld 标志，不回调。 */
  attachHandlers(handlers: CollabSyncHandlers): void {
    this.syncHandlers = handlers
  }

  /** 发送本端 steps（prosemirror-collab sendableSteps 的 JSON 化）。 */
  sendSteps(steps: unknown[], clientID: number, version: number): boolean {
    return this.send({ type: 'steps', steps, clientID, version })
  }

  /** leader 应答 sync-request：上报实时文档快照（doc=editor.getJSON()，
   * version=本端已应用的最新权威版本；落后会被服务端判 stale 并重发
   * sync-request，宿主需再次上报直至追平）。 */
  sendSnapshot(doc: unknown, version: number, target: string): boolean {
    return this.send({ type: 'snapshot', doc, version, target })
  }

  /** 发送选区 presence（null 表示清除光标）。 */
  sendPresence(selection: CollabSelection | null): boolean {
    return this.send({ type: 'presence', selection })
  }

  private send(msg: CollabClientMessage): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false
    try {
      this.ws.send(encodeCollabMessage(msg))
      return true
    } catch {
      return false
    }
  }

  private setStatus(status: CollabStatus): void {
    if (this.status === status) return
    this.status = status
    this.opts.onStatus?.(status)
  }

  private open(): void {
    if (this.stopped) return
    // token 动态获取：优先 tokenProvider（token 恢复前为 null 时短退避等待
    // 重试，不消耗失败次数——否则重试永远携带旧 null 快照，永卡「连接中」）。
    const token = this.opts.tokenProvider ? this.opts.tokenProvider() : (this.opts.token ?? null)
    if (token === null) {
      this.retryTimer = setTimeout(() => this.open(), 800)
      return
    }
    try {
      // JWT 走 Sec-WebSocket-Protocol 的 bearer 子协议（与通知 WS 同一校验）。
      this.ws = token
        ? new WebSocket(this.opts.url, ['bearer', token])
        : new WebSocket(this.opts.url)
    } catch {
      this.ws = null
      this.handleDisconnect()
      return
    }
    const ws = this.ws
    ws.onopen = () => {
      if (this.stopped || this.ws !== ws) return
      this.failedAttempts = 0
      // 重连后房间状态全新：清发送暂停（后续以 sync-begin/sync-end 重置）。
      this.sendsHeld = false
      this.setStatus('connected')
      this.send({ type: 'join', name: this.opts.name, color: this.opts.color })
      this.startPing()
    }
    ws.onmessage = (event) => {
      if (this.stopped || this.ws !== ws) return
      this.handleMessage(typeof event.data === 'string' ? event.data : '')
    }
    ws.onclose = () => {
      if (this.ws !== ws) return
      this.ws = null
      this.handleDisconnect()
    }
    ws.onerror = () => {
      /* 失败随后必触发 onclose，统一在 handleDisconnect 处理 */
    }
  }

  private handleDisconnect(): void {
    this.stopPing()
    if (this.stopped) return
    this.participants = []
    this.selfConnId = null
    this.opts.onParticipants?.([])
    const max = this.opts.maxFailedAttempts ?? Number.POSITIVE_INFINITY
    this.failedAttempts += 1
    if (this.failedAttempts > max) {
      this.stopped = true
      this.setStatus('offline')
      this.opts.onFatal?.(this.opts.maxFailedAttempts !== undefined ? `collab reconnect failed after ${this.failedAttempts} attempts` : 'collab connection failed')
      return
    }
    const delay = RETRY_BACKOFF_MS[Math.min(this.failedAttempts - 1, RETRY_BACKOFF_MS.length - 1)]
    this.setStatus('connecting')
    this.retryTimer = window.setTimeout(() => {
      this.retryTimer = null
      this.open()
    }, delay)
  }

  private fatal(message: string): void {
    this.stopped = true
    if (this.retryTimer !== null) {
      window.clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
    this.stopPing()
    if (this.ws) {
      const ws = this.ws
      this.ws = null
      try {
        ws.close()
      } catch {
        /* 已关闭 */
      }
    }
    this.setStatus('offline')
    this.opts.onFatal?.(message)
  }

  private startPing(): void {
    this.stopPing()
    this.pingTimer = window.setInterval(() => {
      this.send({ type: 'ping' })
    }, PING_INTERVAL_MS)
  }

  private stopPing(): void {
    if (this.pingTimer !== null) {
      window.clearInterval(this.pingTimer)
      this.pingTimer = null
    }
  }

  private handleMessage(raw: string): void {
    const msg = decodeCollabMessage(raw)
    if (!msg) return
    switch (msg.type) {
      case 'init':
        this.participants = msg.participants
        this.detectSelf()
        // 先回调再记版本：重连回调内 lastServerVersion 仍为上次连接的值。
        this.opts.onInit?.(msg.version, this.participants)
        this.serverVersion = msg.version
        this.opts.onParticipants?.(this.participants)
        break
      case 'init-doc':
        // 非空房加入者首条消息：leader 实时文档 + 权威版本 = 编辑器基座。
        this.participants = msg.participants
        this.detectSelf()
        this.opts.onInitDoc?.(msg.doc, msg.version, this.participants)
        this.serverVersion = msg.version
        this.opts.onParticipants?.(this.participants)
        break
      case 'steps':
        this.serverVersion = msg.version
        if (this.stepsSink) this.stepsSink(msg.version, msg.steps, msg.clientIDs)
        else this.queuedSteps.push({ version: msg.version, steps: msg.steps, clientIDs: msg.clientIDs })
        break
      case 'sync-begin':
        // 有 pending 加入者：非 leader 暂停 steps 发送（sendsHeld 表达）。
        this.sendsHeld = true
        this.syncHandlers?.onSyncBegin?.()
        break
      case 'sync-request':
        this.syncHandlers?.onSyncRequest?.(msg.target)
        break
      case 'sync-end':
        this.sendsHeld = false
        this.serverVersion = msg.version
        this.syncHandlers?.onSyncEnd?.(msg.version)
        break
      case 'presence': {
        const entry: CollabParticipant = {
          connId: msg.connId,
          userId: msg.userId,
          name: msg.name,
          color: msg.color,
          leader: msg.leader === true,
          selection: msg.selection,
        }
        this.participants = mergeParticipants(this.participants, [entry])
        if (entry.userId && entry.userId === this.opts.userId) this.selfConnId = entry.connId
        this.opts.onPresence?.(entry)
        this.opts.onParticipants?.(this.participants)
        break
      }
      case 'leave':
        this.participants = dropParticipants(this.participants, [msg.connId])
        this.opts.onParticipants?.(this.participants)
        this.opts.onLeave?.(msg.connId)
        break
      case 'error':
        // 非致命（syncing/stale snapshot）：仅提示、保持会话与重试语义；
        // 其余 error 信封维持致命（断开、回退单机）。
        this.opts.onError?.(msg.message)
        if (!NON_FATAL_COLLAB_ERRORS.has(msg.message)) this.fatal(msg.message)
        break
    }
  }

  /** 识别本端 connId：优先按 userId 匹配；单人房间唯一连接即自己。 */
  private detectSelf(): void {
    if (this.opts.userId) {
      const match = this.participants.find((p) => p.userId === this.opts.userId)
      if (match) {
        this.selfConnId = match.connId
        return
      }
    }
    if (this.participants.length === 1) this.selfConnId = this.participants[0].connId
  }
}
