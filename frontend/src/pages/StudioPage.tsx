// AI 创作空间（/studio）：IDE 式整页工作台（参考 JetBrains IDEA / VSCode 布局）。
// 布局（react-resizable-panels，分栏可拖拽、宽度自动持久化，三栏内部各自
// 滚动、整页直达屏幕底部）：
// - 顶栏：项目下拉（名称 + 引擎 Tag + 绑定路径）+ 管理项目弹窗 + 定位到目录；
// - 左栏：文件目录树（antd Tree 目录模式，右键菜单对齐 VSCode/IDEA：打开/
//   打开为编辑/新窗口、新建文档/上传/设为 AI 工作目录、加入 AI 引用、重命名/
//   删除/刷新/复制路径）；docker 项目另挂「沙箱产物」分组 = 任务 diff 树；
// - 中栏（条件渲染）：多 Tab 查看/编辑器（antd Tabs + 查看|编辑 Segmented，
//   默认查看；FileViewerDispatch / FileEditorDispatch 全类型内嵌分发）；
//   未打开文件时中栏不渲染，右栏 AI 工作区自动延展占满；
// - 右栏：AI 工作区（@ant-design/x：Bubble.List 气泡 + Think 思考过程折叠 +
//   ThoughtChain 工具调用链 + Sender 输入）——platform = aiChat 流式对话
//   全功能，docker = 对话建任务 + 任务卡，评审台以「对话 | 评审」Tab 内联。
// 双执行引擎（项目级 engine 字段）与本地持久化（localStorage
// docflow.studio.*）语义不变；任务状态 5s 轮询至终态。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { App as AntdApp, Button, Dropdown, Input, Popover, Radio, Select, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import { Group, Panel, Separator, useDefaultLayout, usePanelRef } from 'react-resizable-panels'
import {
  Bot, Check, Columns3, Eraser, FileText, FolderClosed, FolderPlus, History, Package, Paperclip, PanelLeftClose, PanelLeftOpen,
  PanelRightClose, PanelRightOpen, Pencil, Plus, RefreshCw, Settings2, Sparkles, Upload, X,
} from 'lucide-react'
import {
  EMPTY_DFDOC_JSON, EMPTY_DRAWIO_XML, EMPTY_EXCALIDRAW_JSON, aiChat, createOfficeTemplate, createSpaceFolder, createStudioProject, currentUserId, deleteFile, deleteStudioProject,
  getMe, listAgentTasks, listFiles, listSpaces, listSpaceFiles, listStudioProjects, renameFile,
  searchFiles, updateStudioProject, uploadFile,
} from '../api'
import { Modal, TEXT_FILE_MIME, formatTime } from '../components/FileBrowser'
import type { AgentTask, AIMessage, FileItem, Space, StudioProjectItem } from '../api'
import { applyAgentTask, cancelAgentTask, createAgentTask, discardAgentTask, getAgentTask, rollbackAgentTask } from '../agentTasks'
import type { AgentDiff } from '../agentTasks'
import { AIChatToggleBar, getAIModels, useAIChatToggles } from '../components/AIAssistant'
import type { AIModelOption } from '../components/AIAssistant'
import {
  AIChatComposer, AIMessageList, applyToolResult, normalizeWebSources, toolEntryFrom,
} from '../components/aichat'
import type { AIAttachFile, AIChatTurnData } from '../components/aichat'
import FileTreePanel from '../components/studio/FileTreePanel'
import EditorTabs from '../components/studio/EditorTabs'
import type { OpenTab } from '../components/studio/EditorTabs'
import { useAIFeatures } from '../aiFeature'
import { t, useLocale } from '../i18n'

// ---------- 数据结构与本地持久化 ----------

/** 项目执行引擎：platform=平台文件工具直读写（默认，缺省回退）；docker=Agent 沙箱。 */
export type StudioEngine = 'platform' | 'docker'
/** 项目执行引擎归一（localStorage 旧数据缺省 engine = 'platform' 默认语义）。 */
const projEngine = (p?: StudioProject | null): StudioEngine => (p?.engine === 'docker' ? 'docker' : 'platform')

/** 项目条目（服务端 studio_projects 行；v3.2 服务端化——旧版仅存浏览器
 *  localStorage，换浏览器/清存储即丢入口）。spaceName/folderPath 为创建时
 *  快照（展示用；空缺时惰性解析空间名兜底）。 */
type StudioProject = StudioProjectItem

/** 会话消息（统一共享展示模型 + docker 任务卡扩展）。 */
interface StudioTurn extends AIChatTurnData {
  /** docker 引擎：任务卡（代替正文渲染）。 */
  task?: { id: string; prompt: string }
}
interface StudioSession {
  id: string; title: string; messages: StudioTurn[]; createdAt: string
  /** 会话级模型选择（`${providerId}/${model}` 键；''/缺省 = 项目默认 → 平台默认）。 */
  model?: string
}

const sessionsKey = (uid: string, pid: string) => `docflow.studio.sessions.${uid}.${pid}`
const taskIdsKey = (uid: string) => `docflow.studio.taskids.${uid}`
const tabsKey = (uid: string, pid: string) => `docflow.studio.tabs.${uid}.${pid}`

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

/** 项目执行引擎 Tag（顶栏下拉/管理表格共用；docker=橙色进阶，platform=蓝色默认）。 */
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



// ---------- 沙箱执行引擎选项（docker 项目表单；'' = 跟随平台 auto） ----------

const HARNESS_OPTIONS: Array<{ value: string; label: string; labelEn: string }> = [
  { value: '', label: '自动（按模型协议路由）', labelEn: 'Auto (route by model protocol)' },
  { value: 'claude-code', label: 'Claude Code（Anthropic 协议）', labelEn: 'Claude Code (Anthropic protocol)' },
  { value: 'pi', label: 'pi（OpenAI 兼容协议）', labelEn: 'pi (OpenAI-compatible)' },
  { value: 'builtin', label: '内置 runner（无外部依赖兜底）', labelEn: 'Builtin runner (no external deps)' },
]

/** harness 兼容的 Provider 协议（与后端 agentHarnessTerminal 路由一致）：
 *  claude-code ← anthropic；pi ← openai_compatible（mock 调试通用）。 */
const harnessKindMatch = (harness: string, kind?: string): boolean => {
  if (!kind) return true
  if (harness === 'claude-code') return kind === 'anthropic'
  if (harness === 'pi') return kind === 'openai_compatible' || kind === 'mock'
  return true
}

/** auto harness 按所选模型协议解析的终值（提示用；与后端路由一致）。 */
const autoHarnessOf = (kind?: string): string => {
  if (kind === 'anthropic') return 'Claude Code'
  if (kind === 'openai_compatible' || kind === 'mock') return 'pi'
  return '内置 runner'
}

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
    let parent: string | null = project.root_folder_id
    for (let i = 0; i < segs.length && parent !== null; i++) {
      const items = await listFiles(parent, { spaceId: project.space_id, limit: 500 })
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

  const renderNodes = (list: ArtifactNode[]): ReactNode => list.map((n) => {
    const isDir = n.children !== null
    const canOpen = !isDir && n.action !== 'deleted' && (applied || n.action !== 'ignored')
    const act = n.action ? ART_ACTION[n.action] ?? { label: n.action, labelEn: n.action, cls: 'ign' } : null
    return (
      <div key={n.path} className={`studio-art-row${canOpen || isDir ? ' clickable' : ''}`} title={canOpen ? (zh ? '点击查看' : 'Click to view') : n.path}
        onClick={() => { if (!isDir) void openFile(n) }}>
        {isDir ? <FolderClosed size={14} aria-hidden="true" /> : <FileText size={14} aria-hidden="true" />}
        <span className="name" title={n.name}>{n.name}</span>
        {!isDir && act && <span className={`studio-art-tag studio-art-${act.cls}`}>{applied && n.action !== 'deleted' ? (zh ? '已在平台' : 'In platform') : (zh ? act.label : act.labelEn)}</span>}
        {!isDir && n.size !== undefined && n.action !== 'deleted' && <span className="studio-art-size">{n.size} B</span>}
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
      <div className="studio-art-tree">{renderNodes(nodes)}</div>
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
  // 目录树刷新节拍（新建目录后 +1 经 key 重挂载）与「新建目录」弹窗目标。
  const [treeTick, setTreeTick] = useState(0)
  const [newFolderTarget, setNewFolderTarget] = useState<{ id: string; name: string } | null>(null)
  const [newFolderName, setNewFolderName] = useState('')
  const [newFolderBusy, setNewFolderBusy] = useState(false)
  const { message } = AntdApp.useApp()
  // harness 兼容过滤后的模型选项（docker+harness 收窄协议；platform 全量）。
  const modelOptions = engine === 'docker' && harness ? models.filter((m) => harnessKindMatch(harness, m.providerKind)) : models
  const selectedModel = models.find((m) => m.id === model) ?? null

  /** picker 树「新建目录」：创建成功后选中该目录并刷新树（选中即变更绑定）。 */
  const createPickedFolder = async () => {
    if (!newFolderTarget || !newFolderName.trim() || newFolderBusy) return
    setNewFolderBusy(true)
    try {
      const created = await createSpaceFolder(spaceId, newFolderName.trim(), newFolderTarget.id || null)
      setFolderId(created.id)
      setFolderName(created.name)
      setName((cur) => {
        if (!cur.trim() || cur === autoNameRef.current) {
          autoNameRef.current = created.name
          return created.name
        }
        return cur
      })
      setNewFolderTarget(null)
      setNewFolderName('')
      setTreeTick((n) => n + 1)
      message.success(zh ? `已创建目录「${created.name}」并选中` : `Folder "${created.name}" created & selected`)
    } catch (e) {
      message.error(e instanceof Error ? e.message : (zh ? '创建目录失败' : 'Failed to create folder'))
    } finally {
      setNewFolderBusy(false)
    }
  }

  useEffect(() => {
    void listSpaces()
      .then(async (list) => {
        setSpaces(list)
        const def = initial?.space_id
          ? (list.find((s) => s.id === initial.space_id) ?? list.find((s) => s.is_default) ?? list[0])
          : (list.find((s) => s.is_default) ?? list[0])
        if (!def) return
        setSpaceId(def.id)
        if (initial) {
          // 编辑模式：初始绑定非空间根时预选该目录（空间根保持「空间根目录」态）。
          try {
            const rootId = (await listSpaceFiles(def.id, null)).parent_id
            if (initial.root_folder_id && initial.root_folder_id !== rootId) {
              setFolderId(initial.root_folder_id)
              setFolderName(initial.folder_path?.includes('/') ? (initial.folder_path.split('/').pop() ?? '') : '')
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
        <Button size="small" onClick={() => { setNewFolderTarget({ id: folderId, name: folderName || (zh ? '空间根目录' : 'Space root') }); setNewFolderName('') }}>
          <FolderPlus size={13} aria-hidden="true" />{zh ? '新建目录' : 'New folder'}
        </Button>
        <span className="muted">{zh ? '或点击目录树选择（右键可新建目录）' : 'or pick in the tree (right-click to create folder)'}</span>
      </div>
      <div className="studio-pick-tree">
        {spaceId && (
          <FileTreePanel
            key={`${spaceId}:${treeTick}`}
            zh={zh}
            spaceId={spaceId}
            rootId={null}
            rootTitle={spaces.find((s) => s.id === spaceId)?.name ?? (zh ? '空间根目录' : 'Space root')}
            onlyFolders
            activeFolderId={folderId || null}
            onCreateFolder={(parent) => {
              setNewFolderTarget({ id: parent.id === '__root__' ? '' : parent.id, name: parent.name })
              setNewFolderName('')
            }}
            onPickFolder={(f) => {
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
              {zh
                ? '可执行构建/测试，产物经评审写回。前置条件：backend 容器挂载 Docker socket（见 docker-compose.yml 注释），否则任务将报「Docker 运行时未接入」'
                : 'Runs builds/tests; artifacts apply after review. Requires the Docker socket mounted into backend (see docker-compose.yml), otherwise tasks fail with "Docker runtime unavailable"'}
            </span>
          </Radio>
        </div>
      </Radio.Group>
      {engine === 'docker' && (
        <>
          <label className="studio-form-label">{zh ? '沙箱执行引擎（Harness）' : 'Sandbox harness'}</label>
          <Select value={harness} onChange={(v) => {
            setHarness(v)
            // harness 收窄协议后，已选模型若不兼容则清空（提示重选）。
            if (v && model) {
              const hit = models.find((m) => m.id === model)
              if (hit && !harnessKindMatch(v, hit.providerKind)) setModel('')
            }
          }} style={{ width: '100%' }}
            options={HARNESS_OPTIONS.map((h) => ({ value: h.value, label: zh ? h.label : h.labelEn }))} />
          {!harness && selectedModel && selectedModel.providerKind && (
            <div className="muted" style={{ fontSize: 12, margin: '-4px 0 6px' }}>
              {zh
                ? `auto 将解析为：${autoHarnessOf(selectedModel.providerKind)}（当前模型为 ${selectedModel.providerKind === 'anthropic' ? 'Anthropic' : selectedModel.providerKind === 'mock' ? 'Mock' : 'OpenAI 兼容'} 协议）`
                : `auto resolves to: ${autoHarnessOf(selectedModel.providerKind)}`}
            </div>
          )}
        </>
      )}
      <label className="studio-form-label">{zh ? '默认模型' : 'Default model'}</label>
      <Select value={model || undefined} allowClear onChange={(v) => setModel(v ?? '')} style={{ width: '100%' }}
        placeholder={zh ? '平台默认模型' : 'Platform default model'}
        options={modelOptions.map((m) => ({
          value: m.id,
          label: `${m.providerName || m.providerId} / ${m.model}${m.providerKind === 'anthropic' ? ' · Anthropic' : ''}`,
        }))} />
      <div className="muted" style={{ fontSize: 12, margin: '8px 0 0' }}>
        {zh
          ? '引擎随项目固定；模型在会话中可随时切换（空 = 平台默认）。平台引擎直接读写所选目录；Docker 沙箱产物需经评审写回。'
          : 'Engine is fixed per project; the model can be switched anytime in chat (empty = platform default). Platform engine writes the folder directly; Docker sandbox artifacts apply after review.'}
      </div>
      <div className="modal-actions">
        <Button onClick={onClose}>{zh ? '取消' : 'Cancel'}</Button>
        <Button type="primary" disabled={!name.trim() || !spaceId} loading={busy} onClick={() => void submit()}>{initial ? (zh ? '保存' : 'Save') : (zh ? '创建' : 'Create')}</Button>
      </div>
      {/* 目录树内「新建目录」弹窗：创建成功即选中（见 createPickedFolder）。 */}
      {newFolderTarget && (
        <Modal title={zh ? `在「${newFolderTarget.name}」下新建目录` : `New folder in "${newFolderTarget.name}"`} onClose={() => setNewFolderTarget(null)}>
          <Input
            autoFocus
            value={newFolderName}
            maxLength={60}
            placeholder={zh ? '目录名称' : 'Folder name'}
            onPressEnter={() => void createPickedFolder()}
            onChange={(e) => setNewFolderName(e.target.value)}
          />
          <div className="modal-actions">
            <Button onClick={() => setNewFolderTarget(null)}>{zh ? '取消' : 'Cancel'}</Button>
            <Button type="primary" disabled={!newFolderName.trim()} loading={newFolderBusy} onClick={() => void createPickedFolder()}>{zh ? '创建并选中' : 'Create & select'}</Button>
          </div>
        </Modal>
      )}
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
            <span className="time">{p.created_at ? formatTime(p.created_at) : ''}</span>
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

// ---------- 最近产物（项目目录按时间倒序前 20；AI 面板空态内嵌） ----------

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
      const files = await listFiles(project.root_folder_id, { spaceId: project.space_id, sort: 'updated_at', order: 'desc', limit: 100 })
      setItems(files.filter((f) => f.type === 'file').slice(0, 20))
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [project.root_folder_id, project.space_id])

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

// ---------- 右栏 AI：对话/任务流（platform=aiChat 流式 / docker=建任务） ----------

function StudioChat({ zh, engine, agentOn, onRefreshTasks, onChatSettled, project, taskRoot, sessions, activeId, onActive, onSessions, refs, onToggleRef, tasksById, onReview, onTaskCreated, recentTick, onOpenInTab }: {
  zh: boolean; engine: StudioEngine; agentOn: boolean; onRefreshTasks: () => void; onChatSettled: () => void
  project: StudioProject; taskRoot: string; sessions: StudioSession[]; activeId: string
  onActive: (id: string) => void; onSessions: (updater: (prev: StudioSession[]) => StudioSession[]) => void
  refs: AIAttachFile[]; onToggleRef: (f: { fileId: string; fileName: string }) => void
  tasksById: Record<string, AgentTask>; onReview: (taskId: string) => void; onTaskCreated: (taskId: string) => void
  /** platform 引擎「最近产物」刷新节拍（空态列表展示）。 */
  recentTick: number
  /** 空态「最近产物」点击：中栏 Tab 打开。 */
  onOpenInTab: (f: { id: string; name: string }) => void
}) {
  const { message, modal } = AntdApp.useApp()
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
    seq.current = turns.reduce((mx, x) => Math.max(mx, Number(x.id)), 0)
  }, [activeId]) // turns 取当前值，仅会话切换时重置
  // 卸载/项目切换时中断进行中的流式请求（旧流回调按会话 id 落空）。
  useEffect(() => () => abortRef.current?.abort(), [])

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

  const patchSession = (fn: (msgs: StudioTurn[]) => StudioTurn[], title?: string) => {
    onSessions((prev) => prev.map((s) => (s.id === activeId ? { ...s, title: title ?? s.title, messages: fn(s.messages) } : s)))
  }
  /** 按消息 id 增量更新当前会话内一条 turn（流式 delta/思考/工具/来源回填用）。 */
  const patchTurn = (id: number, patch: Partial<StudioTurn> | ((x: StudioTurn) => Partial<StudioTurn>)) => {
    onSessions((prev) => prev.map((s) => (s.id === activeId
      ? { ...s, messages: s.messages.map((x) => (x.id === id ? { ...x, ...(typeof patch === 'function' ? patch(x) : patch) } : x)) }
      : s)))
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
          work_root: taskRoot || project.root_folder_id,
        },
        {
          onMeta: (meta) => {
            const ws = normalizeWebSources((meta as { sources?: unknown }).sources)
            if (ws.length > 0) patchTurn(assistantId, { webSources: ws })
          },
          onDelta: (chunk) => patchTurn(assistantId, (x) => ({ content: x.content + chunk })),
          // 推理思考增量：独立折叠区聚合（首个增量记录起始时间，完成展示用时）。
          onThinking: (chunk) => patchTurn(assistantId, (x) => ({
            thinking: (x.thinking ?? '') + chunk,
            thinkingStartedAt: x.thinkingStartedAt ?? Date.now(),
          })),
          onSources: (sources) => { if (sources.length > 0) patchTurn(assistantId, { sources }) },
          // 工具调用生命周期：执行前 running（含参数摘要），执行后终态+结果摘要。
          onTool: (tool) => patchTurn(assistantId, (x) => ({ toolCalls: [...(x.toolCalls ?? []), toolEntryFrom(tool)] })),
          onToolResult: (result) => patchTurn(assistantId, (x) => ({ toolCalls: applyToolResult(x.toolCalls ?? [], result) })),
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
      patchTurn(assistantId, (x) => (x.thinkingStartedAt ? { thinkingMS: Math.max(0, Date.now() - x.thinkingStartedAt) } : {}))
      patchTurn(assistantId, { streaming: false })
      setBusy(false)
      if (abortRef.current === ac) abortRef.current = null
      onChatSettled() // AI 可能已写文件：刷新空态「最近产物」
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
      const task = await createAgentTask(taskRoot || project.root_folder_id, prompt, '', 900, {
        harness: project.harness || undefined,
        model: modelOpt && modelOpt.model ? { provider_id: modelOpt.providerId, model_id: modelOpt.model } : undefined,
      })
      onTaskCreated(task.id)
      patchSession((m) => [...m, { id: ++seq.current, role: 'assistant', content: '', task: { id: task.id, prompt } }])
      // v3.4：不自动跳评审（ChatGPT/豆包式 agent 体验——对话流内任务卡实时
      // 展示状态，评审由任务卡「查看评审」显式进入）。
      message.success('任务已创建，进度见下方任务卡（可点「查看评审」）')
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
  /** 停止按钮：docker=取消运行中任务；platform=中断流式生成。 */
  const stopActive = runningTaskId !== null || (engine === 'platform' && busy)
  const onStop = () => {
    if (runningTaskId) cancelRunning()
    else abortRef.current?.abort()
  }

  // Sender header：开关组（模型 + 联网/思考/MCP/我的文件）→ 模板 chips →
  // 引用文件 chips（📎 弹层选择 + #提及自动加入）。
  const composerHeader = (
    <div className="aic-send-header">
      <div className="aic-send-tools">
        <Popover
          trigger="click" placement="topLeft" arrow={false} open={attachOpen}
          onOpenChange={(next) => { setAttachOpen(next); if (next) setAttachQuery('') }}
          content={
            <div className="ai-attach-pop">
              <Input allowClear size="small" value={attachQuery} onChange={(e) => setAttachQuery(e.target.value)} placeholder={zh ? '搜索文件（留空 = 最近访问）' : 'Search files (empty = recent)'} prefix={<Paperclip size={12} strokeWidth={2} aria-hidden="true" />} />
              <div className="ai-attach-list">
                {attachLoading && <div className="ai-attach-state muted">{zh ? '加载中…' : 'Loading…'}</div>}
                {!attachLoading && attachItems.length === 0 && <div className="ai-attach-state muted">{zh ? '没有匹配的文件' : 'No matching files'}</div>}
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
          <Button size="small" type="text" className="ai-attach-btn" aria-label={zh ? '引用文件' : 'Attach files'} title={zh ? '引用文件' : 'Attach files'}>
            <Paperclip size={14} strokeWidth={2} aria-hidden="true" />
          </Button>
        </Popover>
        <span className="ai-input-hint muted">Enter {zh ? '发送' : 'send'} · Shift+Enter {zh ? '换行' : 'newline'} · # {zh ? '引用文件' : 'file'}</span>
      </div>
      {refs.length > 0 && (
        <div className="ai-attach-chips">
          {refs.map((f) => (
            <span key={f.fileId} className="ai-attach-chip">
              <Paperclip size={10} strokeWidth={2} aria-hidden="true" />
              <span className="ai-attach-chip-name" title={f.fileName}>{f.fileName}</span>
              <button type="button" aria-label={zh ? '移除引用' : 'Remove reference'} onClick={() => onToggleRef({ fileId: f.fileId, fileName: f.fileName })}>×</button>
            </span>
          ))}
        </div>
      )}
      {/* v3.3：移除「快捷模板」chips（纯提示词填入价值低且易被误认为执行
          模式；技能模板仍可在管理端维护并经 AI 助理使用）。 */}
      <div className="aic-composer-opts">
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
    </div>
  )

  /** docker 任务卡（renderItem 覆盖默认气泡）。 */
  const renderTurn = useCallback((turn: StudioTurn): ReactNode | undefined => {
    if (turn.role !== 'assistant' || !turn.task) return undefined
    const status = tasksById[turn.task.id]?.status ?? 'queued'
    return (
      <div className="studio-taskcard">
        <div className="studio-taskcard-head">
          <Bot size={14} strokeWidth={2} aria-hidden="true" />
          <span>{zh ? '创作任务' : 'Task'}</span>
          <StatusBadge status={status} />
        </div>
        <div className="studio-taskcard-prompt" title={turn.task.prompt}>{turn.task.prompt}</div>
        <Button size="small" onClick={() => onReview(turn.task!.id)}>{isTerminal(status) ? (zh ? '查看评审' : 'Review') : (zh ? '查看进度' : 'Progress')}</Button>
      </div>
    )
  }, [tasksById, onReview, zh])

  return (
    <section className="studio-chat" aria-label={engine === 'docker' ? 'Agent 任务流' : 'AI 对话流'}>
      {/* 会话管理行（多会话收敛到右栏下拉 + 新建/重命名/删除/清空）。 */}
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
        <Tooltip title={zh ? '重命名会话' : 'Rename session'}>
          <Button size="small" type="text" aria-label={zh ? '重命名会话' : 'Rename session'} disabled={!activeSession} onClick={() => {
            const s = activeSession
            if (!s) return
            let next = s.title
            modal.confirm({
              title: zh ? '重命名会话' : 'Rename session',
              content: <Input defaultValue={s.title} onChange={(e) => { next = e.target.value }} />,
              okText: zh ? '保存' : 'Save',
              onOk: () => {
                const title = next.trim()
                if (!title || title === s.title) return
                onSessions((prev) => prev.map((x) => (x.id === s.id ? { ...x, title } : x)))
              },
            })
          }}><Pencil size={14} aria-hidden="true" /></Button>
        </Tooltip>
        <Tooltip title={zh ? '删除当前会话' : 'Delete session'}>
          <Button size="small" type="text" aria-label={zh ? '删除当前会话' : 'Delete session'} disabled={sessions.length <= 1} onClick={() => removeSession(activeId)}><X size={14} aria-hidden="true" /></Button>
        </Tooltip>
        <Tooltip title={zh ? '清空当前会话' : 'Clear session'}>
          <Button size="small" type="text" aria-label={zh ? '清空当前会话' : 'Clear session'} disabled={turns.length === 0} onClick={() => patchSession(() => [])}><Eraser size={14} aria-hidden="true" /></Button>
        </Tooltip>
      </div>
      {/* 消息流（共享 Bubble.List + Think 思考折叠 + ThoughtChain 工具链）。 */}
      <div className="studio-thread">
        {turns.length === 0 && (
          <div className="ai-empty">
            <div className="ai-empty-icon" aria-hidden="true"><Bot size={26} strokeWidth={2} /></div>
            <div className="ai-empty-title">{engine === 'docker' ? '创作空间 · Agent 任务' : (zh ? '创作空间 · 平台引擎' : 'Studio · Platform engine')}</div>
            <div className="ai-empty-hint muted">
              {engine === 'docker'
                ? (zh ? '输入任务指令，Agent 将在任务根目录批量生成文件；输入 # 可引用文件，产物在「评审」确认后写回。' : 'Type a task prompt; the agent produces files in the task root. Use # to reference files; artifacts apply after review.')
                : (zh ? '输入指令与 AI 对话，AI 将直接读写项目目录（自动留版本）；输入 # 可引用文件，思考与工具调用过程实时可见。' : 'Chat with AI; it reads/writes the project folder directly (auto versioned). Use # for files; thinking and tool calls are shown live.')}
            </div>
            {engine === 'platform' && (
              <div className="studio-recent-group">
                <div className="studio-group"><History size={13} aria-hidden="true" />{zh ? '最近产物' : 'Recent files'}</div>
                <RecentArtifacts zh={zh} project={project} tick={recentTick} onView={onOpenInTab} />
              </div>
            )}
          </div>
        )}
        {turns.length > 0 && (
          <AIMessageList items={turns} zh={zh} className="aic-bubbles" renderItem={renderTurn} />
        )}
      </div>
      {/* 输入区（共享 Sender：发送⇄停止、#提及；header=开关/模板/引用）。 */}
      <AIChatComposer
        zh={zh}
        value={input}
        onChange={setInput}
        onSend={send}
        busy={stopActive}
        onCancel={onStop}
        disabled={engine === 'docker' && !agentOn}
        placeholder={engine === 'docker'
          ? (zh ? '描述任务，Enter 发送给 Agent…' : 'Describe the task; Enter to send to the agent…')
          : (zh ? '描述任务，AI 将直接写入项目目录，Enter 发送…' : 'Describe the task; AI writes into the project folder. Enter to send…')}
        header={composerHeader}
        mentionRoot={taskRoot || project.root_folder_id}
        onMentionPick={(f) => { if (!refs.some((r) => r.fileId === f.id)) onToggleRef({ fileId: f.id, fileName: f.name }) }}
      />
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

// ---------- 页面主体（IDE 式布局：顶栏 + 可拖拽三栏 [左树 | 中编辑 | 右 AI]） ----------

export default function StudioPage() {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const feats = useAIFeatures() // {enabled, agent, ...}：enabled=总开关；agent=智能体任务能力
  const { message, modal } = AntdApp.useApp()
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
  // 中栏多 Tab（按项目持久化；空 = 中栏不渲染，AI 面板延展占满）。
  const [openTabs, setOpenTabs] = useState<OpenTab[]>([])
  const [activeTab, setActiveTab] = useState<string | null>(null)
  // 项目表单弹窗（新建/编辑）与管理项目弹窗。
  const [projForm, setProjForm] = useState<{ mode: 'create' | 'edit'; target?: StudioProject } | null>(null)
  const [manageOpen, setManageOpen] = useState(false)
  // 沙箱产物摘要弹窗（diff 端点不含内容）。
  const [artInfo, setArtInfo] = useState<{ taskId: string; path: string; action: string; size: number; sha256: string } | null>(null)
  const [treeTick, setTreeTick] = useState(0)
  // platform 项目「最近产物」刷新节拍：对话落盘/上传/新建文档后递增。
  const [recentTick, setRecentTick] = useState(0)
  const [creating, setCreating] = useState<'richtext' | 'markdown' | null>(null)
  // 右栏 Tab（docker：对话|评审）。（v3.3 移除聚焦信号与面板头。）
  const [aiTab, setAiTab] = useState<'chat' | 'review'>('chat')
  // 左/右栏折叠（react-resizable-panels v4 imperative collapse/expand；
  // onResize 百分比归零 = 折叠态，用于按钮图标方向）。
  const leftPanelRef = usePanelRef()
  const rightPanelRef = usePanelRef()
  const midPanelRef = usePanelRef()
  const [leftCollapsed, setLeftCollapsed] = useState(false)
  const [midCollapsed, setMidCollapsed] = useState(false)
  const [aiCollapsed, setAiCollapsed] = useState(false)
  // 分栏布局持久化（useDefaultLayout：localStorage，条件渲染中栏时按
  // panelIds 保存多套布局）。
  const { defaultLayout, onLayoutChanged } = useDefaultLayout({ id: 'docflow.studio.layout', panelIds: ['left', 'mid', 'right'] })
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

  // 项目清单自服务端拉取（v3.2 服务端化；随账号走，换浏览器不丢）。任务
  // 追踪映射仍为浏览器本地态（按 uid 键）。
  useEffect(() => {
    if (!uid) return
    void listStudioProjects()
      .then((list) => {
        setProjects(list)
        setPid((cur) => (list.some((p) => p.id === cur) ? cur : (list[0]?.id ?? '')))
      })
      .catch(() => message.error(zh ? '项目列表加载失败' : 'Failed to load projects'))
    setTaskIds(loadJSON(taskIdsKey(uid), {}))
  }, [uid])

  // 旧项目缺 spaceName 时惰性解析一次空间名（listSpaces 查名；失败静默，
  // 展示兜底「未记录路径」）。
  useEffect(() => {
    if (spaceNamesFetchedRef.current || projects.length === 0 || projects.every((p) => p.space_name)) return
    spaceNamesFetchedRef.current = true
    void listSpaces()
      .then((list) => setSpaceNameById(Object.fromEntries(list.map((s) => [s.id, s.name]))))
      .catch(() => { spaceNamesFetchedRef.current = false })
  }, [projects])

  /** 项目路径文案：新项目用创建时快照；旧项目惰性空间名兜底。 */
  const projPathText = (p: StudioProject): string => {
    if (p.folder_path) return p.folder_path
    if (p.space_name) return p.space_name
    const sn = spaceNameById[p.space_id]
    return sn ? `${sn}（未记录目录）` : '未记录路径'
  }

  // 切换项目：载入会话与中栏 Tab（无则建空）并重置工作目录/引用/评审。
  useEffect(() => {
    if (!uid || !pid) {
      setSessions([])
      setSid('')
      setOpenTabs([])
      setActiveTab(null)
      return
    }
    const list = loadJSON<StudioSession[]>(sessionsKey(uid, pid), [])
    const init = list.length > 0 ? list : [newSession()]
    setSessions(init)
    setSid(init[0].id)
    const tabs = loadJSON<OpenTab[]>(tabsKey(uid, pid), []).filter((x) => x && x.id && x.name)
    setOpenTabs(tabs)
    setActiveTab(tabs[0]?.id ?? null)
    setTaskRoot(projects.find((p) => p.id === pid)?.root_folder_id ?? '')
    setRefs([])
    setReviewId('')
    setAiTab('chat')
  }, [uid, pid]) // projects 读取为当前值即可，切换语义由 pid 驱动

  // 持久化写透：会话防抖 400ms（避免流式逐 token 落盘），任务映射/Tab 直接写。
  //（项目清单为服务端态，由 CRUD 流程各自写透，不再本地落盘。）
  useEffect(() => {
    if (!uid || !pid) return
    const timer = window.setTimeout(() => saveJSON(sessionsKey(uid, pid), sessions), 400)
    return () => window.clearTimeout(timer)
  }, [uid, pid, sessions])
  useEffect(() => {
    if (uid) saveJSON(taskIdsKey(uid), taskIds)
  }, [uid, taskIds])
  useEffect(() => {
    if (uid && pid) saveJSON(tabsKey(uid, pid), openTabs)
  }, [uid, pid, openTabs])

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
      .filter((x) => x.root_folder_id === project.root_folder_id || ids.has(x.id))
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
  }, [tasks, taskIds, project])

  const updateSessions = useCallback((fn: (prev: StudioSession[]) => StudioSession[]) => setSessions(fn), [])
  const toggleRef = (f: { fileId: string; fileName: string }) => {
    setRefs((prev) => (prev.some((x) => x.fileId === f.fileId) ? prev.filter((x) => x.fileId !== f.fileId) : [...prev, f]))
  }

  // ---- 中栏 Tab 管理 ----

  /** 打开文件（已开则激活；mode 显式给定则切换）。 */
  const openFile = useCallback((f: { id: string; name: string }, mode?: 'view' | 'edit') => {
    setOpenTabs((prev) => {
      const hit = prev.find((x) => x.id === f.id)
      if (hit) {
        return mode ? prev.map((x) => (x.id === f.id ? { ...x, mode } : x)) : prev
      }
      return [...prev, { id: f.id, name: f.name, mode: mode ?? 'view' }]
    })
    setActiveTab(f.id)
  }, [])
  const closeTab = (id: string) => {
    setOpenTabs((prev) => {
      const idx = prev.findIndex((x) => x.id === id)
      const next = prev.filter((x) => x.id !== id)
      setActiveTab((cur) => (cur === id ? (next[Math.min(idx, next.length - 1)]?.id ?? null) : cur))
      return next
    })
  }
  const setTabMode = (id: string, mode: 'view' | 'edit') => {
    setOpenTabs((prev) => prev.map((x) => (x.id === id ? { ...x, mode } : x)))
  }

  const createProject = async (data: ProjectFormData) => {
    const rootFolderId = data.folderId || (await listSpaceFiles(data.spaceId, null)).parent_id
    // 绑定路径快照：空间根项目 = 空间名；子目录 = 空间名/目录名。
    const folderPath = data.folderId && data.folderName ? `${data.spaceName}/${data.folderName}` : data.spaceName
    try {
      // 服务端注册（id 服务端生成；失败弹错不落本地）。
      const proj = await createStudioProject({
        name: data.name, space_id: data.spaceId, root_folder_id: rootFolderId,
        space_name: data.spaceName, folder_path: folderPath,
        // 执行引擎（缺省 = 'platform' 默认主路径）+ 沙箱 harness/默认模型
        //（'' = 跟随平台/平台默认）。
        engine: data.engine === 'docker' ? 'docker' : 'platform',
        harness: data.harness, model: data.model,
      })
      setProjects((p) => [...p, proj])
      setPid(proj.id)
    } catch (err) {
      message.error(err instanceof Error ? err.message : (zh ? '创建失败' : 'Failed to create'))
      throw err
    }
  }

  /** 编辑项目：名称/空间/绑定目录/引擎/harness/模型（会话与任务映射随
   *  project.id 保留；改绑目录后工作目录与中栏 Tab 重置）。 */
  const updateProject = async (id: string, data: ProjectFormData) => {
    const prev = projects.find((p) => p.id === id)
    const rootFolderId = data.folderId || (await listSpaceFiles(data.spaceId, null)).parent_id
    const folderPath = data.folderId && data.folderName ? `${data.spaceName}/${data.folderName}` : data.spaceName
    try {
      const updated = await updateStudioProject(id, {
        name: data.name, space_id: data.spaceId, root_folder_id: rootFolderId,
        space_name: data.spaceName, folder_path: folderPath,
        engine: data.engine === 'docker' ? 'docker' : 'platform',
        harness: data.harness, model: data.model,
      })
      setProjects((p) => p.map((x) => (x.id === id ? updated : x)))
    } catch (err) {
      message.error(err instanceof Error ? err.message : (zh ? '保存失败' : 'Failed to save'))
      throw err
    }
    if (prev && (prev.root_folder_id !== rootFolderId || prev.space_id !== data.spaceId)) {
      setTaskRoot(rootFolderId)
      setOpenTabs([])
      setActiveTab(null)
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
    }
  }

  // 删除项目：服务端删记录 + 清理该项目的任务追踪，并异步作废其未决 Agent
  // 任务（discard 后平台回收任务工作区与临时导出，容器产物即「删除 Docker
  // 空间数据」的落地语义；已 apply/done 的历史记录一并弃置）。空间内文件
  // 不受影响。
  const deleteProject = async (id: string) => {
    const proj = projects.find((p) => p.id === id)
    try {
      await deleteStudioProject(id)
    } catch (err) {
      message.error(err instanceof Error ? err.message : (zh ? '删除失败' : 'Failed to delete'))
      return
    }
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
    rightPanelRef.current?.expand()
    setAiCollapsed(false)
    setAiTab('review')
    setReviewId(taskId)
  }

  /** 树右键「删除」：软删（目录整树入回收站）；成功后刷新树/最近产物并清引用。 */
  const deleteTreeItem = (f: { id: string; name: string; type?: string }) => {
    const isFolder = f.type === 'folder'
    modal.confirm({
      title: zh ? `删除${isFolder ? '目录' : '文件'}「${f.name}」？` : `Delete ${isFolder ? 'folder' : 'file'} "${f.name}"?`,
      content: zh
        ? (isFolder ? '目录及其全部内容将移入回收站。' : '文件将移入回收站。')
        : (isFolder ? 'The folder and all its contents will be moved to trash.' : 'The file will be moved to trash.'),
      okText: zh ? '删除' : 'Delete',
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await deleteFile(f.id)
        } catch (err) {
          message.error(err instanceof Error ? err.message : '删除失败')
          return
        }
        setOpenTabs((prev) => prev.filter((x) => x.id !== f.id))
        if (activeTab === f.id) setActiveTab((cur) => (cur === f.id ? null : cur))
        if (taskRoot === f.id) setTaskRoot(project?.root_folder_id ?? '')
        setRefs((prev) => prev.filter((x) => x.fileId !== f.id))
        setTreeTick((n) => n + 1)
        setRecentTick((n) => n + 1)
        message.success('已移入回收站')
      },
    })
  }

  /** 树右键「重命名」：弹窗输入新名（同名校验由后端）。 */
  const renameTreeItem = (f: { id: string; name: string }) => {
    let next = f.name
    modal.confirm({
      title: zh ? `重命名「${f.name}」` : `Rename "${f.name}"`,
      content: (
        <Input
          defaultValue={f.name}
          onChange={(e) => { next = e.target.value }}
        />
      ),
      okText: zh ? '保存' : 'Save',
      onOk: async () => {
        const name = next.trim()
        if (!name || name === f.name) return
        try {
          await renameFile(f.id, name)
        } catch (err) {
          message.error(err instanceof Error ? err.message : '重命名失败')
          return
        }
        setOpenTabs((prev) => prev.map((x) => (x.id === f.id ? { ...x, name } : x)))
        setRefs((prev) => prev.map((x) => (x.fileId === f.id ? { ...x, fileName: name } : x)))
        setTreeTick((n) => n + 1)
        message.success('已重命名')
      },
    })
  }

  /** 快捷创建：复用上传管线在指定目录（默认项目根）建文档，建完新窗口打开编辑器。 */
  const createDoc = async (kind: 'richtext' | 'markdown', folderId?: string, folderName?: string) => {
    if (!project || creating) return
    // 目标目录现有名集合：默认名「新文档.md/.dfrt」被占用时自动加 -2/-3… 后缀
    //（上传管道对同名不同文件返回 409，固定名会导致二次点击必然失败）。
    const targetId = folderId || project.root_folder_id
    let taken = new Set<string>()
    try {
      const items = await listFiles(targetId, { spaceId: project.space_id, limit: 500 })
      taken = new Set(items.filter((f) => !f.is_root).map((f) => f.name.toLowerCase()))
    } catch {
      // 列举失败不阻断创建（沿用默认名，冲突时由错误提示兜底）。
    }
    const baseName = kind === 'markdown' ? '新文档.md' : '新文档.dfrt'
    const dot = baseName.lastIndexOf('.')
    const stem = dot > 0 ? baseName.slice(0, dot) : baseName
    const ext = dot > 0 ? baseName.slice(dot) : ''
    let docName = baseName
    for (let n = 2; taken.has(docName.toLowerCase()); n++) docName = `${stem}-${n}${ext}`
    const spec = kind === 'markdown'
      ? { name: docName, content: '# 新文档\n\n', mime: 'text/markdown', route: 'markdown' }
      : { name: docName, content: EMPTY_DFDOC_JSON, mime: 'application/json', route: 'dfdoc' }
    setCreating(kind)
    try {
      const session = await uploadFile(new File([spec.content], spec.name, { type: spec.mime }), targetId, () => {})
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
      if (session.file_id && session.file_id !== NIL_UUID) {
        // 建完直接在中栏 Tab 打开编辑（IDE 式；不再跳新窗口）。
        openFile({ id: session.file_id, name: spec.name }, 'edit')
      } else {
        message.warning('创建成功，但未返回文件 ID；请在目录树中打开')
      }
    } catch (err) {
      message.error(err instanceof Error ? err.message : '创建失败')
    } finally {
      setCreating(null)
    }
    if (folderName) message.info(`已创建到「${folderName}」`)
  }

  /** 新建类型（左栏「新建」下拉；与文件页新建菜单同构）。 */
  type StudioCreateKind = 'folder' | 'md' | 'richtext' | 'textfile' | 'drawio' | 'whiteboard' | 'word' | 'spreadsheet' | 'presentation'
  const CREATE_SPECS: Record<Exclude<StudioCreateKind, 'folder'>, { label: string; ext: string; content: string; mime: string }> = {
    md: { label: zh ? 'Markdown 文档' : 'Markdown', ext: '.md', content: '# 新文档\n\n', mime: 'text/markdown' },
    richtext: { label: zh ? '富文本文档' : 'Rich text', ext: '.dfrt', content: EMPTY_DFDOC_JSON, mime: 'application/json' },
    textfile: { label: zh ? '文本文件（.txt/.html/代码…）' : 'Text file', ext: '', content: '', mime: '' },
    drawio: { label: 'draw.io 图表', ext: '.drawio', content: EMPTY_DRAWIO_XML, mime: 'text/xml' },
    whiteboard: { label: zh ? '白板' : 'Whiteboard', ext: '.excalidraw', content: EMPTY_EXCALIDRAW_JSON, mime: 'application/json' },
    word: { label: 'Word', ext: '.docx', content: '', mime: '' },
    spreadsheet: { label: 'Excel', ext: '.xlsx', content: '', mime: '' },
    presentation: { label: 'PPT', ext: '.pptx', content: '', mime: '' },
  }
  const CREATE_DEFAULT_NAMES: Record<Exclude<StudioCreateKind, 'folder'>, string> = {
    md: '新文档.md', richtext: '新文档.dfrt', textfile: 'untitled.txt', drawio: '新图表.drawio',
    whiteboard: '新白板.excalidraw', word: '新文档.docx', spreadsheet: '新表格.xlsx', presentation: '新演示文稿.pptx',
  }
  // 名称弹窗（folder 与全部文档类型共用；目标目录 = 目录树目标或当前 AI 工作目录或项目根）。
  const [createDialog, setCreateDialog] = useState<{ kind: StudioCreateKind; name: string; parent?: { id: string; name: string } } | null>(null)
  const [createBusy, setCreateBusy] = useState(false)

  /** 新建入口：folder/文本类经名称弹窗（校验扩展名），md/富文本直接建
   *  （自动唯一化后缀）。parent 为目录树右键目标（缺省 = 工作目录/项目根）。 */
  const beginCreate = (kind: StudioCreateKind, parent?: { id: string; name: string }) => {
    if (!project || creating) return
    if (kind === 'md' || kind === 'richtext') {
      void createDoc(kind === 'md' ? 'markdown' : 'richtext', parent?.id, parent?.name)
      return
    }
    setCreateDialog({ kind, name: kind === 'folder' ? (zh ? '新目录' : 'New folder') : CREATE_DEFAULT_NAMES[kind], parent })
  }

  /** 名称弹窗提交：目录走 createSpaceFolder；文档按类型走 upload 管线或
   *  Office 模板端点；建完刷新树（文本/图表类直接在中栏打开编辑）。 */
  const submitCreate = async () => {
    if (!createDialog || !project || createBusy) return
    let name = createDialog.name.trim()
    const kind = createDialog.kind
    if (!name) return
    if (kind === 'textfile' && !/\.[A-Za-z0-9]{1,8}$/.test(name)) {
      message.error(zh ? '请填写包含扩展名的文件名（如 untitled.txt）' : 'Include an extension (e.g. untitled.txt)')
      return
    }
    if (kind !== 'folder' && kind !== 'textfile') {
      const ext = CREATE_SPECS[kind].ext
      if (!name.toLowerCase().endsWith(ext)) name += ext
    }
    setCreateBusy(true)
    try {
      const parent = createDialog.parent?.id ?? taskRoot ?? project.root_folder_id
      const parentName = createDialog.parent?.name ?? (zh ? '所选目录' : 'target folder')
      if (kind === 'folder') {
        await createSpaceFolder(project.space_id, name, parent === '__root__' ? project.root_folder_id : parent)
        message.success(zh ? `已创建目录「${name}」` : `Folder "${name}" created`)
      } else if (kind === 'word' || kind === 'spreadsheet' || kind === 'presentation') {
        const created = await createOfficeTemplate(kind, parent, name)
        message.success(zh ? `已创建「${name}」` : `"${name}" created`)
        openFile({ id: created.id, name: created.name }, 'edit')
      } else {
        const spec = CREATE_SPECS[kind]
        const mime = kind === 'textfile' ? (TEXT_FILE_MIME[name.slice(name.lastIndexOf('.') + 1).toLowerCase()] ?? 'text/plain') : spec.mime
        const session = await uploadFile(new File([spec.content], name, { type: mime }), parent, () => {})
        if (session.file_id && session.file_id !== NIL_UUID) {
          openFile({ id: session.file_id, name }, 'edit')
          message.success(zh ? `已创建「${name}」到「${parentName}」` : `"${name}" created in "${parentName}"`)
        }
      }
      setCreateDialog(null)
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
    } catch (err) {
      message.error(err instanceof Error ? err.message : (zh ? '创建失败' : 'Failed to create'))
    } finally {
      setCreateBusy(false)
    }
  }

  // 上传到平台目录（非 Agent 工作目录——容器产物需经评审「写回」才进入
  // 平台；上传用于交付素材/参考资料，成功后刷新目录树并自动加入引用）。
  /** 目录树拖拽上传（drop 到具体目录；根 = 项目根）：走 onUploadPicked 同
   *  管线，成功刷新树与最近产物。 */
  const onFilesDropped = async (files: File[], folder: { id: string; name: string }) => {
    if (!project || files.length === 0) return
    const targetId = folder.id || project.root_folder_id
    setUploading(true)
    let ok = 0
    for (const file of files) {
      try {
        await uploadFile(file, targetId, () => {})
        ok++
      } catch (err) {
        message.error(`${file.name}：${err instanceof Error ? err.message : '上传失败'}`)
      }
    }
    setUploading(false)
    if (ok > 0) {
      message.success(zh ? `已上传 ${ok} 个文件到「${folder.name}」` : `${ok} file(s) uploaded to "${folder.name}"`)
      setTreeTick((n) => n + 1)
      setRecentTick((n) => n + 1)
    }
  }
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
      {/* 顶栏（v3.3 精简）：项目下拉 + 管理项目 + 打开项目目录 + 弹性空 +
          左/右面板折叠钮（折叠后面板头不可见，顶栏为唯一展开入口）。 */}
      <header className="studio-topbar">
        <Dropdown trigger={['click']} menu={{ items: projMenuItems, onClick: onProjMenuClick }}>
          <button type="button" className="studio-proj-dd" title={project ? projPathText(project) : (zh ? '选择项目' : 'Select project')}>
            <FolderClosed size={14} aria-hidden="true" />
            <span className="name">{project?.name ?? (zh ? '选择项目' : 'Select project')}</span>
            {project && <EngineTag engine={engine} zh={zh} />}
          </button>
        </Dropdown>
        {project && (
          <Tooltip title={zh ? `在文件页打开「${projPathText(project)}」` : `Open "${projPathText(project)}" in Files`}>
            <Button size="small" onClick={() => { window.location.href = `/files?space=${project.space_id}&folder=${project.root_folder_id}` }}>
              <FolderClosed size={13} aria-hidden="true" />{zh ? '打开项目目录' : 'Open folder'}
            </Button>
          </Tooltip>
        )}
        {project && <span className="studio-topbar-path muted" title={projPathText(project)}>{projPathText(project)}</span>}
        <span className="studio-topbar-ops">
          <Tooltip title={leftCollapsed ? (zh ? '展开文件树' : 'Expand file tree') : (zh ? '收起文件树' : 'Collapse file tree')}>
            <Button size="small" type="text" aria-label={zh ? '切换文件树' : 'Toggle file tree'} disabled={!project}
              onClick={() => (leftCollapsed ? leftPanelRef.current?.expand() : leftPanelRef.current?.collapse())}>
              {leftCollapsed ? <PanelLeftOpen size={14} aria-hidden="true" /> : <PanelLeftClose size={14} aria-hidden="true" />}
            </Button>
          </Tooltip>
          {/* 中栏（查看/编辑区）折叠：仅已打开文件时可用（v3.4）。 */}
          <Tooltip title={midCollapsed ? (zh ? '展开编辑区' : 'Expand editor') : (zh ? '收起编辑区' : 'Collapse editor')} disabled={openTabs.length === 0}>
            <Button size="small" type="text" aria-label={zh ? '切换编辑区' : 'Toggle editor'} disabled={openTabs.length === 0}
              onClick={() => (midCollapsed ? midPanelRef.current?.expand() : midPanelRef.current?.collapse())}>
              <Columns3 size={14} aria-hidden="true" />
            </Button>
          </Tooltip>
          <Tooltip title={aiCollapsed ? (zh ? '展开 AI 面板' : 'Expand AI panel') : (zh ? '收起 AI 面板' : 'Collapse AI panel')} disabled={!feats.enabled}>
            <Button size="small" type="text" aria-label={zh ? '切换 AI 面板' : 'Toggle AI panel'} disabled={!feats.enabled}
              onClick={() => (aiCollapsed ? rightPanelRef.current?.expand() : rightPanelRef.current?.collapse())}>
              {aiCollapsed ? <PanelRightOpen size={14} aria-hidden="true" /> : <PanelRightClose size={14} aria-hidden="true" />}
            </Button>
          </Tooltip>
        </span>
      </header>

      {/* 三栏（react-resizable-panels v4）：左树 | 中编辑（条件渲染）| 右 AI。
          分栏宽度经 useDefaultLayout 持久化（localStorage）；未打开文件时
          中栏不渲染，右栏自动延展占满中栏空间。 */}
      <Group orientation="horizontal" className="studio-panels" defaultLayout={defaultLayout} onLayoutChanged={onLayoutChanged}>
        {/* 左栏：文件目录树（可拖拽收窄/按钮折叠）。 */}
        <Panel
          id="left"
          panelRef={leftPanelRef}
          collapsible
          collapsedThreshold="6%"
          className={`studio-col studio-left${leftCollapsed ? ' collapsed' : ''}`}
          defaultSize="19%"
          minSize={170}
          onResize={(size) => setLeftCollapsed(size.asPercentage <= 0.5)}
        >
          <div className="studio-left-head">
            <span className="title"><FolderClosed size={13} aria-hidden="true" />{zh ? '文件' : 'Files'}</span>
            {project && (
              <span className="ops">
                {/* 新建下拉（与文件页新建菜单同构：目录 + 全文档类型；目标 =
                    当前 AI 工作目录或项目根）。 */}
                <Dropdown
                  trigger={['click']}
                  menu={{
                    items: [
                      { key: 'folder', icon: <FolderPlus size={13} aria-hidden="true" />, label: zh ? '目录' : 'Folder' },
                      { type: 'divider' },
                      { key: 'md', icon: <FileText size={13} aria-hidden="true" />, label: zh ? 'Markdown 文档' : 'Markdown' },
                      { key: 'richtext', icon: <Plus size={13} aria-hidden="true" />, label: zh ? '富文本文档' : 'Rich text' },
                      { key: 'textfile', icon: <FileText size={13} aria-hidden="true" />, label: zh ? '文本文件' : 'Text file' },
                      { key: 'drawio', icon: <Package size={13} aria-hidden="true" />, label: 'draw.io' },
                      { key: 'whiteboard', icon: <Plus size={13} aria-hidden="true" />, label: zh ? '白板' : 'Whiteboard' },
                      { type: 'divider' },
                      { key: 'word', icon: <FileText size={13} aria-hidden="true" />, label: 'Word' },
                      { key: 'spreadsheet', icon: <FileText size={13} aria-hidden="true" />, label: 'Excel' },
                      { key: 'presentation', icon: <FileText size={13} aria-hidden="true" />, label: 'PPT' },
                    ] as MenuProps['items'],
                    onClick: ({ key }) => beginCreate(key as 'folder' | 'md' | 'richtext' | 'textfile' | 'drawio' | 'whiteboard' | 'word' | 'spreadsheet' | 'presentation'),
                  }}
                >
                  <Button size="small" type="text" aria-label={zh ? '新建' : 'New'} disabled={creating !== null || createBusy}>
                    <Plus size={14} aria-hidden="true" />
                  </Button>
                </Dropdown>
                <Tooltip title={zh ? `上传到${taskRoot && taskRoot !== project.root_folder_id ? '「' + (zh ? 'AI 工作目录' : 'working root') + '」' : '项目根目录'}（也可拖拽文件到目录树）` : 'Upload to the working root (or drop files onto the tree)'}>
                  <Button size="small" type="text" aria-label={zh ? '上传文件' : 'Upload files'} loading={uploading}
                    onClick={() => openUpload({ id: taskRoot || project.root_folder_id, name: taskRoot && taskRoot !== project.root_folder_id ? (zh ? '工作目录' : 'working root') : project.name })}>
                    <Upload size={14} aria-hidden="true" />
                  </Button>
                </Tooltip>
                <Tooltip title={zh ? '刷新目录树' : 'Refresh tree'}>
                  <Button size="small" type="text" aria-label={zh ? '刷新目录树' : 'Refresh tree'} onClick={() => setTreeTick((n) => n + 1)}><RefreshCw size={14} aria-hidden="true" /></Button>
                </Tooltip>
              </span>
            )}
          </div>
          <div className="studio-left-body">
            {project ? (
              <>
                {engine === 'docker' && <div className="studio-group"><FolderClosed size={13} aria-hidden="true" />{zh ? '项目目录（平台）' : 'Project folders (platform)'}</div>}
                <FileTreePanel
                  key={`${project.id}:${treeTick}`}
                  zh={zh}
                  spaceId={project.space_id}
                  rootId={project.root_folder_id}
                  rootTitle={projPathText(project)}
                  activeFolderId={taskRoot}
                  activeFileId={activeTab}
                  isRefFile={(f) => refs.some((r) => r.fileId === f.id)}
                  onOpenFile={(f) => openFile({ id: f.id, name: f.name }, 'view')}
                  onEditFile={(f) => openFile({ id: f.id, name: f.name }, 'edit')}
                  onSetWorkRoot={(f) => { setTaskRoot(f.id); message.info(zh ? `已将「${f.name}」设为 AI 工作目录` : `Working root set to "${f.name}"`) }}
                  onUploadTo={(f) => openUpload({ id: f.id, name: f.name })}
                  onCreateDoc={(kind, folderId, folderName) => void createDoc(kind, folderId, folderName)}
                  onCreateFolder={(parent) => beginCreate('folder', { id: parent.id === '__root__' ? '' : parent.id, name: parent.name })}
                  onDropFiles={(files, folder) => void onFilesDropped(files, folder)}
                  onToggleRef={(f) => toggleRef({ fileId: f.id, fileName: f.name })}
                  onRename={renameTreeItem}
                  onDelete={deleteTreeItem}
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
                        onOpenFile={(f) => openFile(f, 'view')}
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
        </Panel>

        {/* 中栏：多 Tab 查看/编辑（无打开文件时不渲染，右栏延展）。 */}
        {openTabs.length > 0 && (
          <>
            <Separator className="studio-split" />
            <Panel id="mid" className="studio-col studio-mid" panelRef={midPanelRef} collapsible collapsedThreshold="15%" minSize="22%" defaultSize="44%" onResize={(size) => setMidCollapsed(size.asPercentage <= 0.5)}>
              <EditorTabs
                zh={zh}
                tabs={openTabs}
                activeId={activeTab}
                onActive={setActiveTab}
                onClose={closeTab}
                onModeChange={setTabMode}
              />
            </Panel>
          </>
        )}

        <Separator className="studio-split" />
        {/* 右栏：AI 工作区（可折叠；docker = 对话|评审 双 Tab）。 */}
        <Panel
          id="right"
          panelRef={rightPanelRef}
          collapsible
          collapsedThreshold="8%"
          className={`studio-col studio-right${aiCollapsed ? ' collapsed' : ''}`}
          minSize={300}
          onResize={(size) => setAiCollapsed(size.asPercentage <= 0.5)}
        >
          {/* 右栏（v3.3 去掉面板头省纵向空间）：docker 引擎保留 28px slim
              Tab 行（对话|评审）；platform 直出对话体。折叠钮在顶栏。 */}
          {engine === 'docker' && (
            <div className="studio-right-slim">
              <span className="studio-ai-tabs" role="tablist">
                <button type="button" role="tab" aria-selected={aiTab === 'chat'} className={aiTab === 'chat' ? 'active' : ''} onClick={() => setAiTab('chat')}>{zh ? '对话' : 'Chat'}</button>
                <button type="button" role="tab" aria-selected={aiTab === 'review'} className={aiTab === 'review' ? 'active' : ''} onClick={() => setAiTab('review')}>{zh ? '评审' : 'Review'}</button>
              </span>
            </div>
          )}
          {aiTab === 'review' && engine === 'docker' ? (
            <div className="studio-right-body">
              {projectTasks.length > 0 ? taskPickSelect : <div className="muted studio-pad8">{zh ? '暂无任务：切回「对话」输入指令发起。' : 'No tasks yet; switch to Chat to create one.'}</div>}
              <TaskReview taskId={reviewId || null} onChanged={() => void refreshTasks()} />
            </div>
          ) : (
            <div className="studio-right-body">
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
                  recentTick={recentTick}
                  onOpenInTab={(f) => openFile(f, 'view')}
                />
              ) : (
                <div className="studio-noproject">
                  <div className="studio-home-icon" aria-hidden="true"><Sparkles size={24} strokeWidth={2} /></div>
                  <div className="studio-home-title">{zh ? 'AI 创作空间' : 'AI Studio'}</div>
                  <div className="muted">
                    {zh
                      ? '还没有项目。项目 = 空间 + 根目录 + 执行引擎（平台直读写 / Docker 沙箱）：创建后在此与 AI 协作产出文件。'
                      : 'No project yet. A project = space + root folder + engine (platform direct / Docker sandbox); create one to start creating with AI.'}
                  </div>
                  <div className="studio-home-ops">
                    <Button type="primary" onClick={() => setProjForm({ mode: 'create' })}><Plus size={13} aria-hidden="true" />{zh ? '新建项目' : 'New project'}</Button>
                  </div>
                </div>
              )}
            </div>
          )}
        </Panel>
      </Group>

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
      {/* 左栏「新建」名称弹窗（目录/文本/图表/Office 模板）。 */}
      {createDialog && (
        <Modal
          title={`${zh ? '新建' : 'New'}${createDialog.kind === 'folder' ? (zh ? '目录' : ' folder') : ''}${createDialog.parent ? ` — ${createDialog.parent.name}` : ''}`}
          onClose={() => setCreateDialog(null)}
        >
          <Input
            autoFocus
            value={createDialog.name}
            maxLength={80}
            placeholder={createDialog.kind === 'textfile' ? (zh ? '含扩展名，如 index.html' : 'with extension, e.g. index.html') : (zh ? '名称' : 'Name')}
            onPressEnter={() => void submitCreate()}
            onChange={(e) => setCreateDialog({ ...createDialog, name: e.target.value })}
          />
          <div className="modal-actions">
            <Button onClick={() => setCreateDialog(null)}>{zh ? '取消' : 'Cancel'}</Button>
            <Button type="primary" disabled={!createDialog.name.trim()} loading={createBusy} onClick={() => void submitCreate()}>{zh ? '创建' : 'Create'}</Button>
          </div>
        </Modal>
      )}
    </div>
  )
}
