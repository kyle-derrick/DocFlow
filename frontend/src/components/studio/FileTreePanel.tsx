// Studio 左栏文件目录树（antd Tree 目录模式，替代手写递归 div 树）：
// - 懒加载（loadData 逐层拉取，目录在前/同层按名排序）；
// - VSCode/IDEA 式右键菜单（antd Dropdown contextMenu）：打开/编辑/新窗口、
//   新建文档/上传/设为 AI 工作目录、加入 AI 引用、重命名/删除、刷新/复制路径；
// - 单击文件=查看打开，双击/右键「编辑」=编辑打开；当前文件高亮；
// - picker 模式（onlyFolders）：项目表单目录选择（单击选中目录）。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { App as AntdApp, Dropdown, Tree } from 'antd'
import type { MenuProps } from 'antd'
import type { DataNode } from 'antd/es/tree'
import {
  FileText, FilePlus2, FileType2, Folder, FolderOpen, FolderPlus,
  Globe, Pencil, Plus, RefreshCw, SquarePen, Trash2, Upload, Wrench,
} from 'lucide-react'
import { getFileItem, listFiles } from '../../api'
import type { FileItem } from '../../api'
import { fileIcon } from '../fileIcon'

export { fileIcon }

/** 树节点元数据（key → FileItem；根节点为 null）。 */
type MetaMap = Record<string, FileItem | null>

export interface FileTreePanelProps {
  zh: boolean
  spaceId: string
  /** 根目录 folderId（null = 空间根）。 */
  rootId: string | null
  /** 根节点显示名（项目路径）。 */
  rootTitle: string
  /** 仅目录（项目表单目录选择）。 */
  onlyFolders?: boolean
  /** 目录选择模式：单击目录即选中回调（不展开）。 */
  onPickFolder?: (f: FileItem) => void
  /** 当前打开文件高亮。 */
  activeFileId?: string | null
  /** AI 工作目录高亮。 */
  activeFolderId?: string | null
  /** 文件是否已在 AI 引用中（右键文案 + 树内圆点）。 */
  isRefFile?: (f: FileItem) => boolean
  onOpenFile?: (f: FileItem) => void
  onEditFile?: (f: FileItem) => void
  onSetWorkRoot?: (f: FileItem) => void
  onUploadTo?: (f: FileItem) => void
  onCreateDoc?: (kind: 'richtext' | 'markdown', folderId: string, folderName: string) => void
  /** 「新建目录」回调（picker/项目树均可选；parent 为目标目录，根节点 name=rootTitle、id='__root__'）。 */
  onCreateFolder?: (parent: { id: string; name: string }) => void
  /** 拖拽上传（文件 drop 到目录节点；根 = 项目根；folder.id '' = 根）。 */
  onDropFiles?: (files: File[], folder: { id: string; name: string }) => void
  /** 深链展开：从根目录自动展开到该目录（构建引用树等场景——默认展开
   *  根→当前目录路径；需要逐级向上解析 parent 链）。 */
  expandTo?: string
  onToggleRef?: (f: FileItem) => void
  onRename?: (f: FileItem) => void
  onDelete?: (f: FileItem) => void
  className?: string
}

export default function FileTreePanel({
  zh, spaceId, rootId, rootTitle, onlyFolders = false, onPickFolder, activeFileId, activeFolderId,
  isRefFile, onOpenFile, onEditFile, onSetWorkRoot, onUploadTo, onCreateDoc, onCreateFolder, onDropFiles, expandTo, onToggleRef, onRename, onDelete,
  className,
}: FileTreePanelProps) {
  const { message } = AntdApp.useApp()
  const rootKey = rootId ?? '__root__'
  const [childrenOf, setChildrenOf] = useState<Record<string, FileItem[]>>({})
  const [expanded, setExpanded] = useState<React.Key[]>([])
  const [err, setErr] = useState('')
  // 拖拽上传高亮目标（dragover 中的目录 key；null = 无）。
  const [dropKey, setDropKey] = useState<string | null>(null)
  const metaRef = useRef<MetaMap>({})
  metaRef.current[rootKey] = null
  // key → 全路径（根为 rootTitle；子级 = 父路径 + '/' + 名，加载时写入）。
  const pathRef = useRef<Record<string, string>>({})
  pathRef.current[rootKey] = rootTitle

  const sortItems = (items: FileItem[]): FileItem[] =>
    [...items].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))

  const loadChildren = useCallback(async (folderKey: string): Promise<FileItem[]> => {
    const fid = folderKey === '__root__' ? null : folderKey
    try {
      const items = sortItems((await listFiles(fid, { spaceId, limit: 500 })).filter((f) => !f.is_root && (onlyFolders ? f.type === 'folder' : true)))
      setChildrenOf((p) => ({ ...p, [folderKey]: items }))
      const parentPath = pathRef.current[folderKey] ?? rootTitle
      for (const it of items) {
        metaRef.current[it.id] = it
        pathRef.current[it.id] = `${parentPath}/${it.name}`
      }
      setErr('')
      return items
    } catch (e) {
      setErr(e instanceof Error ? e.message : (zh ? '目录加载失败' : 'Failed to load folder'))
      return []
    }
  }, [spaceId, onlyFolders, zh, rootTitle])

  // expandTo：从根目录自动展开到目标目录（构建引用树等场景）——先向上
  // 解析 parent 链，再逐级 loadChildren + setExpanded。
  useEffect(() => {
    if (!expandTo || !spaceId) return
    let alive = true
    const expandPath = async () => {
      try {
        // 向上构建链（目标 → ... → 根的子目录）。
        const chain: string[] = []
        const seen = new Set<string>()
        let cur: string | null = expandTo
        while (cur && cur !== rootKey && !seen.has(cur)) {
          seen.add(cur)
          chain.unshift(cur)
          const meta = await getFileItem(cur)
          if (!alive) return
          cur = meta.parent_id || null
        }
        if (!alive || chain.length === 0) return
        // 从根开始逐级加载 + 展开。
        await loadChildren(rootKey)
        if (!alive) return
        for (let i = 0; i < chain.length; i++) {
          await loadChildren(chain[i])
          if (!alive) return
        }
        setExpanded(chain)
      } catch {
        // 展开失败静默（树仍可用，只是未自动展开）。
      }
    }
    void expandPath()
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- 仅挂载执行一次
  }, [expandTo, spaceId])

  // 根目录切换：清空重载并展开根。
  useEffect(() => {
    setChildrenOf({})
    setExpanded([rootKey])
    metaRef.current = { [rootKey]: null }
    pathRef.current = { [rootKey]: rootTitle }
    void loadChildren(rootKey)
  }, [rootKey, rootTitle, loadChildren])

  const copyPath = (path: string) => {
    void navigator.clipboard.writeText(path)
      .then(() => message.success(zh ? '路径已复制' : 'Path copied'))
      .catch(() => message.error(zh ? '复制失败' : 'Copy failed'))
  }

  /** 节点全路径（加载时写入 pathRef；根 = rootTitle）。 */
  const pathOf = (key: string): string => pathRef.current[key] ?? rootTitle

  /** 右键菜单（目录/文件分列；VSCode/IDEA 式）。 */
  const ctxItems = (f: FileItem | null): MenuProps['items'] => {
    if (!f) {
      // 根节点：新建目录/上传/新建/刷新/复制路径。
      const items: NonNullable<MenuProps['items']> = []
      if (onCreateFolder) items.push({ key: 'newfolder', icon: <FolderPlus size={13} aria-hidden="true" />, label: zh ? '新建目录' : 'New folder' })
      if (onUploadTo) items.push({ key: 'upload', icon: <Upload size={13} aria-hidden="true" />, label: zh ? '上传文件到此目录' : 'Upload to this folder' })
      if (onCreateDoc) items.push({
        key: 'newdoc', icon: <FilePlus2 size={13} aria-hidden="true" />, label: zh ? '新建文档' : 'New document', children: [
          { key: 'newdoc:md', icon: <FileType2 size={13} aria-hidden="true" />, label: zh ? 'Markdown 文档' : 'Markdown document' },
          { key: 'newdoc:rt', icon: <SquarePen size={13} aria-hidden="true" />, label: zh ? '富文本文档' : 'Rich text document' },
        ],
      })
      items.push({ key: 'refresh', icon: <RefreshCw size={13} aria-hidden="true" />, label: zh ? '刷新' : 'Refresh' })
      items.push({ type: 'divider' })
      items.push({ key: 'copypath', label: zh ? '复制路径' : 'Copy path' })
      return items
    }
    if (f.type === 'folder') {
      const isOpen = expanded.includes(f.id)
      const items: NonNullable<MenuProps['items']> = [
        { key: 'expand', icon: isOpen ? <Folder size={13} aria-hidden="true" /> : <FolderOpen size={13} aria-hidden="true" />, label: isOpen ? (zh ? '收起' : 'Collapse') : (zh ? '展开' : 'Expand') },
      ]
      if (onPickFolder) items.push({ key: 'pick', icon: <Plus size={13} aria-hidden="true" />, label: zh ? '选择此目录' : 'Select this folder' })
      if (onCreateFolder) items.push({ key: 'newfolder', icon: <FolderPlus size={13} aria-hidden="true" />, label: zh ? '在此新建目录' : 'New folder here' })
      if (onSetWorkRoot) items.push({ key: 'workroot', icon: <Wrench size={13} aria-hidden="true" />, label: zh ? '设为 AI 工作目录' : 'Set as AI working root' })
      if (onUploadTo) items.push({ key: 'upload', icon: <Upload size={13} aria-hidden="true" />, label: zh ? '上传文件到此目录' : 'Upload to this folder' })
      if (onCreateDoc) items.push({
        key: 'newdoc', icon: <FilePlus2 size={13} aria-hidden="true" />, label: zh ? '新建文档' : 'New document', children: [
          { key: 'newdoc:md', icon: <FileType2 size={13} aria-hidden="true" />, label: zh ? 'Markdown 文档' : 'Markdown document' },
          { key: 'newdoc:rt', icon: <SquarePen size={13} aria-hidden="true" />, label: zh ? '富文本文档' : 'Rich text document' },
        ],
      })
      items.push({ key: 'refresh', icon: <RefreshCw size={13} aria-hidden="true" />, label: zh ? '刷新' : 'Refresh' })
      items.push({ type: 'divider' })
      if (onRename) items.push({ key: 'rename', icon: <Pencil size={13} aria-hidden="true" />, label: zh ? '重命名' : 'Rename' })
      if (onDelete) items.push({ key: 'delete', icon: <Trash2 size={13} aria-hidden="true" />, label: zh ? '删除' : 'Delete', danger: true })
      if (onRename || onDelete) items.push({ type: 'divider' })
      items.push({ key: 'copypath', label: zh ? '复制路径' : 'Copy path' })
      return items
    }
    const items: NonNullable<MenuProps['items']> = [
      { key: 'open', icon: <FileText size={13} aria-hidden="true" />, label: zh ? '打开（查看）' : 'Open (view)' },
      { key: 'openwin', icon: <Globe size={13} aria-hidden="true" />, label: zh ? '新窗口查看' : 'Open in new window' },
    ]
    if (onEditFile) items.push({ key: 'edit', icon: <Pencil size={13} aria-hidden="true" />, label: zh ? '打开为编辑' : 'Open for editing' })
    if (onToggleRef) items.push({ key: 'ref', icon: <Plus size={13} aria-hidden="true" />, label: isRefFile?.(f) ? (zh ? '移除 AI 引用' : 'Remove AI reference') : (zh ? '加入 AI 引用' : 'Add AI reference') })
    items.push({ type: 'divider' })
    if (onRename) items.push({ key: 'rename', icon: <Pencil size={13} aria-hidden="true" />, label: zh ? '重命名' : 'Rename' })
    if (onDelete) items.push({ key: 'delete', icon: <Trash2 size={13} aria-hidden="true" />, label: zh ? '删除' : 'Delete', danger: true })
    items.push({ type: 'divider' })
    items.push({ key: 'copypath', label: zh ? '复制路径' : 'Copy path' })
    return items
  }

  const onCtxClick = (f: FileItem | null, key: string) => ({ key: act }: { key: string }) => {
    // 根节点合成 FileItem（上传/新建目标 = 项目根）。
    const target: FileItem | null = f ?? (key === rootKey ? ({ id: rootId ?? '', type: 'folder', name: rootTitle } as unknown as FileItem) : null)
    if (act === 'expand' && target) {
      setExpanded((p) => (p.includes(target.id) ? p.filter((x) => x !== target.id) : [...p, target.id]))
      void loadChildren(target.id || rootKey)
    } else if (act === 'pick' && target) {
      onPickFolder?.(target)
    } else if (act === 'workroot' && target) {
      onSetWorkRoot?.(target)
    } else if (act === 'upload' && target) {
      onUploadTo?.(target)
    } else if (act === 'newfolder' && target) {
      // target.id 为 ''（根）时由父层换算为其空间根 parent（见 onCreateFolder 契约）。
      onCreateFolder?.({ id: target.id || rootKey, name: target.name })
    } else if (act.startsWith('newdoc:') && target) {
      onCreateDoc?.(act === 'newdoc:md' ? 'markdown' : 'richtext', target.id, target.name)
    } else if (act === 'refresh') {
      void loadChildren(key)
    } else if (act === 'open' && target) {
      onOpenFile?.(target)
    } else if (act === 'openwin' && target) {
      window.open(`/view/${target.id}`, '_blank', 'noopener')
    } else if (act === 'edit' && target) {
      onEditFile?.(target)
    } else if (act === 'ref' && target) {
      onToggleRef?.(target)
    } else if (act === 'rename' && target) {
      onRename?.(target)
    } else if (act === 'delete' && target) {
      onDelete?.(target)
    } else if (act === 'copypath') {
      copyPath(pathOf(key))
    }
  }

  /** 节点 title（Dropdown 包裹：右键菜单 + 自定义行：图标/名称/引用圆点）。 */
  const renderTitle = (key: string): React.ReactNode => {
    const f = key === rootKey ? null : metaRef.current[key] ?? null
    const isFolder = f ? f.type === 'folder' : true
    const isOpen = expanded.includes(key)
    const refd = f && !isFolder && isRefFile?.(f)
    const rowCls = [
      'ftree-row',
      isFolder && activeFolderId === key && key !== rootKey ? ' workroot' : '',
      !isFolder && activeFileId === key ? ' file-active' : '',
      dropKey === key ? ' droptarget' : '',
    ].filter(Boolean).join(' ')
    // 目录节点支持拖拽上传（drop 到该目录；根 = 项目根）。
    const dropProps = isFolder && onDropFiles
      ? {
          onDragOver: (e: React.DragEvent) => {
            if (!e.dataTransfer.types.includes('Files')) return
            e.preventDefault()
            e.dataTransfer.dropEffect = 'copy'
            setDropKey(key)
          },
          onDragLeave: () => setDropKey((cur) => (cur === key ? null : cur)),
          onDrop: (e: React.DragEvent) => {
            e.preventDefault()
            e.stopPropagation()
            setDropKey(null)
            const files = Array.from(e.dataTransfer.files)
            if (files.length > 0) onDropFiles(files, { id: key === rootKey ? '' : key, name: f?.name ?? rootTitle })
          },
        }
      : {}
    return (
      <Dropdown trigger={['contextMenu']} menu={{ items: ctxItems(f), onClick: onCtxClick(f, key) }}>
        <span className={rowCls} title={f?.name ?? rootTitle} {...dropProps}>
          {isFolder
            ? (isOpen ? <FolderOpen size={14} strokeWidth={2} aria-hidden="true" /> : <Folder size={14} strokeWidth={2} aria-hidden="true" />)
            : fileIcon(f!.name)}
          <span className="name">{f?.name ?? rootTitle}</span>
          {refd && <span className="ftree-refdot" title={zh ? '已加入 AI 引用' : 'Referenced by AI'} aria-label={zh ? '已加入 AI 引用' : 'Referenced by AI'} />}
        </span>
      </Dropdown>
    )
  }

  /** FileItem[] → DataNode[]（懒加载：目录 isLeaf=false，文件 isLeaf=true）。 */
  const toNodes = (items: FileItem[]): DataNode[] => items.map((f) => ({
    key: f.id,
    title: renderTitle(f.id),
    isLeaf: f.type !== 'folder',
  }))

  const treeData: DataNode[] = useMemo(() => {
    const build = (key: string): DataNode[] | undefined => {
      const kids = childrenOf[key]
      return kids ? toNodes(kids) : undefined
    }
    const root: DataNode = { key: rootKey, title: renderTitle(rootKey), isLeaf: false, children: build(rootKey) ?? (childrenOf[rootKey] === undefined ? undefined : []) }
    return [root]
    // eslint-disable-next-line react-hooks/exhaustive-deps -- childrenOf/expanded 变化时重建（renderTitle 依赖 ref/drop 状态）
  }, [childrenOf, expanded, rootKey, activeFileId, activeFolderId, isRefFile, dropKey])

  return (
    <div className={`ftree${className ? ` ${className}` : ''}`}>
      {err && <div className="error-text studio-pad8">{err}</div>}
      <Tree
        blockNode
        showIcon
        className="ftree-antd"
        treeData={treeData}
        expandedKeys={expanded}
        expandAction={onPickFolder ? false : 'click'}
        onExpand={(keys) => {
          setExpanded(keys)
        }}
        loadData={(node) => {
          const key = String(node.key)
          if (childrenOf[key]) return Promise.resolve()
          return loadChildren(key).then(() => undefined)
        }}
        selectedKeys={activeFileId ? [activeFileId] : []}
        onSelect={(_keys, info) => {
          const key = String(info.node.key)
          if (key === rootKey) return
          const f = metaRef.current[key]
          if (!f) return
          if (f.type === 'folder') {
            // picker 模式：单击选择（展开收起由箭头承担）；普通模式单击
            // 展开/收起由 antd expandAction 承担，不在此重复切换。
            if (onPickFolder) onPickFolder(f)
            return
          }
          if (!onlyFolders) onOpenFile?.(f)
        }}
      />
    </div>
  )
}
