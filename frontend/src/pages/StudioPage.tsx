// AI 创作空间（/studio）：IDE 式整页工作台（参考 JetBrains IDEA 布局）。
// 顶栏：项目下拉（名称 + 引擎 Tag + 绑定路径）+ 管理项目弹窗 + 定位到目录；
// 左栏：文件目录树（右键菜单：打开/设为工作目录/上传/新建文档/复制路径/
// 刷新；docker 项目另挂「沙箱产物」分组 = 任务 diff 树，未写回产物点击
// 出摘要弹窗——后端 diff 端点仅含 {path,action,size,sha256} 不含内容，
// 写回后才可在平台目录打开）；
// 中栏：点击左侧文件内联查看（FileViewerDispatch 按类型分发）+「编辑」
// 跳对应编辑器路由；空态 = 欢迎卡（引擎说明 + 快捷操作 + 最近产物）；
// 右栏：AI 面板（可折叠窄条）——platform = aiChat 流式对话全功能（模型/
// 开关组/Skill 模板/#引用/df_* 工具展示），docker = 对话建任务 + 任务卡，
// 评审台以「对话 | 评审」Tab 内联。
// 双执行引擎（项目级 engine 字段）与本地持久化（localStorage
// docflow.studio.*）语义不变；任务状态 5s 轮询至终态。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { App as AntdApp, Button, Dropdown, Input, Popover, Radio, Select, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import type { TextAreaRef } from 'antd/es/input/TextArea'
import {
  Bot, Check, ChevronDown, ChevronRight, Eraser, FilePlus2, FileText, FileType2, FolderClosed,
  FolderOpen, History, Package, Paperclip, PanelLeftClose, PanelLeftOpen, PanelRightClose, PanelRightOpen,
  Plus, RefreshCw, Send, Settings2, Sparkles, Square, Trash2, Upload, X,
} from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import {
  EMPTY_DFDOC_JSON, aiChat, currentUserId, getMe, listAgentTasks, listFiles, listSpaces,
  listSpaceFiles, resolveFileById, searchFiles, uploadFile,
} from '../api'
import type { AgentTask, AIMessage, AISource, FileItem, Space } from '../api'
import { applyAgentTask, cancelAgentTask, createAgentTask, discardAgentTask, getAgentTask, rollbackAgentTask } from '../agentTasks'
import type { AgentDiff } from '../agentTasks'
import { Modal, formatTime } from '../components/FileBrowser'
import { AIChatToggleBar, AIToolCalls, AIWebSources, getAIModels, normalizeWebSources, toolCallView, useAIChatToggles } from '../components/AIAssistant'
import type { AIAttachFile, AIToolCallView, AIModelOption, AIWebSource } from '../components/AIAssistant'
import { FileViewerDispatch } from '../pages/ViewerPage'
import { editorRouteFor } from '../openers'
import { useAIFeatures } from '../aiFeature'
import { t, useLocale } from '../i18n'

// ---------- 数据结构与本地持久化 ----------

/** 项目执行引擎：platform=平台文件工具直读写（默认，缺省回退）；docker=Agent 沙箱。 */
export type StudioEngine = 'platform' | 'docker'
/** 项目执行引擎归一（localStorage 旧数据缺省 engine = 'platform' 默认语义）。 */
const projEngine = (p?: StudioProject | null): StudioEngine => (p?.engine === 'docker' ? 'docker' : 'platform')

/** 项目条目（localStorage）：spaceName/folderPath 创建时记录（旧数据缺省，
 *  展示侧惰性解析空间名兜底）。 */
interface StudioProject {
  id: string; name: string; spaceId: string; rootFolderId: string; createdAt: string
  /** 绑定空间名（创建时快照；旧项目缺省）。 */
  spaceName?: string
  /** 绑定路径「空间名/目录名」（空间根项目 = 空间名；旧项目缺省）。 */
  folderPath?: string
  /** 执行引擎（'platform' 默认 | 'docker' 沙箱；缺省视为 'platform'）。 */
  engine?: StudioEngine
  /** Docker 沙箱执行引擎（新建项目时选择；''/缺省 = 跟随平台 auto，任务请求不下发）。 */
  harness?: string
  /** 默认模型（`${providerId}/${model}` 键；''/缺省 = 平台默认）。 */
  model?: string
}
interface StudioTurn {
  id: number; role: 'user' | 'assistant'; content: string; streaming?: boolean; stopped?: boolean; error?: string
  files?: AIAttachFile[]; webSources?: AIWebSource[]; task?: { id: string; prompt: string }
  /** 引用的平台文档来源（platform 引擎 include_docs 的 SSE sources 事件）。 */
  sources?: AISource[]
  /** 外部工具调用（SSE event:tool；随会话一并持久化到 localStorage）。 */
  toolCalls?: AIToolCallView[]
}
interface StudioSession {
  id: string; title: string; messages: StudioTurn[]; createdAt: string
  /** 会话级模型选择（`${providerId}/${model}` 键；''/缺省 = 项目默认 → 平台默认）。 */
  model?: string
}

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

/** 项目执行引擎 Tag（顶栏下拉/管理表格/右栏共用；docker=橙色进阶，platform=蓝色默认）。 */
function EngineTag({ engine, zh }: { engine: StudioEngine; zh: boolean }) {
  return (
    <span className={`studio-engine-tag${engine === 'docker' ? ' docker' : ''}`}>
      {engine === 'docker' ? (zh ? 'Docker 沙箱' : 'Docker sandbox') : (zh ? '平台引擎' : 'Platform engine')}
    </span>
  )
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

/** 文件名 → 编辑路由（uuid 直链；不支持编辑的类型回退 /view）。 */
const editHrefOf = (name: string, id: string) => `${editorRouteFor(name, 'edit')}/${id}`

// ---------- Skill 快捷模板（创作任务模式；{主题} 等占位符提示用户替换） ----------

const SKILL_TEMPLATES: Array<{ label: string; prompt: string }> = [
  { label: '建站落地页', prompt: '请为 {主题} 制作单页落地页网站，产出文件：\n1. index.html：现代响应式单页，语义化结构，内嵌 CSS/JS，无外部依赖；\n2. 包含：导航栏、Hero 主视觉（标题+副标题+行动按钮）、核心卖点 3-6 个卡片、功能详情、用户评价、FAQ、页脚联系方式；\n3. 配色排版统一克制，适配移动端，中文文案专业有说服力。' },
  { label: '项目文档', prompt: '请为「{主题}」项目生成一套 Markdown 项目文档：\n1. README.md：简介、亮点、快速开始、目录结构；\n2. docs/architecture.md：整体架构与模块职责；\n3. docs/getting-started.md：环境要求、安装、配置与运行步骤；\n4. docs/faq.md：常见问题与排查。\n要求结构清晰、标题层级规范、命令可直接复制执行。' },
  { label: '接口文档', prompt: '请为「{主题}」编写接口文档 api.md：\n1. 概述：基础地址、认证方式（Bearer）、通用错误码表；\n2. 按资源分组的接口清单：每个接口给出方法+路径、请求参数表（名称/类型/必填/说明）、成功与错误响应 JSON 示例；\n3. 至少覆盖核心资源的增删改查；\n4. 文末附 curl 调用示例。' },
  { label: '思维导图大纲', prompt: '请为「{主题}」生成思维导图大纲 outline.md（Markdown 多级列表）：\n1. 以主题为中心展开 4-6 个一级分支，每个分支再细分 2-4 层；\n2. 分支命名短促（≤10 字）；\n3. 覆盖概念、方法、案例与延伸阅读。' },
  { label: 'PPT 大纲', prompt: '请为「{主题}」生成 PPT 大纲 slides.md：\n1. 按页组织：每页一个二级标题（第 N 页：标题），下列 3-5 条要点（每条 ≤20 字）；\n2. 结构：封面 → 目录 → 背景/问题 → 方案主体（多页）→ 案例数据 → 总结 → Q&A；\n3. 标注建议的视觉形式（流程图、对比表格等）。' },
  { label: '数据报表', prompt: '请为「{主题}」生成数据报表 report.md：\n1. 报表说明：口径、统计周期、数据来源假设；\n2. 核心指标汇总表（Markdown 表格：指标/本期/上期/环比）；\n3. 分维度明细表与简要解读（每条 1-2 句结论）；\n4. 风险提示与后续行动建议（可执行清单）。' },
]

// ---------- 沙箱执行引擎选项（docker 项目表单；'' = 跟随平台 auto） ----------

const HARNESS_OPTIONS: Array<{ value: string; label: string; labelEn: string }> = [
  { value: '', label: '跟随平台（auto）', labelEn: 'Follow platform (auto)' },
  { value: 'claude-code', label: 'Claude Code（Anthropic 协议）', labelEn: 'Claude Code (Anthropic protocol)' },
  { value: 'pi', label: 'pi（OpenAI 兼容协议）', labelEn: 'pi (OpenAI-compatible)' },
  { value: 'builtin', label: '内置 runner（builtin）', labelEn: 'Builtin runner' },
]

/** 任务产物动作 → 展示标签（与评审 diff 的 action 同源）。 */
const ART_ACTION: Record<string, { label: string; labelEn: string; cls: string }> = {
  added: { label: '新增', labelEn: 'Added', cls: 'add' },
  created: { label: '新增', labelEn: 'Added', cls: 'add' },
  modified: { label: '修改', labelEn: 'Modified', cls: 'mod' },
  updated: { label: '修改', labelEn: 'Modified', cls: 'mod' },
  deleted: { label: '删除', labelEn: 'Deleted', cls: 'del' },
  ignored: { label: '忽略', labelEn: 'Ignored', cls: 'ign' },
}

// ---------- 任务产物树（diff 路径清单 → 层级树；docker 沙箱产物视图） ----------

/** 产物树节点：目录 children 为数组；文件 children=null 且带动作/大小。 */
interface ArtifactNode { name: string; path: string; children: ArtifactNode[] | null; action?: string; size?: number }

function insertArtifactPath(nodes: ArtifactNode[], segs: string[], prefix: string, action: string, size: number): void {
  const name = segs[0]
  const path = prefix ? `${prefix}/${name}` : name
  if (segs.length === 1) {
    nodes.push({ name, path, children: null, action, size })
    return
  }
  let dir = nodes.find((n) => n.children !== null && n.name === name)
  if (!dir) {
    dir = { name, path, children: [] }
    nodes.push(dir)
  }
  insertArtifactPath(dir.children as ArtifactNode[], segs.slice(1), path, action, size)
}

/** diff 清单 → 层级树（目录在前、同层按名排序）。 */
function buildArtifactTree(diff: AgentDiff[]): ArtifactNode[] {
  const nodes: ArtifactNode[] = []
  for (const d of diff) {
    const segs = d.path.split('/').filter(Boolean)
    if (segs.length > 0) insertArtifactPath(nodes, segs, '', d.action, d.size)
  }
  const sortNodes = (list: ArtifactNode[]): void => {
    list.sort((a, b) => ((a.children !== null) === (b.children !== null) ? a.name.localeCompare(b.name) : a.children !== null ? -1 : 1))
    for (const n of list) if (n.children) sortNodes(n.children)
  }
  sortNodes(nodes)
  return nodes
}

// ---------- 懒加载目录树（左栏文件树 / 项目表单选目录共用；支持右键菜单） ----------

function LazyTree({
  zh, spaceId, rootId, onlyFolders = false, rootPath = '', activeFolderId, activeFileId,
  isRefFile, onFolder, onFile, onUploadFolder, onCreateDoc, onToggleRef,
}: {
  zh: boolean
  spaceId: string; rootId: string | null; onlyFolders?: boolean; rootPath?: string
  activeFolderId?: string | null; activeFileId?: string | null
  /** 文件是否已在 AI 引用中（右键菜单文案切换用）。 */
  isRefFile?: (f: FileItem) => boolean
  onFolder?: (f: FileItem) => void
  onFile?: (f: FileItem) => void
  onUploadFolder?: (f: FileItem) => void
  /** 目录右键「新建文档」：目标目录 id + 目录名。 */
  onCreateDoc?: (kind: 'richtext' | 'markdown', folderId: string, folderName: string) => void
  onToggleRef?: (f: FileItem) => void
}) {
  const { message } = AntdApp.useApp()
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

  /** 目录刷新：丢弃缓存后重拉（右键菜单「刷新」）。 */
  const reload = useCallback((fid: string | null) => {
    loadedRef.current.delete(`${spaceId}:${fid ?? 'root'}`)
    void load(fid)
  }, [spaceId, load])

  useEffect(() => {
    loadedRef.current = new Set()
    setChildren({})
    setOpen({})
    setErr('')
    void load(rootId)
  }, [rootId, load])

  const copyPath = (path: string) => {
    void navigator.clipboard.writeText(path)
      .then(() => message.success(zh ? '路径已复制' : 'Path copied'))
      .catch(() => message.error(zh ? '复制失败' : 'Copy failed'))
  }

  /** 节点右键菜单（文件 = 打开/新窗口/编辑/引用/复制路径；目录 = 展开/
   *  工作目录/上传/新建文档/刷新/复制路径）。 */
  const ctxItems = (f: FileItem, isOpen: boolean): MenuProps['items'] => {
    if (f.type === 'folder') {
      const items: NonNullable<MenuProps['items']> = [
        { key: 'expand', label: isOpen ? (zh ? '收起' : 'Collapse') : (zh ? '展开' : 'Expand') },
        { key: 'workroot', label: zh ? '设为 AI 工作目录' : 'Set as AI working root' },
      ]
      if (onUploadFolder) items.push({ key: 'upload', label: zh ? '上传文件到此目录' : 'Upload to this folder' })
      if (onCreateDoc) {
        items.push({
          key: 'newdoc', label: zh ? '新建文档' : 'New document', children: [
            { key: 'newdoc:md', label: zh ? 'Markdown 文档' : 'Markdown document' },
            { key: 'newdoc:rt', label: zh ? '富文本文档' : 'Rich text document' },
          ],
        })
      }
      items.push({ key: 'refresh', label: zh ? '刷新' : 'Refresh' })
      items.push({ type: 'divider' })
      items.push({ key: 'copypath', label: zh ? '复制路径' : 'Copy path' })
      return items
    }
    const items: NonNullable<MenuProps['items']> = [
      { key: 'open', label: zh ? '打开（查看）' : 'Open (view)' },
      { key: 'openwin', label: zh ? '新窗口查看' : 'Open in new window' },
      { key: 'edit', label: zh ? '编辑' : 'Edit' },
    ]
    if (onToggleRef) items.push({ key: 'ref', label: isRefFile?.(f) ? (zh ? '移除 AI 引用' : 'Remove AI reference') : (zh ? '加入 AI 引用' : 'Add AI reference') })
    items.push({ type: 'divider' })
    items.push({ key: 'copypath', label: zh ? '复制路径' : 'Copy path' })
    return items
  }

  const onCtxClick = (f: FileItem, fullPath: string) => ({ key }: { key: string }) => {
    if (key === 'expand') {
      setOpen((p) => ({ ...p, [f.id]: !p[f.id] }))
      void load(f.id)
    } else if (key === 'workroot') {
      setOpen((p) => ({ ...p, [f.id]: true }))
      void load(f.id)
      onFolder?.(f)
    } else if (key === 'upload') {
      onUploadFolder?.(f)
    } else if (key.startsWith('newdoc:')) {
      onCreateDoc?.(key === 'newdoc:md' ? 'markdown' : 'richtext', f.id, f.name)
    } else if (key === 'refresh') {
      reload(f.id)
    } else if (key === 'open') {
      onFile?.(f)
    } else if (key === 'openwin') {
      window.open(`/view/${f.id}`, '_blank', 'noopener')
    } else if (key === 'edit') {
      window.location.href = editHrefOf(f.name, f.id)
    } else if (key === 'ref') {
      onToggleRef?.(f)
    } else if (key === 'copypath') {
      copyPath(fullPath)
    }
  }

  const renderNodes = (fid: string | null, depth: number, prefix: string): ReactNode => {
    const key = fid ?? 'root'
    const list = children[key]
    if (!list && loading[key]) return <div className="studio-tree-state muted">加载中…</div>
    if (!list || list.length === 0) return <div className="studio-tree-state muted">（空目录）</div>
    return list.map((f) => {
      const folder = f.type === 'folder'
      const isOpen = open[f.id] === true
      const fullPath = prefix ? `${prefix}/${f.name}` : f.name
      return (
        <div key={f.id}>
          <Dropdown trigger={['contextMenu']} menu={{ items: ctxItems(f, isOpen), onClick: onCtxClick(f, fullPath) }}>
            <div
              className={`studio-tree-row${folder && activeFolderId === f.id ? ' active' : ''}${!folder && activeFileId === f.id ? ' file-active' : ''}`}
              style={{ paddingLeft: depth * 14 + 4 }}
              title={f.name}
              onClick={() => {
                if (!folder) return onFile?.(f)
                setOpen((p) => ({ ...p, [f.id]: !p[f.id] }))
                void load(f.id)
              }}
            >
              {folder ? (isOpen ? <ChevronDown size={13} aria-hidden="true" /> : <ChevronRight size={13} aria-hidden="true" />) : <span className="studio-tree-caret" />}
              {folder ? (isOpen ? <FolderOpen size={14} aria-hidden="true" /> : <FolderClosed size={14} aria-hidden="true" />) : <FileText size={14} aria-hidden="true" />}
              <span className="name" title={f.name}>{f.name}</span>
              {!folder && isRefFile?.(f) && <span className="studio-tree-refdot" title="已加入 AI 引用" aria-label="已加入 AI 引用" />}
            </div>
          </Dropdown>
          {folder && isOpen && renderNodes(f.id, depth + 1, fullPath)}
        </div>
      )
    })
  }

  return <div className="studio-tree">{err ? <div className="error-text studio-pad8">{err}</div> : renderNodes(rootId, 0, rootPath)}</div>
}

// ---------- 任务产物树（当前选中任务的 diff 渲染；apply 后可打开平台文件） ----------

function ArtifactTree({ zh, taskId, status, project, onOpenFile, onSummary }: {
  zh: boolean; taskId: string; status: string; project: StudioProject
  onOpenFile: (f: { id: string; name: string }) => void
  /** 未写回产物「查看」：diff 端点不含内容 → 摘要弹窗（父层渲染）。 */
  onSummary: (info: { taskId: string; path: string; action: string; size: number; sha256: string }) => void
}) {
  const { message } = AntdApp.useApp()
  const [detail, setDetail] = useState<Awaited<ReturnType<typeof getAgentTask>> | null>(null)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})
  const prevStatus = useRef('')

  const load = useCallback(async () => {
    if (!taskId) return
    try {
      setDetail(await getAgentTask(taskId))
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : '任务详情加载失败')
    }
  }, [taskId])

  useEffect(() => {
    setDetail(null)
    setErr('')
    setCollapsed({})
    void load()
  }, [taskId, load])
  // 状态翻转（如运行中 → 待评审产出 diff / 写回后）自动刷新一次。
  useEffect(() => {
    if (prevStatus.current && prevStatus.current !== status) void load()
    prevStatus.current = status
  }, [status, load])

  /** 已 apply 的产物：按相对路径在项目根目录下逐层定位平台文件。 */
  const resolvePlatformFile = async (relPath: string): Promise<FileItem | null> => {
    const segs = relPath.split('/').filter(Boolean)
    let parent: string | null = project.rootFolderId
    for (let i = 0; i < segs.length && parent !== null; i++) {
      const items = await listFiles(parent, { spaceId: project.spaceId, limit: 500 })
      const hit = items.find((x) => x.name === segs[i])
      if (!hit) return null
      if (i === segs.length - 1) return hit.type === 'folder' ? null : hit
      if (hit.type !== 'folder') return null
      parent = hit.id
    }
    return null
  }

  const openFile = async (node: ArtifactNode) => {
    if (!detail || node.children !== null || busy) return
    if (detail.task.status === 'applied' && node.action !== 'deleted') {
      setBusy(true)
      try {
        const f = await resolvePlatformFile(node.path)
        if (f) onOpenFile({ id: f.id, name: f.name })
        else message.warning(zh ? '未在平台目录中找到该文件（可能已被移动或删除）' : 'File not found in platform folders (moved or deleted?)')
      } catch {
        message.error(zh ? '定位平台文件失败' : 'Unable to locate the platform file')
      } finally {
        setBusy(false)
      }
    } else {
      const d = detail.diff.find((x) => x.path === node.path)
      onSummary({ taskId, path: node.path, action: node.action ?? '', size: node.size ?? d?.size ?? 0, sha256: d?.sha256 ?? '' })
    }
  }

  const nodes = useMemo(() => buildArtifactTree(detail?.diff ?? []), [detail])
  const applied = detail?.task.status === 'applied'

  if (err) return <div className="error-text studio-pad8">{err}</div>
  if (!detail) return <div className="muted studio-pad8">{zh ? '产物加载中…' : 'Loading artifacts…'}</div>

  const renderNodes = (list: ArtifactNode[], depth: number): ReactNode => list.map((n) => {
    const isDir = n.children !== null
    const isOpen = !collapsed[n.path]
    const canOpen = !isDir && n.action !== 'deleted' && (applied || n.action !== 'ignored')
    const act = n.action ? ART_ACTION[n.action] ?? { label: n.action, labelEn: n.action, cls: 'ign' } : null
    return (
      <div key={n.path}>
        <Dropdown trigger={['contextMenu']} menu={{ items: canOpen ? [{ key: 'view', label: zh ? '查看' : 'View' }] : [], onClick: () => void openFile(n) }}>
          <div
            className="studio-tree-row"
            style={{ paddingLeft: depth * 14 + 4, cursor: canOpen || isDir ? 'pointer' : 'default' }}
            title={canOpen ? (zh ? '点击查看' : 'Click to view') : n.path}
            onClick={() => {
              if (isDir) setCollapsed((p) => ({ ...p, [n.path]: !p[n.path] }))
              else void openFile(n)
            }}
          >
            {isDir ? (isOpen ? <ChevronDown size={13} aria-hidden="true" /> : <ChevronRight size={13} aria-hidden="true" />) : <span className="studio-tree-caret" />}
            {isDir ? (isOpen ? <FolderOpen size={14} aria-hidden="true" /> : <FolderClosed size={14} aria-hidden="true" />) : <FileText size={14} aria-hidden="true" />}
            <span className="name" title={n.name}>{n.name}</span>
            {!isDir && act && <span className={`studio-art-tag studio-art-${act.cls}`}>{applied && n.action !== 'deleted' ? (zh ? '已在平台' : 'In platform') : (zh ? act.label : act.labelEn)}</span>}
            {!isDir && n.size !== undefined && n.action !== 'deleted' && <span className="studio-art-size">{n.size} B</span>}
          </div>
        </Dropdown>
        {isDir && isOpen && renderNodes(n.children as ArtifactNode[], depth + 1)}
      </div>
    )
  })

  if (nodes.length === 0) {
    return (
      <div className="muted studio-pad8">
        {isTerminal(status)
          ? (zh ? '该任务暂无产物差异（可能被丢弃/回滚或未产出文件）。' : 'No artifact diff for this task (discarded/rolled back or no output).')
          : (zh ? '任务进行中，产物清单在任务结束后生成。' : 'Task running; artifacts appear when it finishes.')}
      </div>
    )
  }
  return (
    <>
      <div className="studio-tree-hint muted">
        {applied
          ? (zh ? '产物已写回平台：点击文件可查看' : 'Applied to platform: click a file to view')
          : (zh ? '容器工作目录产物：写回（右侧评审）后进入平台' : 'Container artifacts: apply (right panel) to enter platform')}
      </div>
      <div className="studio-tree">{renderNodes(nodes, 0)}</div>
    </>
  )
}

// ---------- 项目表单弹窗（新建 / 编辑共用：名称/空间/目录/引擎/harness/模型） ----------

interface ProjectFormData {
  name: string; spaceId: string; folderId: string; spaceName: string; folderName: string
  engine: StudioEngine; harness: string; model: string
}

function ProjectFormModal({ zh, agentOn, initial, onClose, onSubmit }: {
  zh: boolean
  /** Docker 沙箱能力（ai.status agent 标志）：false 时 Docker 选项标「未启用」仍可选。 */
  agentOn: boolean
  /** 编辑目标（缺省 = 新建）。 */
  initial?: StudioProject | null
  onClose: () => void
  onSubmit: (data: ProjectFormData) => Promise<void>
}) {
  const [spaces, setSpaces] = useState<Space[]>([])
  const [spaceId, setSpaceId] = useState('')
  const [name, setName] = useState('')
  const [folderId, setFolderId] = useState('')
  const [folderName, setFolderName] = useState('')
  // 执行引擎（默认 platform = 平台文件工具直读写；docker = Agent 沙箱，
  // 选择后才展示 harness 下拉）与默认模型（'' = 平台默认），随项目存
  // localStorage；platform 下模型供 aiChat 会话选择，docker 下随任务下发。
  const [engine, setEngine] = useState<StudioEngine>('platform')
  const [harness, setHarness] = useState('')
  const [model, setModel] = useState('')
  const [models, setModels] = useState<AIModelOption[]>([])
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  // 跟踪「自动填充」的项目名：名称为空或仍等于上次自动值时才随目录覆盖（手改后不再动）。
  const autoNameRef = useRef('')

  useEffect(() => {
    void listSpaces()
      .then(async (list) => {
        setSpaces(list)
        const def = initial?.spaceId
          ? (list.find((s) => s.id === initial.spaceId) ?? list.find((s) => s.is_default) ?? list[0])
          : (list.find((s) => s.is_default) ?? list[0])
        if (!def) return
        setSpaceId(def.id)
        if (initial) {
          // 编辑模式：初始绑定非空间根时预选该目录（空间根保持「空间根目录」态）。
          try {
            const rootId = (await listSpaceFiles(def.id, null)).parent_id
            if (initial.rootFolderId && initial.rootFolderId !== rootId) {
              setFolderId(initial.rootFolderId)
              setFolderName(initial.folderPath?.includes('/') ? (initial.folderPath.split('/').pop() ?? '') : '')
            }
          } catch {
            /* 空间根解析失败：保持空（提交时按空间根处理） */
          }
        }
      })
      .catch((e) => setErr(e instanceof Error ? e.message : '空间加载失败'))
    // 可选模型（chat 类；未配置模型时下拉仅剩「平台默认」占位）。
    void getAIModels().then(setModels)
    if (initial) {
      setName(initial.name)
      setEngine(projEngine(initial))
      setHarness(initial.harness ?? '')
      setModel(initial.model ?? '')
    }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps -- 仅挂载时初始化（initial 稳定）

  const submit = async () => {
    if (!name.trim() || !spaceId || busy) return
    setBusy(true)
    setErr('')
    try {
      await onSubmit({
        name: name.trim(), spaceId, folderId, folderName,
        spaceName: spaces.find((s) => s.id === spaceId)?.name ?? '',
        engine, harness, model,
      })
      onClose() // 成功关闭弹窗；失败留在弹窗内展示错误
    } catch (e) {
      setErr(e instanceof Error ? e.message : (initial ? '保存失败' : '创建失败'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={initial ? (zh ? '编辑项目' : 'Edit project') : (zh ? '新建项目' : 'New project')} onClose={onClose}>
      {err && <div className="error-text">{err}</div>}
      <label className="studio-form-label">{zh ? '项目名称' : 'Project name'}</label>
      <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={zh ? '如：产品官网' : 'e.g. Product site'} maxLength={60} onPressEnter={() => void submit()} />
      <label className="studio-form-label">{zh ? '所属空间' : 'Space'}</label>
      <Select value={spaceId || undefined} onChange={(v) => { setSpaceId(v); setFolderId(''); setFolderName('') }} placeholder={zh ? '选择空间' : 'Select a space'} style={{ width: '100%' }}
        options={spaces.map((s) => ({ value: s.id, label: `${s.name}${s.is_default ? (zh ? '（默认）' : ' (default)') : ''}` }))} />
      <label className="studio-form-label">{zh ? '任务根目录' : 'Task root folder'}</label>
      <div className="studio-pick-head">
        <button type="button" className={`studio-root-pick${folderId === '' ? ' active' : ''}`} onClick={() => { setFolderId(''); setFolderName('') }}>{zh ? '空间根目录' : 'Space root'}</button>
        <span className="muted">{zh ? '或展开选择子目录' : 'or pick a subfolder'}</span>
      </div>
      <div className="studio-pick-tree">
        {spaceId && (
          <LazyTree
            zh={zh}
            spaceId={spaceId} rootId={null} onlyFolders activeFolderId={folderId || undefined}
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
      {/* 执行引擎：platform（默认，零基础设施）| docker（进阶沙箱）。Agent 未启用时
          Docker 选项标「未启用」仍可选（创建后右栏引导切回平台引擎）。 */}
      <label className="studio-form-label">{zh ? '执行引擎' : 'Execution engine'}</label>
      <Radio.Group value={engine} onChange={(e) => setEngine(e.target.value as StudioEngine)}>
        <div className="studio-engine-options">
          <Radio value="platform">
            <span className="studio-engine-name">{zh ? '平台引擎（默认）' : 'Platform engine (default)'}</span>
            <span className="studio-engine-desc">
              {zh ? 'AI 直接读写项目目录（自动版本保护，无需 Docker）' : 'AI reads/writes the project folder directly (auto versioning, no Docker needed)'}
            </span>
          </Radio>
          <Radio value="docker">
            <span className="studio-engine-name">
              {zh ? 'Docker 沙箱（进阶）' : 'Docker sandbox (advanced)'}
              {!agentOn && <span className="studio-engine-offtag">{zh ? '未启用' : 'Not enabled'}</span>}
            </span>
            <span className="studio-engine-desc">
              {zh ? '可执行构建/测试，产物经评审写回' : 'Runs builds/tests; artifacts apply after review'}
            </span>
          </Radio>
        </div>
      </Radio.Group>
      {engine === 'docker' && (
        <>
          <label className="studio-form-label">{zh ? '沙箱执行引擎（Harness）' : 'Sandbox harness'}</label>
          <Select value={harness} onChange={setHarness} style={{ width: '100%' }}
            options={HARNESS_OPTIONS.map((h) => ({ value: h.value, label: zh ? h.label : h.labelEn }))} />
        </>
      )}
      <label className="studio-form-label">{zh ? '默认模型' : 'Default model'}</label>
      <Select value={model || undefined} allowClear onChange={(v) => setModel(v ?? '')} style={{ width: '100%' }}
        placeholder={zh ? '平台默认模型' : 'Platform default model'}
        options={models.map((m) => ({ value: m.id, label: `${m.providerName || m.providerId} / ${m.model}` }))} />
      <div className="muted" style={{ fontSize: 12, margin: '8px 0 0' }}>
        {zh
          ? '引擎随项目固定；模型在会话中可随时切换（空 = 平台默认）。平台引擎直接读写所选目录；Docker 沙箱产物需经评审写回。'
          : 'Engine is fixed per project; the model can be switched anytime in chat (empty = platform default). Platform engine writes the folder directly; Docker sandbox artifacts apply after review.'}
      </div>
      <div className="modal-actions">
        <Button onClick={onClose}>{zh ? '取消' : 'Cancel'}</Button>
        <Button type="primary" disabled={!name.trim() || !spaceId} loading={busy} onClick={() => void submit()}>{initial ? (zh ? '保存' : 'Save') : (zh ? '创建' : 'Create')}</Button>
      </div>
    </Modal>
  )
}

// ---------- 管理项目弹窗（项目表格：名称/引擎/路径/创建时间 + 编辑/删除） ----------

function ManageProjectsModal({ zh, projects, currentId, pathText, onClose, onNew, onEdit, onDelete }: {
  zh: boolean; projects: StudioProject[]; currentId: string
  pathText: (p: StudioProject) => string
  onClose: () => void
  onNew: () => void
  onEdit: (p: StudioProject) => void
  onDelete: (p: StudioProject) => void
}) {
  return (
    <Modal wide title={zh ? '管理项目' : 'Manage projects'} onClose={onClose}>
      <div className="studio-mgr-bar">
        <span className="muted">{zh ? `共 ${projects.length} 个项目` : `${projects.length} project(s)`}</span>
        <Button size="small" type="primary" onClick={onNew}><Plus size={13} aria-hidden="true" />{zh ? '新建项目' : 'New project'}</Button>
      </div>
      <div className="studio-mgr-table">
        <div className="studio-mgr-row head">
          <span>{zh ? '名称' : 'Name'}</span>
          <span>{zh ? '引擎' : 'Engine'}</span>
          <span>{zh ? '路径' : 'Path'}</span>
          <span>{zh ? '创建时间' : 'Created'}</span>
          <span />
        </div>
        {projects.length === 0 && <div className="muted studio-pad8">{zh ? '还没有项目，点右上「新建项目」创建。' : 'No projects yet; create one with “New project”.'}</div>}
        {projects.map((p) => (
          <div key={p.id} className={`studio-mgr-row${p.id === currentId ? ' cur' : ''}`}>
            <span className="name" title={p.name}>
              {p.name}
              {p.id === currentId && <span className="studio-mgr-cur">{zh ? '当前' : 'current'}</span>}
            </span>
            <span><EngineTag engine={projEngine(p)} zh={zh} /></span>
            <span className="path" title={pathText(p)}>{pathText(p)}</span>
            <span className="time">{formatTime(p.createdAt)}</span>
            <span className="ops">
              <Button size="small" type="text" onClick={() => onEdit(p)}>{zh ? '编辑' : 'Edit'}</Button>
              <Button size="small" type="text" danger onClick={() => onDelete(p)}>{zh ? '删除' : 'Delete'}</Button>
            </span>
          </div>
        ))}
      </div>
    </Modal>
  )
}

// ---------- 沙箱产物摘要弹窗（diff 端点不含内容 → 摘要 + 去评审引导） ----------

function ArtifactSummaryModal({ zh, info, onGoReview, onClose }: {
  zh: boolean
  info: { taskId: string; path: string; action: string; size: number; sha256: string } | null
  onGoReview: () => void
  onClose: () => void
}) {
  if (!info) return null
  const act = ART_ACTION[info.action] ?? { label: info.action || '-', labelEn: info.action || '-', cls: 'ign' }
  return (
    <Modal title={zh ? `沙箱产物 · ${info.path}` : `Sandbox artifact · ${info.path}`} onClose={onClose}>
      <div className="studio-art-summary">
        <div className="row"><span className="k">{zh ? '路径' : 'Path'}</span><span className="v mono">{info.path}</span></div>
        <div className="row"><span className="k">{zh ? '动作' : 'Action'}</span><span className={`v tag studio-art-${act.cls}`}>{zh ? act.label : act.labelEn}</span></div>
        <div className="row"><span className="k">{zh ? '大小' : 'Size'}</span><span className="v mono">{info.size} B</span></div>
        {info.sha256 && <div className="row"><span className="k">SHA-256</span><span className="v mono" title={info.sha256}>{info.sha256.slice(0, 24)}…</span></div>}
        <div className="note">
          {zh
            ? '该产物仍在容器工作目录中，尚未写回平台——后端任务 diff 接口仅返回清单（路径/动作/大小/哈希），不包含文件内容。请在右侧「评审」确认后写回，再从平台目录打开与编辑。'
            : 'This artifact is still in the container workspace and has not been applied. The task diff API returns a manifest only (path/action/size/hash) without file content. Apply it from the right “Review” panel, then open it from the platform folder.'}
        </div>
        <div className="modal-actions">
          <Button onClick={onClose}>{zh ? '关闭' : 'Close'}</Button>
          <Button type="primary" onClick={() => { onGoReview(); onClose() }}>{zh ? '去评审' : 'Go to review'}</Button>
        </div>
      </div>
    </Modal>
  )
}

// ---------- 中栏：文件查看（FileViewerDispatch 按类型分发 + 编辑/新窗口） ----------

function FilePane({ zh, file, onClose }: { zh: boolean; file: { id: string; name: string }; onClose: () => void }) {
  return (
    <section className="studio-view" aria-label={zh ? '文件查看' : 'File view'}>
      <div className="studio-view-head">
        <FileText size={14} aria-hidden="true" />
        <span className="name" title={file.name}>{file.name}</span>
        <span className="ops">
          <Button size="small" onClick={() => { window.location.href = editHrefOf(file.name, file.id) }}>{zh ? '编辑' : 'Edit'}</Button>
          <Button size="small" onClick={() => window.open(`/view/${file.id}`, '_blank', 'noopener')}>{zh ? '新窗口打开' : 'New window'}</Button>
          <Button size="small" type="text" aria-label={zh ? '关闭' : 'Close'} title={zh ? '关闭' : 'Close'} onClick={onClose}><X size={14} aria-hidden="true" /></Button>
        </span>
      </div>
      <div className="studio-view-body">
        <div className="preview-embed">
          <FileViewerDispatch
            key={file.id}
            fileId={file.id}
            name={file.name}
            resolveRawUrl={async () => {
              try {
                const r = await resolveFileById(file.id, { mode: 'view' })
                return r.raw_url
              } catch {
                return null
              }
            }}
          />
        </div>
      </div>
    </section>
  )
}

// ---------- 中栏空态：欢迎卡（引擎说明 + 快捷操作 + 最近产物） ----------

function WelcomePane({ zh, project, engine, agentOn, creating, recentTick, onNewDoc, onUpload, onAskAI, onView }: {
  zh: boolean; project: StudioProject; engine: StudioEngine; agentOn: boolean
  creating: 'richtext' | 'markdown' | null; recentTick: number
  onNewDoc: (kind: 'richtext' | 'markdown') => void
  onUpload: () => void
  onAskAI: () => void
  onView: (f: { id: string; name: string }) => void
}) {
  return (
    <div className="studio-home">
      <div className="studio-home-card">
        <div className="studio-home-icon" aria-hidden="true"><Sparkles size={24} strokeWidth={2} /></div>
        <div className="studio-home-title">
          {zh ? 'AI 创作空间' : 'AI Studio'} · {project.name}
          <EngineTag engine={engine} zh={zh} />
        </div>
        <div className="studio-home-desc muted">
          {engine === 'docker'
            ? (zh
              ? 'Docker 沙箱引擎：在右侧 AI 栏输入任务指令，Agent 将在容器工作目录批量产出文件；产物经「评审」确认后写回平台目录。左侧为项目目录与沙箱产物树，点击文件可在此查看。'
              : 'Docker sandbox engine: type a task in the right AI panel and the agent produces files in the container workspace; artifacts enter the platform after review & apply. The left tree shows platform folders and sandbox artifacts; click a file to view it here.')
            : (zh
              ? '平台引擎：在右侧 AI 栏对话，AI 直接读写项目目录（自动留版本保护）；左侧目录树点击文件可在此查看，右键更多操作。'
              : 'Platform engine: chat in the right AI panel and it reads/writes the project folder directly (auto versioned). Click a file in the left tree to view it here; right-click for more actions.')}
        </div>
        {engine === 'docker' && !agentOn && (
          <div className="studio-agent-off">
            <Bot size={14} aria-hidden="true" />
            <span>{zh ? '管理员未启用 Docker 沙箱，建议在「管理项目」中把项目改为平台引擎。' : 'Docker sandbox is not enabled by the administrator; switching the project to the platform engine is recommended.'}</span>
          </div>
        )}
        <div className="studio-home-ops">
          <Button size="small" disabled={creating !== null} onClick={() => onNewDoc('markdown')}><FileType2 size={13} aria-hidden="true" />{zh ? '新建 Markdown' : 'New Markdown'}</Button>
          <Button size="small" disabled={creating !== null} onClick={() => onNewDoc('richtext')}><FilePlus2 size={13} aria-hidden="true" />{zh ? '新建富文本' : 'New rich text'}</Button>
          <Button size="small" onClick={onUpload}><Upload size={13} aria-hidden="true" />{zh ? '上传文件' : 'Upload'}</Button>
          <Button size="small" type="primary" onClick={onAskAI}><Sparkles size={13} aria-hidden="true" />{zh ? '让 AI 生成' : 'Ask AI'}</Button>
        </div>
      </div>
      <div className="studio-home-recent">
        <div className="studio-group"><History size={13} aria-hidden="true" />{zh ? '最近产物' : 'Recent files'}</div>
        <RecentArtifacts zh={zh} project={project} tick={recentTick} onView={onView} />
      </div>
    </div>
  )
}

// ---------- 最近产物（项目目录按时间倒序前 20；中栏欢迎卡内嵌） ----------

function RecentArtifacts({ zh, project, tick, onView }: {
  zh: boolean; project: StudioProject; tick: number
  onView: (f: { id: string; name: string }) => void
}) {
  const [items, setItems] = useState<FileItem[]>([])
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const files = await listFiles(project.rootFolderId, { spaceId: project.spaceId, sort: 'updated_at', order: 'desc', limit: 100 })
      setItems(files.filter((f) => f.type === 'file').slice(0, 20))
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [project.rootFolderId, project.spaceId])

  useEffect(() => { void load() }, [load, tick])

  return (
    <>
      {err && <div className="error-text studio-pad8">{err}</div>}
      {loading && items.length === 0 && <div className="muted studio-pad8">{zh ? '加载中…' : 'Loading…'}</div>}
      {!loading && !err && items.length === 0 && (
        <div className="muted studio-pad8">{zh ? '项目目录暂无文件：AI 写入的文件将出现在这里。' : 'No files yet; AI-written files will appear here.'}</div>
      )}
      {items.map((f) => (
        <button key={f.id} type="button" className="studio-recent-item" title={f.name} onClick={() => onView({ id: f.id, name: f.name })}>
          <FileText size={13} strokeWidth={2} aria-hidden="true" />
          <span className="name">{f.name}</span>
          <span className="time">{formatTime(f.updated_at)}</span>
        </button>
      ))}
    </>
  )
}

// ---------- 右栏 AI：引用文件管理（紧凑条） ----------

function RefsPanel({ zh, refs, onRemove, onView }: { zh: boolean; refs: AIAttachFile[]; onRemove: (fileId: string) => void; onView: (f: { id: string; name: string }) => void }) {
  return (
    <div className="studio-refs">
      <span className="studio-refs-label"><Paperclip size={12} aria-hidden="true" />{refs.length > 0 ? (zh ? `引用（${refs.length}）` : `Refs (${refs.length})`) : (zh ? '引用' : 'Refs')}</span>
      {refs.length === 0 ? (
        <span className="muted">{zh ? '目录树右键「加入 AI 引用」，或输入框 📎 添加。' : 'Right-click a tree file → “Add AI reference”, or use 📎 in the input.'}</span>
      ) : (
        <span className="studio-ref-list">
          {refs.map((f) => (
            <span key={f.fileId} className="studio-ref-chip">
              <FileText size={12} aria-hidden="true" />
              <button type="button" className="name" title={f.fileName} onClick={() => onView({ id: f.fileId, name: f.fileName })}>{f.fileName}</button>
              <button type="button" className="op x" aria-label="移除引用" onClick={() => onRemove(f.fileId)}>×</button>
            </span>
          ))}
        </span>
      )}
    </div>
  )
}

// ---------- 右栏 AI：对话/任务流（platform=aiChat 流式 / docker=建任务） ----------

function StudioChat({ zh, engine, agentOn, onRefreshTasks, onChatSettled, project, taskRoot, sessions, activeId, onActive, onSessions, refs, onToggleRef, tasksById, onReview, onTaskCreated, focusSignal }: {
  zh: boolean; engine: StudioEngine; agentOn: boolean; onRefreshTasks: () => void; onChatSettled: () => void
  project: StudioProject; taskRoot: string; sessions: StudioSession[]; activeId: string
  onActive: (id: string) => void; onSessions: (updater: (prev: StudioSession[]) => StudioSession[]) => void
  refs: AIAttachFile[]; onToggleRef: (f: { fileId: string; fileName: string }) => void
  tasksById: Record<string, AgentTask>; onReview: (taskId: string) => void; onTaskCreated: (taskId: string) => void
  /** 欢迎卡「让 AI 生成」等外部聚焦信号（递增触发 focus）。 */
  focusSignal: number
}) {
  const { message } = AntdApp.useApp()
  const activeSession = sessions.find((s) => s.id === activeId) ?? sessions[0] ?? null
  const turns = activeSession?.messages ?? []
  // 模型选择：会话显式选择 > 项目默认 > 平台默认（''）；随会话持久化。
  const modelKey = activeSession?.model ?? project.model ?? ''
  const [models, setModels] = useState<AIModelOption[]>([])
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
  // platform 引擎每轮流式请求的中止器（「停止」按钮语义，同 AIAssistant）。
  const abortRef = useRef<AbortController | null>(null)

  // 可选模型（chat 类，AIAssistant 同源缓存）+ 对话开关组（与 AIAssistant
  // 共享持久化标记：联网/思考/MCP/我的文件）。docker 任务模式下开关写入
  // prompt 前缀随任务下发；platform 对话模式下随 aiChat 请求体下发。
  useEffect(() => {
    let alive = true
    void getAIModels().then((list) => { if (alive) setModels(list) })
    return () => { alive = false }
  }, [])
  const toggles = useAIChatToggles(models, modelKey)

  const setSessionModel = (v: string) => {
    onSessions((prev) => prev.map((s) => (s.id === activeId ? { ...s, model: v } : s)))
  }

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [turns])
  useEffect(() => {
    seq.current = turns.reduce((mx, x) => Math.max(mx, x.id), 0)
  }, [activeId]) // turns 取当前值，仅会话切换时重置
  // 卸载/项目切换时中断进行中的流式请求（旧流回调按会话 id 落空）。
  useEffect(() => () => abortRef.current?.abort(), [])
  // 外部聚焦信号（欢迎卡「让 AI 生成」）。
  useEffect(() => {
    if (focusSignal > 0) inputRef.current?.focus()
  }, [focusSignal])

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
  /** 按消息 id 增量更新当前会话内一条 turn（流式 delta/工具/来源回填用）。 */
  const patchTurn = (id: number, patch: Partial<StudioTurn> | ((x: StudioTurn) => Partial<StudioTurn>)) => {
    onSessions((prev) => prev.map((s) => (s.id === activeId
      ? { ...s, messages: s.messages.map((x) => (x.id === id ? { ...x, ...(typeof patch === 'function' ? patch(x) : patch) } : x)) }
      : s)))
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

  // ---- platform 引擎：aiChat 流式对话（AI 助手引擎完整嵌入 Studio 会话） ----

  const sendChat = async (text: string) => {
    // 会话历史 → messages（排除错误/空内容/任务卡），当前问题并入末尾
    //（非 RAG 语义，与 AIAssistant 纯对话模式一致）。
    const history: AIMessage[] = turns
      .filter((x) => !x.error && !x.task && x.content)
      .map((x) => ({ role: x.role, content: x.content }))
    const messages: AIMessage[] = [...history, { role: 'user', content: text }]
    const selectedModel = models.find((m) => m.id === modelKey) ?? null
    const userId = ++seq.current
    const assistantId = ++seq.current
    patchSession((m) => [
      ...m,
      { id: userId, role: 'user', content: text, files: refs.length > 0 ? refs : undefined },
      { id: assistantId, role: 'assistant', content: '', streaming: true },
    ], text.slice(0, 16))
    const ac = new AbortController()
    abortRef.current = ac
    try {
      await aiChat(
        {
          messages,
          model: selectedModel ? { providerId: selectedModel.providerId, modelId: selectedModel.model } : undefined,
          fileIds: refs.length > 0 ? refs.map((r) => r.fileId) : undefined,
          // 联网/思考/MCP/我的文件开关（与 AIAssistant 共享持久化标记）。
          web_search: toggles.web ? true : undefined,
          think: toggles.think ? true : undefined,
          use_mcp: toggles.mcp ? true : undefined,
          include_docs: toggles.docs ? true : undefined,
          // 平台引擎主路径：df_* 平台文件工具注入 + 工作目录 = 项目根
          //（树右键「设为 AI 工作目录」时跟随，AI 直接读写、自动留版本）。
          use_files: true,
          work_root: taskRoot || project.rootFolderId,
        },
        {
          onMeta: (meta) => {
            const ws = normalizeWebSources((meta as { sources?: unknown }).sources)
            if (ws.length > 0) patchTurn(assistantId, { webSources: ws })
          },
          onDelta: (chunk) => patchTurn(assistantId, (x) => ({ content: x.content + chunk })),
          onSources: (sources) => { if (sources.length > 0) patchTurn(assistantId, { sources }) },
          // 工具调用（df_write_file 等）：Wrench 小标签在消息流内体现。
          onTool: (tool) => patchTurn(assistantId, (x) => ({ toolCalls: [...(x.toolCalls ?? []), toolCallView(tool)] })),
        },
        ac.signal,
      )
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') {
        patchTurn(assistantId, { stopped: true }) // 停止：保留已生成内容，非错误
      } else {
        patchTurn(assistantId, { error: err instanceof Error ? err.message : '请求失败' })
      }
    } finally {
      patchTurn(assistantId, { streaming: false })
      setBusy(false)
      if (abortRef.current === ac) abortRef.current = null
      onChatSettled() // AI 可能已写文件：刷新中栏「最近产物」
    }
  }

  // ---- docker 引擎：输入即建 Agent 任务（现有任务流不变） ----

  const sendTask = async (text: string) => {
    // 开关组 → 任务 prompt 头（任务模式语义：Agent 容器断网运行，联网/
    // 文件检索等能力由 runner 读取前缀自行决策；与对话页直连开关同源）。
    const flags: string[] = []
    if (toggles.web) flags.push(zh ? '联网搜索' : 'web search')
    if (toggles.think) flags.push(zh ? '深度思考' : 'deep thinking')
    if (toggles.docs) flags.push(zh ? '引用我的文件' : 'my files')
    if (toggles.mcp) flags.push('MCP')
    const flagPrefix = flags.length > 0 ? `${zh ? `【已开启：${flags.join('、')}】` : `[Enabled: ${flags.join(', ')}]`}\n` : ''
    // 引用文件以《文件名》列表附加到任务提示词（Agent 可在工作区内读取）。
    const prompt = flagPrefix + text + (refs.length > 0 ? `\n\n请参考文件：${refs.map((r) => `《${r.fileName}》`).join('、')}` : '')
    // 模型意图（会话选择 > 项目默认 > 不下发）：网关侧仍按平台默认模型
    // 执行，后端仅记录意图（env DOCFLOW_MODEL + 任务日志）。
    const modelOpt = models.find((m) => m.id === modelKey) ?? null
    patchSession((m) => [...m, { id: ++seq.current, role: 'user', content: text, files: refs.length > 0 ? refs : undefined }], text.slice(0, 16))
    try {
      const task = await createAgentTask(taskRoot || project.rootFolderId, prompt, '', 900, {
        harness: project.harness || undefined,
        model: modelOpt && modelOpt.model ? { provider_id: modelOpt.providerId, model_id: modelOpt.model } : undefined,
      })
      onTaskCreated(task.id)
      patchSession((m) => [...m, { id: ++seq.current, role: 'assistant', content: '', task: { id: task.id, prompt } }])
      message.success('任务已创建，可在右侧「评审」查看进度')
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
    if (!text || busy) return
    if (engine === 'docker' && !agentOn) return // 沙箱未启用仅拦截 docker 项目
    setBusy(true)
    setInput('')
    setMentionOpen(false)
    setMentionQuery('')
    if (engine === 'docker') void sendTask(text)
    else void sendChat(text)
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
  /** 底部圆钮：docker=取消运行中任务；platform=中断流式生成。 */
  const stopActive = runningTaskId !== null || (engine === 'platform' && busy)
  const onStop = () => {
    if (runningTaskId) cancelRunning()
    else abortRef.current?.abort()
  }

  return (
    <section className="studio-chat" aria-label={engine === 'docker' ? 'Agent 任务流' : 'AI 对话流'}>
      {/* 会话管理行（中栏 Tabs 移除后，多会话收敛到右栏下拉）。 */}
      <div className="studio-sess">
        <Select
          className="studio-sess-select"
          size="small" variant="borderless"
          value={activeSession?.id}
          onChange={onActive}
          popupMatchSelectWidth={false}
          placeholder={zh ? '会话' : 'Session'}
          options={sessions.map((s) => ({ value: s.id, label: s.title || (zh ? '（未命名）' : '(untitled)') }))}
        />
        <Tooltip title={zh ? '新会话' : 'New session'}>
          <Button size="small" type="text" aria-label={zh ? '新会话' : 'New session'} onClick={addSession}><Plus size={14} aria-hidden="true" /></Button>
        </Tooltip>
        <Tooltip title={zh ? '删除当前会话' : 'Delete session'}>
          <Button size="small" type="text" aria-label={zh ? '删除当前会话' : 'Delete session'} disabled={sessions.length <= 1} onClick={() => removeSession(activeId)}><Trash2 size={14} aria-hidden="true" /></Button>
        </Tooltip>
        <Tooltip title={zh ? '清空当前会话' : 'Clear session'}>
          <Button size="small" type="text" aria-label={zh ? '清空当前会话' : 'Clear session'} disabled={turns.length === 0} onClick={() => patchSession(() => [])}><Eraser size={14} aria-hidden="true" /></Button>
        </Tooltip>
      </div>
      <div className="ai-thread" ref={listRef}>
        {turns.length === 0 && (
          <div className="ai-empty">
            <div className="ai-empty-icon" aria-hidden="true"><Bot size={26} strokeWidth={2} /></div>
            <div className="ai-empty-title">{engine === 'docker' ? '创作空间 · Agent 任务' : (zh ? '创作空间 · 平台引擎' : 'Studio · Platform engine')}</div>
            <div className="ai-empty-hint muted">
              {engine === 'docker'
                ? '输入任务指令，Agent 将在任务根目录批量生成文件；输入 # 可引用文件，产物在「评审」确认后写回。'
                : (zh ? '输入指令与 AI 对话，AI 将直接读写项目目录（自动留版本）；输入 # 可引用文件。' : 'Chat with AI; it reads/writes the project folder directly (auto versioned). Use # to reference files.')}
            </div>
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
                      {/* 外部工具调用（MCP + df_* 平台文件工具）：Wrench 小标签逐条列出。 */}
                      {turn.toolCalls && <AIToolCalls toolCalls={turn.toolCalls} zh={zh} />}
                      {/* 我的文件（include_docs）引用来源：文件链接（platform 引擎）。 */}
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
        {/* 模型选择（会话级，项目默认预选；未配置模型时隐藏）+ 对话开关组
            （与 AIAssistant 共享 useAIChatToggles 语义；docker 任务模式写入
            prompt 头随 createAgentTask 下发，platform 对话模式随 aiChat 下发）。 */}
        <div className="ai-composer-opts studio-composer-opts">
          {models.length > 0 && (
            <Select
              className="ai-model-select"
              size="small"
              allowClear
              value={modelKey || undefined}
              placeholder={zh ? '模型：项目/平台默认' : 'Model: default'}
              onChange={(v) => setSessionModel(v ?? '')}
              options={models.map((m) => ({
                value: m.id,
                label: (
                  <span className="ai-model-option">
                    <span className="ai-model-option-name">{m.providerName || m.providerId} / {m.model}</span>
                    {m.capabilities.map((c) => (<span key={c} className="ai-model-cap">{c}</span>))}
                  </span>
                ),
              }))}
            />
          )}
          <AIChatToggleBar
            compact
            zh={zh}
            web={toggles.web}
            think={toggles.think}
            thinkBlocked={toggles.thinkBlocked}
            mcp={toggles.mcp}
            mcpAvailable={toggles.mcpAvailable}
            docs={toggles.docs}
            docsAvailable={toggles.docsAvailable}
            onWeb={toggles.setWeb}
            onThink={toggles.setThink}
            onMcp={toggles.setMcp}
            onDocs={toggles.setDocs}
          />
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
              disabled={engine === 'docker' && !agentOn}
              placeholder={engine === 'docker'
                ? '描述任务，Enter 发送给 Agent…'
                : (zh ? '描述任务，AI 将直接写入项目目录，Enter 发送…' : 'Describe the task; AI writes into the project folder. Enter to send…')}
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
                    <div className="ai-attach-state muted">
                      {engine === 'docker'
                        ? '引用文件将附加到任务提示词（也可在输入框输入 # 提及）'
                        : (zh ? '引用文件将作为上下文随对话发送（也可在输入框输入 # 提及）' : 'Referenced files are sent as chat context (or mention with #)')}
                    </div>
                  </div>
                }
              >
                <Button size="small" type="text" className="ai-attach-btn" aria-label="引用文件" title="引用文件">
                  <Paperclip size={14} strokeWidth={2} aria-hidden="true" />
                </Button>
              </Popover>
              <span className="ai-input-hint muted">Enter 发送 · Shift+Enter 换行 · # 引用文件</span>
            </span>
            {stopActive ? (
              <Button className="ai-stop-btn" shape="circle" size="small"
                aria-label={runningTaskId ? '停止任务' : (zh ? '停止生成' : 'Stop generating')}
                title={runningTaskId ? '取消当前运行中的任务' : (zh ? '停止本次生成' : 'Stop generating')}
                onClick={onStop}>
                <Square size={10} fill="currentColor" strokeWidth={0} aria-hidden="true" />
              </Button>
            ) : (
              <Button className="ai-send-btn" type="primary" shape="circle" size="small" disabled={!input.trim() || busy || (engine === 'docker' && !agentOn)} aria-label="发送" title="发送" onClick={() => send(input)}>
                <Send size={13} strokeWidth={2} aria-hidden="true" />
              </Button>
            )}
          </div>
        </div>
      </div>
    </section>
  )
}

// ---------- 右栏评审 Tab：任务评审（详情 + diff 勾选 + 写回/丢弃/回滚） ----------

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
      {!taskId && <div className="muted studio-pad8">在上方选择任务查看评审。</div>}
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

// ---------- 页面主体（IDE 式布局：顶栏 + 左树/中查看/右 AI） ----------

export default function StudioPage() {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const feats = useAIFeatures() // {enabled, agent, ...}：enabled=总开关；agent=智能体任务能力
  const { message } = AntdApp.useApp()
  const [uid, setUid] = useState('')
  const [projects, setProjects] = useState<StudioProject[]>([])
  const [pid, setPid] = useState('')
  // 旧项目（无 spaceName/folderPath 字段）惰性解析的空间名缓存（一次性拉取）。
  const [spaceNameById, setSpaceNameById] = useState<Record<string, string>>({})
  const spaceNamesFetchedRef = useRef(false)
  const [taskRoot, setTaskRoot] = useState('')
  const [taskIds, setTaskIds] = useState<Record<string, string[]>>({})
  const [tasks, setTasks] = useState<AgentTask[]>([])
  const [sessions, setSessions] = useState<StudioSession[]>([])
  const [sid, setSid] = useState('')
  const [refs, setRefs] = useState<AIAttachFile[]>([])
  const [reviewId, setReviewId] = useState('')
  const [viewFile, setViewFile] = useState<{ id: string; name: string } | null>(null)
  // 项目表单弹窗（新建/编辑）与管理项目弹窗。
  const [projForm, setProjForm] = useState<{ mode: 'create' | 'edit'; target?: StudioProject } | null>(null)
  const [manageOpen, setManageOpen] = useState(false)
  // 沙箱产物摘要弹窗（diff 端点不含内容）。
  const [artInfo, setArtInfo] = useState<{ taskId: string; path: string; action: string; size: number; sha256: string } | null>(null)
  const [treeTick, setTreeTick] = useState(0)
  // platform 项目「最近产物」刷新节拍：对话落盘/上传/新建文档后递增。
  const [recentTick, setRecentTick] = useState(0)
  const [creating, setCreating] = useState<'richtext' | 'markdown' | null>(null)
  // 左/右栏折叠 + 右栏 Tab（docker：对话|评审）+ AI 输入聚焦信号。
  const [leftCollapsed, setLeftCollapsed] = useState(false)
  const [aiCollapsed, setAiCollapsed] = useState(false)
  const [aiTab, setAiTab] = useState<'chat' | 'review'>('chat')
  const [aiFocus, setAiFocus] = useState(0)
  // 上传到平台目录：目标目录 + 隐藏 file input（多选）。
  const uploadInputRef = useRef<HTMLInputElement | null>(null)
  const [uploadTarget, setUploadTarget] = useState<{ id: string; name: string } | null>(null)
  const [uploading, setUploading] = useState(false)

  const project = projects.find((p) => p.id === pid) ?? null
  // 当前项目执行引擎（缺省 = 'platform' 默认）。
  const engine = projEngine(project)

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

  /** 项目路径文案：新项目用创建时快照；旧项目惰性空间名兜底。 */
  const projPathText = (p: StudioProject): string => {
    if (p.folderPath) return p.folderPath
    if (p.spaceName) return p.spaceName
    const sn = spaceNameById[p.spaceId]
    return sn ? `${sn}（未记录目录）` : '未记录路径'
  }

  // 切换项目：载入会话（无则建空会话）并重置工作目录/引用/评审/查看。
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
    setViewFile(null)
    setAiTab('chat')
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

  const createProject = async (data: ProjectFormData) => {
    const rootFolderId = data.folderId || (await listSpaceFiles(data.spaceId, null)).parent_id
    // 绑定路径快照：空间根项目 = 空间名；子目录 = 空间名/目录名。
    const folderPath = data.folderId && data.folderName ? `${data.spaceName}/${data.folderName}` : data.spaceName
    const proj: StudioProject = {
      id: localId(), name: data.name, spaceId: data.spaceId, rootFolderId, createdAt: new Date().toISOString(),
      spaceName: data.spaceName, folderPath,
      // 执行引擎（缺省 = 'platform' 默认主路径）+ 沙箱 harness/默认模型
      //（'' = 跟随平台/平台默认；旧项目缺省字段兼容）。
      engine: data.engine === 'docker' ? 'docker' : 'platform',
      harness: data.harness, model: data.model,
    }
    setProjects((p) => [...p, proj])
    setPid(proj.id)
  }

  /** 编辑项目：名称/空间/绑定目录/引擎/harness/模型（会话与任务映射随
   *  project.id 保留；改绑目录后工作目录与中栏查看重置）。 */
  const updateProject = async (id: string, data: ProjectFormData) => {
    const prev = projects.find((p) => p.id === id)
    const rootFolderId = data.folderId || (await listSpaceFiles(data.spaceId, null)).parent_id
    const folderPath = data.folderId && data.folderName ? `${data.spaceName}/${data.folderName}` : data.spaceName
    setProjects((p) => p.map((x) => x.id === id ? {
      ...x, name: data.name, spaceId: data.spaceId, rootFolderId, spaceName: data.spaceName, folderPath,
      engine: data.engine === 'docker' ? 'docker' : 'platform', harness: data.harness, model: data.model,
    } : x))
    if (prev && (prev.rootFolderId !== rootFolderId || prev.spaceId !== data.spaceId)) {
      setTaskRoot(rootFolderId)
      setViewFile(null)
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
    }
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
  /** 任务卡「查看评审」/ 产物摘要「去评审」：切右栏评审 Tab 并选中任务。 */
  const goReview = (taskId: string) => {
    setAiCollapsed(false)
    setAiTab('review')
    setReviewId(taskId)
  }

  /** 快捷创建：复用上传管线在指定目录（默认项目根）建文档，建完新窗口打开编辑器。 */
  const createDoc = async (kind: 'richtext' | 'markdown', folderId?: string, folderName?: string) => {
    if (!project || creating) return
    const spec = kind === 'markdown'
      ? { name: '新文档.md', content: '# 新文档\n\n', mime: 'text/markdown', route: 'markdown' }
      : { name: '新文档.dfrt', content: EMPTY_DFDOC_JSON, mime: 'application/json', route: 'dfdoc' }
    setCreating(kind)
    try {
      const session = await uploadFile(new File([spec.content], spec.name, { type: spec.mime }), folderId || project.rootFolderId, () => {})
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
      if (session.file_id && session.file_id !== NIL_UUID) window.open(`/${spec.route}/${session.file_id}`, '_blank', 'noopener')
      else message.warning('创建成功，但未返回文件 ID；请在目录树中打开')
    } catch (err) {
      message.error(err instanceof Error ? err.message : '创建失败')
    } finally {
      setCreating(null)
    }
    if (folderName) message.info(`已创建到「${folderName}」`)
  }

  // 上传到平台目录（非 Agent 工作目录——容器产物需经评审「写回」才进入
  // 平台；上传用于交付素材/参考资料，成功后刷新目录树并自动加入引用）。
  const openUpload = (target: { id: string; name: string }) => {
    setUploadTarget(target)
    uploadInputRef.current?.click()
  }
  const onUploadPicked = async (files: FileList | null) => {
    const target = uploadTarget
    if (!target || !files || files.length === 0 || uploading) return
    setUploading(true)
    let ok = 0
    for (const file of Array.from(files)) {
      try {
        const session = await uploadFile(file, target.id, () => {})
        ok++
        const fileId = session.file_id
        if (fileId && fileId !== NIL_UUID) {
          setRefs((prev) => (prev.some((x) => x.fileId === fileId) ? prev : [...prev, { fileId, fileName: file.name }]))
        }
      } catch (err) {
        message.error(`${file.name}：${err instanceof Error ? err.message : '上传失败'}`)
      }
    }
    setUploading(false)
    if (ok > 0) {
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
      message.success(`已上传 ${ok} 个文件到「${target.name}」并加入引用`)
    }
  }

  /** 欢迎卡「让 AI 生成」：展开右栏对话并聚焦输入框。 */
  const askAI = () => {
    setAiCollapsed(false)
    setAiTab('chat')
    setAiFocus((n) => n + 1)
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

  // 顶栏项目下拉菜单（项目列表 + 管理项目 + 新建项目）。
  const projMenuItems: MenuProps['items'] = [
    ...projects.map((p) => ({
      key: p.id,
      label: (
        <div className={`studio-proj-option${p.id === pid ? ' active' : ''}`}>
          <span className="l1"><span className="name">{p.name}</span><EngineTag engine={projEngine(p)} zh={zh} /></span>
          <span className="l2" title={projPathText(p)}>{projPathText(p)}</span>
        </div>
      ),
    })),
    { type: 'divider' as const },
    { key: '__manage', label: <span className="studio-proj-menuop"><Settings2 size={13} aria-hidden="true" />{zh ? '管理项目…' : 'Manage projects…'}</span> },
    { key: '__new', label: <span className="studio-proj-menuop"><Plus size={13} aria-hidden="true" />{zh ? '新建项目…' : 'New project…'}</span> },
  ]
  const onProjMenuClick: MenuProps['onClick'] = ({ key }) => {
    if (key === '__manage') setManageOpen(true)
    else if (key === '__new') setProjForm({ mode: 'create' })
    else setPid(key)
  }

  // docker 左栏「沙箱产物」分组头部的任务选择（与右栏评审 Tab 同源 reviewId）。
  const taskPickSelect = (
    <Select
      size="small" variant="borderless"
      className="studio-taskpick"
      value={reviewId || undefined}
      onChange={(v) => setReviewId(v ?? '')}
      allowClear
      placeholder={zh ? '选择任务…' : 'Pick a task…'}
      popupMatchSelectWidth={false}
      options={projectTasks.map((x) => ({
        value: x.id,
        label: `${TASK_STATUS[x.status]?.label ?? x.status} · ${x.prompt.slice(0, 24)}`,
      }))}
    />
  )

  return (
    <div className="studio-page">
      {/* 顶栏：项目下拉 + 管理项目 + 定位到目录 + 当前路径。 */}
      <header className="studio-topbar">
        <Dropdown trigger={['click']} menu={{ items: projMenuItems, onClick: onProjMenuClick }}>
          <button type="button" className="studio-proj-dd" title={project ? projPathText(project) : (zh ? '选择项目' : 'Select project')}>
            <FolderClosed size={14} aria-hidden="true" />
            <span className="name">{project?.name ?? (zh ? '选择项目' : 'Select project')}</span>
            {project && <EngineTag engine={engine} zh={zh} />}
            <ChevronDown size={13} aria-hidden="true" />
          </button>
        </Dropdown>
        <Tooltip title={zh ? '管理项目（编辑/删除/新建）' : 'Manage projects (edit/delete/new)'}>
          <Button size="small" onClick={() => setManageOpen(true)}><Settings2 size={13} aria-hidden="true" />{zh ? '管理项目' : 'Projects'}</Button>
        </Tooltip>
        {project && (
          <Tooltip title={zh ? `在文件页打开「${projPathText(project)}」所在空间` : 'Open the space in Files'}>
            <Button size="small" onClick={() => { window.location.href = `/files?space=${project.spaceId}` }}>
              <FolderOpen size={13} aria-hidden="true" />{zh ? '定位到目录' : 'Locate folder'}
            </Button>
          </Tooltip>
        )}
        {project && <span className="studio-topbar-path muted" title={projPathText(project)}>{projPathText(project)}</span>}
      </header>

      <div className="studio-cols">
        {/* 左栏：文件目录树（可折叠）。 */}
        {leftCollapsed ? (
          <aside className="studio-rail" aria-label={zh ? '展开文件树' : 'Expand file tree'}>
            <Tooltip title={zh ? '展开文件树' : 'Expand file tree'} placement="right">
              <button type="button" onClick={() => setLeftCollapsed(false)} aria-label={zh ? '展开文件树' : 'Expand file tree'}><PanelLeftOpen size={15} aria-hidden="true" /></button>
            </Tooltip>
            <span className="studio-rail-icon" aria-hidden="true"><FolderClosed size={14} /></span>
          </aside>
        ) : (
          <aside className="studio-col studio-left" aria-label={zh ? '文件目录树' : 'File tree'}>
            <div className="studio-left-head">
              <span className="title"><FolderClosed size={13} aria-hidden="true" />{zh ? '文件' : 'Files'}</span>
              <span className="ops">
                <Tooltip title={zh ? '收起文件树' : 'Collapse file tree'}>
                  <Button size="small" type="text" aria-label={zh ? '收起文件树' : 'Collapse file tree'} onClick={() => setLeftCollapsed(true)}><PanelLeftClose size={14} aria-hidden="true" /></Button>
                </Tooltip>
                {project && (
                  <>
                    <Tooltip title={engine === 'docker'
                      ? (zh ? '上传文件到工作目录（进入平台目录；Agent 容器产物需评审「写回」后进入平台）' : 'Upload into the working folder (agent outputs enter via apply)')
                      : (zh ? `上传文件到${taskRoot && taskRoot !== project.rootFolderId ? '所选工作' : '项目根'}目录` : 'Upload into the project folder')}>
                      <Button size="small" type="text" aria-label={zh ? '上传文件' : 'Upload files'} loading={uploading}
                        onClick={() => openUpload({ id: taskRoot || project.rootFolderId, name: taskRoot && taskRoot !== project.rootFolderId ? (zh ? '工作目录' : 'working root') : project.name })}>
                        <Upload size={13} aria-hidden="true" />
                      </Button>
                    </Tooltip>
                    <Tooltip title={zh ? '新建 Markdown' : 'New Markdown'}>
                      <Button size="small" type="text" aria-label={zh ? '新建 Markdown' : 'New Markdown'} disabled={creating !== null} onClick={() => void createDoc('markdown')}><FileType2 size={13} aria-hidden="true" /></Button>
                    </Tooltip>
                    <Tooltip title={zh ? '新建富文本' : 'New rich text'}>
                      <Button size="small" type="text" aria-label={zh ? '新建富文本' : 'New rich text'} disabled={creating !== null} onClick={() => void createDoc('richtext')}><FilePlus2 size={13} aria-hidden="true" /></Button>
                    </Tooltip>
                    <Tooltip title={zh ? '刷新目录树' : 'Refresh tree'}>
                      <Button size="small" type="text" aria-label={zh ? '刷新目录树' : 'Refresh tree'} onClick={() => setTreeTick((n) => n + 1)}><RefreshCw size={13} aria-hidden="true" /></Button>
                    </Tooltip>
                  </>
                )}
              </span>
            </div>
            <div className="studio-left-body">
              {project ? (
                <>
                  {engine === 'docker' && <div className="studio-group"><FolderClosed size={13} aria-hidden="true" />{zh ? '项目目录（平台）' : 'Project folders (platform)'}</div>}
                  <LazyTree
                    key={`${project.id}:${treeTick}`}
                    zh={zh}
                    spaceId={project.spaceId}
                    rootId={project.rootFolderId}
                    rootPath={projPathText(project)}
                    activeFolderId={taskRoot}
                    activeFileId={viewFile?.id}
                    isRefFile={(f) => refs.some((r) => r.fileId === f.id)}
                    onFolder={(f) => setTaskRoot(f.id)}
                    onFile={(f) => setViewFile({ id: f.id, name: f.name })}
                    onUploadFolder={(f) => openUpload({ id: f.id, name: f.name })}
                    onCreateDoc={(kind, folderId) => void createDoc(kind, folderId)}
                    onToggleRef={(f) => toggleRef({ fileId: f.id, fileName: f.name })}
                  />
                  {engine === 'docker' && (
                    <>
                      <div className="studio-group"><Package size={13} aria-hidden="true" />{zh ? '沙箱产物（容器工作目录）' : 'Sandbox artifacts (container)'}</div>
                      {projectTasks.length > 0
                        ? taskPickSelect
                        : <div className="muted studio-pad8">{zh ? '暂无任务：在右侧 AI 栏输入指令发起。' : 'No tasks yet; type a prompt in the right AI panel.'}</div>}
                      {reviewId && (
                        <ArtifactTree
                          zh={zh}
                          taskId={reviewId}
                          status={tasksById[reviewId]?.status ?? 'queued'}
                          project={project}
                          onOpenFile={setViewFile}
                          onSummary={setArtInfo}
                        />
                      )}
                    </>
                  )}
                </>
              ) : (
                <div className="muted studio-pad8">{zh ? '先在顶栏选择或新建一个项目。' : 'Pick or create a project in the top bar first.'}</div>
              )}
            </div>
          </aside>
        )}

        {/* 中栏：文件查看/编辑区（空态 = 欢迎卡）。 */}
        <section className="studio-col studio-mid">
          {project ? (
            viewFile ? (
              <FilePane zh={zh} file={viewFile} onClose={() => setViewFile(null)} />
            ) : (
              <WelcomePane
                zh={zh}
                project={project}
                engine={engine}
                agentOn={feats.agent}
                creating={creating}
                recentTick={recentTick}
                onNewDoc={(kind) => void createDoc(kind)}
                onUpload={() => openUpload({ id: taskRoot || project.rootFolderId, name: project.name })}
                onAskAI={askAI}
                onView={setViewFile}
              />
            )
          ) : (
            <div className="studio-home">
              <div className="studio-home-card">
                <div className="studio-home-icon" aria-hidden="true"><Sparkles size={24} strokeWidth={2} /></div>
                <div className="studio-home-title">{zh ? 'AI 创作空间' : 'AI Studio'}</div>
                <div className="studio-home-desc muted">
                  {zh
                    ? '还没有项目。项目 = 空间 + 根目录 + 执行引擎（平台直读写 / Docker 沙箱）：创建后在此与 AI 协作产出文件。'
                    : 'No project yet. A project = space + root folder + engine (platform direct / Docker sandbox); create one to start creating with AI.'}
                </div>
                <div className="studio-home-ops">
                  <Button type="primary" onClick={() => setProjForm({ mode: 'create' })}><Plus size={13} aria-hidden="true" />{zh ? '新建项目' : 'New project'}</Button>
                </div>
              </div>
            </div>
          )}
        </section>

        {/* 右栏：AI 面板（可折叠；docker = 对话|评审 双 Tab）。 */}
        {aiCollapsed ? (
          <aside className="studio-rail" aria-label={zh ? '展开 AI 面板' : 'Expand AI panel'}>
            <Tooltip title={zh ? '展开 AI 面板' : 'Expand AI panel'} placement="left">
              <button type="button" onClick={() => setAiCollapsed(false)} aria-label={zh ? '展开 AI 面板' : 'Expand AI panel'}><PanelRightOpen size={15} aria-hidden="true" /></button>
            </Tooltip>
            <span className="studio-rail-icon" aria-hidden="true"><Sparkles size={14} /></span>
          </aside>
        ) : (
          <aside className="studio-col studio-right" aria-label={zh ? 'AI 面板' : 'AI panel'}>
            <div className="studio-right-head">
              {engine === 'docker' ? (
                <span className="studio-ai-tabs" role="tablist">
                  <button type="button" role="tab" aria-selected={aiTab === 'chat'} className={aiTab === 'chat' ? 'active' : ''} onClick={() => setAiTab('chat')}>{zh ? '对话' : 'Chat'}</button>
                  <button type="button" role="tab" aria-selected={aiTab === 'review'} className={aiTab === 'review' ? 'active' : ''} onClick={() => setAiTab('review')}>{zh ? '评审' : 'Review'}</button>
                </span>
              ) : (
                <span className="studio-right-title"><Sparkles size={13} aria-hidden="true" />{zh ? 'AI 助手' : 'AI assistant'}</span>
              )}
              <span className="ops">
                <Tooltip title={zh ? '收起 AI 面板' : 'Collapse AI panel'}>
                  <Button size="small" type="text" aria-label={zh ? '收起 AI 面板' : 'Collapse AI panel'} onClick={() => setAiCollapsed(true)}><PanelRightClose size={14} aria-hidden="true" /></Button>
                </Tooltip>
              </span>
            </div>
            {aiTab === 'review' && engine === 'docker' ? (
              <div className="studio-right-body">
                {projectTasks.length > 0 ? taskPickSelect : <div className="muted studio-pad8">{zh ? '暂无任务：切回「对话」输入指令发起。' : 'No tasks yet; switch to Chat to create one.'}</div>}
                <TaskReview taskId={reviewId || null} onChanged={() => void refreshTasks()} />
              </div>
            ) : (
              <div className="studio-right-body">
                <RefsPanel zh={zh} refs={refs} onRemove={(fileId) => setRefs((prev) => prev.filter((x) => x.fileId !== fileId))} onView={setViewFile} />
                {project ? (
                  <StudioChat
                    zh={zh}
                    engine={engine}
                    agentOn={feats.agent}
                    onRefreshTasks={() => void refreshTasks()}
                    onChatSettled={() => setRecentTick((n) => n + 1)}
                    project={project}
                    taskRoot={taskRoot}
                    sessions={sessions}
                    activeId={sid}
                    onActive={setSid}
                    onSessions={updateSessions}
                    refs={refs}
                    onToggleRef={toggleRef}
                    tasksById={tasksById}
                    onReview={goReview}
                    onTaskCreated={onTaskCreated}
                    focusSignal={aiFocus}
                  />
                ) : (
                  <div className="muted studio-pad8">{zh ? '选择项目后开始与 AI 协作。' : 'Pick a project to start.'}</div>
                )}
              </div>
            )}
          </aside>
        )}
      </div>

      {/* 沙箱产物摘要（未写回：diff 端点仅清单，无内容）。 */}
      <ArtifactSummaryModal zh={zh} info={artInfo} onGoReview={() => artInfo && goReview(artInfo.taskId)} onClose={() => setArtInfo(null)} />
      {/* 上传到平台目录的隐藏 input（左栏头部/右键菜单触发）。 */}
      <input ref={uploadInputRef} type="file" multiple hidden
        onChange={(e) => { void onUploadPicked(e.target.files); e.target.value = '' }} />
      {manageOpen && (
        <ManageProjectsModal
          zh={zh}
          projects={projects}
          currentId={pid}
          pathText={projPathText}
          onClose={() => setManageOpen(false)}
          onNew={() => setProjForm({ mode: 'create' })}
          onEdit={(p) => setProjForm({ mode: 'edit', target: p })}
          onDelete={(p) => { void deleteProject(p.id) }}
        />
      )}
      {projForm && (
        <ProjectFormModal
          zh={zh}
          agentOn={feats.agent}
          initial={projForm.mode === 'edit' ? projForm.target ?? null : null}
          onClose={() => setProjForm(null)}
          onSubmit={async (data) => {
            if (projForm.mode === 'edit' && projForm.target) await updateProject(projForm.target.id, data)
            else await createProject(data)
          }}
        />
      )}
    </div>
  )
}
