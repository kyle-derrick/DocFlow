// 文件管理左侧目录树（替代 Wiki 视图的空间内导航价值）：
// - FolderTreeNav：当前空间的目录树 UI（根 = 空间根；子目录懒加载，
//   目录下同时展示文件叶子节点——不可展开、点击经 by-path 查看页新窗口
//   打开；当前目录高亮 + 自动展开祖先；点击目录节点切换 FileBrowser
//   当前目录）。
// - FileBrowserWithTree：FilesPage / TeamSpacePage 布局层包装器——在
//   FileBrowser 外面包左侧树栏（240px，可折叠），不修改 FileBrowser 内部
//   实现：FileBrowser 未暴露受控当前目录 prop，故用「key 重挂载回到根 +
//   逐段点击其自身渲染的目录行/卡片按钮」最小侵入方式驱动面包屑导航；
//   同时经注入的 listItems 包装感知每次目录列表结果，完成树节点登记、
//   当前目录高亮同步（用户在 FileBrowser 内点击目录/面包屑时树跟随）。
import { useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { FileItem, FileQueryOptions, encodePathSegments } from '../api'
import FileBrowser from './FileBrowser'
import type { FileBrowserProps } from './FileBrowser'
import { useLocale } from '../i18n'

/** 根节点 key（面包屑 id=null 的映射；UUID 目录 id 不会与之冲突）。 */
const ROOT_KEY = '__root__'

interface TreeNode {
  id: string
  name: string
  /** 节点类型：目录可展开，文件为叶子（不可展开）。 */
  type: 'folder' | 'file'
  /** 父节点 key；根下为 ROOT_KEY。 */
  parentId: string
  /** 子目录 id 列表；null = 未加载（显示可展开箭头）。仅目录有意义。 */
  childIds: string[] | null
  /** 直接子文件 id 列表（树内叶子展示）。仅目录有意义。 */
  fileIds: string[]
}

/** 在 FileBrowser 渲染结果里按名称查找目录入口（列表视图 .name-btn 无
 *  title 者为目录行；网格视图文件夹卡片 .file-card-body）。同目录同名
 *  唯一（后端 NAME_CONFLICT），按名称匹配可靠。 */
function findFolderEntry(root: HTMLElement, name: string): HTMLElement | null {
  for (const btn of Array.from(root.querySelectorAll<HTMLButtonElement>('button.name-btn'))) {
    // 文件行的 name-btn 带 title（预览提示），目录行没有。
    if (btn.hasAttribute('title')) continue
    const icon = btn.querySelector('.icon')
    const text = (btn.textContent ?? '').replace(icon?.textContent ?? '', '').trim()
    if (text === name) return btn
  }
  for (const card of Array.from(root.querySelectorAll<HTMLButtonElement>('button.file-card-body'))) {
    const nameEl = card.querySelector('.file-card-name')
    if (nameEl && (nameEl.textContent ?? '').trim() === name) return card
  }
  return null
}

/** FileBrowser 的列表查询是否处于跨目录检索模式（标签/收藏/最近）。 */
function isSearchQuery(opts?: FileQueryOptions): boolean {
  return Boolean(opts && (opts.recent || opts.tagId || opts.starred !== undefined))
}

/** 纯展示目录树：节点数据与展开状态由包装器注入。 */
export default function FolderTreeNav({
  rootLabel,
  nodes,
  expanded,
  loadingKeys,
  currentKey,
  onToggleExpand,
  onSelect,
  onSelectFile,
  collapsed,
  onToggleCollapse,
  errorText,
}: {
  rootLabel: string
  nodes: Record<string, TreeNode>
  expanded: Set<string>
  loadingKeys: Set<string>
  currentKey: string
  onToggleExpand: (key: string) => void
  onSelect: (key: string) => void
  /** 文件叶子点击（新窗口 by-path 查看）；缺省时文件节点仅展示不可点。 */
  onSelectFile?: (key: string) => void
  collapsed: boolean
  onToggleCollapse: () => void
  errorText: string
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'

  const renderRow = (key: string, depth: number): ReactNode => {
    const node = nodes[key]
    if (!node) return null
    // 文件叶子：不可展开，点击经 onSelectFile 新窗口打开查看页。
    if (node.type === 'file') {
      const clickable = Boolean(onSelectFile)
      return (
        <div key={key} className="folder-tree-row file-leaf">
          <span className="folder-tree-caret" aria-hidden="true" />
          <button
            type="button"
            className="folder-tree-name"
            title={clickable ? (zh ? '在新窗口查看' : 'View in new window') : node.name}
            disabled={!clickable}
            onClick={() => onSelectFile?.(key)}
          >
            <span className="folder-tree-file-icon" aria-hidden="true">📄</span>
            <span className="folder-tree-name-text">{node.name}</span>
          </button>
        </div>
      )
    }
    const isExpanded = expanded.has(key)
    const isLoading = loadingKeys.has(key)
    const hasChildren = node.childIds === null || node.childIds.length > 0
    return (
      <div key={key}>
        <div
          className={`folder-tree-row${key === currentKey ? ' active' : ''}`}
          style={{ paddingLeft: 4 + depth * 14 }}
        >
          <button
            type="button"
            className="folder-tree-caret"
            aria-label={isExpanded ? '收起' : '展开'}
            onClick={() => hasChildren && onToggleExpand(key)}
          >
            {isLoading ? '⋯' : !hasChildren ? '·' : isExpanded ? '▾' : '▸'}
          </button>
          <button
            type="button"
            className="folder-tree-name"
            title={node.name}
            onClick={() => onSelect(key)}
          >
            <span aria-hidden="true">{key === ROOT_KEY ? '🏠' : '📁'}</span>
            <span className="folder-tree-name-text">{key === ROOT_KEY ? rootLabel : node.name}</span>
          </button>
        </div>
        {isExpanded && node.childIds?.map((id) => renderRow(id, depth + 1))}
        {isExpanded && node.fileIds.map((id) => renderRow(id, depth + 1))}
      </div>
    )
  }

  if (collapsed) {
    return (
      <aside className="folder-tree-nav collapsed">
        <button
          type="button"
          className="btn ghost small folder-tree-collapse-btn"
          title={zh ? '展开目录树' : 'Expand folder tree'}
          aria-label={zh ? '展开目录树' : 'Expand folder tree'}
          onClick={onToggleCollapse}
        >
          »
        </button>
      </aside>
    )
  }

  return (
    <aside className="folder-tree-nav">
      <div className="folder-tree-head">
        <span>{zh ? '目录' : 'Folders'}</span>
        <button
          type="button"
          className="btn ghost small folder-tree-collapse-btn"
          title={zh ? '收起目录树' : 'Collapse folder tree'}
          aria-label={zh ? '收起目录树' : 'Collapse folder tree'}
          onClick={onToggleCollapse}
        >
          «
        </button>
      </div>
      {errorText && <div className="folder-tree-error">{errorText}</div>}
      {renderRow(ROOT_KEY, 0)}
    </aside>
  )
}

/**
 * FileBrowser + 左侧目录树布局包装器（props 透传 FileBrowser）：
 * - listChildren 缺省复用 listItems（树内过滤 folder）；
 * - 树节点登记：包装 listItems 感知每次非检索目录列表（含 FileBrowser
 *   自身导航与 reload），子目录即时入树——用户在 FileBrowser 内移动时
 *   树的当前目录高亮同步跟随；
 * - 树点击导航：key 重挂载 FileBrowser 回根目录，随后按路径段依次点击
 *   其目录行进入目标目录（面包屑由 FileBrowser 自身构建，语义完整）。
 */
export function FileBrowserWithTree({ listChildren, ...browserProps }: FileBrowserProps & {
  /** 目录树取子目录的数据源；缺省复用 listItems（目录 + 文件全量）。 */
  listChildren?: (parentId: string | null) => Promise<FileItem[]>
}) {
  const rootLabel = browserProps.rootLabel
  const [nodes, setNodes] = useState<Record<string, TreeNode>>(() => ({
    [ROOT_KEY]: { id: ROOT_KEY, name: rootLabel, type: 'folder', parentId: ROOT_KEY, childIds: null, fileIds: [] },
  }))
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set([ROOT_KEY]))
  const [loadingKeys, setLoadingKeys] = useState<Set<string>>(new Set())
  const [treeError, setTreeError] = useState('')
  const [currentKey, setCurrentKey] = useState<string>(ROOT_KEY)
  const [collapsed, setCollapsed] = useState(false)
  // FileBrowser 重挂载 key（树点击导航时 +1，回到根后逐段点击进入）。
  const [navKey, setNavKey] = useState(0)
  const hostRef = useRef<HTMLDivElement | null>(null)
  const nodesRef = useRef(nodes)
  nodesRef.current = nodes
  // 待点击进入的路径段（根→目标）；navSeq 用于作废旧导航序列。
  const pendingRef = useRef<string[]>([])
  const navSeqRef = useRef(0)
  // listItems/listChildren 可能为调用方内联函数（每次渲染新引用），经 ref
  // 持有最新值以保持 fetchChildren / ensureLoaded 标识稳定（避免祖先展开
  // effect 因依赖变化重复触发懒加载）。
  const listItemsRef = useRef(browserProps.listItems)
  listItemsRef.current = browserProps.listItems
  const listChildrenRef = useRef(listChildren)
  listChildrenRef.current = listChildren

  const fetchChildren = useCallback(async (parentId: string | null): Promise<FileItem[]> => {
    if (listChildrenRef.current) return listChildrenRef.current(parentId)
    // 树内同时展示文件叶子（点击行为见 openFileFromTree），取全量列表。
    const { items } = await listItemsRef.current(parentId)
    return items
  }, [])

  /** 登记某目录的子项（列表结果 → 树节点）；childIds/fileIds 以最新列表为准。 */
  const registerListing = useCallback((parentKey: string, items: FileItem[]) => {
    setNodes((prev) => {
      const parent = prev[parentKey]
      if (!parent) return prev
      const next = { ...prev }
      const folders = items.filter((it) => it.type === 'folder')
      const files = items.filter((it) => it.type === 'file')
      for (const it of folders) {
        next[it.id] = {
          id: it.id, name: it.name, type: 'folder', parentId: parentKey,
          childIds: next[it.id]?.childIds ?? null, fileIds: next[it.id]?.fileIds ?? [],
        }
      }
      for (const it of files) {
        next[it.id] = { id: it.id, name: it.name, type: 'file', parentId: parentKey, childIds: [], fileIds: [] }
      }
      next[parentKey] = { ...parent, childIds: folders.map((it) => it.id), fileIds: files.map((it) => it.id) }
      return next
    })
  }, [])

  /** 懒加载某节点子目录（已加载则跳过）。 */
  const ensureLoaded = useCallback(
    async (key: string) => {
      if (nodesRef.current[key]?.childIds != null) return
      setLoadingKeys((prev) => new Set(prev).add(key))
      setTreeError('')
      try {
        const items = await fetchChildren(key === ROOT_KEY ? null : key)
        registerListing(key, items)
      } catch (err) {
        setTreeError(err instanceof Error ? err.message : '目录加载失败')
      } finally {
        setLoadingKeys((prev) => {
          const n = new Set(prev)
          n.delete(key)
          return n
        })
      }
    },
    [fetchChildren, registerListing],
  )

  /** 展开祖先链（当前目录变化时高亮可达）。 */
  useEffect(() => {
    if (currentKey === ROOT_KEY) return
    const chain: string[] = []
    let k: string | undefined = currentKey
    const seen = new Set<string>()
    while (k && k !== ROOT_KEY && !seen.has(k)) {
      seen.add(k)
      chain.unshift(k)
      k = nodesRef.current[k]?.parentId
    }
    for (const c of chain) {
      setExpanded((prev) => (prev.has(c) ? prev : new Set(prev).add(c)))
    }
    // 当前目录自身的子目录不自动加载（保持懒加载，展开时再取）。
    for (const ancestor of chain.slice(0, -1)) void ensureLoaded(ancestor)
  }, [currentKey, ensureLoaded])

  // ---- 树点击 → 驱动 FileBrowser 导航 ----

  const tryNavStep = useCallback((seq: number, segment: string, attempt: number) => {
    if (navSeqRef.current !== seq) return
    const host = hostRef.current
    if (!host) return
    const entry = findFolderEntry(host, segment)
    if (entry) {
      pendingRef.current = pendingRef.current.slice(1)
      entry.click()
      return
    }
    // 列表仍在加载/渲染：短间隔重试（约 6s 上限后放弃，停留在已到达目录）。
    if (attempt < 40) {
      window.setTimeout(() => {
        if (navSeqRef.current === seq && pendingRef.current[0] === segment) {
          tryNavStep(seq, segment, attempt + 1)
        }
      }, 150)
    }
  }, [])

  const scheduleNavStep = useCallback(() => {
    const seq = navSeqRef.current
    const segment = pendingRef.current[0]
    if (!segment) return
    window.setTimeout(() => {
      if (navSeqRef.current === seq && pendingRef.current[0] === segment) tryNavStep(seq, segment, 0)
    }, 120)
  }, [tryNavStep])

  /** 包装 listItems：感知目录列表（非检索模式）→ 登记树节点 + 同步当前目录。 */
  const drivenListItems = useCallback(
    async (parentId: string | null, opts?: FileQueryOptions) => {
      const res = await listItemsRef.current(parentId, opts)
      if (!isSearchQuery(opts)) {
        const key = parentId == null ? ROOT_KEY : parentId
        if (nodesRef.current[key]) registerListing(key, res.items)
        setCurrentKey(key)
        scheduleNavStep()
      }
      return res
    },
    [registerListing, scheduleNavStep],
  )

  /** 树节点点击：重挂载回根 + 逐段点击进入（根节点直接回根）。 */
  const selectFromTree = (key: string) => {
    const path: string[] = []
    const seen = new Set<string>()
    let k: string | undefined = key
    while (k && k !== ROOT_KEY && !seen.has(k)) {
      seen.add(k)
      const node: TreeNode | undefined = nodesRef.current[k]
      if (!node) break
      path.unshift(node.name)
      k = node.parentId
    }
    navSeqRef.current += 1
    pendingRef.current = key === ROOT_KEY ? [] : path
    setCurrentKey(key)
    setNavKey((n) => n + 1)
    // 展开目标祖先（含根），保证高亮路径可见。
    let cur: string | undefined = key
    const chain: string[] = []
    const seen2 = new Set<string>()
    while (cur && cur !== ROOT_KEY && !seen2.has(cur)) {
      seen2.add(cur)
      chain.unshift(cur)
      cur = nodesRef.current[cur]?.parentId
    }
    setExpanded((prev) => {
      const n = new Set(prev)
      chain.forEach((c) => n.add(c))
      return n
    })
  }

  /**
   * 文件叶子点击：新窗口打开 by-path 查看页（查看弹窗在 FileBrowser 内部、
   * 无法从树触达，退而求其次走独立查看页）。树内路径已知，包装器自行拼接；
   * ns 缺失时（未传命名空间）不传本回调，文件节点灰显不可点。
   */
  const openFileFromTree = (key: string) => {
    const node: TreeNode | undefined = nodesRef.current[key]
    const ns = browserProps.ns
    if (!node || node.type !== 'file' || !ns) return
    const segs: string[] = []
    const seen = new Set<string>()
    let k: string | undefined = node.parentId
    while (k && k !== ROOT_KEY && !seen.has(k)) {
      seen.add(k)
      const parent: TreeNode | undefined = nodesRef.current[k]
      if (!parent) return
      segs.unshift(parent.name)
      k = parent.parentId
    }
    segs.push(node.name)
    const url = new URL(
      `/view/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${encodePathSegments(segs)}/`,
      window.location.origin,
    )
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  const toggleExpand = (key: string) => {
    const node = nodesRef.current[key]
    if (!node) return
    if (expanded.has(key)) {
      setExpanded((prev) => {
        const n = new Set(prev)
        n.delete(key)
        return n
      })
      return
    }
    setExpanded((prev) => new Set(prev).add(key))
    if (node.childIds == null) void ensureLoaded(key)
  }

  // 根目录子节点由 FileBrowser 首挂载的列表登记；树兜底自取一次，
  // 覆盖 FileBrowser 处于检索模式（列表不登记）时的树可用性。
  useEffect(() => {
    void ensureLoaded(ROOT_KEY)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="files-tree-layout">
      <FolderTreeNav
        rootLabel={rootLabel}
        nodes={nodes}
        expanded={expanded}
        loadingKeys={loadingKeys}
        currentKey={currentKey}
        onToggleExpand={toggleExpand}
        onSelect={selectFromTree}
        onSelectFile={browserProps.ns ? openFileFromTree : undefined}
        collapsed={collapsed}
        onToggleCollapse={() => setCollapsed((v) => !v)}
        errorText={treeError}
      />
      <div className="files-tree-main" ref={hostRef}>
        <FileBrowser key={navKey} {...browserProps} listItems={drivenListItems} />
      </div>
    </div>
  )
}
