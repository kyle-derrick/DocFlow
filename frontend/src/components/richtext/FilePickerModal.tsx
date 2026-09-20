// 插入面板的文件选择对话框：
// - 范围：我的文件（搜索走 /search 名称+内容、空关键词时展示最近访问）
//   + 可选其他空间（根目录清单，客户端名称过滤）；
// - 类型过滤：drawio / excalidraw / image / any（按扩展名）；
// - 图片模式附本地上传：上传到 md 所在目录的 assets/ 子目录（不存在自动
//   创建，复用既有上传 API），完成后同样回调 onPick；
// - 无「新建」入口（按约定只选已有文件；drawio/白板新建走文件页「新建」菜单）。
import { useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { Button, Input, Select, Upload as AntdUpload } from 'antd'
import { FileText, Image as ImageIcon, Upload } from 'lucide-react'
import {
  Space,
  UploadPhase,
  createFolder,
  createSpaceFolder,
  listFiles,
  listSpaceFiles,
  listSpaces,
  resolveNamespaceOf,
  searchFiles,
  uploadFile,
} from '../../api'
import { useLocale } from '../../i18n'
import { Modal, formatTime } from '../FileBrowser'

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

/** 选择器行（listFiles / searchFiles / listSpaceFiles 三来源的最小公共字段）。 */
interface PickerRow {
  id: string
  name: string
  updated_at: string
}

/** 查找（或创建）parentId 下的 assets/ 子目录；ns 提供时走空间端点
 *（listSpaceFiles / createSpaceFolder），否则默认空间端点。parentId 为 null 时位于默认空间根目录。 */
export async function ensureAssetsFolder(
  parentId: string | null,
  ns?: { type: 'space'; scope: string } | null,
): Promise<string | null> {
  const find = async () => {
    const items = ns
      ? (await listSpaceFiles(ns.scope, parentId)).files
      : await listFiles(parentId)
    return items.find((it) => it.type === 'folder' && it.name === 'assets') ?? null
  }
  const existing = await find()
  if (existing) return existing.id
  try {
    const created = ns
      ? await createSpaceFolder(ns.scope, 'assets', parentId)
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
  const [spaces, setSpaces] = useState<Space[]>([])
  const [items, setItems] = useState<PickerRow[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [uploadPhase, setUploadPhase] = useState<UploadPhase | ''>('')

  // 打开时加载空间列表（可选范围来源；默认空间并入「我的文件」）。
  useEffect(() => {
    if (!open) return
    setQuery('')
    setScope('mine')
    setError('')
    setUploadPhase('')
    void listSpaces().then(setSpaces).catch(() => setSpaces([]))
  }, [open])

  // 列表加载：我的文件（搜索/最近）或空间根目录（客户端过滤）。
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
          const files = (await listSpaceFiles(scope, null)).files
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

  // 图片本地上传（v2.2 antd Upload：beforeUpload 拦截自管）：定位/创建
  // assets 目录（按 md 所在目录探测命名空间分发端点）→ 上传 → 以嵌入块
  // 引用回调。
  const handleUpload = async (file: File) => {
    if (!file) return
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
    <Modal wide title={title} onClose={onClose}>
      <form className="rich-text-picker" onSubmit={onSubmit}>
        <div className="rich-text-picker-toolbar">
          <Select
            value={scope}
            onChange={(v) => setScope(v)}
            aria-label={zh ? '范围' : 'Scope'}
            options={[
              { value: 'mine', label: zh ? '我的文件' : 'My files' },
              ...spaces.filter((s) => !s.is_default).map((s) => ({ value: s.id, label: s.name })),
            ]}
          />
          <Input
            autoFocus
            allowClear
            value={query}
            placeholder={scope === 'mine'
              ? (zh ? '按名称搜索（空 = 最近文件）' : 'Search by name (empty = recent)')
              : (zh ? '在空间根目录筛选名称' : 'Filter names in space root')}
            onChange={(e) => setQuery(e.target.value)}
          />
          {filter === 'image' && (
            <AntdUpload
              accept="image/*"
              maxCount={1}
              showUploadList={false}
              beforeUpload={(f) => {
                void handleUpload(f)
                return false
              }}
            >
              <Button disabled={uploadPhase !== ''} loading={uploadPhase !== ''}>
                <Upload size={14} strokeWidth={2} aria-hidden="true" /> {zh ? '上传到 assets/' : 'Upload to assets/'}
              </Button>
            </AntdUpload>
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
    </Modal>
  )
}
