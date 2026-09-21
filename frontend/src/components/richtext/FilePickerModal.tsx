// 插入面板的文件选择对话框（v2.7 目录树版）：
// - 布局：左侧目录树（antd Tree 懒加载子目录）+ 右侧当前目录文件列表 +
//   面包屑（.rich-text-picker-layout）；顶部范围切换（我的文件 = 默认空间 /
//   其他空间）与名称过滤；
// - 类型过滤：drawio / excalidraw / image / any（按扩展名）；无匹配时提示；
// - 图片模式附本地上传：上传到文档所在目录的 assets/ 子目录（不存在自动
//   创建，复用既有上传 API），完成后同样回调 onPick；
// - allowCreate（drawio/excalidraw 过滤时）：附「新建白图/新建白板」——
//   以空模板上传到文档所在目录（缺省空间根）后回调 onPick（嵌入引用）。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Breadcrumb, Button, Input, Select, Spin, Tree, Upload as AntdUpload } from 'antd'
import type { DataNode } from 'antd/es/tree'
import { FilePlus2, FileText, Folder, FolderOpen, Image as ImageIcon, Upload } from 'lucide-react'
import {
  EMPTY_DRAWIO_XML,
  EMPTY_EXCALIDRAW_JSON,
  Space,
  UploadPhase,
  createFolder,
  createSpaceFolder,
  listFiles,
  listSpaceFiles,
  listSpaces,
  resolveNamespaceOf,
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
  /** 文档所在目录 ID；图片本地上传/新建白图的目标（其下 assets/ 子目录）。 */
  uploadParentId?: string | null
  /** drawio/excalidraw 过滤时显示「新建白图/白板」按钮（空模板上传后回调）。 */
  allowCreate?: boolean
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

/** 选择器行（目录/文件条目的最小公共字段）。 */
interface PickerRow {
  id: string
  name: string
  updated_at: string
}

/** 目录节点（树 + 面包屑共用）。 */
interface DirNode {
  id: string
  name: string
  parent: DirNode | null
}

/** 查找（或创建）parentId 下的 assets/ 子目录；ns 提供时走空间端点
 *（listSpaceFiles / createSpaceFolder），否则默认空间端点。parentId 为 null 时位于默认空间根目录。 */
export async function ensureAssetsFolder(
  parentId: string | null,
  ns?: { type: 'space'; scope: string } | null,
): Promise<string | null> {
  const find = async () => {
    const items = ns
      ? (await listSpaceFiles(ns.scope, parentId, { limit: 1000 })).files
      : await listFiles(parentId, { limit: 1000 })
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

export default function FilePickerModal({ open, filter, onPick, onClose, uploadParentId, allowCreate = false }: FilePickerModalProps) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const [query, setQuery] = useState('')
  const [scope, setScope] = useState<'mine' | string>('mine')
  const [spaces, setSpaces] = useState<Space[]>([])
  // 当前目录（null = 所选范围根）。切换范围/打开时复位。
  const [current, setCurrent] = useState<DirNode | null>(null)
  const [files, setFiles] = useState<PickerRow[] | null>(null)
  const [folders, setFolders] = useState<PickerRow[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [uploadPhase, setUploadPhase] = useState<UploadPhase | ''>('')
  // 空间命名空间（scope!=='mine' 时为 {type:'space',scope}；否则 null=默认空间）。
  const ns = useMemo(() => (scope !== 'mine' ? { type: 'space' as const, scope } : null), [scope])

  const listDir = useCallback(async (dirId: string | null): Promise<{ files: PickerRow[]; folders: PickerRow[] }> => {
    // limit 1000：默认 limit=100 会截断多子项目录（后端钳制 1..1000）。
    const items = ns ? (await listSpaceFiles(ns.scope, dirId, { limit: 1000 })).files : await listFiles(dirId, { limit: 1000 })
    const outFiles: PickerRow[] = []
    const outFolders: PickerRow[] = []
    for (const it of items as Array<{ id: string; name: string; type: string; updated_at: string }>) {
      const row = { id: it.id, name: it.name, updated_at: it.updated_at }
      if (it.type === 'folder') outFolders.push(row)
      else outFiles.push(row)
    }
    outFolders.sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'))
    return { files: outFiles, folders: outFolders }
  }, [ns])

  // 打开时加载空间列表与根目录；切换范围时复位目录并重载。
  useEffect(() => {
    if (!open) return
    setQuery('')
    setError('')
    setUploadPhase('')
    setCurrent(null)
    void listSpaces().then(setSpaces).catch(() => setSpaces([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  useEffect(() => {
    if (!open) return
    let alive = true
    setLoading(true)
    setError('')
    void listDir(current ? current.id : null)
      .then(({ files: fs, folders: dirs }) => {
        if (!alive) return
        setFiles(fs)
        setFolders(dirs)
      })
      .catch((err) => {
        if (alive) {
          setFiles([])
          setFolders([])
          setError(err instanceof Error ? err.message : (zh ? '加载失败' : 'Failed to load'))
        }
      })
      .finally(() => { if (alive) setLoading(false) })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, scope, current?.id, listDir])

  // 目录树数据（一级展开懒加载；key=id，title=名称）。
  const [treeData, setTreeData] = useState<DataNode[]>([])
  const loadedDirsRef = useRef(new Set<string>())
  // 受控展开：Tree 首挂载时 treeData 尚为空（open 后 effect 才填充），
  // rc-tree 会把 defaultExpandedKeys 中不存在的 key 丢弃导致根节点永不
  // 展开——改用受控 expandedKeys 常驻 ['__root__']。
  const [expandedKeys, setExpandedKeys] = useState<React.Key[]>(['__root__'])

  useEffect(() => {
    if (!open) return
    loadedDirsRef.current = new Set(['__root__'])
    setExpandedKeys(['__root__'])
    setTreeData([{ key: '__root__', title: scope === 'mine' ? (zh ? '我的文件' : 'My files') : (spaces.find((s) => s.id === scope)?.name ?? '空间') }])
    // 根节点初始展开不触发 loadData（rc-tree 仅在用户交互展开时回调），
    // 根级子目录在此即时加载，避免树初始为空。
    void loadChildren('__root__').then((children) => {
      setTreeData((prev) => prev.map((n) => (String(n.key) === '__root__' ? { ...n, children } : n)))
    }).catch(() => { /* 失败保持折叠 */ })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, scope, spaces.length])

  const loadChildren = useCallback(async (dirKey: string): Promise<DataNode[]> => {
    const dirId = dirKey === '__root__' ? null : dirKey
    const items = ns ? (await listSpaceFiles(ns.scope, dirId, { limit: 1000 })).files : await listFiles(dirId, { limit: 1000 })
    return items
      .filter((it) => it.type === 'folder')
      .sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'))
      .map((it) => ({
        key: it.id,
        title: it.name,
        icon: <Folder size={14} strokeWidth={2} aria-hidden="true" />,
        isLeaf: false,
      }))
  }, [ns])

  const onTreeSelect = (keys: React.Key[]) => {
    const key = keys[0]
    if (!key) return
    const dirId = String(key) === '__root__' ? null : String(key)
    void listDir(dirId).then(({ files: fs, folders: dirs }) => {
      setFiles(fs)
      setFolders(dirs)
      // 面包屑目录链：按当前树无法直接回溯父链，用轻量记录（进入目录时由
      // 文件列表「进入」按钮构造链；树选择直接以该目录为当前目录，面包屑
      // 仍可经“返回上级”逐级回退）。
      setCurrent((prev) => {
        const found = findDirInChain(prev, String(key))
        if (found) return found
        return prev && dirId === null ? null : { id: String(key), name: String(key), parent: prev }
      })
    }).catch(() => { /* 保持现状 */ })
  }

  /** 在既有链上查找目录（点面包屑/树上已进过的目录时保持正确父子链）。 */
  const findDirInChain = (node: DirNode | null, id: string): DirNode | null => {
    for (let cur = node; cur; cur = cur.parent) {
      if (cur.id === id) return cur
    }
    return null
  }

  const enterFolder = (row: PickerRow) => {
    setCurrent((prev) => ({ id: row.id, name: row.name, parent: prev }))
  }

  const q = query.trim().toLowerCase()
  const visibleFiles = (files ?? []).filter((f) => matchFilter(f.name, filter) && (!q || f.name.toLowerCase().includes(q)))

  if (!open) return null

  const title = zh ? FILTER_TITLES[filter].zh : FILTER_TITLES[filter].en

  // 图片本地上传（v2.2 antd Upload：beforeUpload 拦截自管）：定位/创建
  // assets 目录（按文档所在目录探测命名空间分发端点）→ 上传 → 以嵌入块
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
      const nsOfParent = parentId ? await resolveNamespaceOf(parentId) : null
      const assetsId = await ensureAssetsFolder(parentId, nsOfParent)
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

  // 新建白图/白板（allowCreate 且 drawio/excalidraw 过滤）：空模板上传到
  // 文档所在目录（缺省空间根），随机后缀防重名，完成后直接回调 onPick。
  const handleCreateBlank = async () => {
    const spec = filter === 'drawio'
      ? { base: zh ? '新图表' : 'diagram', ext: '.drawio', content: EMPTY_DRAWIO_XML, mime: 'text/xml' }
      : { base: zh ? '新白板' : 'whiteboard', ext: '.excalidraw', content: EMPTY_EXCALIDRAW_JSON, mime: 'application/json' }
    const name = `${spec.base}-${Math.random().toString(36).slice(2, 6)}${spec.ext}`
    setUploadPhase('uploading')
    setError('')
    try {
      const parentId = uploadParentId ?? null
      const session = await uploadFile(new File([spec.content], name, { type: spec.mime }), parentId, (phase) => setUploadPhase(phase))
      if (!session.file_id) throw new Error(zh ? '创建完成但未返回文件 ID' : 'Created without file id')
      onPick({ id: session.file_id, name })
    } catch (err) {
      setError(err instanceof Error ? err.message : (zh ? '创建失败' : 'Create failed'))
    } finally {
      setUploadPhase('')
    }
  }

  // 面包屑链（current → 根）。
  const crumbs: Array<{ id: string | null; name: string }> = []
  for (let cur: DirNode | null = current; cur; cur = cur.parent) crumbs.unshift({ id: cur.id, name: cur.name })
  const rootCrumb = scope === 'mine' ? (zh ? '我的文件' : 'My files') : (spaces.find((s) => s.id === scope)?.name ?? '空间')

  return (
    <Modal wide title={title} onClose={onClose}>
      <form className="rich-text-picker rich-text-picker-tree" onSubmit={onSubmit}>
        <div className="rich-text-picker-toolbar">
          <Select
            value={scope}
            onChange={(v) => { setScope(v); setCurrent(null) }}
            aria-label={zh ? '范围' : 'Scope'}
            options={[
              { value: 'mine', label: zh ? '我的文件' : 'My files' },
              ...spaces.filter((s) => !s.is_default).map((s) => ({ value: s.id, label: s.name })),
            ]}
          />
          <Input
            allowClear
            value={query}
            placeholder={zh ? '在当前目录按名称过滤' : 'Filter names in this folder'}
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
          {allowCreate && (filter === 'drawio' || filter === 'excalidraw') && (
            <Button disabled={uploadPhase !== ''} loading={uploadPhase !== ''} onClick={() => void handleCreateBlank()}>
              <FilePlus2 size={14} strokeWidth={2} aria-hidden="true" />
              {filter === 'drawio' ? (zh ? '新建白图' : 'New diagram') : (zh ? '新建白板' : 'New whiteboard')}
            </Button>
          )}
        </div>
        {error && <div className="error-text">{error}</div>}
        <div className="rich-text-picker-layout">
          {/* 左：目录树（懒加载）。 */}
          <div className="rich-text-picker-side">
            <div className="rich-text-picker-side-head">
              <FolderOpen size={13} strokeWidth={2} aria-hidden="true" />
              <span>{zh ? '目录' : 'Folders'}</span>
            </div>
            <Tree
              blockNode
              showIcon
              selectedKeys={current ? [current.id] : ['__root__']}
              expandedKeys={expandedKeys}
              onExpand={(keys) => setExpandedKeys(keys)}
              treeData={treeData}
              loadData={(node) => {
                if (loadedDirsRef.current.has(String(node.key))) return Promise.resolve()
                loadedDirsRef.current.add(String(node.key))
                return loadChildren(String(node.key)).then((children) => {
                  setTreeData((prev) => {
                    const update = (nodes: DataNode[]): DataNode[] =>
                      nodes.map((n) => (n.key === node.key ? { ...n, children } : { ...n, children: update(n.children ?? []) }))
                    return update(prev)
                  })
                }).catch(() => { /* 失败保持折叠 */ })
              }}
              onSelect={(keys) => onTreeSelect(keys)}
            />
          </div>
          {/* 右：当前目录（面包屑 + 文件列表）。 */}
          <div className="rich-text-picker-main">
            <Breadcrumb
              items={[
                { title: rootCrumb, onClick: () => setCurrent(null) },
                ...crumbs.map((c) => ({ title: c.name })),
              ]}
            />
            <div className="rich-text-picker-list">
              {loading && <div className="hint"><Spin size="small" /> {zh ? '加载中…' : 'Loading…'}</div>}
              {!loading && folders.length > 0 && folders.map((dir) => (
                <button
                  key={`d-${dir.id}`}
                  type="button"
                  className="rich-text-picker-item rich-text-picker-dir"
                  onClick={() => enterFolder(dir)}
                >
                  <span className="icon"><Folder size={14} strokeWidth={2} aria-hidden="true" /></span>
                  <span className="rich-text-picker-name">{dir.name}</span>
                  <span className="muted">{zh ? '进入 ›' : 'Open ›'}</span>
                </button>
              ))}
              {!loading && visibleFiles.length === 0 && !error && (
                <div className="empty">{zh ? '没有匹配的文件' : 'No matching files'}</div>
              )}
              {!loading && visibleFiles.map((item) => (
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
          </div>
        </div>
      </form>
    </Modal>
  )
}
