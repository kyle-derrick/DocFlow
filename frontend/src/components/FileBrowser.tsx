// 通用文件浏览组件：从 FilesPage 提炼的目录列表 / 面包屑 / 上传 / 下载 /
// 预览（含 office 文档的 ONLYOFFICE「编辑」入口）/ 新建文件夹逻辑，
// 个人空间与团队空间共用。
// 通过注入 listItems / createFolderFn / uploadFn / downloadFn / previewFn
// 适配不同后端端点；写操作 403 时统一提示「无写权限」。
import { FormEvent, ReactNode, useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ApiError,
  FileItem,
  PreviewContent,
  PreviewKind,
  UploadPhase,
  downloadFile,
  fetchPreview,
  isOfficeFile,
  onlyOfficeStatus,
} from '../api'

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
  listItems: (parentId: string | null) => Promise<DirListing>
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
}: FileBrowserProps) {
  const doDownload = downloadFn ?? downloadFile
  const doPreview = previewFn ?? fetchPreview
  const navigate = useNavigate()

  const [crumbs, setCrumbs] = useState<Crumb[]>([{ id: null, folderId: null, name: rootLabel }])
  const [items, setItems] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

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

  const load = async (parentId: string | null) => {
    setLoading(true)
    setError('')
    try {
      const { items: list, folderId } = await listItems(parentId)
      setItems(sortItems(list))
      if (folderId) {
        // 团队根目录：列表响应回填真实目录 ID，供上传/建目录使用。
        setCrumbs((prev) => prev.map((c, i) => (i === prev.length - 1 ? { ...c, folderId } : c)))
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败')
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(null)
  }, [])

  useEffect(() => {
    if (reloadKey) void load(currentParent)
  }, [reloadKey])

  const openFolder = (item: FileItem) => {
    setCrumbs((prev) => [...prev, { id: item.id, folderId: item.id, name: item.name }])
    void load(item.id)
  }

  const gotoCrumb = (index: number) => {
    setCrumbs((prev) => prev.slice(0, index + 1))
    void load(crumbs[index].id)
  }

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

  return (
    <div className="file-browser">
      <div className="page-head">
        <h2>{title}</h2>
        <div className="toolbar">
          {createFolderFn && (
            <button className="btn" onClick={() => { setFolderOpen(true); setFolderName(''); setFolderError('') }}>
              ＋ 新建文件夹
            </button>
          )}
          {uploadFn && (
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

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">加载中…</div>}
      {!loading && items.length === 0 && !error && (
        <div className="empty">{emptyHint ?? '此目录为空，上传文件或新建文件夹开始使用'}</div>
      )}

      {items.length > 0 && (
        <table className="file-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>修改时间</th>
              <th className="col-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.id}>
                <td>
                  {item.type === 'folder' ? (
                    <button className="name-btn" onClick={() => openFolder(item)}>
                      <span className="icon">📁</span>
                      {item.name}
                    </button>
                  ) : (
                    <button className="name-btn" title="预览" onClick={() => void openPreview(item)}>
                      <span className="icon">📄</span>
                      {item.name}
                    </button>
                  )}
                </td>
                <td className="muted">{formatTime(item.updated_at)}</td>
                <td className="col-actions">
                  {item.type === 'file' && ooEnabled && isOfficeFile(item.name) && (
                    <button className="btn small" title="ONLYOFFICE 在线编辑" onClick={() => navigate(`/edit/${item.id}`)}>
                      编辑
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
