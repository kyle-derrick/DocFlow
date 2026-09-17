// 通用文件浏览组件：从 FilesPage 提炼的目录列表 / 面包屑 / 上传 / 下载 /
// 预览（含 office 文档的 ONLYOFFICE「编辑」入口）/ 新建文件夹逻辑，
// 个人空间与团队空间共用。
// 通过注入 listItems / createFolderFn / uploadFn / downloadFn / previewFn
// 适配不同后端端点；写操作 403 时统一提示「无写权限」。
// v1.0 追加：多选 + 批量移动/删除（部分成功语义）、行内星标切换、
// 行内标签管理（打/去标签、新建）、顶栏标签/收藏筛选与服务端排序。
// v1.2 追加：列表/网格视图切换（设计 6.3.7；偏好持久化 localStorage，
// Ctrl/Cmd+1、Ctrl/Cmd+2 快捷键见设计 6.16.1）。
import { FormEvent, ReactNode, useEffect, useRef, useState } from 'react'
import { zipSync, strToU8 } from 'fflate'
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
  isDrawioFile,
  isExcalidrawFile,
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

export function Modal({
  title,
  onClose,
  wide,
  className,
  children,
}: {
  title: string
  onClose: () => void
  wide?: boolean
  /** 追加到 .modal 的自定义类（如查看弹窗 modal-viewer 加宽加高）。 */
  className?: string
  children: ReactNode
}) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className={`modal${wide ? ' wide' : ''}${className ? ` ${className}` : ''}`} onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          <h3>{title}</h3>
          <button type="button" className="btn ghost" onClick={onClose} aria-label="关闭">×</button>
        </div>
        {children}
      </div>
    </div>
  )
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

function sortItems(items: FileItem[]): FileItem[] {
  return [...items].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))
}

/** 写操作错误文案：403 统一为「无写权限」（团队 viewer、只读目录等）。 */
function writeErrorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError && err.status === 403) return '无写权限'
  return err instanceof Error ? err.message : fallback
}

export interface FileBrowserProps {
  /** 页面标题（渲染在工具栏左侧）。 */
  title: string
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
}

export default function FileBrowser({
  title,
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
}: FileBrowserProps) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
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
  // 「＋ 新建」下拉开关。
  const [createMenuOpen, setCreateMenuOpen] = useState(false)
  // 「⬆ 上传」下拉开关（上传文件 / 上传目录合一入口）。
  const [uploadMenuOpen, setUploadMenuOpen] = useState(false)

  // 右键菜单与新建/上传下拉点击外部关闭（菜单内部动作在冒泡阶段完成后收口）。
  useEffect(() => {
    if (!ctxMenu && !createMenuOpen && !uploadMenuOpen) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (el?.closest?.('.ctx-menu, .create-menu-wrap')) return
      setCtxMenu(null)
      setCreateMenuOpen(false)
      setUploadMenuOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [ctxMenu, createMenuOpen, uploadMenuOpen])

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
  const [createKind, setCreateKind] = useState<'md' | 'txt' | 'code' | 'html' | 'drawio' | 'whiteboard' | 'word' | 'spreadsheet' | 'presentation' | 'xmind' | null>(null)
  const [createName, setCreateName] = useState('')
  const [createError, setCreateError] = useState('')

  const [previewTarget, setPreviewTarget] = useState<FileItem | null>(null)

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

  const handleBatchTrash = async () => {
    if (selectedIds.length === 0) return
    if (!window.confirm(formatMessage(msg('batchTrashConfirm'), { n: selectedIds.length }))) return
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
  // 不代表条目位置）或 ns 缺失时回退 UUID 路由。
  const routeFor = (prefix: 'view' | 'edit', item: FileItem) => {
    if (!ns || searchMode) return `/${prefix}/${item.id}`
    const segs = [...pathSegmentsOf(crumbs), item.name].map(encodeURIComponent)
    return `/${prefix}/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${segs.join('/')}/`
  }

  // ---- 目录「作为网页打开」（resolve 目录下 index.html → 新窗口 by-path 查看页） ----

  // 面包屑拼当前目录路径段（含团队空间）；逐段 encodeURIComponent 后拼 URL。
  const openAsWebsite = async (item: FileItem) => {
    if (!ns) return
    const segments = [...pathSegmentsOf(crumbs), item.name]
    setError('')
    try {
      await resolvePath(ns.type, ns.scope, [...segments, 'index.html'].join('/'), { mode: 'view' })
    } catch {
      // index.html 不存在：尝试 index.htm（打开时以文件 raw_url 渲染）。
      try {
        await resolvePath(ns.type, ns.scope, [...segments, 'index.htm'].join('/'), { mode: 'view' })
      } catch {
        setError('该目录没有 index.html')
        return
      }
    }
    const encoded = encodePathSegments(segments)
    openEditorWindow(`/view/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${encoded}/`)
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
  }

  const closePreview = () => {
    setPreviewTarget(null)
  }

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

  const createSpec = createKind ? {
    md: { label: 'Markdown', ext: '.md', content: '# 新文档\n', mime: 'text/markdown', route: 'markdown' },
    txt: { label: 'TXT', ext: '.txt', content: '', mime: 'text/plain', route: 'text' },
    code: { label: '代码', ext: '.js', content: '', mime: 'text/javascript', route: 'code' },
    html: { label: 'HTML', ext: '.html', content: '<!doctype html>\n<html><body></body></html>\n', mime: 'text/html', route: 'view' },
    drawio: { label: 'draw.io', ext: '.drawio', content: EMPTY_DRAWIO_XML, mime: 'text/xml', route: 'drawio' },
    whiteboard: { label: '白板', ext: '.excalidraw', content: EMPTY_EXCALIDRAW_JSON, mime: 'application/json', route: 'excalidraw' },
    xmind: { label: 'XMind', ext: '.xmind', content: '', mime: 'application/x-xmind', route: 'view' },
    word: { label: 'Word', ext: '.docx', content: '', mime: '', route: 'edit' },
    spreadsheet: { label: 'Excel', ext: '.xlsx', content: '', mime: '', route: 'edit' },
    presentation: { label: 'PPT', ext: '.pptx', content: '', mime: '', route: 'edit' },
  }[createKind] : null

  const beginNamedCreate = (kind: NonNullable<typeof createKind>) => {
    const defaults: Record<NonNullable<typeof createKind>, string> = {
      md: '新文档.md', txt: '新文本.txt', code: 'main.js', html: 'index.html', drawio: '新图表.drawio',
      whiteboard: '新白板.excalidraw', xmind: '新思维导图.xmind', word: '新文档.docx',
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
    if (!name.toLowerCase().endsWith(createSpec.ext)) name += createSpec.ext
    if (/[/\\:*?"<>|]/.test(name)) {
      setCreateError('文件名不能包含 / \\ : * ? " < > |')
      return
    }
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
        let bytes: string | Uint8Array = createSpec.content
        if (createKind === 'xmind') {
          bytes = zipSync({
            'content.json': strToU8(JSON.stringify([{ id: 'root', class: 'sheet', title: name.replace(/\.xmind$/i, ''), rootTopic: { id: 'topic', class: 'topic', title: '中心主题' } }])),
            'metadata.json': strToU8(JSON.stringify({ creator: { name: 'DocFlow' }, activeSheetId: 'root' })),
          })
        }
        const content = typeof bytes === 'string' ? bytes : new Blob([bytes.slice().buffer])
        const created = await uploadFn(new File([content], name, { type: createSpec.mime }), currentFolderId, (phase) => {
          setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase } : r)))
        })
        fileID = typeof created === 'object' && created !== null && 'file_id' in created
          ? String((created as { file_id?: string }).file_id ?? '') : ''
      }
      await load(currentParent)
      setCreateKind(null)
      if (fileID && child) child.location.href = `/${createSpec.route}/${fileID}`
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
      if (uploadMenuOpen) {
        setUploadMenuOpen(false)
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

  // 默认点击始终查看；只有明确的“编辑”菜单才进入编辑器。
  const openItem = (item: FileItem) => {
    if (item.type === 'folder') {
      if (item.has_index_web && ns && !searchMode) void openAsWebsite(item)
      else if (!searchMode) openFolder(item)
      return
    }
    openPreview(item)
  }

  // 条目操作菜单（列表行「⋯」、右键菜单、网格卡片菜单共用）：
  // 打开（按默认打开方式）/ 打开方式子菜单 / 目录「下载为 ZIP」/ 打开方式
  // 管理入口 + 既有「查看 / 编辑 / 预览 / 下载 / 标签 / 复制」与调用方 rowActions。
  const itemMenuContent = (item: FileItem) => (
    <>
      {item.type === 'folder' && <button type="button" className="btn small" onClick={() => openFolder(item)}>
        {item.has_index_web ? (locale === 'zh-CN' ? '目录浏览' : 'Browse folder') : (locale === 'zh-CN' ? '进入' : 'Open')}
      </button>}
      {item.type === 'folder' && (
        <button
          type="button"
          className="btn small"
          disabled={zipBusyId !== null}
          onClick={() => void handleZipDownload(item)}
        >
          {zipBusyId === item.id
            ? (locale === 'zh-CN' ? '打包中…' : 'Zipping…')
            : (locale === 'zh-CN' ? '下载为 ZIP' : 'Download as ZIP')}
        </button>
      )}
      {item.type === 'file' && (
        <>
          <button type="button" className="btn small" onClick={() => openPreview(item)}>{locale === 'zh-CN' ? '查看' : 'View'}</button>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('view', item))}>{locale === 'zh-CN' ? '新窗口查看' : 'View in new window'}</button>
        </>
      )}
      {item.type === 'file' && openWithOptions(item.name).some((op) => op !== 'default' && op !== 'web') && (
        <div className="openwith-group" role="group" aria-label={locale === 'zh-CN' ? '打开方式' : 'Open with'}>
          <div className="ctx-menu-label">{locale === 'zh-CN' ? '打开方式（选择后设为默认）' : 'Open with (sets default)'}</div>
          {openWithOptions(item.name).map((op) => (
            <button
              key={op}
              type="button"
              className="btn small openwith-item"
              title={openerLabel(op, locale === 'zh-CN')}
              onClick={() => void handleOpenWithChoice(item, op)}
            >
              <span className={`openwith-dot${resolveOpener(item.name, openWith) === op ? ' on' : ''}`} aria-hidden="true" />
              {openerLabel(op, locale === 'zh-CN')}
            </button>
          ))}
          <button
            type="button"
            className="btn small ghost"
            onClick={() => {
              setCtxMenu(null)
              setCardMenuFor(null)
              setOpenWithMgrOpen(true)
            }}
          >
            {locale === 'zh-CN' ? '管理默认打开方式…' : 'Manage defaults…'}
          </button>
        </div>
      )}
      {item.type === 'folder' && ns && !searchMode && (
        <button
          type="button"
          className="btn small"
          onClick={() => void openAsWebsite(item)}
          title={locale === 'zh-CN' ? '将该目录作为静态网站打开（index.html）' : 'Open this folder as a website (index.html)'}
        >
          {locale === 'zh-CN' ? '作为网页打开' : 'Open as website'}
        </button>
      )}
      {item.type === 'file' && item.name.toLowerCase().endsWith('.zip') && (
        <button
          type="button"
          className="btn small"
          disabled={unpackBusyId !== null}
          onClick={() => void handleUnpack(item)}
        >
          {unpackBusyId === item.id
            ? locale === 'zh-CN' ? '解包中…' : 'Unpacking…'
            : locale === 'zh-CN' ? '解包为目录' : 'Unpack to folder'}
        </button>
      )}
      {item.type === 'file' && !(ooEnabled && isOfficeFile(item.name)) && (
        <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('view', item))}>
          {locale === 'zh-CN' ? '查看' : 'View'}
        </button>
      )}
      {item.type === 'file' && ooEnabled && isOfficeFile(item.name) && (
        <>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('view', item))}>
            {locale === 'zh-CN' ? '查看 Office' : 'View Office'}
          </button>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('edit', item))}>
            {locale === 'zh-CN' ? '编辑 Office' : 'Edit Office'}
          </button>
        </>
      )}
      {item.type === 'file' && drawioEnabled && isDrawioFile(item.name) && (
        <>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('edit', item))}>
            {locale === 'zh-CN' ? '图表编辑' : 'Edit diagram'}
          </button>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('view', item))}>
            {locale === 'zh-CN' ? '图表查看' : 'View diagram'}
          </button>
        </>
      )}
      {item.type === 'file' && (() => {
        return isTextEditable(item.name) ? <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('edit', item))}>
          {locale === 'zh-CN' ? '编辑文本' : 'Edit text'}
        </button> : null
      })()}
      {item.type === 'file' && isExcalidrawFile(item.name) && (
        <>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('edit', item))}>
            {locale === 'zh-CN' ? '白板编辑' : 'Edit whiteboard'}
          </button>
          <button type="button" className="btn small" onClick={() => openEditorWindow(routeFor('view', item))}>
            {locale === 'zh-CN' ? '白板查看' : 'View whiteboard'}
          </button>
        </>
      )}
      {item.type === 'file' && (
        <button type="button" className="btn small" onClick={() => void handleDownload(item)}>{msg('download')}</button>
      )}
      <button type="button" className="btn small" onClick={() => void openTagModal(item)}>{msg('tag')}</button>
      {item.type === 'file' && copyFn && (
        <button type="button" className="btn small" onClick={() => openCopyDialog(item)}>{msg('copy')}</button>
      )}
      {rowActions?.(item)}
    </>
  )

  return (
    <div className="file-browser">
      <div className="page-head">
        {title && <h2>{title}</h2>}
        <div className="toolbar">
          {/* 列表/网格切换合一：单按钮按当前模式显示对侧图标（title 提示目标模式）。 */}
          <button
            type="button"
            className="btn view-toggle-btn"
            title={viewMode === 'list' ? msg('viewModeGrid') : msg('viewModeList')}
            aria-label={viewMode === 'list' ? msg('viewModeGrid') : msg('viewModeList')}
            aria-pressed={viewMode === 'grid'}
            onClick={() => changeViewMode(viewMode === 'list' ? 'grid' : 'list')}
          >
            {viewMode === 'list' ? '▦' : '☰'}
          </button>
          {(createFolderFn || uploadFn) && !searchMode && (
            <div className="create-menu-wrap">
              <button type="button" className="btn" aria-haspopup="menu" aria-expanded={createMenuOpen} onClick={() => setCreateMenuOpen((v) => !v)}>
                ＋ {locale === 'zh-CN' ? '新建' : 'New'}
              </button>
              {createMenuOpen && (
                <div className="file-card-menu create-menu" role="menu" onClick={() => setCreateMenuOpen(false)}>
                  {createFolderFn && (
                    <button type="button" className="btn small" onClick={() => { setFolderOpen(true); setFolderName(''); setFolderError('') }}>
                      {locale === 'zh-CN' ? '文件夹' : 'Folder'}
                    </button>
                  )}
                  {uploadFn && (['md', 'txt', 'code', 'html', 'drawio', 'whiteboard', 'word', 'spreadsheet', 'presentation', 'xmind'] as const).map((kind) => (
                    <button key={kind} type="button" className="btn small" disabled={docCreating} onClick={() => beginNamedCreate(kind)}>
                      {{ md: 'Markdown', txt: 'TXT', code: '代码', html: 'HTML', drawio: 'draw.io', whiteboard: '白板', word: 'Word', spreadsheet: 'Excel', presentation: 'PPT', xmind: 'XMind' }[kind]}
                    </button>
                  ))}
                  <div className="ctx-menu-label">PDF 仅支持上传和查看，不能空白创建</div>
                </div>
              )}
            </div>
          )}
          {/* 上传合一：「⬆ 上传」下拉提供文件/目录两个入口（目录上传依赖建目录权限）。 */}
          {uploadFn && !searchMode && (
            <div className="create-menu-wrap">
              <button
                type="button"
                className="btn primary"
                aria-haspopup="menu"
                aria-expanded={uploadMenuOpen}
                disabled={dirUpload !== null}
                onClick={() => setUploadMenuOpen((v) => !v)}
              >
                {dirUpload !== null
                  ? (locale === 'zh-CN' ? '上传中…' : 'Uploading…')
                  : (locale === 'zh-CN' ? '⬆ 上传' : '⬆ Upload')}
              </button>
              {uploadMenuOpen && (
                <div className="file-card-menu create-menu" role="menu" onClick={() => setUploadMenuOpen(false)}>
                  <button type="button" className="btn small" onClick={() => fileInputRef.current?.click()}>
                    {locale === 'zh-CN' ? '上传文件' : 'Upload files'}
                  </button>
                  {createFolderFn && (
                    <button
                      type="button"
                      className="btn small"
                      disabled={dirUpload !== null}
                      onClick={() => dirInputRef.current?.click()}
                      title={locale === 'zh-CN' ? '选择本地目录，按原目录结构上传到当前目录下' : 'Pick a local folder and upload with its structure'}
                    >
                      {locale === 'zh-CN' ? '上传目录' : 'Upload folder'}
                    </button>
                  )}
                </div>
              )}
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
        </div>
      </div>

      {/* 顶栏筛选：目录内搜索 + 标签 / 收藏下拉（排序已表头化，网格视图无表头、
          保留一个精简排序下拉；全部/收藏/最近视图切换在 SpaceSwitcher 行）。 */}
      <div className="filter-bar">
        <label className="filter-item directory-search">
          <input
            type="search"
            value={directoryQuery}
            placeholder={locale === 'zh-CN' ? '搜索当前目录…' : 'Search this folder…'}
            onChange={(event) => setDirectoryQuery(event.target.value)}
          />
        </label>
        <label className="filter-item">
          <span>{msg('tag')}</span>
          <select data-hotkey="filter" value={tagFilter} onChange={(e) => { setTagFilter(e.target.value); setRecentView(false) }}>
            <option value="">{msg('all')}</option>
            {tags.map((t) => (
              <option key={t.id} value={t.id}>#{t.name}</option>
            ))}
          </select>
        </label>
        <label className="filter-item">
          <span>{msg('viewStarred')}</span>
          <select value={starredFilter} onChange={(e) => { setStarredFilter(e.target.value); setRecentView(false) }}>
            <option value="">{msg('all')}</option>
            <option value="true">{msg('starredOnly')}</option>
            <option value="false">{msg('starredNo')}</option>
          </select>
        </label>
        {viewMode === 'grid' && (
          <label className="filter-item">
            <span>{msg('sort')}</span>
            <select
              value={`${sortKey}:${sortOrder}`}
              onChange={(e) => {
                const [key, order] = e.target.value.split(':')
                setSortKey(key as 'name' | 'updated_at' | 'size')
                setSortOrder(order as 'asc' | 'desc')
              }}
            >
              <option value="name:asc">{msg('sortOrderName')} ↑</option>
              <option value="name:desc">{msg('sortOrderName')} ↓</option>
              <option value="updated_at:desc">{msg('sortOrderUpdated')} ↓</option>
              <option value="updated_at:asc">{msg('sortOrderUpdated')} ↑</option>
              <option value="size:desc">{msg('sortOrderSize')} ↓</option>
              <option value="size:asc">{msg('sortOrderSize')} ↑</option>
            </select>
          </label>
        )}
        {searchMode && (
          <button className="btn ghost small" onClick={clearFilters}>{msg('clearFilters')}</button>
        )}
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
          <button className="btn small" disabled={batchBusy || batchShareBusy} onClick={openMoveDialog}>{msg('batchMove')}</button>
          <button className="btn small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchDownload()}>
            {msg('batchDownload')}
          </button>
          <button className="btn small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchShare()}>
            {batchShareBusy ? msg('loading') : msg('batchShare')}
          </button>
          <button className="btn small" disabled={batchBusy || batchShareBusy} onClick={openBatchTagDialog}>
            {msg('batchTag')}
          </button>
          <button className="btn small" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchStar()}>
            {batchBusy ? msg('loading') : allSelectedStarred ? msg('batchUnstar') : msg('batchStar')}
          </button>
          <button className="btn small danger" disabled={batchBusy || batchShareBusy} onClick={() => void handleBatchTrash()}>
            {msg('delete')}
          </button>
          <button className="btn ghost small" onClick={() => setSelected(new Set())}>{msg('clearSelection')}</button>
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
          <button type="button" className="btn ghost small" onClick={() => setDirResult(null)} aria-label={msg('close')}>×</button>
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
                    {item.is_starred ? '★' : '☆'}
                  </button>
                  {item.type === 'folder' && !searchMode ? (
                    <button className="name-btn" onClick={() => openItem(item)}>
                      <span className="icon">{item.has_index_web ? '🌐' : '📁'}</span>
                      {item.name}{item.has_index_web && <span className="web-folder-badge">网页</span>}
                    </button>
                  ) : item.type === 'folder' ? (
                    <span className="name-btn muted">
                      <span className="icon">📁</span>
                      {item.name}
                    </span>
                  ) : (
                    <button className="name-btn" title={msg('preview')} onClick={() => openItem(item)}>
                      <span className="icon">📄</span>
                      {item.name}
                    </button>
                  )}
                </td>
                <td className="muted">{formatTime(item.updated_at)}</td>
                <td className="col-actions">
                  {/* 操作收进「⋯」/右键菜单（操作项较多，不再平铺）。 */}
                  <button
                    className="btn ghost small card-menu-btn"
                    title={msg('actions')}
                    aria-haspopup="menu"
                    onClick={(e) => {
                      const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
                      setCtxMenu({ item, x: rect.left, y: rect.bottom + 4 })
                    }}
                  >
                    ⋯
                  </button>
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
                      {item.is_starred ? '★' : '☆'}
                    </button>
                    <button
                      className="btn ghost small card-menu-btn"
                      title={msg('actions')}
                      aria-haspopup="menu"
                      aria-expanded={cardMenuFor === item.id}
                      onClick={() => setCardMenuFor(cardMenuFor === item.id ? null : item.id)}
                    >
                      ⋯
                    </button>
                  </span>
                </div>
                <button
                  className="file-card-body"
                  title={item.type === 'file' ? (locale === 'zh-CN' ? '查看' : 'View') : item.name}
                  onClick={() => openItem(item)}
                >
                  <span className="file-card-icon">{item.type === 'folder' ? (item.has_index_web ? '🌐' : '📁') : '📄'}</span>
                  <span className="file-card-name" title={item.name}>{item.name}{item.has_index_web && <span className="web-folder-badge">网页</span>}</span>
                  <span className="file-card-meta muted">
                    {size > 0 ? `${formatSize(size)} · ` : ''}
                    {formatTime(item.updated_at)}
                  </span>
                </button>
                {cardMenuFor === item.id && (
                  <div className="file-card-menu" role="menu" onClick={() => setCardMenuFor(null)}>
                    {itemMenuContent(item)}
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
            <button className="btn ghost small" onClick={() => setUploads([])}>清空</button>
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
              <input autoFocus value={folderName} onChange={(e) => setFolderName(e.target.value)} placeholder="新文件夹" />
            </label>
            {folderError && <div className="error-text">{folderError}</div>}
            <div className="modal-actions">
              <button type="button" className="btn" onClick={() => setFolderOpen(false)}>取消</button>
              <button type="submit" className="btn primary" disabled={!folderName.trim()}>创建</button>
            </div>
          </form>
        </Modal>
      )}

      {createKind && createSpec && (
        <Modal title={`新建 ${createSpec.label}`} onClose={() => !docCreating && setCreateKind(null)}>
          <form onSubmit={handleNamedCreate}>
            <label className="field">
              <span>文件名</span>
              <input autoFocus value={createName} onChange={(e) => setCreateName(e.target.value)} placeholder={`名称${createSpec.ext}`} />
            </label>
            <p className="hint">未填写 {createSpec.ext} 扩展名时会自动补全。</p>
            {createError && <div className="error-text">{createError}</div>}
            <div className="modal-actions">
              <button type="button" className="btn" disabled={docCreating} onClick={() => setCreateKind(null)}>取消</button>
              <button type="submit" className="btn primary" disabled={docCreating || !createName.trim()}>{docCreating ? '创建中…' : '创建并打开'}</button>
            </div>
          </form>
        </Modal>
      )}

      {moveOpen && (
        <Modal title={`移动 ${selected.size} 项`} onClose={() => setMoveOpen(false)}>
          <form onSubmit={handleBatchMove}>
            <label className="field">
              <span>目标目录</span>
              <select value={moveTarget} onChange={(e) => setMoveTarget(e.target.value)}>
                {uniqueMoveCandidates.map((c) => (
                  <option key={c.id || 'root'} value={c.id}>{c.label}</option>
                ))}
              </select>
            </label>
            <label className="field">
              <span>或输入目标目录 UUID</span>
              <input
                value={moveManual}
                onChange={(e) => setMoveManual(e.target.value)}
                placeholder="可选；填写后优先生效"
              />
            </label>
            <p className="hint">单项失败不会回滚其余项；目标目录存在同名项时该项跳过。</p>
            {moveError && <div className="error-text">{moveError}</div>}
            <div className="modal-actions">
              <button type="button" className="btn" onClick={() => setMoveOpen(false)}>取消</button>
              <button type="submit" className="btn primary" disabled={batchBusy || selected.size === 0}>
                {batchBusy ? '移动中…' : '移动'}
              </button>
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
                  <input readOnly value={link.url} onFocus={(e) => e.currentTarget.select()} />
                  <button
                    className="btn small"
                    onClick={() => void copyShareLink(link.url, index)}
                  >
                    {copiedShareIdx === index ? msg('copied') : msg('copyLink')}
                  </button>
                </div>
              </div>
            ))}
          </div>
          <div className="modal-actions">
            <button className="btn" onClick={() => setShareListOpen(false)}>{msg('close')}</button>
          </div>
        </Modal>
      )}

      {batchTagOpen && (
        <Modal title={formatMessage(msg('batchTagTitle'), { n: selected.size })} onClose={() => setBatchTagOpen(false)}>
          <form onSubmit={handleBatchTag}>
            <label className="field">
              <span>{msg('tag')}</span>
              <select autoFocus value={batchTagId} onChange={(e) => setBatchTagId(e.target.value)}>
                {tags.map((t) => (
                  <option key={t.id} value={t.id}>#{t.name}</option>
                ))}
              </select>
            </label>
            {batchTagError && <div className="error-text">{batchTagError}</div>}
            <div className="modal-actions">
              <button type="button" className="btn" onClick={() => setBatchTagOpen(false)}>{msg('cancel')}</button>
              <button type="submit" className="btn primary" disabled={batchTagBusy || !batchTagId}>
                {batchTagBusy ? msg('loading') : msg('apply')}
              </button>
            </div>
          </form>
        </Modal>
      )}

      {copyTarget && copyFn && (
        <Modal title={formatMessage(msg('copyTitle'), { name: copyTarget.name })} onClose={() => setCopyTarget(null)}>
          <form onSubmit={handleCopy}>
            <label className="field">
              <span>{msg('copyTargetLabel')}</span>
              <input
                autoFocus
                value={copyParent}
                onChange={(e) => setCopyParent(e.target.value)}
                placeholder="3f0c9c2e-…"
              />
            </label>
            {copyError && <div className="error-text">{copyError}</div>}
            <div className="modal-actions">
              <button type="button" className="btn" onClick={() => setCopyTarget(null)}>{msg('cancel')}</button>
              <button type="submit" className="btn primary" disabled={copyBusy}>
                {copyBusy ? msg('loading') : msg('copy')}
              </button>
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
              <input
                value={tagModalNewName}
                onChange={(e) => setTagModalNewName(e.target.value)}
                placeholder="新标签名称（≤64 字符）"
                maxLength={64}
              />
              <button type="submit" className="btn" disabled={tagModalBusy || !tagModalNewName.trim()}>
                创建并打标
              </button>
            </form>
            {tagModalError && <div className="error-text">{tagModalError}</div>}
          </div>
        </Modal>
      )}

      {previewTarget && (
        <Modal wide className="modal-viewer" title={`查看「${previewTarget.name}」`} onClose={closePreview}>
          <div className="preview-open-actions">
            <button className="btn small" onClick={() => openEditorWindow(routeFor('view', previewTarget))}>新窗口打开</button>
            <button className="btn small" onClick={() => void handleDownload(previewTarget)}>下载</button>
          </div>
          {/* 弹窗内嵌完整查看器（与独立查看页同一分发器）：office/
              drawio/白板/xmind/mermaid/md/网页/文本等全部类型就地渲染。 */}
          <div className="preview-embed">
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
          </div>
        </Modal>
      )}

      {/* 管理默认打开方式：列出已配置 ext→opener，可即时修改（PUT）或删除（DELETE）。 */}
      {openWithMgrOpen && (
        <Modal wide title={locale === 'zh-CN' ? '管理默认打开方式' : 'Manage default openers'} onClose={() => setOpenWithMgrOpen(false)}>
          <p className="hint">
            {locale === 'zh-CN'
              ? '按扩展名保存的默认打开方式；修改即时保存，删除后恢复按文件类型自动选择。'
              : 'Per-extension default openers. Changes save immediately; deleting restores automatic selection by file type.'}
          </p>
          {Object.keys(openWith).length === 0 ? (
            <p className="hint">
              {locale === 'zh-CN'
                ? '暂无已配置项。在文件菜单「打开方式」中选择即可保存为默认。'
                : 'No defaults configured yet. Pick one from a file’s “Open with” menu to save.'}
            </p>
          ) : (
            <div className="openwith-mgr">
              {Object.entries(openWith)
                .sort(([a], [b]) => a.localeCompare(b))
                .map(([ext, opener]) => (
                  <div key={ext} className="openwith-mgr-row">
                    <span className="openwith-mgr-ext">.{ext}</span>
                    <select
                      value={opener}
                      aria-label={`.${ext}`}
                      onChange={(e) => void handleMgrChange(ext, e.target.value as OpenWithOpener)}
                    >
                      {ALL_OPENERS.map((op) => (
                        <option key={op} value={op}>{openerLabel(op, locale === 'zh-CN')}</option>
                      ))}
                    </select>
                    <button type="button" className="btn small danger" onClick={() => void handleMgrDelete(ext)}>
                      {msg('delete')}
                    </button>
                  </div>
                ))}
            </div>
          )}
          {openWithMgrError && <div className="error-text">{openWithMgrError}</div>}
          <div className="modal-actions">
            <button type="button" className="btn" onClick={() => setOpenWithMgrOpen(false)}>{msg('close')}</button>
          </div>
        </Modal>
      )}

      {/* 右键 / 列表行「⋯」菜单：视口定位浮层，内容与网格卡片菜单一致。 */}
      {ctxMenu && (
        <div
          className="ctx-menu file-card-menu"
          role="menu"
          style={{
            left: `${Math.max(8, Math.min(ctxMenu.x, window.innerWidth - 228))}px`,
            top: `${Math.max(8, Math.min(ctxMenu.y, window.innerHeight - 360))}px`,
          }}
          onClick={() => setCtxMenu(null)}
        >
          {itemMenuContent(ctxMenu.item)}
        </div>
      )}
    </div>
  )
}
