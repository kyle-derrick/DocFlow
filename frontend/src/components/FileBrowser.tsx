// 通用文件浏览组件：从 FilesPage 提炼的目录列表 / 面包屑 / 上传 / 下载 /
// 预览（含 office 文档的 ONLYOFFICE「编辑」入口）/ 新建文件夹逻辑，
// 个人空间与团队空间共用。
// 通过注入 listItems / createFolderFn / uploadFn / downloadFn / previewFn
// 适配不同后端端点；写操作 403 时统一提示「无写权限」。
// v1.0 追加：多选 + 批量移动/删除（部分成功语义）、行内星标切换、
// 行内标签管理（打/去标签、新建）、顶栏标签/收藏筛选与服务端排序。
// v1.2 追加：列表/网格视图切换（设计 6.3.7；偏好持久化 localStorage，
// Ctrl/Cmd+1、Ctrl/Cmd+2 快捷键见设计 6.16.1）。
// v1.3 布局重构：顶栏+筛选栏合并为单行工具带（.files-toolbar，紧邻
// SpaceSwitcher 行下方省行高）；页面级大标题移除只留面包屑；上传改拆分
// 按钮（主点击=上传文件，附落下拉=上传目录）；「全部/收藏/最近」视图
// 切换在 SpaceSwitcher 行下拉化（见 SpaceSwitcher）；查看弹窗标题行
// 改造（标题左、拆分操作按钮右、标题字号缩小）；网页目录（has_index_web）
// 点击改弹窗内嵌 iframe；office 文档弹窗内嵌 OnlyOffice 只读视图（与
// 独立查看页一致，FileViewerDispatch 统一分发）；树点击文件经
// fileOpenSignal 受控信号触发本组件弹窗（见 FolderTreeNav）。
import { FormEvent, ReactNode, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { App as AntdApp, Button, Dropdown, Input, Menu, Modal as AntdModal, Select } from 'antd'
import type { MenuProps } from 'antd'
import {
  FileText,
  Folder,
  Globe,
  LayoutGrid,
  List,
  MoreHorizontal,
  Plus,
  Settings,
  Star,
  Upload,
} from 'lucide-react'
import {
  ApiError,
  BatchItemResult,
  EMPTY_DRAWIO_XML,
  EMPTY_EXCALIDRAW_JSON,
  FileItem,
  FileQueryOptions,
  FileWithVersion,
  OpenWithMap,
  OpenWithOpener,
  Tag,
  UploadPhase,
  addFileTag,
  batchMoveFiles,
  batchTrashFiles,
  createOfficeTemplate,
  createShare,
  createTag,
  deleteOpenWith,
  downloadBatchFiles,
  downloadFile,
  downloadFolderZip,
  drawioStatus,
  getFileMeta,
  isDrawioFile,
  isExcalidrawFile,
  isHtmlFile,
  isOfficeFile,
  listFileTags,
  listOpenWith,
  listTags,
  onlyOfficeStatus,
  removeFileTag,
  encodePathSegments,
  resolveFileById,
  resolvePath,
  setFileStarred,
  setOpenWith,
  summarizeBatchResults,
  unpackZip,
} from '../api'
import {
  ALL_OPENERS,
  extOf,
  isCodeFile,
  openWithOptions,
  openerLabel,
  resolveOpener,
} from '../openers'
import { useHotkeys } from '../useHotkeys'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'
// 弹窗内嵌查看：复用独立查看页的按类型分发器（office/drawio/白板/
// xmind/mermaid/md/网页/文本等），保证弹窗与新窗口打开渲染一致。
import { FileViewerDispatch } from '../pages/ViewerPage'

/** 文件浏览视图模式（设计 6.3.7）：list = 现有表格，grid = 卡片网格。 */
export type ViewMode = 'list' | 'grid'

/** 视图偏好持久化 key（个人空间与团队空间共用，见设计 6.3.7）。 */
const VIEW_MODE_KEY = 'docflow.viewMode'

function loadViewMode(): ViewMode {
  return window.localStorage.getItem(VIEW_MODE_KEY) === 'grid' ? 'grid' : 'list'
}

function saveViewMode(mode: ViewMode): void {
  // 隐私模式等 localStorage 不可用时静默跳过（偏好仅本次会话生效）。
  try {
    window.localStorage.setItem(VIEW_MODE_KEY, mode)
  } catch {
    /* ignore */
  }
}

/** 字节数人类可读格式（网格卡片元信息；口径与版本历史一致）。 */
function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`
}

export function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

/**
 * 视口内收缩定位 fixed 菜单：按元素实测尺寸把 (x, y) 收进视口
 * （右/下越界时向左/上收，保 8px 边距）——比按固定估算宽高翻转可靠，
 * 右键菜单与树右键菜单共用。
 */
export function clampFixedMenu(el: HTMLElement, x: number, y: number): void {
  const rect = el.getBoundingClientRect()
  const vw = window.innerWidth
  const vh = window.innerHeight
  let left = x
  let top = y
  if (left + rect.width > vw - 8) left = Math.max(8, vw - 8 - rect.width)
  if (top + rect.height > vh - 8) top = Math.max(8, vh - 8 - rect.height)
  el.style.left = `${left}px`
  el.style.top = `${top}px`
}

/**
 * 全站通用弹窗（antd Modal 薄封装，保持既有签名）：title/onClose/wide/
 * className/headExtra/children 与旧自写 Modal 一致，40+ 调用点零改动；
 * 视觉经 styles.css「antd Modal 适配」节对齐旧 .modal（含 modal-viewer
 * 加宽）。onClose 映射 onCancel（mask 点击 / Esc / 关闭按钮均触发）。
 */
export function Modal({
  title,
  onClose,
  wide,
  className,
  headExtra,
  children,
}: {
  title: string
  onClose: () => void
  wide?: boolean
  /** 追加到弹窗根元素的自定义类（如查看弹窗 modal-viewer 加宽加高）。 */
  className?: string
  /** 标题行右侧追加内容（查看弹窗的「新窗口查看/编辑/下载」拆分按钮组）。 */
  headExtra?: ReactNode
  children: ReactNode
}) {
  return (
    <AntdModal
      open
      centered
      footer={null}
      width={wide ? 760 : 420}
      onCancel={onClose}
      title={
        headExtra ? (
          <div className="docflow-modal-title-row">
            <span className="docflow-modal-title-text">{title}</span>
            <span className="docflow-modal-head-extra">{headExtra}</span>
          </div>
        ) : (
          title
        )
      }
      className={className ? `docflow-modal ${className}` : 'docflow-modal'}
    >
      {children}
    </AntdModal>
  )
}

/** antd modal API 类型（App.useApp().modal）。 */
export type AntdModalApi = ReturnType<typeof AntdApp.useApp>['modal']

/** modal.confirm 的 Promise 封装：确认 resolve(true)、取消 resolve(false)。 */
export function confirmDialog(
  modal: AntdModalApi,
  opts: { title: string; content?: ReactNode; okText: string; danger?: boolean; cancelText: string },
): Promise<boolean> {
  return new Promise((resolve) => {
    modal.confirm({
      title: opts.title,
      content: opts.content,
      okText: opts.okText,
      okButtonProps: { danger: opts.danger },
      cancelText: opts.cancelText,
      onOk: () => resolve(true),
      onCancel: () => resolve(false),
    })
  })
}

/** window.prompt 的 antd 替代：modal.confirm + 受控 Input，确认回传输入值、取消回传 null。 */
export function promptViaModal(
  modal: AntdModalApi,
  opts: { title: string; label?: string; initialValue?: string; placeholder?: string; okText: string; cancelText: string },
): Promise<string | null> {
  let value = opts.initialValue ?? ''
  const SyncedInput = () => {
    const [text, setText] = useState(value)
    return (
      <div style={{ marginTop: 12 }}>
        {opts.label && <div style={{ marginBottom: 8 }}>{opts.label}</div>}
        <Input
          autoFocus
          allowClear
          value={text}
          placeholder={opts.placeholder}
          onChange={(e) => {
            setText(e.target.value)
            value = e.target.value
          }}
        />
      </div>
    )
  }
  return new Promise((resolve) => {
    modal.confirm({
      title: opts.title,
      icon: null,
      content: <SyncedInput />,
      okText: opts.okText,
      cancelText: opts.cancelText,
      onOk: () => resolve(value.trim()),
      onCancel: () => resolve(null),
    })
  })
}

export const phaseText: Record<UploadPhase | 'error', string> = {
  creating: '创建会话…',
  uploading: '上传中…',
  completing: '提交处理…',
  verifying: '校验中…',
  scanning: '安全扫描中…',
  available: '已完成',
  quarantined: '已隔离',
  failed: '失败',
  error: '失败',
}

/** 批量错误码 → 中文提示（与后端 openapi BatchResultItem.error_code 对应）。 */
export const batchErrorText: Record<string, string> = {
  NOT_FOUND: '不存在或无权访问',
  FORBIDDEN: '无写权限',
  ROOT: '根目录不可操作',
  NAME_CONFLICT: '目标目录存在同名项',
  INVALID_TARGET: '不能移动到自身或其子目录',
  PARENT_DELETED: '原目录已删除',
  NOT_DELETED: '不在回收站',
  INTERNAL: '服务内部错误',
}

export function describeBatchResults(results: BatchItemResult[]): string {
  const failures = results.filter((r) => !r.ok)
  if (failures.length === 0) return `全部 ${results.length} 项成功`
  const parts = failures.map((r) => `${r.id.slice(0, 8)}…：${batchErrorText[r.error_code ?? 'INTERNAL'] ?? r.error_code}`)
  return `${summarizeBatchResults(results)}。${parts.join('；')}`
}

interface UploadRow {
  key: number
  name: string
  phase: UploadPhase | 'error'
  error?: string
}

interface Crumb {
  /** 列表查询用的目录 ID；null 表示根（个人根 / 团队根，按注入的 listItems 语义）。 */
  id: string | null
  /** 真实目录 ID（上传/建目录用）；团队根由列表响应回填，个人根为 null。 */
  folderId: string | null
  name: string
}

/** listItems 的返回：目录条目 + 当前列出目录的真实 ID。 */
export interface DirListing {
  items: FileItem[]
  folderId: string | null
}

/**
 * 面包屑 → 命名空间内相对路径段（去掉首段根标签；个人空间与团队空间
 * 通用，供「作为网页打开」等需要按路径 resolve 的场景拼路径复用）。
 */
export function pathSegmentsOf(crumbs: Array<{ id: string | null; name: string }>): string[] {
  return crumbs.slice(1).map((c) => c.name)
}

/** 「编辑文本」入口的适用扩展名（md/markdown/txt）。 */
function isTextEditable(name: string): boolean {
  const lower = name.toLowerCase()
  return lower.endsWith('.md') || lower.endsWith('.markdown') || lower.endsWith('.txt')
}

/** 「文本文件」新建：按用户自带扩展名推断 MIME（File 构造用）。 */
const TEXT_FILE_MIME: Record<string, string> = {
  html: 'text/html',
  htm: 'text/html',
  css: 'text/css',
  js: 'text/javascript',
  mjs: 'text/javascript',
  json: 'application/json',
  md: 'text/markdown',
  markdown: 'text/markdown',
  txt: 'text/plain',
}

/** 「文本文件」新建：按扩展名选择创建后打开的编辑器路由（与 openers 分发一致）。 */
function textFileRoute(name: string): 'view' | 'markdown' | 'code' | 'text' {
  const lower = name.toLowerCase()
  if (lower.endsWith('.html') || lower.endsWith('.htm')) return 'view'
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return 'markdown'
  if (isCodeFile(lower)) return 'code'
  return 'text'
}

function sortItems(items: FileItem[]): FileItem[] {
  return [...items].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))
}

/** 写操作错误文案：403 统一为「无写权限」（团队 viewer、只读目录等）。 */
function writeErrorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.status === 403) return '无写权限'
  return err instanceof Error ? err.message : fallback
}

export interface FileBrowserProps {
  /** 面包屑根名称。 */
  rootLabel: string
  listItems: (parentId: string | null, opts?: FileQueryOptions) => Promise<DirListing>
  /** 提供时显示「新建文件夹」。 */
  createFolderFn?: (name: string, parentId: string | null) => Promise<unknown>
  /** 提供时显示「上传文件」。 */
  uploadFn?: (file: File, parentId: string | null, onPhase: (phase: UploadPhase) => void) => Promise<unknown>
  /** 下载实现，缺省走个人文件端点。 */
  downloadFn?: (item: FileItem) => Promise<void>
  /** 每行追加操作按钮（分享 / 重命名 / 删除等由调用方渲染）。 */
  rowActions?: (item: FileItem) => ReactNode
  emptyHint?: string
  /** 变化时重新加载当前目录（外部操作成功后刷新列表用）。 */
  reloadKey?: number
  /** 提供时批量移动对话框含「根目录」选项（个人空间；空目标即个人根）。 */
  rootTargetLabel?: string
  /**
   * 受控视图（全部/收藏/最近）：由 SpaceSwitcher 驱动（视图切换 UI 已上移到
   * 空间切换行，本组件顶栏不再渲染）；变化时同步内部筛选状态并重新查询。
   */
  activeView?: 'all' | 'starred' | 'recent'
  /** 提供时文件行显示「复制」（parentId 为目标目录 UUID；留空目标由本组件解析为源目录）。 */
  copyFn?: (fileId: string, parentId: string) => Promise<unknown>
  /** 文件元数据来源（GET /files/{id}）；提供时网格视图卡片惰性补齐文件大小。 */
  fileMetaFn?: (fileId: string) => Promise<FileWithVersion | null>
  /**
   * 当前空间命名空间（供目录「作为网页打开」按路径 resolve）：个人空间
   * personal + 自己 user UUID；团队空间 team + 团队 ID。检索模式（跨目录）
   * 下面包屑不代表条目位置，菜单项自动隐藏。
   */
  ns?: { type: 'personal' | 'team'; scope: string }
  /**
   * 外部「打开文件」受控信号（左侧目录树文件节点点击触发）：seq 变化时
   * 在当前列表按 fileId 定位并打开查看弹窗；不在当前目录时经
   * fileMetaFn/getFileMeta 拉取元数据后打开。pathSegments 提供时（树内
   * 已知路径）弹窗内 by-path 路由按其构建，避免面包屑不对应。
   */
  fileOpenSignal?: { fileId: string; seq: number; pathSegments?: string[] }
}

export default function FileBrowser({
  rootLabel,
  listItems,
  createFolderFn,
  uploadFn,
  downloadFn,
  rowActions,
  emptyHint,
  reloadKey,
  rootTargetLabel,
  activeView,
  copyFn,
  fileMetaFn,
  ns,
  fileOpenSignal,
}: FileBrowserProps) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const { modal: antdModal } = AntdApp.useApp()
  const doDownload = downloadFn ?? downloadFile

  const [crumbs, setCrumbs] = useState<Crumb[]>([{ id: null, folderId: null, name: rootLabel }])
  const [items, setItems] = useState<FileItem[]>([])
  const [directoryQuery, setDirectoryQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // 顶栏筛选与服务端排序（标签/收藏激活时进入跨目录检索模式；最近访问为独立视图）。
  const [tags, setTags] = useState<Tag[]>([])
  const [tagFilter, setTagFilter] = useState('')
  const [starredFilter, setStarredFilter] = useState('')
  const [recentView, setRecentView] = useState(false)
  const [sortKey, setSortKey] = useState<'name' | 'updated_at' | 'size'>('name')
  const [sortOrder, setSortOrder] = useState<'asc' | 'desc'>('asc')
  const searchMode = tagFilter !== '' || starredFilter !== '' || recentView

  // 多选与批量操作。
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [batchBusy, setBatchBusy] = useState(false)

  // 视图模式（设计 6.3.7）：list / grid，偏好持久化到 localStorage，
  // 个人空间与团队空间共用同一偏好（状态在两视图间共享，选择互通）。
  const [viewMode, setViewMode] = useState<ViewMode>(loadViewMode)

  // 网格卡片的「⋯」操作菜单：当前展开的条目 ID（null = 关闭）。
  const [cardMenuFor, setCardMenuFor] = useState<string | null>(null)

  // 右键 / 列表行「⋯」菜单：目标条目 + 视口坐标（null = 关闭）。
  const [ctxMenu, setCtxMenu] = useState<{ item: FileItem; x: number; y: number } | null>(null)
  // 菜单浮层元素：渲染后按实测尺寸收进视口（右/下越界向左/上翻，见 clampFixedMenu）。
  const ctxMenuRef = useRef<HTMLDivElement | null>(null)
  useLayoutEffect(() => {
    if (!ctxMenu || !ctxMenuRef.current) return
    clampFixedMenu(ctxMenuRef.current, ctxMenu.x, ctxMenu.y)
  }, [ctxMenu])
  // 「＋ 新建」下拉开关。
  const [createMenuOpen, setCreateMenuOpen] = useState(false)

  // 右键菜单与新建下拉点击外部关闭（菜单内部动作在冒泡阶段完成后收口）。
  useEffect(() => {
    if (!ctxMenu && !createMenuOpen) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (el?.closest?.('.ctx-menu, .create-menu-wrap, .split-btn')) return
      setCtxMenu(null)
      setCreateMenuOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [ctxMenu, createMenuOpen])

  const changeViewMode = (mode: ViewMode) => {
    setCardMenuFor(null)
    setViewMode(mode)
    saveViewMode(mode)
  }

  // 网格视图文件大小：列表接口不返回 size，经 fileMetaFn 惰性补齐（ref 缓存
  // 避免重复请求；-1 占位表示已请求过/未知，不再重试）。
  const sizeCache = useRef<Map<string, number>>(new Map())
  const [, setSizesTick] = useState(0)
  useEffect(() => {
    if (viewMode !== 'grid' || !fileMetaFn) return
    const targets = items.filter((it) => it.type === 'file' && !sizeCache.current.has(it.id))
    if (targets.length === 0) return
    targets.forEach((it) => sizeCache.current.set(it.id, -1))
    void Promise.all(targets.map((it) => fileMetaFn(it.id))).then((metas) => {
      let changed = false
      metas.forEach((meta, i) => {
        const size = meta?.current_version?.size ?? 0
        if (size > 0) {
          sizeCache.current.set(targets[i].id, size)
          changed = true
        }
      })
      if (changed) setSizesTick((n) => n + 1)
    })
  }, [viewMode, items, fileMetaFn])

  // 卡片菜单点击外部关闭（点菜单按钮本身由其 onClick 处理开合切换）。
  useEffect(() => {
    if (cardMenuFor === null) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (el?.closest?.('.file-card-menu, .card-menu-btn')) return
      setCardMenuFor(null)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [cardMenuFor])

  const [batchNotice, setBatchNotice] = useState('')
  const [batchError, setBatchError] = useState('')
  // zip 解包为目录树（POST /files/:id/unpack）：进行中条目 ID + 部分失败明细随结果展示。
  const [unpackBusyId, setUnpackBusyId] = useState<string | null>(null)
  const [moveOpen, setMoveOpen] = useState(false)
  const [moveTarget, setMoveTarget] = useState('')
  const [moveManual, setMoveManual] = useState('')
  const [moveError, setMoveError] = useState('')

  // 批量分享：逐个创建公开分享后弹出链接列表。
  const [batchShareBusy, setBatchShareBusy] = useState(false)
  const [shareLinks, setShareLinks] = useState<Array<{ name: string; url: string }>>([])
  const [shareListOpen, setShareListOpen] = useState(false)
  const [copiedShareIdx, setCopiedShareIdx] = useState(-1)

  // 批量打标签：从已有标签中选择一个应用到全部选中项。
  const [batchTagOpen, setBatchTagOpen] = useState(false)
  const [batchTagId, setBatchTagId] = useState('')
  const [batchTagBusy, setBatchTagBusy] = useState(false)
  const [batchTagError, setBatchTagError] = useState('')

  // 行内复制：目标目录 UUID 输入，留空复制到源目录。
  const [copyTarget, setCopyTarget] = useState<FileItem | null>(null)
  const [copyParent, setCopyParent] = useState('')
  const [copyBusy, setCopyBusy] = useState(false)
  const [copyError, setCopyError] = useState('')

  // 行内标签管理。
  const [tagModalTarget, setTagModalTarget] = useState<FileItem | null>(null)
  const [tagModalFileTagIds, setTagModalFileTagIds] = useState<Set<string>>(new Set())
  const [tagModalNewName, setTagModalNewName] = useState('')
  const [tagModalError, setTagModalError] = useState('')
  const [tagModalBusy, setTagModalBusy] = useState(false)

  // ONLYOFFICE 集成探测（会话级缓存）：启用且为 office 文档时文件行显示「编辑」。
  const [ooEnabled, setOoEnabled] = useState(false)
  useEffect(() => {
    let alive = true
    void onlyOfficeStatus().then((s) => {
      if (alive) setOoEnabled(s.enabled)
    })
    return () => {
      alive = false
    }
  }, [])

  // draw.io 图表编辑集成探测（会话级缓存）：启用时 .drawio 文件行显示
  //「图表」按钮、工具栏显示「新建图表」。
  const [drawioEnabled, setDrawioEnabled] = useState(false)
  useEffect(() => {
    let alive = true
    void drawioStatus().then((s) => {
      if (alive) setDrawioEnabled(s.enabled)
    })
    return () => {
      alive = false
    }
  }, [])

  // ---- 默认打开方式偏好（/me/open-with）：初始化加载；保存/删除后即时更新本地映射。 ----
  const [openWith, setOpenWithMap] = useState<OpenWithMap>({})
  const [openWithMgrOpen, setOpenWithMgrOpen] = useState(false)
  const [openWithMgrError, setOpenWithMgrError] = useState('')
  // 管理弹窗「新增偏好」行：扩展名输入 + 打开方式下拉。
  const [mgrNewExt, setMgrNewExt] = useState('')
  const [mgrNewOpener, setMgrNewOpener] = useState<OpenWithOpener>('text')
  const [mgrAdding, setMgrAdding] = useState(false)
  useEffect(() => {
    let alive = true
    void listOpenWith()
      .then((map) => {
        if (alive) setOpenWithMap(map)
      })
      .catch(() => {
        /* 偏好不可用（旧后端等）：按内置默认分发 */
      })
    return () => {
      alive = false
    }
  }, [])

  // ---- 目录上传（webkitdirectory）：进行中进度与结束后的成功/失败明细。 ----
  const dirInputRef = useRef<HTMLInputElement>(null)
  const [dirUpload, setDirUpload] = useState<{ done: number; total: number } | null>(null)
  const [dirResult, setDirResult] = useState<{
    root: string
    ok: number
    failures: Array<{ path: string; reason: string }>
  } | null>(null)

  // ---- 目录打包下载（download.zip）：进行中的条目 ID（按钮禁用/文案用）。 ----
  const [zipBusyId, setZipBusyId] = useState<string | null>(null)

  const [folderOpen, setFolderOpen] = useState(false)
  const [folderName, setFolderName] = useState('')
  const [folderError, setFolderError] = useState('')
  const [createKind, setCreateKind] = useState<'md' | 'textfile' | 'drawio' | 'whiteboard' | 'word' | 'spreadsheet' | 'presentation' | null>(null)
  const [createName, setCreateName] = useState('')
  const [createError, setCreateError] = useState('')

  // 查看弹窗：目标条目（文件 / 网页目录）；webPreviewUrl 为网页目录内嵌
  // iframe 的 raw_url；previewPathOverride 为外部（树）打开文件的命名空间
  // 路径段（弹窗内 by-path 路由用，见 routeFor）。
  const [previewTarget, setPreviewTarget] = useState<FileItem | null>(null)
  const [webPreviewUrl, setWebPreviewUrl] = useState<string | null>(null)
  const [previewPathOverride, setPreviewPathOverride] = useState<string[] | null>(null)

  const [uploads, setUploads] = useState<UploadRow[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const uploadKey = useRef(0)

  const currentParent = crumbs[crumbs.length - 1].id
  const currentFolderId = crumbs[crumbs.length - 1].folderId
  const visibleItems = directoryQuery.trim()
    ? items.filter((item) => item.name.toLocaleLowerCase().includes(directoryQuery.trim().toLocaleLowerCase()))
    : items

  const currentOpts = (): FileQueryOptions => ({
    tagId: tagFilter || null,
    starred: starredFilter === '' ? undefined : starredFilter === 'true',
    recent: recentView,
    sort: sortKey,
    order: sortOrder,
  })

  const refreshTags = () => {
    void listTags()
      .then(setTags)
      .catch(() => {})
  }

  useEffect(() => {
    refreshTags()
  }, [])

  const load = async (parentId: string | null) => {
    setLoading(true)
    setError('')
    setSelected(new Set())
    try {
      const opts = currentOpts()
      const { items: list, folderId } = await listItems(searchMode ? null : parentId, opts)
      // name 排序保持「目录优先 + 名称」的本地归并（服务端 name 为纯字典序）；
      // 其余排序键与最近访问视图（last_access_at 倒序）直接采用服务端顺序。
      setItems(sortKey === 'name' && !opts.recent ? sortItems(list) : list)
      if (folderId) {
        // 团队根目录：列表响应回填真实目录 ID，供上传/建目录使用。
        setCrumbs((prev) => prev.map((c, i) => (i === prev.length - 1 ? { ...c, folderId } : c)))
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('loadFailed'))
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 筛选/排序/视图变化时重新查询当前目录（跳过首挂载，避免与初始 load 重复）。
  const mounted = useRef(false)
  useEffect(() => {
    if (!mounted.current) {
      mounted.current = true
      return
    }
    void load(currentParent)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tagFilter, starredFilter, recentView, sortKey, sortOrder])

  useEffect(() => {
    if (reloadKey) void load(currentParent)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reloadKey])

  const openFolder = (item: FileItem) => {
    if (searchMode) return
    setCrumbs((prev) => [...prev, { id: item.id, folderId: item.id, name: item.name }])
    void load(item.id)
  }

  const gotoCrumb = (index: number) => {
    if (searchMode) return
    setCrumbs((prev) => prev.slice(0, index + 1))
    void load(crumbs[index].id)
  }

  const clearFilters = () => {
    setTagFilter('')
    setStarredFilter('')
    setRecentView(false)
  }

  // 表头排序：点击已激活键翻转方向；切换键时取该键默认方向
  //（名称升序；修改时间/大小默认降序 = 最新/最大优先）。
  const toggleSort = (key: 'name' | 'updated_at' | 'size') => {
    if (sortKey === key) {
      setSortOrder(sortOrder === 'asc' ? 'desc' : 'asc')
      return
    }
    setSortKey(key)
    setSortOrder(key === 'name' ? 'asc' : 'desc')
  }

  // 视图切换（全部 / 收藏 / 最近）：UI 已上移到 SpaceSwitcher，本组件经受控
  // prop activeView 驱动——外部值变化时同步内部筛选状态（收藏视图即
  // starred=true；最近视图走 ?recent=true，后端忽略其余过滤；其余清空），
  // 由此触发下方的筛选 effect 重新查询。
  useEffect(() => {
    if (!activeView) return
    setRecentView(activeView === 'recent')
    setTagFilter('')
    setStarredFilter(activeView === 'starred' ? 'true' : '')
  }, [activeView])

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const allSelected = items.length > 0 && items.every((item) => selected.has(item.id))
  const toggleSelectAll = () => {
    setSelected(allSelected ? new Set() : new Set(items.map((item) => item.id)))
  }

  const selectedIds = [...selected]
  const finishBatch = (results: BatchItemResult[]) => {
    setBatchNotice(describeBatchResults(results))
    setBatchError('')
    void load(currentParent)
  }

  // ---- 批量移动 ----

  // 候选目标：根（个人）、面包屑祖先、当前目录的子目录（排除被选中的目录自身）。
  const moveCandidates: { id: string; label: string }[] = []
  if (rootTargetLabel) moveCandidates.push({ id: '', label: rootTargetLabel })
  crumbs.slice(0, -1).forEach((c) => {
    if (c.folderId) moveCandidates.push({ id: c.folderId, label: c.name })
  })
  items.forEach((item) => {
    if (item.type === 'folder' && !selected.has(item.id)) moveCandidates.push({ id: item.id, label: `当前目录 / ${item.name}` })
  })
  const uniqueMoveCandidates = moveCandidates.filter((c, i) => moveCandidates.findIndex((x) => x.id === c.id) === i)

  const openMoveDialog = () => {
    setMoveTarget(uniqueMoveCandidates[0]?.id ?? '')
    setMoveManual('')
    setMoveError('')
    setMoveOpen(true)
  }

  const handleBatchMove = async (e: FormEvent) => {
    e.preventDefault()
    if (selectedIds.length === 0) return
    const manual = moveManual.trim()
    let target = moveTarget
    if (manual) {
      if (!/^[0-9a-fA-F-]{36}$/.test(manual)) {
        setMoveError('目标目录 UUID 格式不正确')
        return
      }
      target = manual
    }
    if (!target && !rootTargetLabel) {
      setMoveError('请选择或输入目标目录')
      return
    }
    setBatchBusy(true)
    setMoveError('')
    try {
      const results = await batchMoveFiles(selectedIds, target)
      setMoveOpen(false)
      finishBatch(results)
    } catch (err) {
      setMoveError(writeErrorText(err, '移动失败'))
    } finally {
      setBatchBusy(false)
    }
  }

  // 批量删除确认：antd Modal.confirm（走 App 上下文，明暗/accent 主题一致）。
  const handleBatchTrash = () => {
    if (selectedIds.length === 0) return
    antdModal.confirm({
      title: msg('delete'),
      content: formatMessage(msg('batchTrashConfirm'), { n: selectedIds.length }),
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setBatchBusy(true)
        setBatchNotice('')
        try {
          const results = await batchTrashFiles(selectedIds)
          finishBatch(results)
        } catch (err) {
          setBatchError(writeErrorText(err, msg('deleteFailed')))
        } finally {
          setBatchBusy(false)
        }
      },
    })
  }

  // ---- 批量下载（zip 流） ----

  const handleBatchDownload = async () => {
    if (selectedIds.length === 0) return
    setBatchBusy(true)
    setBatchError('')
    setBatchNotice('')
    try {
      await downloadBatchFiles(selectedIds)
    } catch (err) {
      setBatchError(err instanceof Error ? err.message : msg('downloadFailed'))
    } finally {
      setBatchBusy(false)
    }
  }

  // ---- 批量分享（逐个创建公开分享，完成后弹链接列表；文件与目录均可） ----

  const handleBatchShare = async () => {
    const targets = items.filter((it) => selected.has(it.id))
    if (targets.length === 0) {
      setBatchError(msg('batchShareEmpty'))
      return
    }
    setBatchShareBusy(true)
    setBatchError('')
    setBatchNotice('')
    const links: Array<{ name: string; url: string }> = []
    let failed = 0
    for (const f of targets) {
      try {
        const created = await createShare({ fileId: f.id, permission: 'download', visibility: 'public' })
        if (created.token) links.push({ name: f.name, url: `${window.location.origin}/s/${created.token}` })
        else failed++
      } catch {
        failed++
      }
    }
    setBatchShareBusy(false)
    if (failed > 0) setBatchError(formatMessage(msg('batchSharePartial'), { ok: links.length, fail: failed }))
    else setBatchNotice(formatMessage(msg('batchShareDone'), { n: links.length }))
    if (links.length > 0) {
      setShareLinks(links)
      setCopiedShareIdx(-1)
      setShareListOpen(true)
    }
  }

  const copyShareLink = async (url: string, index: number) => {
    try {
      await navigator.clipboard.writeText(url)
      setCopiedShareIdx(index)
    } catch {
      setBatchError(msg('clipboardCopyFailed'))
    }
  }

  // ---- 批量打标签（对每个选中项打同一已有标签） ----

  const openBatchTagDialog = () => {
    if (tags.length === 0) {
      setBatchError(msg('noTagsHint'))
      return
    }
    setBatchTagId(tags[0]?.id ?? '')
    setBatchTagError('')
    setBatchTagOpen(true)
  }

  const handleBatchTag = async (e: FormEvent) => {
    e.preventDefault()
    if (!batchTagId || selectedIds.length === 0) return
    setBatchTagBusy(true)
    setBatchTagError('')
    let ok = 0
    let fail = 0
    for (const id of selectedIds) {
      try {
        await addFileTag(id, batchTagId)
        ok++
      } catch {
        fail++
      }
    }
    setBatchTagBusy(false)
    if (fail > 0) {
      setBatchTagError(formatMessage(msg('batchTagPartial'), { ok, fail }))
      return
    }
    setBatchTagOpen(false)
    setBatchError('')
    setBatchNotice(formatMessage(msg('batchTagOk'), { n: ok }))
  }

  // ---- 批量收藏 / 取消收藏（循环 PATCH starred） ----

  const selectedItems = items.filter((it) => selected.has(it.id))
  const allSelectedStarred = selectedItems.length > 0 && selectedItems.every((it) => it.is_starred)

  const handleBatchStar = async () => {
    if (selectedIds.length === 0) return
    const target = !allSelectedStarred
    setBatchBusy(true)
    setBatchError('')
    setBatchNotice('')
    let fail = 0
    for (const id of selectedIds) {
      try {
        await setFileStarred(id, target)
      } catch {
        fail++
      }
    }
    setBatchBusy(false)
    if (fail > 0) {
      setBatchError(formatMessage(msg('batchStarFailed'), { fail }))
      return
    }
    setBatchNotice(formatMessage(target ? msg('batchStarOk') : msg('batchUnstarOk'), { n: selectedIds.length }))
    void load(currentParent)
  }

  // ---- 行内复制（目标目录 UUID 可选，留空复制到源目录） ----

  const openCopyDialog = (item: FileItem) => {
    setCopyTarget(item)
    setCopyParent('')
    setCopyError('')
  }

  const handleCopy = async (e: FormEvent) => {
    e.preventDefault()
    if (!copyTarget || !copyFn) return
    const manual = copyParent.trim()
    if (manual && !/^[0-9a-fA-F-]{36}$/.test(manual)) {
      setCopyError(msg('uuidInvalid'))
      return
    }
    const parent = manual || copyTarget.parent_id || ''
    if (!parent) {
      setCopyError(msg('uuidInvalid'))
      return
    }
    setCopyBusy(true)
    setCopyError('')
    try {
      await copyFn(copyTarget.id, parent)
      setCopyTarget(null)
      setCopyParent('')
      setBatchError('')
      setBatchNotice(msg('copyOk'))
      await load(currentParent)
    } catch (err) {
      setCopyError(writeErrorText(err, msg('fileCopyFailed')))
    } finally {
      setCopyBusy(false)
    }
  }

  // ---- 星标 ----

  const toggleStar = async (item: FileItem) => {
    setError('')
    try {
      const updated = await setFileStarred(item.id, !item.is_starred)
      setItems((prev) => prev.map((it) => (it.id === item.id ? { ...it, is_starred: updated.is_starred } : it)))
    } catch (err) {
      setError(err instanceof Error ? err.message : '收藏操作失败')
    }
  }

  // ---- 行内标签 ----

  const openTagModal = async (item: FileItem) => {
    setTagModalTarget(item)
    setTagModalNewName('')
    setTagModalError('')
    setTagModalFileTagIds(new Set())
    try {
      const attached = await listFileTags(item.id)
      setTagModalFileTagIds(new Set(attached.map((t) => t.id)))
    } catch (err) {
      setTagModalError(err instanceof Error ? err.message : '加载标签失败')
    }
  }

  const toggleFileTag = async (tag: Tag) => {
    if (!tagModalTarget) return
    const attached = tagModalFileTagIds.has(tag.id)
    setTagModalBusy(true)
    setTagModalError('')
    try {
      if (attached) {
        await removeFileTag(tagModalTarget.id, tag.id)
        setTagModalFileTagIds((prev) => {
          const next = new Set(prev)
          next.delete(tag.id)
          return next
        })
      } else {
        await addFileTag(tagModalTarget.id, tag.id)
        setTagModalFileTagIds((prev) => new Set(prev).add(tag.id))
      }
    } catch (err) {
      setTagModalError(writeErrorText(err, '标签操作失败'))
    } finally {
      setTagModalBusy(false)
    }
  }

  const handleCreateTagAndAttach = async (e: FormEvent) => {
    e.preventDefault()
    if (!tagModalTarget) return
    const name = tagModalNewName.trim()
    if (!name) return
    setTagModalBusy(true)
    setTagModalError('')
    try {
      const created = await createTag(name)
      setTags((prev) => [...prev, created].sort((a, b) => a.name.localeCompare(b.name)))
      await addFileTag(tagModalTarget.id, created.id)
      setTagModalFileTagIds((prev) => new Set(prev).add(created.id))
      setTagModalNewName('')
    } catch (err) {
      setTagModalError(err instanceof Error ? err.message : '创建标签失败')
    } finally {
      setTagModalBusy(false)
    }
  }

  // ---- 既有操作 ----

  const handleCreateFolder = async (e: FormEvent) => {
    e.preventDefault()
    if (!createFolderFn) return
    const name = folderName.trim()
    if (!name) return
    setFolderError('')
    try {
      await createFolderFn(name, currentFolderId)
      setFolderOpen(false)
      setFolderName('')
      await load(currentParent)
    } catch (err) {
      setFolderError(writeErrorText(err, '创建失败'))
    }
  }

  const handleDownload = async (item: FileItem) => {
    setError('')
    try {
      await doDownload(item)
    } catch (err) {
      setError(err instanceof Error ? err.message : '下载失败')
    }
  }

  // ---- 打开路由：by-path 优先（网页相对引用不漂移），检索模式回退 uuid ----

  // 命名空间内按路径构造 /view|edit/by-path URL：URL 携带完整目录路径，
  // html/js 内部相对引用（ajax、相对 src/href）按路径层级解析不漂移；
  // by-path 页按扩展名自动分发查看器/编辑器。跨目录检索模式（面包屑
  // 不代表条目位置）或 ns 缺失时回退 UUID 路由；外部（树）打开的文件
  // 携带 previewPathOverride 时按其路径构建。
  const routeFor = (prefix: 'view' | 'edit', item: FileItem) => {
    if (!ns || searchMode) return `/${prefix}/${item.id}`
    const segs = (
      item.id === previewTarget?.id && previewPathOverride
        ? previewPathOverride
        : [...pathSegmentsOf(crumbs), item.name]
    ).map(encodeURIComponent)
    return `/${prefix}/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${segs.join('/')}/`
  }

  // ---- 目录「作为网页打开」（resolve 目录下 index.html → raw_url / 新窗口） ----

  /** 目录在命名空间内的路径段（面包屑拼当前目录路径，含团队空间）；检索模式返回 null。 */
  const folderSegmentsOf = (item: FileItem): string[] | null => {
    if (!ns || searchMode) return null
    return [...pathSegmentsOf(crumbs), item.name]
  }

  /** 解析网页目录入口（index.html，回退 index.htm）的 raw_url；不存在返回 null。 */
  const resolveWebFolderUrl = async (segments: string[]): Promise<string | null> => {
    if (!ns) return null
    try {
      return (await resolvePath(ns.type, ns.scope, [...segments, 'index.html'].join('/'), { mode: 'view' })).raw_url
    } catch {
      // index.html 不存在：尝试 index.htm。
      try {
        return (await resolvePath(ns.type, ns.scope, [...segments, 'index.htm'].join('/'), { mode: 'view' })).raw_url
      } catch {
        return null
      }
    }
  }

  /** 菜单「作为网页打开（新窗口）」：resolve 校验入口存在后打开独立查看页。 */
  const openAsWebsite = async (item: FileItem) => {
    const segments = folderSegmentsOf(item)
    if (!ns || !segments) return
    setError('')
    const url = await resolveWebFolderUrl(segments)
    if (!url) {
      setError('该目录没有 index.html')
      return
    }
    const encoded = encodePathSegments(segments)
    openEditorWindow(`/view/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${encoded}/`)
  }

  /**
   * 网页目录弹窗内嵌查看（默认点击行为）：resolve 目录入口 raw_url 后
   * 复用 previewTarget 机制弹窗，内容渲染 sandbox iframe（菜单保留
   * 「作为网页打开（新窗口）」入口）。
   */
  const openWebFolderPreview = async (item: FileItem) => {
    const segments = folderSegmentsOf(item)
    if (!segments) return
    setError('')
    const url = await resolveWebFolderUrl(segments)
    if (!url) {
      setError('该目录没有 index.html')
      return
    }
    setPreviewTarget(item)
    setPreviewPathOverride(null)
    setWebPreviewUrl(url)
  }

  // ---- zip 解包为目录树（父目录下以 zip 名建目录，部分成功语义） ----

  const handleUnpack = async (item: FileItem) => {
    if (unpackBusyId) return
    setUnpackBusyId(item.id)
    setBatchNotice(`正在解包「${item.name}」…`)
    setBatchError('')
    try {
      const r = await unpackZip(item.id)
      const summary = `新建 ${r.created_folders} 个目录、${r.created_files} 个文件${r.skipped > 0 ? `，同名跳过 ${r.skipped} 项` : ''}`
      if (r.failures && r.failures.length > 0) {
        const detail = r.failures.map((f) => `${f.path}：${f.error}`).join('；')
        setBatchNotice('')
        setBatchError(`解包完成（${summary}），但 ${r.failures.length} 项失败：${detail}`)
      } else {
        setBatchError('')
        setBatchNotice(`解包完成：${summary}。`)
      }
      await load(currentParent)
    } catch (err) {
      setBatchNotice('')
      setBatchError(writeErrorText(err, '解包失败'))
    } finally {
      setUnpackBusyId(null)
    }
  }

  // 弹窗查看：内容渲染由 FileViewerDispatch 就地完成（无预加载逻辑）。
  const openPreview = (item: FileItem) => {
    setPreviewTarget(item)
    setPreviewPathOverride(null)
    setWebPreviewUrl(null)
  }

  const closePreview = () => {
    setPreviewTarget(null)
    setPreviewPathOverride(null)
    setWebPreviewUrl(null)
  }

  // 外部「打开文件」受控信号（左侧目录树文件节点点击）：当前列表命中直接
  // 弹窗；未命中（不在当前目录）经 fileMetaFn/getFileMeta 拉取元数据后弹窗，
  // pathSegments 一并记录供弹窗内 by-path 路由使用。
  useEffect(() => {
    if (!fileOpenSignal || fileOpenSignal.seq <= 0) return
    const { fileId, pathSegments } = fileOpenSignal
    const found = items.find((it) => it.id === fileId && it.type === 'file')
    if (found) {
      setPreviewTarget(found)
      setPreviewPathOverride(null)
      setWebPreviewUrl(null)
      return
    }
    const meta = fileMetaFn ?? ((id: string) => getFileMeta(id))
    void meta(fileId)
      .then((m) => {
        if (!m) return
        setPreviewTarget(m)
        setPreviewPathOverride(pathSegments ?? null)
        setWebPreviewUrl(null)
      })
      .catch(() => {
        /* 元数据拉取失败（无权限/已删除）：静默 */
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileOpenSignal])

  const handleFilesPicked = async (files: FileList | null) => {
    if (!uploadFn || !files || files.length === 0) return
    for (const file of Array.from(files)) {
      const key = ++uploadKey.current
      setUploads((prev) => [...prev, { key, name: file.name, phase: 'creating' }])
      try {
        await uploadFn(file, currentFolderId, (phase) => {
          setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase } : r)))
        })
      } catch (err) {
        setUploads((prev) =>
          prev.map((r) =>
            r.key === key ? { ...r, phase: 'error', error: writeErrorText(err, '上传失败') } : r,
          ),
        )
      }
    }
    if (fileInputRef.current) fileInputRef.current.value = ''
    await load(currentParent)
  }

  const [docCreating, setDocCreating] = useState(false)

  // 新建项规格：md（Markdown/富文本）、textfile（文本文件合一：TXT/代码/
  // HTML 由用户填写的扩展名决定）、drawio、白板与 office 模板；
  // XMind 已移除（前端仅解析查看、不支持编辑，空白创建价值低）。
  const createSpec = createKind ? {
    md: { label: 'Markdown / 富文本', ext: '.md', content: '# 新文档\n', mime: 'text/markdown', route: 'markdown' },
    // 「文本文件」：扩展名由用户自带（默认建议 untitled.txt），MIME 与
    // 创建后路由按扩展名推断（见 TEXT_FILE_MIME / textFileRoute）。
    textfile: { label: '文本文件', ext: '', content: '', mime: '', route: '' },
    drawio: { label: 'draw.io', ext: '.drawio', content: EMPTY_DRAWIO_XML, mime: 'text/xml', route: 'drawio' },
    whiteboard: { label: '白板', ext: '.excalidraw', content: EMPTY_EXCALIDRAW_JSON, mime: 'application/json', route: 'excalidraw' },
    word: { label: 'Word', ext: '.docx', content: '', mime: '', route: 'edit' },
    spreadsheet: { label: 'Excel', ext: '.xlsx', content: '', mime: '', route: 'edit' },
    presentation: { label: 'PPT', ext: '.pptx', content: '', mime: '', route: 'edit' },
  }[createKind] : null

  const beginNamedCreate = (kind: NonNullable<typeof createKind>) => {
    const defaults: Record<NonNullable<typeof createKind>, string> = {
      md: '新文档.md', textfile: 'untitled.txt', drawio: '新图表.drawio',
      whiteboard: '新白板.excalidraw', word: '新文档.docx',
      spreadsheet: '新表格.xlsx', presentation: '新演示文稿.pptx',
    }
    setCreateKind(kind)
    setCreateName(defaults[kind])
    setCreateError('')
    setCreateMenuOpen(false)
  }

  const handleNamedCreate = async (e: FormEvent) => {
    e.preventDefault()
    if (!createKind || !createSpec || docCreating) return
    let name = createName.trim()
    if (!name) return
    if (createKind === 'textfile') {
      // 文本文件：必须自带扩展名（弹框占位符已提示支持类型）。
      if (!/\.[A-Za-z0-9]{1,8}$/.test(name)) {
        setCreateError('请填写包含扩展名的文件名（如 untitled.txt、index.html）')
        return
      }
    } else if (!name.toLowerCase().endsWith(createSpec.ext)) {
      name += createSpec.ext
    }
    if (/[/\\:*?"<>|]/.test(name)) {
      setCreateError('文件名不能包含 / \\ : * ? " < > |')
      return
    }
    // 文本文件按扩展名推断 MIME 与创建后打开的编辑器路由。
    const ext = name.slice(name.lastIndexOf('.') + 1).toLowerCase()
    const fileMime = createKind === 'textfile' ? (TEXT_FILE_MIME[ext] ?? 'text/plain') : createSpec.mime
    const openRoute = createKind === 'textfile' ? textFileRoute(name) : createSpec.route
    const child = window.open('about:blank', '_blank')
    setDocCreating(true)
    setCreateError('')
    const key = ++uploadKey.current
    setUploads((prev) => [...prev, { key, name, phase: 'creating' }])
    try {
      let fileID = ''
      if (createKind === 'word' || createKind === 'spreadsheet' || createKind === 'presentation') {
        const created = await createOfficeTemplate(createKind, currentFolderId, name)
        fileID = created.id
      } else if (uploadFn) {
        const created = await uploadFn(new File([createSpec.content], name, { type: fileMime }), currentFolderId, (phase) => {
          setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase } : r)))
        })
        fileID = typeof created === 'object' && created !== null && 'file_id' in created
          ? String((created as { file_id?: string }).file_id ?? '') : ''
        // 全零 UUID（uuid.Nil）视为无目标文件，避免打开 /text/00000000-…。
        if (fileID === '00000000-0000-0000-0000-000000000000') fileID = ''
      }
      await load(currentParent)
      setCreateKind(null)
      if (fileID && child) child.location.href = `/${openRoute}/${fileID}`
      else child?.close()
    } catch (err) {
      child?.close()
      setCreateError(writeErrorText(err, '新建失败'))
      setUploads((prev) => prev.map((r) => r.key === key ? { ...r, phase: 'error', error: writeErrorText(err, '新建失败') } : r))
    } finally {
      setDocCreating(false)
    }
  }

  // 页面快捷键（v1.1）：n 新建文件夹 / u 上传 / Delete 删除选中 /
  // Escape 依次关弹窗（预览→标签→移动→新建文件夹），无弹窗时清空选择。
  // v1.2（设计 6.16.1）：Ctrl/Cmd+1 列表视图、Ctrl/Cmd+2 网格视图
  // （经 useHotkeys 的 'mod+' 白名单注册）。
  useHotkeys({
    'mod+1': () => changeViewMode('list'),
    'mod+2': () => changeViewMode('grid'),
    n: () => {
      if (createFolderFn && !searchMode) {
        setFolderOpen(true)
        setFolderName('')
        setFolderError('')
      }
    },
    u: () => {
      if (uploadFn && !searchMode) fileInputRef.current?.click()
    },
    Delete: () => {
      if (selected.size > 0 && !batchBusy) void handleBatchTrash()
    },
    Escape: () => {
      if (ctxMenu) {
        setCtxMenu(null)
        return
      }
      if (openWithMgrOpen) {
        setOpenWithMgrOpen(false)
        return
      }
      if (createMenuOpen) {
        setCreateMenuOpen(false)
        return
      }
      if (cardMenuFor) {
        setCardMenuFor(null)
        return
      }
      if (previewTarget) {
        closePreview()
        return
      }
      if (shareListOpen) {
        setShareListOpen(false)
        return
      }
      if (batchTagOpen) {
        setBatchTagOpen(false)
        return
      }
      if (copyTarget) {
        setCopyTarget(null)
        return
      }
      if (tagModalTarget) {
        setTagModalTarget(null)
        return
      }
      if (moveOpen) {
        setMoveOpen(false)
        return
      }
      if (folderOpen) {
        setFolderOpen(false)
        return
      }
      if (selected.size > 0) setSelected(new Set())
    },
  })

  // 集成编辑/查看页统一在新窗口打开（独立窗口便于与文件列表并行操作，
  // 编辑器自身带「返回」：window.open 打开的窗口可直接关闭）。
  const openEditorWindow = (path: string) => {
    const url = new URL(path, window.location.origin)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  // ---- 默认打开方式：偏好（按 ext）优先，未配置按文件类型自动选择。 ----

  const openFileWith = (item: FileItem, opener: OpenWithOpener) => {
    // by-path 优先：编辑类 opener 统一 /edit/by-path（按扩展名分发编辑器），
    // 其余走查看；office/drawio 集成未启用时回落查看。
    const wantsEdit = opener !== 'default' && opener !== 'web'
    const prefix = wantsEdit && !(opener === 'office' && !ooEnabled) && !(opener === 'drawio' && !drawioEnabled) ? 'edit' : 'view'
    openEditorWindow(routeFor(prefix, item))
  }
  /** 「打开方式」选择：PUT 保存为默认（即时生效，失败提示但不阻断）并以该方式打开。 */
  const handleOpenWithChoice = async (item: FileItem, opener: OpenWithOpener) => {
    const ext = extOf(item.name)
    if (ext) {
      try {
        await setOpenWith(ext, opener)
        setOpenWithMap((prev) => ({ ...prev, [ext]: opener }))
      } catch (err) {
        setError(err instanceof Error ? err.message : '保存默认打开方式失败')
      }
    }
    openFileWith(item, opener)
  }

  /** 管理弹窗内修改某扩展名的默认打开器。 */
  const handleMgrChange = async (ext: string, opener: OpenWithOpener) => {
    setOpenWithMgrError('')
    try {
      await setOpenWith(ext, opener)
      setOpenWithMap((prev) => ({ ...prev, [ext]: opener }))
    } catch (err) {
      setOpenWithMgrError(err instanceof Error ? err.message : '保存失败')
    }
  }

  /** 管理弹窗内删除某扩展名偏好（恢复按文件类型自动选择）。 */
  const handleMgrDelete = async (ext: string) => {
    setOpenWithMgrError('')
    try {
      await deleteOpenWith(ext)
      setOpenWithMap((prev) => {
        const next = { ...prev }
        delete next[ext]
        return next
      })
    } catch (err) {
      setOpenWithMgrError(err instanceof Error ? err.message : '删除失败')
    }
  }

  /** 管理弹窗内新增偏好（扩展名 + 打开方式；后端规范化为小写去点）。 */
  const handleMgrAdd = async (e: FormEvent) => {
    e.preventDefault()
    const raw = mgrNewExt.trim()
    if (!raw || mgrAdding) return
    // 与后端 NormalizeOpenWithExt 一致：去点小写后 1..16 位 [a-z0-9]。
    const ext = raw.toLowerCase().replace(/^\.+/, '')
    if (!/^[a-z0-9]{1,16}$/.test(ext)) {
      setOpenWithMgrError(locale === 'zh-CN' ? '扩展名须为 1..16 位字母/数字（可带前导点）' : 'Extension must be 1..16 letters/digits')
      return
    }
    setMgrAdding(true)
    setOpenWithMgrError('')
    try {
      await setOpenWith(ext, mgrNewOpener)
      setOpenWithMap((prev) => ({ ...prev, [ext]: mgrNewOpener }))
      setMgrNewExt('')
    } catch (err) {
      setOpenWithMgrError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setMgrAdding(false)
    }
  }

  // ---- 目录打包下载 ZIP（download.zip 流式归档，认证 fetch 转 blob 保存）。 ----

  const handleZipDownload = async (item: FileItem) => {
    if (zipBusyId) return
    setZipBusyId(item.id)
    setError('')
    try {
      await downloadFolderZip(item.id, item.name)
    } catch (err) {
      setError(err instanceof Error ? err.message : '目录打包下载失败')
    } finally {
      setZipBusyId(null)
    }
  }

  // ---- 目录上传：按 webkitRelativePath 重建目录树，目录先于子项创建。 ----

  const handleDirPicked = async (files: FileList | null) => {
    if (!uploadFn || !createFolderFn || dirUpload || !files || files.length === 0) return
    if (dirInputRef.current) dirInputRef.current.value = ''
    const list = Array.from(files)
    const relOf = (f: File) => (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
    const firstRel = relOf(list[0])
    // 目录上传的目标根 = 当前目录下与所选目录同名的首段文件夹；无目录段时直接入当前目录。
    const rootName = firstRel.includes('/') ? firstRel.split('/')[0] : '目录'
    const failures: Array<{ path: string; reason: string }> = []
    // 目录相对路径 → 已建/既有目录 ID（'' = 当前目录；递归确保父目录先创建）。
    const dirIds = new Map<string, string>()
    const rootId = currentFolderId ?? ''
    dirIds.set('', rootId)
    const ensureDir = async (path: string): Promise<string> => {
      const known = dirIds.get(path)
      if (known !== undefined) return known
      const idx = path.lastIndexOf('/')
      const parentPath = idx >= 0 ? path.slice(0, idx) : ''
      const name = idx >= 0 ? path.slice(idx + 1) : path
      const parentId = await ensureDir(parentPath)
      let id = ''
      try {
        const created = await createFolderFn(name, parentId || null)
        id = (created as { id?: string } | null)?.id ?? ''
      } catch (err) {
        // 已存在（409 name conflict）视为成功：列出父目录定位同名目录。
        if (err instanceof ApiError && err.status === 409) {
          try {
            const { items: siblings } = await listItems(parentId || null)
            id = siblings.find((it) => it.type === 'folder' && it.name === name)?.id ?? ''
          } catch {
            /* 定位失败走下方抛出 */
          }
        }
        if (!id) throw err
      }
      dirIds.set(path, id)
      return id
    }

    setDirResult(null)
    setDirUpload({ done: 0, total: list.length })
    let ok = 0
    for (let i = 0; i < list.length; i++) {
      const file = list[i]
      const rel = relOf(file)
      const segments = rel.split('/').filter(Boolean)
      const dirPath = segments.slice(0, -1).join('/')
      try {
        const parentId = dirPath ? await ensureDir(dirPath) : rootId
        await uploadFn(file, parentId || null, () => {})
        ok++
      } catch (err) {
        failures.push({ path: rel, reason: writeErrorText(err, '上传失败') })
      }
      setDirUpload({ done: i + 1, total: list.length })
    }
    setDirUpload(null)
    setDirResult({ root: rootName, ok, failures })
    await load(currentParent)
  }

  // 默认点击：文件 → 弹窗查看；网页目录（has_index_web）→ 弹窗内嵌 iframe
  //（菜单保留「作为网页打开（新窗口）」）；普通目录 → 进入。
  const openItem = (item: FileItem) => {
    if (item.type === 'folder') {
      if (item.has_index_web && ns && !searchMode) void openWebFolderPreview(item)
      else if (!searchMode) openFolder(item)
      return
    }
    openPreview(item)
  }

  /**
   * 查看弹窗标题行操作区（与标题同行，左标题右操作）：
   * - 网页目录：「作为网页打开（新窗口）」；
   * - 文件：「新窗口查看」拆分按钮（主点击=默认查看 by-path；下拉可选
   *   查看 Office / 查看网页 / 查看文本 / 下载，按文件类型显示适用项）
   *   +「编辑」拆分按钮（主点击=默认编辑；下拉可选 编辑 Office / 编辑文本 /
   *   图表编辑 / 白板编辑，无适用编辑方式的类型不渲染）。
   */
  const previewHeadExtra = (item: FileItem) => {
    if (item.type === 'folder') {
      return (
        <Button
          size="small"
          title={locale === 'zh-CN' ? '在独立窗口打开该静态网站' : 'Open this site in a new window'}
          onClick={() => void openAsWebsite(item)}
        >
          {locale === 'zh-CN' ? '作为网页打开（新窗口）' : 'Open as website'}
        </Button>
      )
    }
    const lower = item.name.toLowerCase()
    const isOffice = isOfficeFile(lower)
    const isHtml = isHtmlFile(lower)
    const isTxtLike = isTextEditable(lower) || isCodeFile(lower)
    const isDrawio = isDrawioFile(lower)
    const isBoard = isExcalidrawFile(lower)
    const viewOptions: Array<{ label: string; run: () => void }> = []
    if (isOffice && ooEnabled) viewOptions.push({ label: locale === 'zh-CN' ? '查看 Office' : 'View Office', run: () => openEditorWindow(routeFor('view', item)) })
    if (isHtml) viewOptions.push({ label: locale === 'zh-CN' ? '查看网页' : 'View web', run: () => openEditorWindow(routeFor('view', item)) })
    // 查看文本走 /view 只读分发（by-path），绝不进 /text 编辑页（查看=纯渲染）。
    if (isTxtLike) viewOptions.push({ label: locale === 'zh-CN' ? '查看文本' : 'View text', run: () => openEditorWindow(routeFor('view', item)) })
    viewOptions.push({ label: msg('download'), run: () => void handleDownload(item) })
    const editOptions: Array<{ label: string; run: () => void }> = []
    if (isOffice && ooEnabled) editOptions.push({ label: locale === 'zh-CN' ? '编辑 Office' : 'Edit Office', run: () => openEditorWindow(routeFor('edit', item)) })
    if (isTxtLike) editOptions.push({ label: locale === 'zh-CN' ? '编辑文本' : 'Edit text', run: () => openEditorWindow(routeFor('edit', item)) })
    if (isDrawio && drawioEnabled) editOptions.push({ label: locale === 'zh-CN' ? '图表编辑' : 'Edit diagram', run: () => openEditorWindow(routeFor('edit', item)) })
    if (isBoard) editOptions.push({ label: locale === 'zh-CN' ? '白板编辑' : 'Edit whiteboard', run: () => openEditorWindow(routeFor('edit', item)) })
    // 默认编辑路由：office/drawio 集成未启用时回落只读查看（与 openFileWith 一致）。
    const editFallbackView = (isOffice && !ooEnabled) || (isDrawio && !drawioEnabled)
    const menuOf = (options: Array<{ label: string; run: () => void }>): MenuProps => ({
      items: options.map((op) => ({ key: op.label, label: op.label })),
      onClick: ({ key }) => options.find((op) => op.label === key)?.run(),
    })
    return (
      <>
        <Dropdown.Button
          size="small"
          menu={menuOf(viewOptions)}
          onClick={() => openEditorWindow(routeFor('view', item))}
        >
          {locale === 'zh-CN' ? '新窗口查看' : 'View in new window'}
        </Dropdown.Button>
        {editOptions.length > 0 && (
          <Dropdown.Button
            size="small"
            menu={menuOf(editOptions)}
            onClick={() => openEditorWindow(routeFor(editFallbackView ? 'view' : 'edit', item))}
          >
            {locale === 'zh-CN' ? '编辑' : 'Edit'}
          </Dropdown.Button>
        )}
      </>
    )
  }

  // 条目操作菜单（列表行「⋯」、右键菜单、网格卡片菜单共用，antd Menu）：
  // 打开（按默认打开方式）/ 打开方式分组 / 目录「下载为 ZIP」/ 打开方式
  // 管理入口 + 既有「查看 / 编辑 / 预览 / 下载 / 标签 / 复制」与调用方
  // rowActions（作为菜单项 label 内嵌，点击行为由调用方按钮自带）。
  const itemMenuItems = (item: FileItem): MenuProps['items'] => {
    const items: NonNullable<MenuProps['items']> = []
    if (item.type === 'folder') {
      items.push({
        key: 'open',
        label: item.has_index_web ? (locale === 'zh-CN' ? '目录浏览' : 'Browse folder') : (locale === 'zh-CN' ? '进入' : 'Open'),
      })
      items.push({
        key: 'zip',
        disabled: zipBusyId !== null,
        label: zipBusyId === item.id
          ? (locale === 'zh-CN' ? '打包中…' : 'Zipping…')
          : (locale === 'zh-CN' ? '下载为 ZIP' : 'Download as ZIP'),
      })
    }
    if (item.type === 'file') {
      items.push({ key: 'preview', label: locale === 'zh-CN' ? '查看' : 'View' })
      items.push({ key: 'view-new', label: locale === 'zh-CN' ? '新窗口查看' : 'View in new window' })
    }
    if (item.type === 'file' && openWithOptions(item.name).some((op) => op !== 'default' && op !== 'web')) {
      items.push({
        type: 'group',
        label: locale === 'zh-CN' ? '打开方式（选择后设为默认）' : 'Open with (sets default)',
        children: [
          ...openWithOptions(item.name).map((op) => ({
            key: `openwith:${op}`,
            label: (
              <>
                <span className={`openwith-dot${resolveOpener(item.name, openWith) === op ? ' on' : ''}`} aria-hidden="true" />
                {openerLabel(op, locale === 'zh-CN')}
              </>
            ),
          })),
          { key: 'openwith-mgr', label: locale === 'zh-CN' ? '管理默认打开方式…' : 'Manage defaults…' },
        ],
      })
    }
    if (item.type === 'folder' && ns && !searchMode) {
      items.push({ key: 'open-web', label: locale === 'zh-CN' ? '作为网页打开' : 'Open as website' })
    }
    if (item.type === 'file' && item.name.toLowerCase().endsWith('.zip')) {
      items.push({
        key: 'unpack',
        disabled: unpackBusyId !== null,
        label: unpackBusyId === item.id
          ? (locale === 'zh-CN' ? '解包中…' : 'Unpacking…')
          : (locale === 'zh-CN' ? '解包为目录' : 'Unpack to folder'),
      })
    }
    if (item.type === 'file' && ooEnabled && isOfficeFile(item.name)) {
      items.push({ key: 'edit-office', label: locale === 'zh-CN' ? '编辑 Office' : 'Edit Office' })
    }
    if (item.type === 'file' && drawioEnabled && isDrawioFile(item.name)) {
      items.push({ key: 'edit-drawio', label: locale === 'zh-CN' ? '图表编辑' : 'Edit diagram' })
      items.push({ key: 'view-drawio', label: locale === 'zh-CN' ? '图表查看' : 'View diagram' })
    }
    if (item.type === 'file' && isTextEditable(item.name)) {
      items.push({ key: 'edit-text', label: locale === 'zh-CN' ? '编辑文本' : 'Edit text' })
    }
    if (item.type === 'file' && isExcalidrawFile(item.name)) {
      items.push({ key: 'edit-board', label: locale === 'zh-CN' ? '白板编辑' : 'Edit whiteboard' })
      items.push({ key: 'view-board', label: locale === 'zh-CN' ? '白板查看' : 'View whiteboard' })
    }
    if (item.type === 'file') {
      items.push({ key: 'download', label: msg('download') })
    }
    items.push({ key: 'tag', label: msg('tag') })
    if (item.type === 'file' && copyFn) {
      items.push({ key: 'copy', label: msg('copy') })
    }
    const extra = rowActions?.(item)
    if (extra) items.push({ key: 'row-actions', label: extra })
    return items
  }

  /** 菜单项点击分发（key 见 itemMenuItems）。 */
  const runItemMenuAction = (item: FileItem, key: string) => {
    switch (key) {
      case 'open':
        openFolder(item)
        return
      case 'zip':
        void handleZipDownload(item)
        return
      case 'preview':
        openPreview(item)
        return
      case 'view-new':
        openEditorWindow(routeFor('view', item))
        return
      case 'open-web':
        void openAsWebsite(item)
        return
      case 'unpack':
        void handleUnpack(item)
        return
      case 'download':
        void handleDownload(item)
        return
      case 'tag':
        void openTagModal(item)
        return
      case 'copy':
        openCopyDialog(item)
        return
      case 'openwith-mgr':
        setOpenWithMgrOpen(true)
        return
      case 'edit-office':
      case 'edit-drawio':
      case 'edit-board':
      case 'edit-text':
        openEditorWindow(routeFor('edit', item))
        return
      case 'view-drawio':
      case 'view-board':
        openEditorWindow(routeFor('view', item))
        return
      default:
        if (key.startsWith('openwith:')) void handleOpenWithChoice(item, key.slice('openwith:'.length) as OpenWithOpener)
    }
  }

  /** 渲染条目操作菜单（antd Menu，透明背景由 .ctx-antd-menu 适配）。 */
  const renderItemMenu = (item: FileItem) => (
    <Menu
      className="ctx-antd-menu"
      mode="vertical"
      selectable={false}
      items={itemMenuItems(item)}
      onClick={({ key }) => runItemMenuAction(item, key)}
    />
  )

  return (
    <div className="file-browser">
      {/* 顶部工具带（SpaceSwitcher 行下方合并为一行，省两行）：视图切换 /
          ＋新建 / ⬆上传（拆分按钮）/ 目录搜索 / 标签、收藏过滤（网格视图
          含排序）/ 默认打开方式入口。页面级大标题已移除，仅保留面包屑行。 */}
      <div className="files-toolbar">
          {/* 列表/网格切换合一：单按钮按当前模式显示对侧图标（title 提示目标模式）。 */}
          <Button
            type="text"
            className="view-toggle-btn"
            title={viewMode === 'list' ? msg('viewModeGrid') : msg('viewModeList')}
            aria-label={viewMode === 'list' ? msg('viewModeGrid') : msg('viewModeList')}
            aria-pressed={viewMode === 'grid'}
            onClick={() => changeViewMode(viewMode === 'list' ? 'grid' : 'list')}
          >
            {viewMode === 'list'
              ? <LayoutGrid size={16} strokeWidth={2} aria-hidden="true" />
              : <List size={16} strokeWidth={2} aria-hidden="true" />}
          </Button>
          {(createFolderFn || uploadFn) && !searchMode && (
            <Dropdown
              trigger={['click']}
              open={createMenuOpen}
              onOpenChange={setCreateMenuOpen}
              menu={{
                items: [
                  ...(createFolderFn
                    ? [{ key: 'folder', label: locale === 'zh-CN' ? '文件夹' : 'Folder' }]
                    : []),
                  ...uploadFn
                    ? (['md', 'textfile', 'drawio', 'whiteboard', 'word', 'spreadsheet', 'presentation'] as const).map((kind) => ({
                        key: kind,
                        disabled: docCreating,
                        label: { md: 'Markdown / 富文本', textfile: '文本文件（.txt/.html/.js…）', drawio: 'draw.io', whiteboard: '白板', word: 'Word', spreadsheet: 'Excel', presentation: 'PPT' }[kind],
                      }))
                    : [],
                  { type: 'group' as const, label: 'PDF 仅支持上传和查看，不能空白创建' },
                ],
                onClick: ({ key }) => {
                  if (key === 'folder') {
                    setFolderOpen(true)
                    setFolderName('')
                    setFolderError('')
                  } else {
                    beginNamedCreate(key as NonNullable<typeof createKind>)
                  }
                },
              }}
            >
              <Button icon={<Plus size={14} strokeWidth={2} aria-hidden="true" />}>
                {locale === 'zh-CN' ? '新建' : 'New'}
              </Button>
            </Dropdown>
          )}
          {/* 上传拆分按钮（antd Dropdown.Button）：主点击=上传文件；箭头下拉
              含「上传目录」（目录上传依赖建目录权限）。 */}
          {uploadFn && !searchMode && (
            <div className="create-menu-wrap">
              <Dropdown.Button
                type="primary"
                disabled={dirUpload !== null}
                menu={{
                  items: [
                    { key: 'files', label: locale === 'zh-CN' ? '上传文件' : 'Upload files' },
                    ...(createFolderFn
                      ? [{ key: 'folder', label: locale === 'zh-CN' ? '上传目录' : 'Upload folder', disabled: dirUpload !== null }]
                      : []),
                  ],
                  onClick: ({ key }) => (key === 'files' ? fileInputRef.current?.click() : dirInputRef.current?.click()),
                }}
                onClick={() => fileInputRef.current?.click()}
              >
                <Upload size={14} strokeWidth={2} aria-hidden="true" />{' '}
                {dirUpload !== null ? (locale === 'zh-CN' ? '上传中…' : 'Uploading…') : locale === 'zh-CN' ? '上传' : 'Upload'}
              </Dropdown.Button>
              <input
                ref={fileInputRef}
                type="file"
                multiple
                hidden
                onChange={(e) => void handleFilesPicked(e.target.files)}
              />
              {createFolderFn && (
                <input
                  ref={dirInputRef}
                  type="file"
                  multiple
                  hidden
                  onChange={(e) => void handleDirPicked(e.target.files)}
                  {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
                />
              )}
            </div>
          )}
        {/* 过滤区：目录内搜索（antd Input allowClear）+ 标签 / 收藏 antd Select
            （排序已表头化，网格视图无表头、保留一个精简排序下拉；全部/收藏/最近
            视图切换在 SpaceSwitcher 行）。 */}
        <span className="toolbar-spacer" />
        <label className="filter-item directory-search">
          <Input
            allowClear
            value={directoryQuery}
            placeholder={locale === 'zh-CN' ? '搜索当前目录…' : 'Search this folder…'}
            onChange={(event) => setDirectoryQuery(event.target.value)}
          />
        </label>
        <label className="filter-item">
          <span>{msg('tag')}</span>
          <Select
            className="filter-select"
            value={tagFilter}
            onChange={(v) => { setTagFilter(v); setRecentView(false) }}
            options={[{ value: '', label: msg('all') }, ...tags.map((tg) => ({ value: tg.id, label: `#${tg.name}` }))]}
          />
        </label>
        <label className="filter-item">
          <span>{msg('viewStarred')}</span>
          <Select
            className="filter-select"
            value={starredFilter}
            onChange={(v) => { setStarredFilter(v); setRecentView(false) }}
            options={[
              { value: '', label: msg('all') },
              { value: 'true', label: msg('starredOnly') },
              { value: 'false', label: msg('starredNo') },
            ]}
          />
        </label>
        {viewMode === 'grid' && (
          <label className="filter-item">
            <span>{msg('sort')}</span>
            <Select
              className="filter-select"
              value={`${sortKey}:${sortOrder}`}
              onChange={(v) => {
                const [key, order] = v.split(':')
                setSortKey(key as 'name' | 'updated_at' | 'size')
                setSortOrder(order as 'asc' | 'desc')
              }}
              options={[
                { value: 'name:asc', label: `${msg('sortOrderName')} ↑` },
                { value: 'name:desc', label: `${msg('sortOrderName')} ↓` },
                { value: 'updated_at:desc', label: `${msg('sortOrderUpdated')} ↓` },
                { value: 'updated_at:asc', label: `${msg('sortOrderUpdated')} ↑` },
                { value: 'size:desc', label: `${msg('sortOrderSize')} ↓` },
                { value: 'size:asc', label: `${msg('sortOrderSize')} ↑` },
              ]}
            />
          </label>
        )}
        {searchMode && (
          <Button type="text" size="small" onClick={clearFilters}>{msg('clearFilters')}</Button>
        )}
        {/* 默认打开方式管理入口：与右键菜单「打开方式 → 管理默认打开方式…」
            打开同一弹窗；工具带常驻提升可发现性（#21）。 */}
        <Button
          type="text"
          size="small"
          title={locale === 'zh-CN' ? '管理各扩展名的默认打开方式' : 'Manage default openers'}
          onClick={() => setOpenWithMgrOpen(true)}
        >
          <Settings size={14} strokeWidth={2} aria-hidden="true" /> {locale === 'zh-CN' ? '默认打开方式' : 'Openers'}
        </Button>
      </div>

      {!searchMode && (
        <nav className="breadcrumb">
          {crumbs.map((crumb, index) => (
            <span key={crumb.id ?? 'root'} className="crumb">
              {index > 0 && <span className="sep">/</span>}
              <button
                className={index === crumbs.length - 1 ? 'current' : ''}
                onClick={() => gotoCrumb(index)}
              >
                {crumb.name}
              </button>
            </span>
          ))}
        </nav>
      )}
      {searchMode && (
        <div className="hint search-mode-hint">
          {recentView ? msg('recentHint') : msg('searchModeHint')}
        </div>
      )}

      {selected.size > 0 && (
        <div className="batch-bar">
          <span>{formatMessage(msg('selectedCount'), { n: selected.size })}</span>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={openMoveDialog}>{msg('batchMove')}</Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchDownload()}>
            {msg('batchDownload')}
          </Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchShare()}>
            {batchShareBusy ? msg('loading') : msg('batchShare')}
          </Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={openBatchTagDialog}>
            {msg('batchTag')}
          </Button>
          <Button size="small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchStar()}>
            {batchBusy ? msg('loading') : allSelectedStarred ? msg('batchUnstar') : msg('batchStar')}
          </Button>
          <Button size="small" danger disabled={batchBusy || batchShareBusy} onClick={handleBatchTrash}>
            {msg('delete')}
          </Button>
          <Button type="text" size="small" onClick={() => setSelected(new Set())}>{msg('clearSelection')}</Button>
        </div>
      )}
      {batchNotice && <div className="banner ok">{batchNotice}</div>}
      {batchError && <div className="banner error">{batchError}</div>}

      {dirUpload && (
        <div className="banner dir-upload-progress">
          {locale === 'zh-CN'
            ? `目录上传中 已完成 ${dirUpload.done}/${dirUpload.total}`
            : `Uploading folder ${dirUpload.done}/${dirUpload.total}`}
        </div>
      )}
      {dirResult && (
        <div className={`banner ${dirResult.failures.length > 0 ? 'error' : 'ok'} dir-upload-result`}>
          <span>
            {locale === 'zh-CN'
              ? `「${dirResult.root}」上传完成：成功 ${dirResult.ok}/${dirResult.ok + dirResult.failures.length} 个文件`
              : `“${dirResult.root}” uploaded: ${dirResult.ok}/${dirResult.ok + dirResult.failures.length} files`}
            {dirResult.failures.length > 0 && (
              <>
                {locale === 'zh-CN' ? '；失败明细：' : '; failures: '}
                {dirResult.failures.slice(0, 10).map((f) => `${f.path}：${f.reason}`).join('；')}
                {dirResult.failures.length > 10
                  ? (locale === 'zh-CN' ? `；等共 ${dirResult.failures.length} 项` : `; ${dirResult.failures.length} in total`)
                  : ''}
              </>
            )}
          </span>
          <Button type="text" size="small" onClick={() => setDirResult(null)} aria-label={msg('close')}>×</Button>
        </div>
      )}

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && visibleItems.length === 0 && !error && (
        <div className="empty">{directoryQuery.trim() || searchMode ? msg('noMatch') : emptyHint ?? (locale === 'zh-CN' ? '此目录为空，上传文件或新建文件夹开始使用' : 'This folder is empty. Upload a file or create a folder to get started.')}</div>
      )}

      {visibleItems.length > 0 && viewMode === 'list' && (
        <table className="file-table">
          <thead>
            <tr>
              <th className="col-check">
                <input
                  type="checkbox"
                  checked={allSelected}
                  onChange={toggleSelectAll}
                  aria-label={msg('selectAll')}
                />
              </th>
              <th>
                {/* 表头排序：点击切换排序键/方向（复用服务端排序状态）。 */}
                <button
                  type="button"
                  className={`th-sort${sortKey === 'name' ? ' active' : ''}`}
                  onClick={() => toggleSort('name')}
                  title={msg('sort')}
                >
                  {msg('name')}
                  {sortKey === 'name' && (
                    <span className="th-sort-arrow" aria-hidden="true">{sortOrder === 'asc' ? '↑' : '↓'}</span>
                  )}
                </button>
              </th>
              <th>
                <button
                  type="button"
                  className={`th-sort${sortKey === 'updated_at' ? ' active' : ''}`}
                  onClick={() => toggleSort('updated_at')}
                  title={msg('sort')}
                >
                  {msg('sortOrderUpdated')}
                  {sortKey === 'updated_at' && (
                    <span className="th-sort-arrow" aria-hidden="true">{sortOrder === 'asc' ? '↑' : '↓'}</span>
                  )}
                </button>
              </th>
              <th className="col-actions">{msg('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {visibleItems.map((item) => (
              <tr
                key={item.id}
                className={selected.has(item.id) ? 'selected' : ''}
                onContextMenu={(e) => {
                  e.preventDefault()
                  setCtxMenu({ item, x: e.clientX, y: e.clientY })
                }}
              >
                <td className="col-check">
                  <input
                    type="checkbox"
                    checked={selected.has(item.id)}
                    onChange={() => toggleSelect(item.id)}
                    aria-label={`${msg('selectItem')} ${item.name}`}
                  />
                </td>
                <td>
                  <button
                    className="star-btn"
                    title={item.is_starred ? '取消收藏' : '收藏'}
                    onClick={() => void toggleStar(item)}
                  >
                    <Star size={16} strokeWidth={2} aria-hidden="true" fill={item.is_starred ? 'currentColor' : 'none'} />
                  </button>
                  {item.type === 'folder' && !searchMode ? (
                    <button className="name-btn" onClick={() => openItem(item)}>
                      <span className="icon">{item.has_index_web
                        ? <Globe size={14} strokeWidth={2} aria-hidden="true" />
                        : <Folder size={14} strokeWidth={2} aria-hidden="true" />}</span>
                      {item.name}{item.has_index_web && <span className="web-folder-badge">网页</span>}
                    </button>
                  ) : item.type === 'folder' ? (
                    <span className="name-btn muted">
                      <span className="icon"><Folder size={14} strokeWidth={2} aria-hidden="true" /></span>
                      {item.name}
                    </span>
                  ) : (
                    <button className="name-btn" title={msg('preview')} onClick={() => openItem(item)}>
                      <span className="icon"><FileText size={14} strokeWidth={2} aria-hidden="true" /></span>
                      {item.name}
                    </button>
                  )}
                </td>
                <td className="muted">{formatTime(item.updated_at)}</td>
                <td className="col-actions">
                  {/* 操作收进「⋯」/右键菜单（操作项较多，不再平铺）。 */}
                  <Button
                    type="text"
                    size="small"
                    className="card-menu-btn"
                    title={msg('actions')}
                    aria-haspopup="menu"
                    onClick={(e) => {
                      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
                      setCtxMenu({ item, x: rect.left, y: rect.bottom + 4 })
                    }}
                  >
                    <MoreHorizontal size={16} strokeWidth={2} aria-hidden="true" />
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {/* 网格视图（设计 6.3.7）：卡片 = 图标/文件名/大小/时间；文件夹卡片点击
          进入，文件卡片点击预览；右上「⋯」打开与列表行一致的操作菜单；
          选择状态与列表视图共享（多选 + 批量工具条两视图通用）。 */}
      {visibleItems.length > 0 && viewMode === 'grid' && (
        <div className="file-grid">
          {visibleItems.map((item) => {
            const size = sizeCache.current.get(item.id) ?? 0
            return (
              <div
                key={item.id}
                className={`file-card${selected.has(item.id) ? ' selected' : ''}`}
                onContextMenu={(e) => {
                  e.preventDefault()
                  setCtxMenu({ item, x: e.clientX, y: e.clientY })
                }}
              >
                <div className="file-card-top">
                  <input
                    type="checkbox"
                    checked={selected.has(item.id)}
                    onChange={() => toggleSelect(item.id)}
                    aria-label={`${msg('selectItem')} ${item.name}`}
                  />
                  <span className="file-card-top-actions">
                    <button
                      className="star-btn"
                      title={item.is_starred ? '取消收藏' : '收藏'}
                      onClick={() => void toggleStar(item)}
                    >
                      <Star size={16} strokeWidth={2} aria-hidden="true" fill={item.is_starred ? 'currentColor' : 'none'} />
                    </button>
                    <Button
                      type="text"
                      size="small"
                      className="card-menu-btn"
                      title={msg('actions')}
                      aria-haspopup="menu"
                      aria-expanded={cardMenuFor === item.id}
                      onClick={() => setCardMenuFor(cardMenuFor === item.id ? null : item.id)}
                    >
                      <MoreHorizontal size={16} strokeWidth={2} aria-hidden="true" />
                    </Button>
                  </span>
                </div>
                <button
                  className="file-card-body"
                  title={item.type === 'file' ? (locale === 'zh-CN' ? '查看' : 'View') : item.name}
                  onClick={() => openItem(item)}
                >
                  <span className="file-card-icon">{item.type === 'folder'
                    ? (item.has_index_web
                      ? <Globe size={22} strokeWidth={2} aria-hidden="true" />
                      : <Folder size={22} strokeWidth={2} aria-hidden="true" />)
                    : <FileText size={22} strokeWidth={2} aria-hidden="true" />}</span>
                  <span className="file-card-name" title={item.name}>{item.name}{item.has_index_web && <span className="web-folder-badge">网页</span>}</span>
                  <span className="file-card-meta muted">
                    {size > 0 ? `${formatSize(size)} · ` : ''}
                    {formatTime(item.updated_at)}
                  </span>
                </button>
                {cardMenuFor === item.id && (
                  <div className="file-card-menu" role="menu" onClick={() => setCardMenuFor(null)}>
                    {renderItemMenu(item)}
                  </div>
                )}
              </div>
            )
          })}
        </div>
      )}

      {uploads.length > 0 && (
        <div className="upload-panel">
          <div className="upload-panel-head">
            <span>上传任务</span>
            <Button type="text" size="small" onClick={() => setUploads([])}>清空</Button>
          </div>
          {uploads.map((row) => (
            <div key={row.key} className="upload-row">
              <span className="upload-name">{row.name}</span>
              <span className={`badge ${row.phase}`}>{phaseText[row.phase]}</span>
              {row.error && <span className="error-text">{row.error}</span>}
            </div>
          ))}
        </div>
      )}

      {folderOpen && createFolderFn && (
        <Modal title="新建文件夹" onClose={() => setFolderOpen(false)}>
          <form onSubmit={handleCreateFolder}>
            <label className="field">
              <span>名称</span>
              <Input autoFocus allowClear value={folderName} onChange={(e) => setFolderName(e.target.value)} placeholder="新文件夹" />
            </label>
            {folderError && <div className="error-text">{folderError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setFolderOpen(false)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={!folderName.trim()}>创建</Button>
            </div>
          </form>
        </Modal>
      )}

      {createKind && createSpec && (
        <Modal title={`新建 ${createSpec.label}`} onClose={() => !docCreating && setCreateKind(null)}>
          <form onSubmit={handleNamedCreate}>
            <label className="field">
              <span>文件名</span>
              <Input autoFocus allowClear value={createName} onChange={(e) => setCreateName(e.target.value)} placeholder={`名称${createSpec.ext}`} />
            </label>
            <p className="hint">未填写 {createSpec.ext} 扩展名时会自动补全。</p>
            {createError && <div className="error-text">{createError}</div>}
            <div className="modal-actions">
              <Button disabled={docCreating} onClick={() => setCreateKind(null)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={docCreating || !createName.trim()}>{docCreating ? '创建中…' : '创建并打开'}</Button>
            </div>
          </form>
        </Modal>
      )}

      {moveOpen && (
        <Modal title={`移动 ${selected.size} 项`} onClose={() => setMoveOpen(false)}>
          <form onSubmit={handleBatchMove}>
            <label className="field">
              <span>目标目录</span>
              <Select
                value={moveTarget}
                onChange={(v) => setMoveTarget(v)}
                options={uniqueMoveCandidates.map((c) => ({ value: c.id, label: c.label }))}
              />
            </label>
            <label className="field">
              <span>或输入目标目录 UUID</span>
              <Input
                allowClear
                value={moveManual}
                onChange={(e) => setMoveManual(e.target.value)}
                placeholder="可选；填写后优先生效"
              />
            </label>
            <p className="hint">单项失败不会回滚其余项；目标目录存在同名项时该项跳过。</p>
            {moveError && <div className="error-text">{moveError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setMoveOpen(false)}>取消</Button>
              <Button type="primary" htmlType="submit" disabled={batchBusy || selected.size === 0}>
                {batchBusy ? '移动中…' : '移动'}
              </Button>
            </div>
          </form>
        </Modal>
      )}

      {shareListOpen && shareLinks.length > 0 && (
        <Modal wide title={msg('batchShareTitle')} onClose={() => setShareListOpen(false)}>
          <p className="hint">{msg('batchShareLinks')}</p>
          <div className="share-link-list">
            {shareLinks.map((link, index) => (
              <div key={link.url} className="share-link-item">
                <span className="share-link-name muted">{link.name}</span>
                <div className="share-link">
                  <Input readOnly value={link.url} onFocus={(e) => e.currentTarget.select()} />
                  <Button
                    size="small"
                    onClick={() => void copyShareLink(link.url, index)}
                  >
                    {copiedShareIdx === index ? msg('copied') : msg('copyLink')}
                  </Button>
                </div>
              </div>
            ))}
          </div>
          <div className="modal-actions">
            <Button onClick={() => setShareListOpen(false)}>{msg('close')}</Button>
          </div>
        </Modal>
      )}

      {batchTagOpen && (
        <Modal title={formatMessage(msg('batchTagTitle'), { n: selected.size })} onClose={() => setBatchTagOpen(false)}>
          <form onSubmit={handleBatchTag}>
            <label className="field">
              <span>{msg('tag')}</span>
              <Select
                autoFocus
                value={batchTagId}
                onChange={(v) => setBatchTagId(v)}
                options={tags.map((t) => ({ value: t.id, label: `#${t.name}` }))}
              />
            </label>
            {batchTagError && <div className="error-text">{batchTagError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setBatchTagOpen(false)}>{msg('cancel')}</Button>
              <Button type="primary" htmlType="submit" disabled={batchTagBusy || !batchTagId}>
                {batchTagBusy ? msg('loading') : msg('apply')}
              </Button>
            </div>
          </form>
        </Modal>
      )}

      {copyTarget && copyFn && (
        <Modal title={formatMessage(msg('copyTitle'), { name: copyTarget.name })} onClose={() => setCopyTarget(null)}>
          <form onSubmit={handleCopy}>
            <label className="field">
              <span>{msg('copyTargetLabel')}</span>
              <Input
                autoFocus
                allowClear
                value={copyParent}
                onChange={(e) => setCopyParent(e.target.value)}
                placeholder="3f0c9c2e-…"
              />
            </label>
            {copyError && <div className="error-text">{copyError}</div>}
            <div className="modal-actions">
              <Button onClick={() => setCopyTarget(null)}>{msg('cancel')}</Button>
              <Button type="primary" htmlType="submit" disabled={copyBusy}>
                {copyBusy ? msg('loading') : msg('copy')}
              </Button>
            </div>
          </form>
        </Modal>
      )}

      {tagModalTarget && (
        <Modal title={`标签「${tagModalTarget.name}」`} onClose={() => setTagModalTarget(null)}>
          <div className="tag-modal">
            {tags.length === 0 ? (
              <p className="hint">你还没有标签，先创建一个。</p>
            ) : (
              <div className="check-list">
                {tags.map((t) => (
                  <label key={t.id} className="check-item">
                    <input
                      type="checkbox"
                      disabled={tagModalBusy}
                      checked={tagModalFileTagIds.has(t.id)}
                      onChange={() => void toggleFileTag(t)}
                    />
                    <span>#{t.name}</span>
                  </label>
                ))}
              </div>
            )}
            <form className="tag-create" onSubmit={handleCreateTagAndAttach}>
              <Input
                allowClear
                value={tagModalNewName}
                onChange={(e) => setTagModalNewName(e.target.value)}
                placeholder="新标签名称（≤64 字符）"
                maxLength={64}
              />
              <Button htmlType="submit" disabled={tagModalBusy || !tagModalNewName.trim()}>
                创建并打标
              </Button>
            </form>
            {tagModalError && <div className="error-text">{tagModalError}</div>}
          </div>
        </Modal>
      )}

      {previewTarget && (
        <Modal
          wide
          className="modal-viewer"
          title={previewTarget.type === 'folder' ? `网页目录「${previewTarget.name}」` : `查看「${previewTarget.name}」`}
          onClose={closePreview}
          headExtra={previewHeadExtra(previewTarget)}
        >
          {/* 弹窗内容：网页目录 = sandbox iframe（raw_url）；其余类型（含
              office，内嵌 OnlyOffice 只读视图）统一经 FileViewerDispatch
              就地内嵌渲染，与独立查看页完全一致。 */}
          <div className="preview-embed">
            {previewTarget.type === 'folder' ? (
              webPreviewUrl ? (
                <iframe
                  className="standalone-viewer-frame"
                  sandbox="allow-scripts allow-forms allow-popups allow-modals"
                  src={webPreviewUrl}
                  title={previewTarget.name}
                />
              ) : (
                <div className="text-editor-state">正在加载网页…</div>
              )
            ) : (
              <FileViewerDispatch
                fileId={previewTarget.id}
                name={previewTarget.name}
                resolveRawUrl={async () => {
                  try {
                    const r = await resolveFileById(previewTarget.id, { mode: 'view' })
                    return r.raw_url
                  } catch {
                    return null
                  }
                }}
              />
            )}
          </div>
        </Modal>
      )}

      {/* 管理默认打开方式：任意扩展名 × 全部打开方式可新增；已配置项可即时
          修改（PUT）或删除（DELETE）。 */}
      {openWithMgrOpen && (
        <Modal wide title={locale === 'zh-CN' ? '管理默认打开方式' : 'Manage default openers'} onClose={() => setOpenWithMgrOpen(false)}>
          <p className="hint">
            {locale === 'zh-CN'
              ? '按扩展名保存的默认打开方式；下方可新增任意扩展名的偏好，修改即时保存，删除后恢复按文件类型自动选择。'
              : 'Per-extension default openers. Add any extension below; changes save immediately; deleting restores automatic selection.'}
          </p>
          <div className="openwith-mgr">
            {Object.entries(openWith)
              .sort(([a], [b]) => a.localeCompare(b))
              .map(([ext, opener]) => (
                <div key={ext} className="openwith-mgr-row">
                  <span className="openwith-mgr-ext">.{ext}</span>
                  <Select
                    value={opener}
                    aria-label={`.${ext}`}
                    className="openwith-mgr-select"
                    onChange={(v) => void handleMgrChange(ext, v as OpenWithOpener)}
                    options={ALL_OPENERS.map((op) => ({ value: op, label: openerLabel(op, locale === 'zh-CN') }))}
                  />
                  <Button size="small" danger onClick={() => void handleMgrDelete(ext)}>
                    {msg('delete')}
                  </Button>
                </div>
              ))}
            {Object.keys(openWith).length === 0 && (
              <p className="hint">
                {locale === 'zh-CN'
                  ? '暂无已配置项：可在下方直接新增，或在文件菜单「打开方式」中选择保存。'
                  : 'No defaults configured yet. Add one below, or pick from a file’s “Open with” menu.'}
              </p>
            )}
            <form className="openwith-mgr-row openwith-mgr-add" onSubmit={handleMgrAdd}>
              <Input
                className="openwith-mgr-ext-input"
                allowClear
                value={mgrNewExt}
                onChange={(e) => setMgrNewExt(e.target.value)}
                placeholder={locale === 'zh-CN' ? '扩展名，如 docx' : 'extension, e.g. docx'}
                aria-label={locale === 'zh-CN' ? '扩展名' : 'Extension'}
                maxLength={17}
              />
              <Select
                value={mgrNewOpener}
                className="openwith-mgr-select"
                aria-label={locale === 'zh-CN' ? '打开方式' : 'Opener'}
                onChange={(v) => setMgrNewOpener(v as OpenWithOpener)}
                options={ALL_OPENERS.map((op) => ({ value: op, label: openerLabel(op, locale === 'zh-CN') }))}
              />
              <Button size="small" type="primary" htmlType="submit" disabled={mgrAdding || !mgrNewExt.trim()}>
                {mgrAdding ? (locale === 'zh-CN' ? '保存中…' : 'Saving…') : (locale === 'zh-CN' ? '新增' : 'Add')}
              </Button>
            </form>
          </div>
          {openWithMgrError && <div className="error-text">{openWithMgrError}</div>}
          <div className="modal-actions">
            <Button onClick={() => setOpenWithMgrOpen(false)}>{msg('close')}</Button>
          </div>
        </Modal>
      )}

      {/* 右键 / 列表行「⋯」菜单：视口定位浮层保留（初始按点击坐标，渲染后经
          clampFixedMenu 按实测尺寸收缩进视口），菜单面板换 antd Menu 视觉。 */}
      {ctxMenu && (
        <div
          ref={ctxMenuRef}
          className="ctx-menu"
          role="menu"
          style={{ left: `${ctxMenu.x}px`, top: `${ctxMenu.y}px` }}
          onClick={() => setCtxMenu(null)}
        >
          {renderItemMenu(ctxMenu.item)}
        </div>
      )}
    </div>
  )
}
