// AI 创作空间（/studio）：以「项目 = 空间 + 根目录」为工作单元的三层工作台。
// 左：项目列表 + 目录/任务双栏；中：多会话 Agent 任务流（每条输入即一条
// 智能体任务：任务卡 + 右栏评审；Skill 模板填入、# 引用文件）；右：引用
// 文件管理 + 任务评审（diff 勾选写回/丢弃/回滚）。数据本地持久化
//（localStorage docflow.studio.*），任务状态 5s 轮询至终态；复用全局 ai-* 样式。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { App as AntdApp, Button, Input, Popover, Segmented, Select, Tooltip } from 'antd'
import type { TextAreaRef } from 'antd/es/input/TextArea'
import { Bot, Check, ChevronDown, ChevronRight, FilePlus2, FileText, FileType2, FolderClosed, FolderOpen, Paperclip, Plus, RefreshCw, Search, Send, Sparkles, Square, Trash2 } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { EMPTY_DFDOC_JSON, currentUserId, getMe, listAgentTasks, listFiles, listSpaces, listSpaceFiles, searchFiles, uploadFile } from '../api'
import type { AgentTask, FileItem, Space } from '../api'
import { applyAgentTask, cancelAgentTask, createAgentTask, discardAgentTask, getAgentTask, rollbackAgentTask } from '../agentTasks'
import { FileViewModal, Modal, formatTime } from '../components/FileBrowser'
import { AIToolCalls, AIWebSources } from '../components/AIAssistant'
import type { AIAttachFile, AIToolCallView, AIWebSource } from '../components/AIAssistant'
import { useAIFeatures } from '../aiFeature'
import { t, useLocale } from '../i18n'

// ---------- 数据结构与本地持久化 ----------

/** 项目条目（localStorage）：spaceName/folderPath 创建时记录（旧数据缺省，
 *  展示侧惰性解析空间名兜底）。 */
interface StudioProject {
  id: string; name: string; spaceId: string; rootFolderId: string; createdAt: string
  /** 绑定空间名（创建时快照；旧项目缺省）。 */
  spaceName?: string
  /** 绑定路径「空间名/目录名」（空间根项目 = 空间名；旧项目缺省）。 */
  folderPath?: string
}
interface StudioTurn {
  id: number; role: 'user' | 'assistant'; content: string; streaming?: boolean; stopped?: boolean; error?: string
  files?: AIAttachFile[]; webSources?: AIWebSource[]; task?: { id: string; prompt: string }
  /** 外部工具调用（SSE event:tool；随会话一并持久化到 localStorage）。 */
  toolCalls?: AIToolCallView[]
}
interface StudioSession { id: string; title: string; messages: StudioTurn[]; createdAt: string }

const projectsKey = (uid: string) => `docflow.studio.projects.${uid}`
const sessionsKey = (uid: string, pid: string) => `docflow.studio.sessions.${uid}.${pid}`
const taskIdsKey = (uid: string) => `docflow.studio.taskids.${uid}`

function loadJSON<T>(key: string, fallback: T): T {
  try {
    const raw = window.localStorage.getItem(key)
    return raw ? (JSON.parse(raw) as T) : fallback
  } catch {
    return fallback
  }
}
function saveJSON(key: string, value: unknown): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value))
  } catch {
    /* ignore */
  }
}
const localId = () => crypto.randomUUID()
const newSession = (): StudioSession => ({ id: localId(), title: '新会话', messages: [], createdAt: new Date().toISOString() })

/** 上传会话对新建文件可能回显的全零 UUID（视为无目标文件）。 */
const NIL_UUID = '00000000-0000-0000-0000-000000000000'

const TASK_STATUS: Record<string, { label: string; cls: string }> = {
  queued: { label: '排队中', cls: 'wait' }, running: { label: '运行中', cls: 'run' },
  succeeded: { label: '待评审', cls: 'ok' }, applied: { label: '已写回', cls: 'ok' },
  failed: { label: '失败', cls: 'bad' }, cancelled: { label: '已取消', cls: 'bad' },
  discarded: { label: '已丢弃', cls: 'bad' }, rolled_back: { label: '已回滚', cls: 'bad' },
}
const isTerminal = (status: string) => status !== 'queued' && status !== 'running'

function StatusBadge({ status }: { status: string }) {
  const meta = TASK_STATUS[status] ?? { label: status, cls: 'wait' }
  return <span className={`studio-badge studio-badge-${meta.cls}`}>{meta.label}</span>
}

async function sha256Hex(text: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))
  return Array.from(new Uint8Array(digest)).map((b) => b.toString(16).padStart(2, '0')).join('')
}

/** 解析 # 文件提及：取光标前最近一个「#」，且其后到光标无空格/换行才算有效。 */
function detectMention(text: string, caret: number): { idx: number; query: string } | null {
  const idx = text.slice(0, caret).lastIndexOf('#')
  if (idx < 0) return null
  const query = text.slice(idx + 1, caret)
  return /[\s]/.test(query) ? null : { idx, query }
}

// ---------- Skill 快捷模板（创作任务模式；{主题} 等占位符提示用户替换） ----------

const SKILL_TEMPLATES: Array<{ label: string; prompt: string }> = [
  { label: '建站落地页', prompt: '请为 {主题} 制作单页落地页网站，产出文件：\n1. index.html：现代响应式单页，语义化结构，内嵌 CSS/JS，无外部依赖；\n2. 包含：导航栏、Hero 主视觉（标题+副标题+行动按钮）、核心卖点 3-6 个卡片、功能详情、用户评价、FAQ、页脚联系方式；\n3. 配色排版统一克制，适配移动端，中文文案专业有说服力。' },
  { label: '项目文档', prompt: '请为「{主题}」项目生成一套 Markdown 项目文档：\n1. README.md：简介、亮点、快速开始、目录结构；\n2. docs/architecture.md：整体架构与模块职责；\n3. docs/getting-started.md：环境要求、安装、配置与运行步骤；\n4. docs/faq.md：常见问题与排查。\n要求结构清晰、标题层级规范、命令可直接复制执行。' },
  { label: '接口文档', prompt: '请为「{主题}」编写接口文档 api.md：\n1. 概述：基础地址、认证方式（Bearer）、通用错误码表；\n2. 按资源分组的接口清单：每个接口给出方法+路径、请求参数表（名称/类型/必填/说明）、成功与错误响应 JSON 示例；\n3. 至少覆盖核心资源的增删改查；\n4. 文末附 curl 调用示例。' },
  { label: '思维导图大纲', prompt: '请为「{主题}」生成思维导图大纲 outline.md（Markdown 多级列表）：\n1. 以主题为中心展开 4-6 个一级分支，每个分支再细分 2-4 层；\n2. 分支命名短促（≤10 字）；\n3. 覆盖概念、方法、案例与延伸阅读。' },
  { label: 'PPT 大纲', prompt: '请为「{主题}」生成 PPT 大纲 slides.md：\n1. 按页组织：每页一个二级标题（第 N 页：标题），下列 3-5 条要点（每条 ≤20 字）；\n2. 结构：封面 → 目录 → 背景/问题 → 方案主体（多页）→ 案例数据 → 总结 → Q&A；\n3. 标注建议的视觉形式（流程图、对比表格等）。' },
  { label: '数据报表', prompt: '请为「{主题}」生成数据报表 report.md：\n1. 报表说明：口径、统计周期、数据来源假设；\n2. 核心指标汇总表（Markdown 表格：指标/本期/上期/环比）；\n3. 分维度明细表与简要解读（每条 1-2 句结论）；\n4. 风险提示与后续行动建议（可执行清单）。' },
]

// ---------- 懒加载目录树（项目目录树 / 新建项目选目录共用） ----------

function LazyTree({ spaceId, rootId, onlyFolders = false, activeFolderId, activeFileIds, onFolder, onFile }: {
  spaceId: string; rootId: string | null; onlyFolders?: boolean; activeFolderId?: string | null; activeFileIds?: string[]
  onFolder?: (f: FileItem) => void; onFile?: (f: FileItem) => void
}) {
  const [children, setChildren] = useState<Record<string, FileItem[]>>({})
  const [open, setOpen] = useState<Record<string, boolean>>({})
  const [loading, setLoading] = useState<Record<string, boolean>>({})
  const [err, setErr] = useState('')
  const loadedRef = useRef<Set<string>>(new Set())

  const load = useCallback(async (fid: string | null) => {
    const key = fid ?? 'root'
    const dedupe = `${spaceId}:${key}`
    if (loadedRef.current.has(dedupe)) return
    loadedRef.current.add(dedupe)
    setLoading((p) => ({ ...p, [key]: true }))
    try {
      const items = (await listFiles(fid, { spaceId, limit: 500 })).filter((f) => !f.is_root && (onlyFolders ? f.type === 'folder' : true))
      items.sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))
      setChildren((p) => ({ ...p, [key]: items }))
    } catch (e) {
      loadedRef.current.delete(dedupe)
      setErr(e instanceof Error ? e.message : '目录加载失败')
    } finally {
      setLoading((p) => ({ ...p, [key]: false }))
    }
  }, [spaceId, onlyFolders])

  useEffect(() => {
    loadedRef.current = new Set()
    setChildren({})
    setOpen({})
    setErr('')
    void load(rootId)
  }, [rootId, load])

  const renderNodes = (fid: string | null, depth: number): ReactNode => {
    const key = fid ?? 'root'
    const list = children[key]
    if (!list && loading[key]) return <div className="studio-tree-state muted">加载中…</div>
    if (!list || list.length === 0) return <div className="studio-tree-state muted">（空目录）</div>
    return list.map((f) => {
      const folder = f.type === 'folder'
      const isOpen = open[f.id] === true
      return (
        <div key={f.id}>
          <div
            className={`studio-tree-row${folder && activeFolderId === f.id ? ' active' : ''}${!folder && activeFileIds?.includes(f.id) ? ' file-active' : ''}`}
            style={{ paddingLeft: depth * 14 + 4 }}
            onClick={() => {
              if (!folder) return onFile?.(f)
              setOpen((p) => ({ ...p, [f.id]: !p[f.id] }))
              void load(f.id)
              onFolder?.(f)
            }}
          >
            {folder ? (isOpen ? <ChevronDown size={13} aria-hidden="true" /> : <ChevronRight size={13} aria-hidden="true" />) : <span className="studio-tree-caret" />}
            {folder ? (isOpen ? <FolderOpen size={14} aria-hidden="true" /> : <FolderClosed size={14} aria-hidden="true" />) : <FileText size={14} aria-hidden="true" />}
            <span className="name" title={f.name}>{f.name}</span>
            {!folder && (
              <button type="button" className="studio-tree-edit" title="新窗口查看/编辑"
                onClick={(e) => { e.stopPropagation(); window.open(`/view/${f.id}`, '_blank', 'noopener') }}>编辑</button>
            )}
          </div>
          {folder && isOpen && renderNodes(f.id, depth + 1)}
        </div>
      )
    })
  }

  return <div className="studio-tree">{err ? <div className="error-text studio-pad8">{err}</div> : renderNodes(rootId, 0)}</div>
}

// ---------- 新建项目弹窗（空间下拉 + 目录懒加载树 + 名称） ----------

function NewProjectModal({ onClose, onCreate }: { onClose: () => void; onCreate: (name: string, spaceId: string, folderId: string, meta?: { spaceName?: string; folderName?: string }) => Promise<void> }) {
  const [spaces, setSpaces] = useState<Space[]>([])
  const [spaceId, setSpaceId] = useState('')
  const [name, setName] = useState('')
  const [folderId, setFolderId] = useState('')
  // 选中目录名（路径展示用；空 = 空间根）。提交失败留在弹窗内展示错误，
  // 成功后 onClose 关闭（既有逻辑核对无误）。
  const [folderName, setFolderName] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  // 跟踪「自动填充」的项目名：名称为空或仍等于上次自动值时才随目录覆盖（手改后不再动）。
  const autoNameRef = useRef('')

  useEffect(() => {
    void listSpaces()
      .then((list) => {
        setSpaces(list)
        const def = list.find((s) => s.is_default) ?? list[0]
        if (def) setSpaceId(def.id)
      })
      .catch((e) => setErr(e instanceof Error ? e.message : '空间加载失败'))
  }, [])

  const submit = async () => {
    if (!name.trim() || !spaceId || busy) return
    setBusy(true)
    setErr('')
    try {
      await onCreate(name.trim(), spaceId, folderId, {
        spaceName: spaces.find((s) => s.id === spaceId)?.name ?? '',
        folderName,
      })
      onClose() // 创建成功关闭弹窗；失败留在弹窗内展示错误
    } catch (e) {
      setErr(e instanceof Error ? e.message : '创建失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title="新建项目" onClose={onClose}>
      {err && <div className="error-text">{err}</div>}
      <label className="studio-form-label">项目名称</label>
      <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="如：产品官网" maxLength={60} onPressEnter={() => void submit()} />
      <label className="studio-form-label">所属空间</label>
      <Select value={spaceId || undefined} onChange={(v) => { setSpaceId(v); setFolderId('') }} placeholder="选择空间" style={{ width: '100%' }}
        options={spaces.map((s) => ({ value: s.id, label: `${s.name}${s.is_default ? '（默认）' : ''}` }))} />
      <label className="studio-form-label">任务根目录</label>
      <div className="studio-pick-head">
        <button type="button" className={`studio-root-pick${folderId === '' ? ' active' : ''}`} onClick={() => { setFolderId(''); setFolderName('') }}>空间根目录</button>
        <span className="muted">或展开选择子目录</span>
      </div>
      <div className="studio-pick-tree">
        {spaceId && (
          <LazyTree
            spaceId={spaceId} rootId={null} onlyFolders activeFolderId={folderId}
            onFolder={(f) => {
              setFolderId(f.id)
              setFolderName(f.name)
              // 选中子目录：名称为空或仍为上次自动填充值 → 默认填目录名（用户手改过则不覆盖）。
              setName((cur) => {
                if (!cur.trim() || cur === autoNameRef.current) {
                  autoNameRef.current = f.name
                  return f.name
                }
                return cur
              })
            }}
          />
        )}
      </div>
      <div className="modal-actions">
        <Button onClick={onClose}>取消</Button>
        <Button type="primary" disabled={!name.trim() || !spaceId} loading={busy} onClick={() => void submit()}>创建</Button>
      </div>
    </Modal>
  )
}

// ---------- 中栏：多会话 Agent 任务流（任务指令 → 任务卡 + # 引用文件） ----------

function StudioChat({ zh, agentOn, onRefreshTasks, project, taskRoot, sessions, activeId, onActive, onSessions, refs, onToggleRef, tasksById, onReview, onTaskCreated }: {
  zh: boolean; agentOn: boolean; onRefreshTasks: () => void; project: StudioProject; taskRoot: string; sessions: StudioSession[]; activeId: string
  onActive: (id: string) => void; onSessions: (updater: (prev: StudioSession[]) => StudioSession[]) => void
  refs: AIAttachFile[]; onToggleRef: (f: { fileId: string; fileName: string }) => void
  tasksById: Record<string, AgentTask>; onReview: (taskId: string) => void; onTaskCreated: (taskId: string) => void
}) {
  const { message } = AntdApp.useApp()
  const turns = sessions.find((s) => s.id === activeId)?.messages ?? sessions[0]?.messages ?? []
  const [input, setInput] = useState('')
  const [busy, setBusy] = useState(false)
  const [attachOpen, setAttachOpen] = useState(false)
  const [attachQuery, setAttachQuery] = useState('')
  const [attachItems, setAttachItems] = useState<Array<{ id: string; name: string }>>([])
  const [attachLoading, setAttachLoading] = useState(false)
  // # 文件提及浮层：mentionQuery 为光标前「#」之后的即时词。
  const [mentionOpen, setMentionOpen] = useState(false)
  const [mentionQuery, setMentionQuery] = useState('')
  const [mentionItems, setMentionItems] = useState<Array<{ id: string; name: string }>>([])
  const [mentionActive, setMentionActive] = useState(0)
  const [mentionLoading, setMentionLoading] = useState(false)
  const listRef = useRef<HTMLDivElement | null>(null)
  const inputRef = useRef<TextAreaRef | null>(null)
  const seq = useRef(0)

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [turns])
  useEffect(() => {
    seq.current = turns.reduce((mx, x) => Math.max(mx, x.id), 0)
  }, [activeId]) // turns 取当前值，仅会话切换时重置

  // 引用文件弹层数据（空关键词 = 最近访问；否则 350ms 防抖全文搜索）。
  useEffect(() => {
    if (!attachOpen) return
    let alive = true
    const q = attachQuery.trim()
    const run = (p: Promise<Array<{ id: string; name: string; type?: string }>>) => {
      setAttachLoading(true)
      void p
        .then((items) => { if (alive) setAttachItems(items.filter((x) => x.type !== 'folder').map((x) => ({ id: x.id, name: x.name }))) })
        .catch(() => { if (alive) setAttachItems([]) })
        .finally(() => { if (alive) setAttachLoading(false) })
    }
    if (!q) {
      run(listFiles(null, { recent: true, limit: 20 }))
      return () => { alive = false }
    }
    const timer = window.setTimeout(() => run(searchFiles(q, 20)), 350)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
  }, [attachOpen, attachQuery])

  // #提及候选：空词 = 项目根目录文件；否则 250ms 防抖全文搜索（均过滤目录）。
  useEffect(() => {
    if (!mentionOpen) return
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
      run(listFiles(project.rootFolderId, { limit: 20 }))
      return () => { alive = false }
    }
    const timer = window.setTimeout(() => run(searchFiles(q, 20)), 250)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
  }, [mentionOpen, mentionQuery, project.rootFolderId])

  const patchSession = (fn: (msgs: StudioTurn[]) => StudioTurn[], title?: string) => {
    onSessions((prev) => prev.map((s) => (s.id === activeId ? { ...s, title: title ?? s.title, messages: fn(s.messages) } : s)))
  }

  // 选中提及项：把「#词」替换为 `文件名` 并将文件加入引用 chips，光标落在反引号后。
  const insertMention = (f: { id: string; name: string }) => {
    const ta = inputRef.current?.resizableTextArea?.textArea ?? null
    const caret = ta ? ta.selectionStart : input.length
    const det = detectMention(input, caret)
    const next = det ? `${input.slice(0, det.idx)}\`${f.name}\`${input.slice(caret)}` : `${input} \`${f.name}\``
    setInput(next)
    setMentionOpen(false)
    setMentionQuery('')
    if (!refs.some((r) => r.fileId === f.id)) onToggleRef({ fileId: f.id, fileName: f.name })
    const pos = det ? det.idx + f.name.length + 2 : next.length
    requestAnimationFrame(() => {
      ta?.focus()
      ta?.setSelectionRange(pos, pos)
    })
  }

  const sendTask = async (text: string) => {
    // 引用文件以《文件名》列表附加到任务提示词（Agent 可在工作区内读取）。
    const prompt = text + (refs.length > 0 ? `\n\n请参考文件：${refs.map((r) => `《${r.fileName}》`).join('、')}` : '')
    patchSession((m) => [...m, { id: ++seq.current, role: 'user', content: text, files: refs.length > 0 ? refs : undefined }], text.slice(0, 16))
    try {
      const task = await createAgentTask(taskRoot || project.rootFolderId, prompt, '', 900)
      onTaskCreated(task.id)
      patchSession((m) => [...m, { id: ++seq.current, role: 'assistant', content: '', task: { id: task.id, prompt } }])
      message.success('任务已创建，可在右栏评审进度')
      onReview(task.id)
    } catch (err) {
      const msg = err instanceof Error ? err.message : '创建任务失败'
      patchSession((m) => [...m, { id: ++seq.current, role: 'assistant', content: '', error: `${msg}\n请确认管理端已启用「AI 智能体（Agent）」并配置默认镜像后重试。` }])
    } finally {
      setBusy(false)
    }
  }

  const send = (question: string) => {
    const text = question.trim()
    if (!text || busy || !agentOn) return
    setBusy(true)
    setInput('')
    setMentionOpen(false)
    setMentionQuery('')
    void sendTask(text)
  }

  const addSession = () => {
    const s = newSession()
    onSessions((prev) => [s, ...prev])
    onActive(s.id)
  }
  const removeSession = (id: string) => {
    onSessions((prev) => {
      const next = prev.filter((s) => s.id !== id)
      if (id === activeId) onActive(next[0]?.id ?? '')
      return next.length > 0 ? next : [newSession()]
    })
  }

  // 当前会话中仍在排队/运行的任务（最新优先）：「停止」按钮对其发起取消。
  const runningTaskId = useMemo(() => {
    for (let i = turns.length - 1; i >= 0; i--) {
      const t = turns[i].task
      if (t && !isTerminal(tasksById[t.id]?.status ?? 'queued')) return t.id
    }
    return null
  }, [turns, tasksById])
  const cancelRunning = () => {
    if (!runningTaskId) return
    void cancelAgentTask(runningTaskId)
      .then(() => message.success('已请求取消任务'))
      .catch((e) => message.error(e instanceof Error ? e.message : '取消失败'))
      .finally(() => onRefreshTasks())
  }

  return (
    <section className="studio-chat" aria-label="Agent 任务流">
      <div className="studio-tabs">
        {sessions.map((s) => (
          <div key={s.id} className={`studio-tab${s.id === activeId ? ' active' : ''}`} onClick={() => onActive(s.id)}>
            <span className="t" title={s.title}>{s.title}</span>
            <button type="button" className="x" aria-label="删除会话" onClick={(e) => { e.stopPropagation(); removeSession(s.id) }}>×</button>
          </div>
        ))}
        <Tooltip title="新会话">
          <Button size="small" type="text" aria-label="新会话" onClick={addSession}><Plus size={14} aria-hidden="true" /></Button>
        </Tooltip>
      </div>
      {!agentOn && (
        <div className="studio-agent-off">
          <Bot size={14} aria-hidden="true" />
          <span>AI 智能体未启用：请管理员在 平台管理→AI 智能体 中开启并确认默认镜像。</span>
        </div>
      )}
      <div className="ai-toolbar">
        <div className="ai-toolbar-group">
          <Sparkles size={14} strokeWidth={2} aria-hidden="true" style={{ color: 'var(--primary)' }} />
          <span style={{ fontSize: 12, fontWeight: 600 }}>{project.name}</span>
          <span className="muted" style={{ fontSize: 12 }}>Agent 任务流</span>
        </div>
        <Button size="small" type="text" disabled={turns.length === 0} aria-label="清空当前会话" title="清空当前会话"
          onClick={() => patchSession(() => [])}>
          <Trash2 size={14} strokeWidth={2} aria-hidden="true" />
        </Button>
      </div>
      <div className="ai-thread" ref={listRef}>
        {turns.length === 0 && (
          <div className="ai-empty">
            <div className="ai-empty-icon" aria-hidden="true"><Bot size={26} strokeWidth={2} /></div>
            <div className="ai-empty-title">创作空间 · Agent 任务</div>
            <div className="ai-empty-hint muted">输入任务指令，Agent 将在任务根目录批量生成文件；输入 # 可引用文件，产物在右栏评审后写回。</div>
          </div>
        )}
        {turns.map((turn) => (
          <div key={turn.id} className={`ai-turn ai-turn-${turn.role}`}>
            {turn.role === 'user' ? (
              <div className="ai-bubble ai-bubble-user">
                {turn.content}
                {turn.files && turn.files.length > 0 && (
                  <span className="ai-turn-files">
                    {turn.files.map((f) => (
                      <span key={f.fileId} className="ai-turn-file-chip" title={f.fileName}>
                        <Paperclip size={10} strokeWidth={2} aria-hidden="true" /><span>{f.fileName}</span>
                      </span>
                    ))}
                  </span>
                )}
              </div>
            ) : turn.task ? (
              <div className="studio-taskcard">
                <div className="studio-taskcard-head">
                  <Bot size={14} strokeWidth={2} aria-hidden="true" />
                  <span>创作任务</span>
                  <StatusBadge status={tasksById[turn.task.id]?.status ?? 'queued'} />
                </div>
                <div className="studio-taskcard-prompt" title={turn.task.prompt}>{turn.task.prompt}</div>
                <Button size="small" onClick={() => onReview(turn.task!.id)}>{isTerminal(tasksById[turn.task.id]?.status ?? 'queued') ? '查看评审' : '查看进度'}</Button>
              </div>
            ) : (
              <>
                <div className="ai-avatar" aria-hidden="true"><Sparkles size={13} strokeWidth={2} /></div>
                <div className="ai-bubble ai-bubble-assistant">
                  {turn.error ? (
                    <div className="ai-error-bubble"><div className="ai-error-msg">{turn.error}</div></div>
                  ) : (
                    <>
                      {turn.content
                        ? <div className="markdown-preview ai-markdown"><ReactMarkdown remarkPlugins={[remarkGfm]}>{turn.content}</ReactMarkdown></div>
                        : turn.streaming ? <span className="ai-thinking">生成中…</span>
                          : turn.stopped ? <span className="ai-thinking muted">已停止</span>
                            : null}
                      {turn.streaming && turn.content && <span className="ai-caret" aria-hidden="true" />}
                      {/* 外部工具调用（MCP，历史会话回显）：Wrench 小标签逐条列出。 */}
                      {turn.toolCalls && <AIToolCalls toolCalls={turn.toolCalls} zh={zh} />}
                      {turn.webSources && <AIWebSources sources={turn.webSources} zh={zh} />}
                    </>
                  )}
                </div>
              </>
            )}
          </div>
        ))}
      </div>
      <div className="ai-composer">
        <div className="studio-tpl-row">
          {SKILL_TEMPLATES.map((tpl) => (
            <button key={tpl.label} type="button" className="studio-tpl-chip" title="点击填入模板（请替换 {主题} 等占位符）" onClick={() => setInput(tpl.prompt)}>{tpl.label}</button>
          ))}
        </div>
        <div className="ai-input-box">
          {refs.length > 0 && (
            <div className="ai-attach-chips">
              {refs.map((f) => (
                <span key={f.fileId} className="ai-attach-chip">
                  <Paperclip size={10} strokeWidth={2} aria-hidden="true" />
                  <span className="ai-attach-chip-name" title={f.fileName}>{f.fileName}</span>
                  <button type="button" aria-label="移除引用" onClick={() => onToggleRef({ fileId: f.fileId, fileName: f.fileName })}>×</button>
                </span>
              ))}
            </div>
          )}
          <div className="studio-mention-wrap">
            <Input.TextArea
              ref={inputRef}
              autoSize={{ minRows: 1, maxRows: 5 }}
              value={input}
              disabled={!agentOn}
              placeholder="描述任务，Enter 发送给 Agent…"
              onChange={(e) => {
                const el = e.target
                const det = detectMention(el.value, el.selectionStart ?? el.value.length)
                setMentionOpen(!!det)
                setMentionQuery(det?.query ?? '')
                setInput(el.value)
              }}
              onBlur={() => setMentionOpen(false)}
              onKeyDown={(e) => {
                if (e.nativeEvent.isComposing) return // IME 组合中：交输入法处理
                if (mentionOpen) {
                  if (e.key === 'ArrowUp' && mentionItems.length > 0) { e.preventDefault(); setMentionActive((i) => (i - 1 + mentionItems.length) % mentionItems.length); return }
                  if (e.key === 'ArrowDown' && mentionItems.length > 0) { e.preventDefault(); setMentionActive((i) => (i + 1) % mentionItems.length); return }
                  if (e.key === 'Enter' && mentionItems.length > 0) { e.preventDefault(); insertMention(mentionItems[mentionActive] ?? mentionItems[0]); return }
                  if (e.key === 'Escape') { e.preventDefault(); setMentionOpen(false); return }
                }
                if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(input) }
              }}
            />
            {mentionOpen && (
              <div className="studio-mention-panel">
                {mentionLoading && <div className="studio-mention-state muted">搜索中…</div>}
                {!mentionLoading && mentionItems.length === 0 && <div className="studio-mention-state muted">没有匹配的文件</div>}
                {mentionItems.map((item, i) => (
                  <button key={item.id} type="button" className={`studio-mention-item${i === mentionActive ? ' active' : ''}`}
                    onMouseDown={(e) => e.preventDefault()} onMouseEnter={() => setMentionActive(i)} onClick={() => insertMention(item)}>
                    <FileText size={13} strokeWidth={2} aria-hidden="true" />
                    <span className="name" title={item.name}>{item.name}</span>
                  </button>
                ))}
              </div>
            )}
          </div>
          <div className="ai-input-footer">
            <span className="ai-input-tools">
              <Popover
                trigger="click" placement="topLeft" arrow={false} open={attachOpen}
                onOpenChange={(next) => { setAttachOpen(next); if (next) setAttachQuery('') }}
                content={
                  <div className="ai-attach-pop">
                    <Input allowClear size="small" value={attachQuery} onChange={(e) => setAttachQuery(e.target.value)} placeholder="搜索文件（留空 = 最近访问）" prefix={<Paperclip size={12} strokeWidth={2} aria-hidden="true" />} />
                    <div className="ai-attach-list">
                      {attachLoading && <div className="ai-attach-state muted">加载中…</div>}
                      {!attachLoading && attachItems.length === 0 && <div className="ai-attach-state muted">没有匹配的文件</div>}
                      {attachItems.map((item) => (
                        <button key={item.id} type="button" className={`ai-attach-item${refs.some((f) => f.fileId === item.id) ? ' selected' : ''}`} onClick={() => onToggleRef({ fileId: item.id, fileName: item.name })}>
                          <FileText size={13} strokeWidth={2} aria-hidden="true" />
                          <span className="name" title={item.name}>{item.name}</span>
                          <Check size={13} strokeWidth={2} aria-hidden="true" className="check" />
                        </button>
                      ))}
                    </div>
                    <div className="ai-attach-state muted">引用文件将附加到任务提示词（也可在输入框输入 # 提及）</div>
                  </div>
                }
              >
                <Button size="small" type="text" className="ai-attach-btn" aria-label="引用文件" title="引用文件">
                  <Paperclip size={14} strokeWidth={2} aria-hidden="true" />
                </Button>
              </Popover>
              <span className="ai-input-hint muted">Enter 发送 · Shift+Enter 换行 · # 引用文件</span>
            </span>
            {runningTaskId ? (
              <Button className="ai-stop-btn" shape="circle" size="small" aria-label="停止任务" title="取消当前运行中的任务" onClick={cancelRunning}>
                <Square size={10} fill="currentColor" strokeWidth={0} aria-hidden="true" />
              </Button>
            ) : (
              <Button className="ai-send-btn" type="primary" shape="circle" size="small" disabled={!input.trim() || busy || !agentOn} aria-label="发送" title="发送" onClick={() => send(input)}>
                <Send size={13} strokeWidth={2} aria-hidden="true" />
              </Button>
            )}
          </div>
        </div>
      </div>
    </section>
  )
}

// ---------- 右栏上：引用文件管理 ----------

function RefsPanel({ refs, onRemove, onView }: { refs: AIAttachFile[]; onRemove: (fileId: string) => void; onView: (f: { id: string; name: string }) => void }) {
  return (
    <div className="studio-card studio-refs">
      <div className="studio-card-title"><Paperclip size={14} aria-hidden="true" />引用文件（{refs.length}）</div>
      {refs.length === 0 ? (
        <div className="muted studio-pad8">在左侧目录树点击文件，或用输入框 📎 添加引用。</div>
      ) : (
        <div className="studio-ref-list">
          {refs.map((f) => (
            <span key={f.fileId} className="studio-ref-chip">
              <FileText size={12} aria-hidden="true" />
              <button type="button" className="name" title={`${f.fileName}（点击查看）`} onClick={() => onView({ id: f.fileId, name: f.fileName })}>{f.fileName}</button>
              <a className="op" href={`/view/${f.fileId}`} target="_blank" rel="noopener noreferrer">编辑</a>
              <button type="button" className="op x" aria-label="移除引用" onClick={() => onRemove(f.fileId)}>×</button>
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

// ---------- 右栏下：任务评审（详情 + diff 勾选 + 写回/丢弃/回滚） ----------

function TaskReview({ taskId, onChanged }: { taskId: string | null; onChanged: () => void }) {
  const { message } = AntdApp.useApp()
  const [detail, setDetail] = useState<Awaited<ReturnType<typeof getAgentTask>> | null>(null)
  const [selected, setSelected] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const initedRef = useRef('')

  const load = useCallback(async (initSelection: boolean) => {
    if (!taskId) return
    try {
      const d = await getAgentTask(taskId)
      setDetail(d)
      setErr('')
      if (initSelection || initedRef.current !== taskId) {
        initedRef.current = taskId
        setSelected(d.diff.filter((x) => x.action !== 'deleted').map((x) => x.path))
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : '任务详情加载失败')
    }
  }, [taskId])

  useEffect(() => {
    setDetail(null)
    setSelected([])
    setErr('')
    void load(true)
  }, [load])

  const status = detail?.task.status ?? ''
  useEffect(() => {
    if (!detail || isTerminal(status)) return
    const timer = window.setInterval(() => void load(false), 5000)
    return () => window.clearInterval(timer)
  }, [detail, status, load])

  const run = async (fail: string, fn: () => Promise<string>) => {
    if (!detail || busy) return
    setBusy(true)
    setErr('')
    try {
      const text = await fn()
      if (text) message.success(text)
      await load(false)
      onChanged()
    } catch (e) {
      const msg = e instanceof Error ? e.message : fail
      setErr(msg)
      message.error(msg)
    } finally {
      setBusy(false)
    }
  }
  const apply = () => run('写回失败', async () => {
    const hasDeletes = detail!.diff.some((d) => selected.includes(d.path) && d.action === 'deleted')
    if (hasDeletes && !window.confirm('删除类产物将移入回收站，确认继续？')) return ''
    const hash = await sha256Hex(JSON.stringify(detail!.diff))
    const res = await applyAgentTask(detail!.task.id, selected, detail!.task.snapshot_id, hash, hasDeletes)
    return res.results.map((r) => `${r.path}: ${r.status}${r.error ? `（${r.error}）` : ''}`).join('；') || '没有选中可写回的产物'
  })
  const discard = () => run('丢弃失败', async () => {
    await discardAgentTask(detail!.task.id)
    return '任务已丢弃'
  })
  const rollback = () => run('回滚失败', async () => {
    await rollbackAgentTask(detail!.task.id)
    return '已回滚到任务前快照'
  })

  return (
    <div className="studio-card studio-review">
      <div className="studio-card-title">
        <Bot size={14} aria-hidden="true" />任务评审
        {detail && <StatusBadge status={detail.task.status} />}
        {detail && <Button size="small" type="text" aria-label="刷新" title="刷新" onClick={() => void load(false)}><RefreshCw size={13} aria-hidden="true" /></Button>}
      </div>
      {err && <div className="error-text">{err}</div>}
      {!taskId && <div className="muted studio-pad8">在左栏「任务」或会话任务卡中选择任务查看评审。</div>}
      {taskId && !detail && <div className="muted studio-pad8">加载中…</div>}
      {detail && (
        <>
          <div className="studio-review-meta">
            <div className="prompt" title={detail.task.prompt}>{detail.task.prompt}</div>
            <div className="muted">创建 {formatTime(detail.task.created_at)}{detail.dry_run ? ' · dry-run' : ''}{detail.task.workspace_expires_at ? ` · 产物保留至 ${formatTime(detail.task.workspace_expires_at)}` : ''}</div>
            {detail.task.error && <div className="error-text">{detail.task.error}</div>}
          </div>
          <div className="studio-diff-list">
            <div className="studio-diff-head">
              <span>产物差异（{detail.diff.length}）</span>
              <span>
                <Button size="small" type="text" onClick={() => setSelected(detail.diff.map((d) => d.path))}>全选</Button>
                <Button size="small" type="text" onClick={() => setSelected([])}>清空</Button>
              </span>
            </div>
            {detail.diff.length === 0 && <div className="muted studio-pad8">暂无产物差异</div>}
            {detail.diff.map((d) => (
              <label key={d.path} className="studio-diff-item">
                <input type="checkbox" checked={selected.includes(d.path)} onChange={(e) => setSelected(e.target.checked ? [...selected, d.path] : selected.filter((p) => p !== d.path))} />
                <span className={`act act-${d.action}`}>{d.action}</span>
                <span className="path" title={d.path}>{d.path}</span>
                <span className="size">{d.size} B</span>
              </label>
            ))}
          </div>
          <details className="studio-log">
            <summary>执行日志（{detail.logs.length}）</summary>
            <pre>{detail.logs.map((l) => `[${l.stream}] ${l.content}`).join('\n')}</pre>
          </details>
          <div className="studio-review-ops">
            <Button size="small" type="primary" disabled={busy || !isTerminal(status) || selected.length === 0} onClick={apply}>写回所选</Button>
            <Button size="small" disabled={busy || !isTerminal(status) || detail.task.status !== 'succeeded'} onClick={discard}>丢弃任务</Button>
            <Button size="small" danger disabled={busy || detail.task.status !== 'applied'} onClick={rollback}>回滚</Button>
          </div>
        </>
      )}
    </div>
  )
}

// ---------- 页面主体（三层布局） ----------

export default function StudioPage() {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const feats = useAIFeatures() // {enabled, agent, ...}：enabled=总开关；agent=智能体任务能力
  const { message } = AntdApp.useApp()
  const [uid, setUid] = useState('')
  const [projects, setProjects] = useState<StudioProject[]>([])
  const [pid, setPid] = useState('')
  // 项目搜索（按名称过滤；项目多后快速定位）。
  const [projQuery, setProjQuery] = useState('')
  // 旧项目（无 spaceName/folderPath 字段）惰性解析的空间名缓存（一次性拉取）。
  const [spaceNameById, setSpaceNameById] = useState<Record<string, string>>({})
  const spaceNamesFetchedRef = useRef(false)
  const [leftTab, setLeftTab] = useState<'dir' | 'tasks'>('dir')
  const [taskRoot, setTaskRoot] = useState('')
  const [taskIds, setTaskIds] = useState<Record<string, string[]>>({})
  const [tasks, setTasks] = useState<AgentTask[]>([])
  const [sessions, setSessions] = useState<StudioSession[]>([])
  const [sid, setSid] = useState('')
  const [refs, setRefs] = useState<AIAttachFile[]>([])
  const [reviewId, setReviewId] = useState('')
  const [viewFile, setViewFile] = useState<{ id: string; name: string } | null>(null)
  const [newProj, setNewProj] = useState(false)
  const [treeTick, setTreeTick] = useState(0)
  const [creating, setCreating] = useState<'richtext' | 'markdown' | null>(null)

  const project = projects.find((p) => p.id === pid) ?? null

  useEffect(() => {
    void getMe().then((m) => setUid(m.id)).catch(() => setUid(currentUserId() ?? 'anon'))
  }, [])

  useEffect(() => {
    if (!uid) return
    const list = loadJSON<StudioProject[]>(projectsKey(uid), [])
    setProjects(list)
    setTaskIds(loadJSON(taskIdsKey(uid), {}))
    setPid(list[0]?.id ?? '')
  }, [uid])

  // 旧项目缺 spaceName 时惰性解析一次空间名（listSpaces 查名；失败静默，
  // 展示兜底「未记录路径」）。
  useEffect(() => {
    if (spaceNamesFetchedRef.current || projects.length === 0 || projects.every((p) => p.spaceName)) return
    spaceNamesFetchedRef.current = true
    void listSpaces()
      .then((list) => setSpaceNameById(Object.fromEntries(list.map((s) => [s.id, s.name]))))
      .catch(() => { spaceNamesFetchedRef.current = false })
  }, [projects])

  /** 项目第二行路径文案：新项目用创建时快照；旧项目惰性空间名兜底。 */
  const projPathText = (p: StudioProject): string => {
    if (p.folderPath) return p.folderPath
    if (p.spaceName) return p.spaceName
    const sn = spaceNameById[p.spaceId]
    return sn ? `${sn}（未记录目录）` : '未记录路径'
  }

  // 名称搜索过滤（大小写不敏感子串）。
  const shownProjects = useMemo(() => {
    const q = projQuery.trim().toLowerCase()
    return q ? projects.filter((p) => p.name.toLowerCase().includes(q)) : projects
  }, [projects, projQuery])

  // 切换项目：载入会话（无则建空会话）并重置任务根/引用/评审。
  useEffect(() => {
    if (!uid || !pid) {
      setSessions([])
      setSid('')
      return
    }
    const list = loadJSON<StudioSession[]>(sessionsKey(uid, pid), [])
    const init = list.length > 0 ? list : [newSession()]
    setSessions(init)
    setSid(init[0].id)
    setTaskRoot(projects.find((p) => p.id === pid)?.rootFolderId ?? '')
    setRefs([])
    setReviewId('')
  }, [uid, pid]) // projects 读取为当前值即可，切换语义由 pid 驱动

  // 持久化写透：会话防抖 400ms（避免流式逐 token 落盘），项目/任务映射直接写。
  useEffect(() => {
    if (!uid || !pid) return
    const timer = window.setTimeout(() => saveJSON(sessionsKey(uid, pid), sessions), 400)
    return () => window.clearTimeout(timer)
  }, [uid, pid, sessions])
  useEffect(() => {
    if (uid) {
      saveJSON(projectsKey(uid), projects)
      saveJSON(taskIdsKey(uid), taskIds)
    }
  }, [uid, projects, taskIds])

  const refreshTasks = useCallback(async () => {
    try {
      setTasks(await listAgentTasks())
    } catch {
      /* agent 未启用时静默，创建任务时展示后端错误 */
    }
  }, [])
  useEffect(() => {
    if (feats.enabled && feats.agent && uid) void refreshTasks()
  }, [feats.enabled, feats.agent, uid, refreshTasks])

  const hasActive = tasks.some((t) => !isTerminal(t.status))
  useEffect(() => {
    if (!hasActive) return
    const timer = window.setInterval(() => void refreshTasks(), 5000)
    return () => window.clearInterval(timer)
  }, [hasActive, refreshTasks])

  const tasksById = useMemo(() => Object.fromEntries(tasks.map((x) => [x.id, x] as [string, AgentTask])), [tasks])
  const projectTasks = useMemo(() => {
    if (!project) return []
    const ids = new Set(taskIds[project.id] ?? [])
    return tasks
      .filter((x) => x.root_folder_id === project.rootFolderId || ids.has(x.id))
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
  }, [tasks, taskIds, project])

  const updateSessions = useCallback((fn: (prev: StudioSession[]) => StudioSession[]) => setSessions(fn), [])
  const toggleRef = (f: { fileId: string; fileName: string }) => {
    setRefs((prev) => (prev.some((x) => x.fileId === f.fileId) ? prev.filter((x) => x.fileId !== f.fileId) : [...prev, f]))
  }

  const createProject = async (name: string, spaceId: string, folderId: string, meta?: { spaceName?: string; folderName?: string }) => {
    const rootFolderId = folderId || (await listSpaceFiles(spaceId, null)).parent_id
    const spaceName = meta?.spaceName ?? ''
    // 绑定路径快照：空间根项目 = 空间名；子目录 = 空间名/目录名。
    const folderPath = folderId && meta?.folderName ? `${spaceName}/${meta.folderName}` : spaceName
    const proj: StudioProject = { id: localId(), name, spaceId, rootFolderId, createdAt: new Date().toISOString(), spaceName, folderPath }
    setProjects((p) => [...p, proj])
    setPid(proj.id)
  }
  // 删除项目：清理该项目的任务追踪，并异步作废其未决 Agent 任务
  //（discard 后平台回收任务工作区与临时导出，容器产物即「删除 Docker
  // 空间数据」的落地语义；已 apply/done 的历史记录一并弃置）。空间内
  // 文件不受影响。
  const deleteProject = async (id: string) => {
    const proj = projects.find((p) => p.id === id)
    setProjects((p) => p.filter((x) => x.id !== id))
    if (pid === id) setPid('')
    message.success('项目已删除（不影响云端文件）')
    if (proj) {
      const ids = taskIds[id] ?? []
      if (ids.length > 0) {
        setTaskIds((prev) => {
          const next = { ...prev }
          delete next[id]
          return next
        })
        // 逐个弃置（失败静默——任务可能已终态/已清理）。
        for (const tid of ids) {
          try {
            await cancelAgentTask(tid).catch(() => undefined)
            await discardAgentTask(tid)
          } catch { /* 已终态 */ }
        }
        void refreshTasks()
      }
    }
  }

  const onTaskCreated = (taskId: string) => {
    setTaskIds((p) => ({ ...p, [pid]: [...new Set([...(p[pid] ?? []), taskId])] }))
    void refreshTasks()
  }

  /** 快捷创建：复用上传管线在项目根目录建文档，建完新窗口打开编辑器。 */
  const createDoc = async (kind: 'richtext' | 'markdown') => {
    if (!project || creating) return
    const spec = kind === 'markdown'
      ? { name: '新文档.md', content: '# 新文档\n\n', mime: 'text/markdown', route: 'markdown' }
      : { name: '新文档.dfrt', content: EMPTY_DFDOC_JSON, mime: 'application/json', route: 'dfdoc' }
    setCreating(kind)
    try {
      const session = await uploadFile(new File([spec.content], spec.name, { type: spec.mime }), project.rootFolderId, () => {})
      setTreeTick((n) => n + 1)
      if (session.file_id && session.file_id !== NIL_UUID) window.open(`/${spec.route}/${session.file_id}`, '_blank', 'noopener')
      else message.warning('创建成功，但未返回文件 ID；请在目录树中打开')
    } catch (err) {
      message.error(err instanceof Error ? err.message : '创建失败')
    } finally {
      setCreating(null)
    }
  }

  // AI 未启用：整页降级提示。
  if (!feats.enabled) {
    return (
      <div className="studio-page">
        <div className="studio-welcome">
          <h2>{zh ? 'AI 创作空间' : 'AI Studio'}</h2>
          <p>{t(locale, 'aiAssistantNotConfigured')}</p>
        </div>
      </div>
    )
  }

  return (
    <div className="studio-page">
      <div className="studio-cols">
        <aside className="studio-col studio-left">
          <div className="studio-card studio-projects">
            <div className="studio-card-title">
              <FolderClosed size={14} aria-hidden="true" />项目
              <Button size="small" type="text" className="studio-title-btn" aria-label="新建项目" title="新建项目" onClick={() => setNewProj(true)}><Plus size={14} aria-hidden="true" /></Button>
            </div>
            {/* 搜索框（按名称过滤；项目多后快速定位）。 */}
            <div className="studio-proj-search">
              <Input
                allowClear
                size="small"
                value={projQuery}
                onChange={(e) => setProjQuery(e.target.value)}
                placeholder="搜索项目…"
                prefix={<Search size={12} strokeWidth={2} aria-hidden="true" />}
              />
            </div>
            {projects.length === 0 && <div className="muted studio-pad8">还没有项目，点 + 新建。</div>}
            {projects.length > 0 && shownProjects.length === 0 && <div className="muted studio-pad8">没有匹配的项目。</div>}
            {shownProjects.map((p) => (
              <div key={p.id} className={`studio-proj-item${p.id === pid ? ' active' : ''}`} onClick={() => setPid(p.id)}>
                <span className="meta">
                  <span className="name" title={p.name}>{p.name}</span>
                  {/* 第二行：绑定路径（空间名/目录名；旧项目惰性解析兜底）。 */}
                  <span className="studio-proj-path" title={projPathText(p)}>{projPathText(p)}</span>
                </span>
                <button type="button" className="x" aria-label={`删除项目 ${p.name}`} onClick={(e) => { e.stopPropagation(); deleteProject(p.id) }}>×</button>
              </div>
            ))}
          </div>
          <div className="studio-card studio-left-main">
            {project ? (
              <>
                <div className="studio-left-head">
                  <Segmented size="small" value={leftTab} onChange={(v) => setLeftTab(v as 'dir' | 'tasks')} options={[{ label: '目录', value: 'dir' }, { label: '任务', value: 'tasks' }]} />
                  {leftTab === 'dir' && (
                    <span className="studio-left-tools">
                      <Button size="small" type="text" aria-label="新建 Markdown" title="新建 Markdown" disabled={creating !== null} onClick={() => void createDoc('markdown')}><FileType2 size={13} aria-hidden="true" /></Button>
                      <Button size="small" type="text" aria-label="新建富文本" title="新建富文本" disabled={creating !== null} onClick={() => void createDoc('richtext')}><FilePlus2 size={13} aria-hidden="true" /></Button>
                    </span>
                  )}
                  {leftTab === 'tasks' && (
                    <Button size="small" type="text" aria-label="刷新任务列表" title="刷新任务列表" onClick={() => void refreshTasks()}><RefreshCw size={13} aria-hidden="true" /></Button>
                  )}
                </div>
                <div className="studio-left-body">
                  {leftTab === 'dir' ? (
                    <>
                      <div className="studio-taskroot muted">点击目录 = 设为任务根；点击文件 = 加入引用</div>
                      <LazyTree
                        key={`${project.id}:${treeTick}`}
                        spaceId={project.spaceId}
                        rootId={project.rootFolderId}
                        activeFolderId={taskRoot}
                        activeFileIds={refs.map((r) => r.fileId)}
                        onFolder={(f) => setTaskRoot(f.id)}
                        onFile={(f) => toggleRef({ fileId: f.id, fileName: f.name })}
                      />
                    </>
                  ) : (
                    <>
                      {projectTasks.length === 0 && <div className="muted studio-pad8">暂无任务：在中间任务流中输入指令发起。</div>}
                      {projectTasks.map((x) => (
                        <div key={x.id} className={`studio-task-item${x.id === reviewId ? ' active' : ''}`} onClick={() => setReviewId(x.id)}>
                          <StatusBadge status={x.status} />
                          <span className="name" title={x.prompt}>{x.prompt}</span>
                          <span className="time">{formatTime(x.created_at)}</span>
                        </div>
                      ))}
                    </>
                  )}
                </div>
              </>
            ) : (
              <div className="muted studio-pad8">先在上方选择或新建一个项目。</div>
            )}
          </div>
        </aside>
        <section className="studio-col studio-mid">
          {project ? (
            <StudioChat
              zh={zh}
              agentOn={feats.agent}
              onRefreshTasks={() => void refreshTasks()}
              project={project}
              taskRoot={taskRoot}
              sessions={sessions}
              activeId={sid}
              onActive={setSid}
              onSessions={updateSessions}
              refs={refs}
              onToggleRef={toggleRef}
              tasksById={tasksById}
              onReview={setReviewId}
              onTaskCreated={onTaskCreated}
            />
          ) : (
            <div className="studio-card studio-empty">
              <Sparkles size={22} aria-hidden="true" />
              <p>选择左侧项目后开始：向 Agent 发布任务、用 # 引用文件、在右侧评审写回。</p>
            </div>
          )}
        </section>
        <aside className="studio-col studio-right">
          <RefsPanel refs={refs} onRemove={(fileId) => setRefs((prev) => prev.filter((x) => x.fileId !== fileId))} onView={setViewFile} />
          <TaskReview taskId={reviewId || null} onChanged={() => void refreshTasks()} />
        </aside>
      </div>
      {viewFile && <FileViewModal file={viewFile} onClose={() => setViewFile(null)} />}
      {newProj && <NewProjectModal onClose={() => setNewProj(false)} onCreate={createProject} />}
    </div>
  )
}
