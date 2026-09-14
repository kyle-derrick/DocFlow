// 通用文件浏览组件：从 FilesPage 提炼的目录列表 / 面包屑 / 上传 / 下载 /
// 预览（含 office 文档的 ONLYOFFICE「编辑」入口）/ 新建文件夹逻辑，
// 个人空间与团队空间共用。
// 通过注入 listItems / createFolderFn / uploadFn / downloadFn / previewFn
// 适配不同后端端点；写操作 403 时统一提示「无写权限」。
// v1.0 追加：多选 + 批量移动/删除（部分成功语义）、行内星标切换、
// 行内标签管理（打/去标签、新建）、顶栏标签/收藏筛选与服务端排序。
import { FormEvent, ReactNode, useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ApiError,
  BatchItemResult,
  EMPTY_DRAWIO_XML,
  FileItem,
  FileQueryOptions,
  PreviewContent,
  PreviewKind,
  Tag,
  UploadPhase,
  addFileTag,
  batchMoveFiles,
  batchTrashFiles,
  createShare,
  createTag,
  downloadBatchFiles,
  downloadFile,
  drawioStatus,
  fetchPreview,
  isDrawioFile,
  isOfficeFile,
  listFileTags,
  listTags,
  onlyOfficeStatus,
  removeFileTag,
  setFileStarred,
  summarizeBatchResults,
} from '../api'
import { useHotkeys } from '../useHotkeys'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

export function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

export function Modal({
  title,
  onClose,
  wide,
  children,
}: {
  title: string
  onClose: () => void
  wide?: boolean
  children: ReactNode
}) {
  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className={`modal${wide ? ' wide' : ''}`} onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          <h3>{title}</h3>
          <button className="btn ghost" onClick={onClose} aria-label="关闭">×</button>
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
  /** 预览实现，缺省走个人文件端点。 */
  previewFn?: (fileId: string) => Promise<PreviewContent>
  /** 每行追加操作按钮（分享 / 重命名 / 删除等由调用方渲染）。 */
  rowActions?: (item: FileItem) => ReactNode
  emptyHint?: string
  /** 变化时重新加载当前目录（外部操作成功后刷新列表用）。 */
  reloadKey?: number
  /** 提供时批量移动对话框含「根目录」选项（个人空间；空目标即个人根）。 */
  rootTargetLabel?: string
  /** 提供时顶部显示视图切换（全部 / 收藏 / 最近，复用 ?starred= 与 ?recent= 参数）。 */
  viewTabs?: boolean
  /** 提供时文件行显示「复制」（parentId 为目标目录 UUID；留空目标由本组件解析为源目录）。 */
  copyFn?: (fileId: string, parentId: string) => Promise<unknown>
}

export default function FileBrowser({
  title,
  rootLabel,
  listItems,
  createFolderFn,
  uploadFn,
  downloadFn,
  previewFn,
  rowActions,
  emptyHint,
  reloadKey,
  rootTargetLabel,
  viewTabs,
  copyFn,
}: FileBrowserProps) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const doDownload = downloadFn ?? downloadFile
  const doPreview = previewFn ?? fetchPreview
  const navigate = useNavigate()

  const [crumbs, setCrumbs] = useState<Crumb[]>([{ id: null, folderId: null, name: rootLabel }])
  const [items, setItems] = useState<FileItem[]>([])
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
  const [batchNotice, setBatchNotice] = useState('')
  const [batchError, setBatchError] = useState('')
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

  const [folderOpen, setFolderOpen] = useState(false)
  const [folderName, setFolderName] = useState('')
  const [folderError, setFolderError] = useState('')

  const [previewTarget, setPreviewTarget] = useState<FileItem | null>(null)
  const [previewLoading, setPreviewLoading] = useState(false)
  const [previewKindState, setPreviewKindState] = useState<PreviewKind | null>(null)
  const [previewUrl, setPreviewUrl] = useState('')
  const [previewText, setPreviewText] = useState('')
  const [previewError, setPreviewError] = useState('')

  const [uploads, setUploads] = useState<UploadRow[]>([])
  const fileInputRef = useRef<HTMLInputElement>(null)
  const uploadKey = useRef(0)

  const currentParent = crumbs[crumbs.length - 1].id
  const currentFolderId = crumbs[crumbs.length - 1].folderId

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

  // 视图切换（全部 / 收藏 / 最近）：与下方标签、收藏筛选联动——收藏视图即
  // starred=true；最近视图走 ?recent=true（后端忽略其余过滤）；其余清空。
  const activeView: 'all' | 'starred' | 'recent' = recentView ? 'recent' : starredFilter === 'true' ? 'starred' : 'all'
  const setView = (view: 'all' | 'starred' | 'recent') => {
    setRecentView(view === 'recent')
    setTagFilter('')
    setStarredFilter(view === 'starred' ? 'true' : '')
  }

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

  // ---- 批量分享（逐个创建公开分享，完成后弹链接列表） ----

  const handleBatchShare = async () => {
    const targets = items.filter((it) => selected.has(it.id) && it.type === 'file')
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

  const closePreview = () => {
    setPreviewLoading(false)
    setPreviewKindState(null)
    if (previewUrl) URL.revokeObjectURL(previewUrl)
    setPreviewUrl('')
    setPreviewText('')
    setPreviewError('')
    setPreviewTarget(null)
  }

  const openPreview = async (item: FileItem) => {
    if (previewUrl) URL.revokeObjectURL(previewUrl)
    setPreviewTarget(item)
    setPreviewLoading(true)
    setPreviewKindState(null)
    setPreviewUrl('')
    setPreviewText('')
    setPreviewError('')
    try {
      const content = await doPreview(item.id)
      setPreviewKindState(content.kind)
      setPreviewUrl(content.url ?? '')
      setPreviewText(content.text ?? '')
    } catch (err) {
      setPreviewError(err instanceof Error ? err.message : '预览加载失败')
    } finally {
      setPreviewLoading(false)
    }
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

  // 新建图表：在当前目录上传「diagram-<timestamp>.drawio」初始模板（普通
  // 文件上传，无需后端专用端点），完成后按文件名定位新文件并跳转编辑页。
  const [diagramCreating, setDiagramCreating] = useState(false)
  const handleCreateDiagram = async () => {
    if (!uploadFn || diagramCreating) return
    const name = `diagram-${Date.now()}.drawio`
    const blob = new File([EMPTY_DRAWIO_XML], name, { type: 'text/xml' })
    setDiagramCreating(true)
    setError('')
    const key = ++uploadKey.current
    setUploads((prev) => [...prev, { key, name, phase: 'creating' }])
    try {
      await uploadFn(blob, currentFolderId, (phase) => {
        setUploads((prev) => prev.map((r) => (r.key === key ? { ...r, phase } : r)))
      })
      const { items: list } = await listItems(currentFolderId)
      const created = list.find((it) => it.type === 'file' && it.name === name)
      if (created) {
        navigate(`/drawio/${created.id}`)
      } else {
        await load(currentParent)
      }
    } catch (err) {
      setUploads((prev) =>
        prev.map((r) =>
          r.key === key ? { ...r, phase: 'error', error: writeErrorText(err, '创建图表失败') } : r,
        ),
      )
    } finally {
      setDiagramCreating(false)
    }
  }

  // 页面快捷键（v1.1）：n 新建文件夹 / u 上传 / Delete 删除选中 /
  // Escape 依次关弹窗（预览→标签→移动→新建文件夹），无弹窗时清空选择。
  useHotkeys({
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

  return (
    <div className="file-browser">
      <div className="page-head">
        <h2>{title}</h2>
        <div className="toolbar">
          {createFolderFn && !searchMode && (
            <button className="btn" onClick={() => { setFolderOpen(true); setFolderName(''); setFolderError('') }}>
              ＋ 新建文件夹
            </button>
          )}
          {uploadFn && drawioEnabled && !searchMode && (
            <button className="btn" disabled={diagramCreating} onClick={() => void handleCreateDiagram()}>
              {diagramCreating ? (locale === 'zh-CN' ? '创建图表中…' : 'Creating…') : locale === 'zh-CN' ? '✎ 新建图表' : '✎ New diagram'}
            </button>
          )}
          {uploadFn && !searchMode && (
            <>
              <button className="btn primary" onClick={() => fileInputRef.current?.click()}>⬆ 上传文件</button>
              <input
                ref={fileInputRef}
                type="file"
                multiple
                hidden
                onChange={(e) => void handleFilesPicked(e.target.files)}
              />
            </>
          )}
        </div>
      </div>

      {/* 顶栏筛选：视图切换（全部/收藏/最近）+ 标签 / 收藏 / 排序（跨目录检索或最近访问模式） */}
      <div className="filter-bar">
        {viewTabs && (
          <div className="seg-group view-tabs" role="tablist">
            {(['all', 'starred', 'recent'] as const).map((view) => (
              <button
                key={view}
                type="button"
                role="tab"
                aria-selected={activeView === view}
                className={`seg${activeView === view ? ' active' : ''}`}
                onClick={() => setView(view)}
              >
                {view === 'all' ? msg('viewAll') : view === 'starred' ? msg('viewStarred') : msg('viewRecent')}
              </button>
            ))}
          </div>
        )}
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
        <label className="filter-item">
          <span>{msg('sort')}</span>
          <select value={sortKey} onChange={(e) => setSortKey(e.target.value as 'name' | 'updated_at' | 'size')}>
            <option value="name">{msg('sortOrderName')}</option>
            <option value="updated_at">{msg('sortOrderUpdated')}</option>
            <option value="size">{msg('sortOrderSize')}</option>
          </select>
        </label>
        <label className="filter-item">
          <span>{msg('direction')}</span>
          <select value={sortOrder} onChange={(e) => setSortOrder(e.target.value as 'asc' | 'desc')}>
            <option value="asc">{msg('orderAsc')}</option>
            <option value="desc">{msg('orderDesc')}</option>
          </select>
        </label>
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

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && items.length === 0 && !error && (
        <div className="empty">{searchMode ? msg('noMatch') : emptyHint ?? (locale === 'zh-CN' ? '此目录为空，上传文件或新建文件夹开始使用' : 'This folder is empty. Upload a file or create a folder to get started.')}</div>
      )}

      {items.length > 0 && (
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
              <th>{msg('name')}</th>
              <th>{msg('sortOrderUpdated')}</th>
              <th className="col-actions">{msg('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.id} className={selected.has(item.id) ? 'selected' : ''}>
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
                    <button className="name-btn" onClick={() => openFolder(item)}>
                      <span className="icon">📁</span>
                      {item.name}
                    </button>
                  ) : item.type === 'folder' ? (
                    <span className="name-btn muted">
                      <span className="icon">📁</span>
                      {item.name}
                    </span>
                  ) : (
                    <button className="name-btn" title="预览" onClick={() => void openPreview(item)}>
                      <span className="icon">📄</span>
                      {item.name}
                    </button>
                  )}
                </td>
                <td className="muted">{formatTime(item.updated_at)}</td>
                <td className="col-actions">
                  <button className="btn small" onClick={() => void openTagModal(item)}>{msg('tag')}</button>
                  {item.type === 'file' && copyFn && (
                    <button className="btn small" onClick={() => openCopyDialog(item)}>{msg('copy')}</button>
                  )}
                  {item.type === 'file' && ooEnabled && isOfficeFile(item.name) && (
                    <button className="btn small" title="ONLYOFFICE 在线编辑" onClick={() => navigate(`/edit/${item.id}`)}>
                      编辑
                    </button>
                  )}
                  {item.type === 'file' && drawioEnabled && isDrawioFile(item.name) && (
                    <button className="btn small" title="draw.io 图表编辑" onClick={() => navigate(`/drawio/${item.id}`)}>
                      图表
                    </button>
                  )}
                  {item.type === 'file' && (
                    <button className="btn small" onClick={() => void handleDownload(item)}>下载</button>
                  )}
                  {rowActions?.(item)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
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
        <Modal wide title={`预览「${previewTarget.name}」`} onClose={closePreview}>
          {previewLoading ? (
            <p className="hint">加载预览…</p>
          ) : previewError ? (
            <div>
              <div className="error-text">{previewError}</div>
              <div className="preview-foot">
                <button className="btn primary" onClick={() => void handleDownload(previewTarget)}>下载</button>
              </div>
            </div>
          ) : previewKindState === 'unsupported' ? (
            <div>
              <div className="empty">该文件类型暂不支持在线预览，请下载后查看</div>
              <div className="preview-foot">
                <button className="btn primary" onClick={() => void handleDownload(previewTarget)}>下载</button>
              </div>
            </div>
          ) : previewKindState === 'image' ? (
            <div className="preview-box">
              <img className="preview-image" src={previewUrl} alt={previewTarget.name} />
            </div>
          ) : previewKindState === 'pdf' ? (
            <div className="preview-box">
              <iframe className="preview-frame" src={previewUrl} title={previewTarget.name} />
            </div>
          ) : previewKindState === 'webpkg' ? (
            // 网页包：sandbox 不含 allow-same-origin（唯一化 origin 沙箱），
            // 内容端点带严格 CSP/nosniff，且不携带主站认证信息。
            <div className="preview-box">
              <iframe className="preview-frame" sandbox="allow-scripts" src={previewUrl} title={previewTarget.name} />
            </div>
          ) : previewKindState === 'text' ? (
            <div className="preview-box">
              <pre className="preview-text">{previewText}</pre>
            </div>
          ) : null}
        </Modal>
      )}
    </div>
  )
}
