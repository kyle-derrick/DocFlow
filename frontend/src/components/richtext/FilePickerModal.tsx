// 插入面板的文件选择对话框：
// - 范围：我的文件（搜索走 /search 名称+内容、空关键词时展示最近访问）
//   + 可选团队空间（根目录清单，客户端名称过滤）；
// - 类型过滤：drawio / excalidraw / image / any（按扩展名）；
// - 图片模式附本地上传：上传到 md 所在目录的 assets/ 子目录（不存在自动
//   创建，复用既有上传 API），完成后同样回调 onPick；
// - 无「新建」入口（按约定只选已有文件；drawio/白板新建走文件页「新建」菜单）。
import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { FileText, Image as ImageIcon, Upload } from 'lucide-react'
import {
  Team,
  UploadPhase,
  createFolder,
  createTeamFolder,
  listFiles,
  listTeamFiles,
  listTeams,
  resolveNamespaceOf,
  searchFiles,
  uploadFile,
} from '../../api'
import { useLocale } from '../../i18n'
import { formatTime } from '../FileBrowser'

export type PickerFilter = 'drawio' | 'excalidraw' | 'image' | 'any'

export interface PickedFile {
  id: string
  name: string
}

export interface FilePickerModalProps {
  open: boolean
  filter: PickerFilter
  onPick: (file: PickedFile) => void
  onClose: () => void
  /** md 文件所在目录 ID；图片本地上传的目标（其下 assets/ 子目录）。 */
  uploadParentId?: string | null
}

const IMAGE_EXTS = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'avif', 'svg']

function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i >= 0 ? name.slice(i + 1).toLowerCase() : ''
}

function matchFilter(name: string, filter: PickerFilter): boolean {
  const ext = extOf(name)
  if (filter === 'drawio') return ext === 'drawio'
  if (filter === 'excalidraw') return ext === 'excalidraw'
  if (filter === 'image') return IMAGE_EXTS.includes(ext)
  return true
}

const FILTER_TITLES: Record<PickerFilter, { zh: string; en: string }> = {
  drawio: { zh: '选择 draw.io 图表', en: 'Pick a draw.io diagram' },
  excalidraw: { zh: '选择 Excalidraw 白板', en: 'Pick an Excalidraw whiteboard' },
  image: { zh: '选择图片', en: 'Pick an image' },
  any: { zh: '选择文件', en: 'Pick a file' },
}

/** 选择器行（listFiles / searchFiles / listTeamFiles 三来源的最小公共字段）。 */
interface PickerRow {
  id: string
  name: string
  updated_at: string
}

/** 查找（或创建）parentId 下的 assets/ 子目录；ns=team 时走团队端点
 *（listTeamFiles / createTeamFolder），否则个人端点。parentId 为 null 时位于个人根目录。 */
export async function ensureAssetsFolder(
  parentId: string | null,
  ns?: { type: 'personal' | 'team'; scope: string } | null,
): Promise<string | null> {
  const find = async () => {
    const items = ns?.type === 'team'
      ? (await listTeamFiles(ns.scope, parentId)).files
      : await listFiles(parentId)
    return items.find((it) => it.type === 'folder' && it.name === 'assets') ?? null
  }
  const existing = await find()
  if (existing) return existing.id
  try {
    const created = ns?.type === 'team'
      ? await createTeamFolder(ns.scope, 'assets', parentId)
      : await createFolder('assets', parentId)
    return created.id
  } catch {
    // 并发创建冲突等场景：再查一次拿既有目录。
    const again = await find()
    return again?.id ?? null
  }
}

export default function FilePickerModal({ open, filter, onPick, onClose, uploadParentId }: FilePickerModalProps) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const [query, setQuery] = useState('')
  const [scope, setScope] = useState<'mine' | string>('mine')
  const [teams, setTeams] = useState<Team[]>([])
  const [items, setItems] = useState<PickerRow[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [uploadPhase, setUploadPhase] = useState<UploadPhase | ''>('')
  const fileInputRef = useRef<HTMLInputElement>(null)

  // 打开时加载团队列表（可选空间来源）。
  useEffect(() => {
    if (!open) return
    setQuery('')
    setScope('mine')
    setError('')
    setUploadPhase('')
    void listTeams().then(setTeams).catch(() => setTeams([]))
  }, [open])

  // 列表加载：我的空间（搜索/最近）或团队根目录（客户端过滤）。
  useEffect(() => {
    if (!open) return
    let alive = true
    setLoading(true)
    setError('')
    const load = async () => {
      try {
        let list: PickerRow[]
        const q = query.trim()
        if (scope === 'mine') {
          list = q
            ? (await searchFiles(q, 50))
                .filter((r) => r.type === 'file')
                .map((r) => ({ id: r.id, name: r.name, updated_at: r.updated_at }))
            : (await listFiles(null, { recent: true }))
                .filter((f) => f.type === 'file')
                .slice(0, 30)
                .map((f) => ({ id: f.id, name: f.name, updated_at: f.updated_at }))
        } else {
          const files = (await listTeamFiles(scope, null)).files
          list = files
            .filter((f) => f.type === 'file')
            .map((f) => ({ id: f.id, name: f.name, updated_at: f.updated_at }))
          if (q) list = list.filter((it) => it.name.toLowerCase().includes(q.toLowerCase()))
        }
        if (!alive) return
        setItems(list.filter((it) => matchFilter(it.name, filter)))
      } catch (err) {
        if (alive) {
          setItems([])
          setError(err instanceof Error ? err.message : (zh ? '加载失败' : 'Failed to load'))
        }
      } finally {
        if (alive) setLoading(false)
      }
    }
    const timer = window.setTimeout(() => void load(), query ? 300 : 0)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, scope, query, filter])

  if (!open) return null

  const title = zh ? FILTER_TITLES[filter].zh : FILTER_TITLES[filter].en

  // 图片本地上传：定位/创建 assets 目录（按 md 所在目录探测个人/团队空间分发
  // 端点）→ 上传 → 以嵌入块引用回调。
  const handleUpload = async (files: FileList | null) => {
    const file = files?.[0]
    if (!file) return
    if (fileInputRef.current) fileInputRef.current.value = ''
    if (filter !== 'image' || !IMAGE_EXTS.includes(extOf(file.name))) {
      setError(zh ? '仅支持上传图片文件' : 'Only image files are supported')
      return
    }
    setUploadPhase('creating')
    setError('')
    try {
      const parentId = uploadParentId ?? null
      const ns = parentId ? await resolveNamespaceOf(parentId) : null
      const assetsId = await ensureAssetsFolder(parentId, ns)
      if (!assetsId) throw new Error(zh ? '无法定位 assets 目录' : 'Cannot locate assets folder')
      setUploadPhase('uploading')
      const session = await uploadFile(file, assetsId, (phase) => setUploadPhase(phase))
      if (!session.file_id) throw new Error(zh ? '上传完成但未返回文件 ID' : 'Upload finished without file id')
      onPick({ id: session.file_id, name: file.name })
    } catch (err) {
      setError(err instanceof Error ? err.message : (zh ? '上传失败' : 'Upload failed'))
    } finally {
      setUploadPhase('')
    }
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal wide" onClick={(e) => e.stopPropagation()}>
        <div className="modal-head">
          <h3>{title}</h3>
          <button type="button" className="btn ghost" onClick={onClose} aria-label="关闭">×</button>
        </div>
        <form className="rich-text-picker" onSubmit={onSubmit}>
          <div className="rich-text-picker-toolbar">
            <select value={scope} onChange={(e) => setScope(e.target.value)} aria-label={zh ? '范围' : 'Scope'}>
              <option value="mine">{zh ? '我的文件' : 'My files'}</option>
              {teams.map((team) => (
                <option key={team.id} value={team.id}>{zh ? '团队' : 'Team'} · {team.name}</option>
              ))}
            </select>
            <input
              type="search"
              autoFocus
              value={query}
              placeholder={scope === 'mine'
                ? (zh ? '按名称搜索（空 = 最近文件）' : 'Search by name (empty = recent)')
                : (zh ? '在团队空间根目录筛选名称' : 'Filter names in team root')}
              onChange={(e) => setQuery(e.target.value)}
            />
            {filter === 'image' && (
              <>
                <button
                  type="button"
                  className="btn"
                  disabled={uploadPhase !== ''}
                  onClick={() => fileInputRef.current?.click()}
                >
                  {uploadPhase
                    ? (zh ? '上传中…' : 'Uploading…')
                    : (<><Upload size={14} strokeWidth={2} aria-hidden="true" /> {zh ? '上传到 assets/' : 'Upload to assets/'}</>)}
                </button>
                <input
                  ref={fileInputRef}
                  type="file"
                  accept="image/*"
                  hidden
                  onChange={(e) => void handleUpload(e.target.files)}
                />
              </>
            )}
          </div>
          {error && <div className="error-text">{error}</div>}
          <div className="rich-text-picker-list">
            {loading && <div className="hint">{zh ? '加载中…' : 'Loading…'}</div>}
            {!loading && items.length === 0 && !error && (
              <div className="empty">{zh ? '没有匹配的文件' : 'No matching files'}</div>
            )}
            {!loading && items.map((item) => (
              <button
                key={item.id}
                type="button"
                className="rich-text-picker-item"
                onClick={() => onPick({ id: item.id, name: item.name })}
              >
                <span className="icon">{filter === 'image'
                  ? <ImageIcon size={14} strokeWidth={2} aria-hidden="true" />
                  : <FileText size={14} strokeWidth={2} aria-hidden="true" />}</span>
                <span className="rich-text-picker-name">{item.name}</span>
                <span className="muted">{formatTime(item.updated_at)}</span>
              </button>
            ))}
          </div>
        </form>
      </div>
    </div>
  )
}
