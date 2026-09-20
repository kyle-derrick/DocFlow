// 文件管理左侧目录树（替代 Wiki 视图的空间内导航价值）：
// - FolderTreeNav：当前空间的目录树 UI（根 = 空间根；子目录懒加载，
//   目录下同时展示文件叶子节点——不可展开、点击经 fileOpenSignal 触发
//   FileBrowser 查看弹窗（不再新窗口）；当前目录高亮 + 自动展开祖先；
//   点击目录节点切换 FileBrowser 当前目录；文件/目录节点右键菜单与
//   文件列表行菜单统一（查看（方式名）/ 编辑（方式名）/ 打开方式 > 分组，
//   复用 .ctx-menu 视觉，见 FileBrowser itemMenuItems 同构结构）。
// - FileBrowserWithTree：FilesPage 布局层包装器——在
//   FileBrowser 外面包左侧树栏（240px，可折叠），不修改 FileBrowser 内部
//   实现；树点击目录经 folderNavSignal 受控信号驱动：FileBrowser 在同一
//   实例内直接把面包屑切到目标链并加载目标目录（不重挂载、不先回根，
//   一次列表请求，无根目录闪现——v2.5 重构，替代旧「key 重挂载回根 +
//   逐段点击其目录行」实现）；同时经注入的 listItems 包装感知每次目录
//   列表结果，完成树节点登记、当前目录高亮同步（用户在 FileBrowser 内
//   点击目录/面包屑时树跟随）。文件节点点击经 fileOpenSignal 受控信号
//   触达 FileBrowser 的查看弹窗。
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { ChevronDown, ChevronRight, FileText, Folder, Home } from 'lucide-react'
import { Menu } from 'antd'
import type { MenuProps } from 'antd'
import { FileItem, FileQueryOptions, OpenWithPrefs, drawioStatus, encodePathSegments, listOpenWith, onlyOfficeStatus } from '../api'
import {
  allEditEntries,
  allViewEntries,
  builtinOpenWith,
  editMethodLabel,
  editOptionsFor,
  effectiveOpenWithFor,
  extOf,
  viewMethodLabel,
  viewOptionsFor,
} from '../openers'
import FileBrowser, { clampFixedMenu } from './FileBrowser'
import type { FileBrowserProps } from './FileBrowser'
import { useLocale } from '../i18n'

/** 根节点 key（面包屑 id=null 的映射；UUID 目录 id 不会与之冲突）。 */
const ROOT_KEY = '__root__'

/**
 * 目录树子级列举的 limit 上限（与后端 /files、/spaces/:id/files 的 limit
 * 上限一致）。列表条数达到该值说明可能被截断——registerListing 据此
 * 不把目录误判为「确认空」。
 */
const LISTING_LIMIT_CAP = 1000

/**
 * caret 状态判定 —— 全树唯一的箭头状态事实源（渲染、右键菜单「展开/收起」
 * 项共用，勿在别处另行推断）：
 * - 'loading'：子级懒加载进行中（显示 ⋯，点击不重复触发）；
 * - 'toggle'：可展开/收起——childIds===null（从未加载过，可能仍有子级：
 *   显示实箭头，点击触发懒加载）或已加载且存在子目录/子文件；
 * - 'empty'：已加载且确认既无子目录也无子文件（占位「·」；点击触发强制
 *   重新加载，数据变化（新建子目录等）后可恢复，杜绝「·」死状态）。
 *
 * v1.6.1 根治说明：此前「确认空」的判定被列表截断污染——后端目录列举
 * 默认 limit=100，子项超过 100 且排序靠前的全是文件时，树收到的列表里
 * 一个子目录都没有，被误判 childIds=[]（确认空）→ 永久「·」且不可展开。
 * 现树/列表均请求 limit=1000（api.ts FileQueryOptions.limit），并在
 * registerListing 对「达到上限且无子目录」的目录保留 childIds=null。
 */
function caretStateOf(node: TreeNode, loading: boolean): 'loading' | 'toggle' | 'empty' {
  if (loading) return 'loading'
  if (node.childIds === null) return 'toggle'
  if (node.childIds.length > 0 || node.fileIds.length > 0) return 'toggle'
  return 'empty'
}

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
  /** 目录下存在 index.html/index.htm（作为网页打开入口用）。 */
  hasIndexWeb: boolean
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
  onOpenFileWith,
  onOpenFolderAsWebsite,
  openPrefs,
  ooEnabled,
  drawioEnabled,
  errorText,
}: {
  rootLabel: string
  nodes: Record<string, TreeNode>
  expanded: Set<string>
  loadingKeys: Set<string>
  currentKey: string
  /** 展开/收起（含懒加载触发）；force=true 时即使已加载也重新拉取（「·」占位点击恢复用）。 */
  onToggleExpand: (key: string, force?: boolean) => void
  onSelect: (key: string) => void
  /** 文件叶子点击（查看弹窗，经包装器 fileOpenSignal 触达 FileBrowser）；缺省时文件节点不可点。 */
  onSelectFile?: (key: string) => void
  /**
   * 文件「查看/编辑/打开方式」（新窗口，by-path + ?open= 方式覆盖）：
   * kind=view|edit，method 缺省 = 生效默认方式（openers 枚举体系）。
   * 缺省时相关菜单项隐藏。
   */
  onOpenFileWith?: (key: string, kind: 'view' | 'edit', method?: string) => void
  /** has_index_web 目录右键菜单「作为网页打开（新窗口）」；缺省时菜单项隐藏。 */
  onOpenFolderAsWebsite?: (key: string) => void
  /** 默认打开方式偏好（打开方式菜单的生效方式计算）。 */
  openPrefs: OpenWithPrefs
  /** OnlyOffice / draw.io 集成可用性（方式门槛过滤，与文件列表菜单一致）。 */
  ooEnabled: boolean
  drawioEnabled: boolean
  errorText: string
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  // 右键菜单：目标节点 key + 视口坐标（null = 关闭）；点击外部/动作后收口
  //（「打开方式」内联子菜单弹层挂 body，一并豁免，否则子菜单点击即被吞）。
  const [ctx, setCtx] = useState<{ key: string; x: number; y: number } | null>(null)
  const ctxRef = useRef<HTMLDivElement | null>(null)
  const ctxNode = ctx ? nodes[ctx.key] : undefined
  useEffect(() => {
    if (!ctx) return
    const onDown = (e: MouseEvent) => {
      const el = e.target as HTMLElement | null
      if (el?.closest?.('.ctx-menu, .ant-menu-submenu-popup')) return
      setCtx(null)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [ctx])
  // 渲染后按实测尺寸把菜单收进视口（复用 FileBrowser 的收缩定位）。
  useLayoutEffect(() => {
    if (!ctx || !ctxRef.current) return
    clampFixedMenu(ctxRef.current, ctx.x, ctx.y)
  }, [ctx])

  /** 集成可用性对方式的门槛（与 FileBrowser.methodEnabled 一致）。 */
  const methodEnabled = (m: string): boolean =>
    !(m === 'office' && !ooEnabled) && !(m === 'drawio' && !drawioEnabled)

  /** 节点右键菜单（文件 = 查看弹窗 + 查看/编辑（方式名）+ 打开方式 >（全量
   * 方式，非法/集成未启用灰显注明原因）；目录 = 进入 + 作为网页打开
   *（has_index_web）+ 展开/收起）。 */
  const ctxMenuItems = (node: TreeNode): MenuProps['items'] => {
    const entries: NonNullable<MenuProps['items']> = []
    if (node.type === 'file') {
      if (onSelectFile) entries.push({ key: 'view-modal', label: zh ? '查看（弹窗）' : 'View (modal)' })
      if (onOpenFileWith) {
        const effective = effectiveOpenWithFor(node.name, openPrefs)
        const viewMethods = viewOptionsFor(extOf(node.name)).filter(methodEnabled)
        const editMethods = editOptionsFor(extOf(node.name)).filter(methodEnabled)
        const effView = viewMethods.includes(effective.view) ? effective.view : viewMethods[0]
        if (effView) {
          entries.push({ key: 'view-default', label: zh ? `查看（${viewMethodLabel(effView, true, extOf(node.name))}）` : `View (${viewMethodLabel(effView, false, extOf(node.name))})` })
        }
        const effEdit = editMethods.includes(effective.edit) ? effective.edit : null
        if (effEdit) {
          entries.push({ key: 'edit-default', label: zh ? `编辑（${editMethodLabel(effEdit, true)}）` : `Edit (${editMethodLabel(effEdit, false)})` })
        }
        // 打开方式 >：全量方式列表——所有可见项均可点（不置灰；对该扩展
        // 非法的方式也可选，点击按用户显式选择经 ?open= 强制分发，兜底由
        // 查看分发层负责）；仅集成未启用（office/drawio）的项隐藏（与文件
        // 列表右键菜单口径一致）。
        const ext = extOf(node.name)
        const av = { office: ooEnabled, drawio: drawioEnabled }
        const gated = (m: string) => (m === 'office' && !ooEnabled) || (m === 'drawio' && !drawioEnabled)
        const viewEntries = allViewEntries(ext, av, zh).filter(({ method }) => !gated(method))
        const editEntries = allEditEntries(ext, av, zh).filter(({ method }) => !gated(method))
        entries.push({
          key: 'openwith',
          label: zh ? '打开方式' : 'Open with',
          children: [
            {
              key: 'group-openwith-view',
              type: 'group' as const,
              label: zh ? '查看' : 'View',
              children: viewEntries.map(({ method }) => ({
                key: `openview:${method}`,
                label: `${zh ? '查看 · ' : 'View · '}${viewMethodLabel(method as Parameters<typeof viewMethodLabel>[0], zh, ext)}`,
              })),
            },
            {
              key: 'group-openwith-edit',
              type: 'group' as const,
              label: zh ? '编辑' : 'Edit',
              children: editEntries.map(({ method }) => ({
                key: `openedit:${method}`,
                label: `${zh ? '编辑 · ' : 'Edit · '}${editMethodLabel(method as Parameters<typeof editMethodLabel>[0], zh)}`,
              })),
            },
          ],
        })
      }
    }
    if (node.type === 'folder') {
      entries.push({ key: 'enter', label: zh ? '进入' : 'Open' })
      if (node.hasIndexWeb && onOpenFolderAsWebsite) {
        entries.push({ key: 'open-web', label: zh ? '作为网页打开（新窗口）' : 'Open as website' })
      }
      // 「展开/收起」项与箭头同口径（caretStateOf 单一判定）：确认空的目录
      // 无此项（展开无意义）；未加载/有子级均可展开。
      if (caretStateOf(node, false) === 'toggle') {
        entries.push({ key: 'toggle', label: expanded.has(node.id) ? (zh ? '收起' : 'Collapse') : zh ? '展开' : 'Expand' })
      }
    }
    return entries
  }

  const runCtxAction = (key: string, action: string) => {
    if (action === 'view-modal') onSelectFile?.(key)
    else if (action === 'view-default') onOpenFileWith?.(key, 'view')
    else if (action === 'edit-default') onOpenFileWith?.(key, 'edit')
    else if (action === 'enter') onSelect(key)
    else if (action === 'open-web') onOpenFolderAsWebsite?.(key)
    else if (action === 'toggle') onToggleExpand(key)
    else if (action.startsWith('openview:')) onOpenFileWith?.(key, 'view', action.slice('openview:'.length))
    else if (action.startsWith('openedit:')) onOpenFileWith?.(key, 'edit', action.slice('openedit:'.length))
  }

  const renderRow = (key: string, depth: number): ReactNode => {
    const node = nodes[key]
    if (!node) return null
    // 文件叶子：不可展开，点击触发查看弹窗（onSelectFile → fileOpenSignal）。
    if (node.type === 'file') {
      const clickable = Boolean(onSelectFile)
      return (
        <div
          key={key}
          className="folder-tree-row file-leaf"
          style={{ paddingLeft: 4 + depth * 14 }}
          onContextMenu={(e) => {
            if (!clickable && !onOpenFileWith) return
            e.preventDefault()
            setCtx({ key, x: e.clientX, y: e.clientY })
          }}
        >
          <span className="folder-tree-caret" aria-hidden="true" />
          <button
            type="button"
            className="folder-tree-name"
            title={clickable ? (zh ? '查看' : 'View') : node.name}
            disabled={!clickable}
            onClick={() => onSelectFile?.(key)}
          >
            <span className="folder-tree-file-icon" aria-hidden="true"><FileText size={14} strokeWidth={2} aria-hidden="true" /></span>
            <span className="folder-tree-name-text">{node.name}</span>
          </button>
        </div>
      )
    }
    const isExpanded = expanded.has(key)
    const isLoading = loadingKeys.has(key)
    // caret 状态唯一判定（见 caretStateOf 注释）：loading=⋯ / toggle=箭头
    //（未加载或确有子级，可展开收起）/ empty=确认空的「·」占位。
    const caret = caretStateOf(node, isLoading)
    // 「·」占位也可点击：强制重新加载（目录在别处新建子项后可恢复），
    // 杜绝「·」死状态；仅 loading 中不可点（防并发重复请求）。
    const caretClickable = caret !== 'loading'
    return (
      <div key={key}>
        <div
          className={`folder-tree-row${key === currentKey ? ' active' : ''}`}
          style={{ paddingLeft: 4 + depth * 14 }}
          onContextMenu={(e) => {
            e.preventDefault()
            setCtx({ key, x: e.clientX, y: e.clientY })
          }}
        >
          <button
            type="button"
            className={`folder-tree-caret${caret === 'empty' ? ' caret-empty' : ''}`}
            aria-label={isExpanded ? '收起' : '展开'}
            aria-disabled={!caretClickable}
            title={caret === 'empty' ? (zh ? '空目录（点击重新加载）' : 'Empty folder (click to reload)') : undefined}
            onClick={() => caretClickable && onToggleExpand(key, caret === 'empty')}
          >
            {caret === 'loading'
              ? '⋯'
              : caret === 'empty'
                ? '·'
                : isExpanded
                  ? <ChevronDown size={14} strokeWidth={2} aria-hidden="true" />
                  : <ChevronRight size={14} strokeWidth={2} aria-hidden="true" />}
          </button>
          {/* v1.6.1 交互（用户明确）：单击目录名 = 进入该目录（onSelect 驱动
              中间列表切换）；展开/收起只经左侧 caret 箭头。根节点同理：单击
              根名回到空间根目录。 */}
          <button
            type="button"
            className="folder-tree-name"
            title={zh ? `${node.name}（进入）` : node.name}
            onClick={() => onSelect(key)}
          >
            <span aria-hidden="true">{key === ROOT_KEY
              ? <Home size={14} strokeWidth={2} aria-hidden="true" />
              : <Folder size={14} strokeWidth={2} aria-hidden="true" />}</span>
            <span className="folder-tree-name-text">{key === ROOT_KEY ? rootLabel : node.name}</span>
          </button>
        </div>
        {/* 子目录与文件叶子同为 depth+1 缩进（文件行也带 paddingLeft，修复
            文件与目录同级显示的问题）。 */}
        {isExpanded && node.childIds?.map((id) => renderRow(id, depth + 1))}
        {isExpanded && node.fileIds.map((id) => renderRow(id, depth + 1))}
      </div>
    )
  }

  // v2.2：去掉折叠按钮——左栏始终显示（原折叠功能有状态串扰问题，且 240px
  // 常驻栏对导航价值大于偶尔让出的宽度）。
  return (
    <aside className="folder-tree-nav">
      <div className="folder-tree-head">
        <span>{zh ? '目录' : 'Folders'}</span>
      </div>
      {errorText && <div className="folder-tree-error">{errorText}</div>}
      {renderRow(ROOT_KEY, 0)}
      {/* 节点右键菜单：与文件列表行菜单同构（查看（方式名）/ 编辑（方式名）/
          打开方式 > 分组；目录 = 作为网页打开 + 展开/收起），动作经包装器
          onOpenFileWith 等回调分发。 */}
      {ctx && ctxNode && (
        <div
          ref={ctxRef}
          className="ctx-menu"
          role="menu"
          style={{ left: `${ctx.x}px`, top: `${ctx.y}px` }}
          onClick={() => setCtx(null)}
        >
          <Menu
            className="ctx-antd-menu"
            mode="vertical"
            selectable={false}
            onClick={({ key }) => runCtxAction(ctx.key, key)}
            items={ctxMenuItems(ctxNode)}
          />
        </div>
      )}
    </aside>
  )
}

/**
 * FileBrowser + 左侧目录树三栏布局包装器（props 透传 FileBrowser）：
 * - 顶栏宿主（.files-topbar）：FileBrowser 的工具行经 toolbarHost portal
 *   渲染为横跨全宽的 44px 顶条（空间切换/搜索/标签/新建/上传/回收站）；
 * - 下方三栏 grid：目录树(240px) + 文件管理(1fr) + 右侧栏(280px，aside
 *   槽——空间视图成员面板；默认空间不传即无第三栏)；各栏内部各自滚动；
 * - listChildren 缺省复用 listItems（树内过滤 folder）；
 * - 树节点登记：包装 listItems 感知每次非检索目录列表（含 FileBrowser
 *   自身导航与 reload），子目录即时入树——用户在 FileBrowser 内移动时
 *   树的当前目录高亮同步跟随；
 * - 树点击导航：经 folderNavSignal 受控信号，FileBrowser 同实例内直接
 *   把面包屑切到根→目标完整链并加载目标目录（不重挂载、不回根，一次
 *   列表请求）。
 */
export function FileBrowserWithTree({ listChildren, aside, treeRootLabel, ...browserProps }: FileBrowserProps & {
  /** 目录树取子目录的数据源；缺省复用 listItems（目录 + 文件全量）。 */
  listChildren?: (parentId: string | null) => Promise<FileItem[]>
  /** 右侧栏内容（空间视图成员面板）；提供时启用三栏布局。 */
  aside?: ReactNode
  /** 目录树根节点显示名（缺省 rootLabel；v2.6 面包屑根改「根目录」短名
   *  后树根仍显示空间名，避免丢失空间上下文）。 */
  treeRootLabel?: string
}) {
  const rootLabel = browserProps.rootLabel
  const treeRoot = treeRootLabel ?? rootLabel
  // 顶栏宿主：FileBrowser 工具行 portal 目标（挂载后经 state 传入）。
  const [toolbarHost, setToolbarHost] = useState<HTMLDivElement | null>(null)
  const [nodes, setNodes] = useState<Record<string, TreeNode>>(() => ({
    [ROOT_KEY]: { id: ROOT_KEY, name: treeRoot, type: 'folder', parentId: ROOT_KEY, childIds: null, fileIds: [], hasIndexWeb: false },
  }))
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set([ROOT_KEY]))
  const [loadingKeys, setLoadingKeys] = useState<Set<string>>(new Set())
  const [treeError, setTreeError] = useState('')
  const [currentKey, setCurrentKey] = useState<string>(ROOT_KEY)
  // 树→FileBrowser 目录导航信号（seq 自增；FileBrowser 同实例内直接切
  // 面包屑并加载目标目录，见 FileBrowser folderNavSignal）。
  const folderNavSeqRef = useRef(0)
  const [folderNavSignal, setFolderNavSignal] = useState<FileBrowserProps['folderNavSignal']>(undefined)
  const nodesRef = useRef(nodes)
  nodesRef.current = nodes
  // FileBrowser 最近一次列表是否处于跨目录检索模式（标签/收藏/最近）——
  // 检索模式下中间列表与树高亮脱钩，目录名单击即使 key===currentKey 也须
  // 重挂载回目录视图。
  const lastSearchRef = useRef(false)
  // 懒加载进行中标记（防同 key 并发重复请求；loadingKeys 是渲染态副本）。
  const inFlightRef = useRef<Set<string>>(new Set())
  // listItems/listChildren 可能为调用方内联函数（每次渲染新引用），经 ref
  // 持有最新值以保持 fetchChildren / ensureLoaded 标识稳定（避免祖先展开
  // effect 因依赖变化重复触发懒加载）。
  const listItemsRef = useRef(browserProps.listItems)
  listItemsRef.current = browserProps.listItems
  const listChildrenRef = useRef(listChildren)
  listChildrenRef.current = listChildren

  const fetchChildren = useCallback(async (parentId: string | null): Promise<FileItem[]> => {
    if (listChildrenRef.current) return listChildrenRef.current(parentId)
    // 树内同时展示文件叶子（点击行为见 openFileFromTree），取全量列表；
    // limit 拉满（后端上限 1000）——默认 100 会截断多子项目录，导致树
    // 把仍有子目录的目录误判为「确认空」（v1.6.1 caret 根治，见
    // caretStateOf 注释）。
    const { items } = await listItemsRef.current(parentId, { limit: LISTING_LIMIT_CAP })
    return items
  }, [])

  /** 登记某目录的子项（列表结果 → 树节点）；childIds/fileIds 以最新列表为准。
   * v1.6 排序隔离：目录树固定自然序（名称，数字感知 localeCompare），不受
   * 工具栏/表头排序影响——服务端排序只作用于中间文件列表。 */
  const naturalNameCompare = (a: FileItem, b: FileItem): number =>
    a.name.localeCompare(b.name, 'zh-Hans-CN', { numeric: true })

  const registerListing = useCallback((parentKey: string, items: FileItem[]) => {
    setNodes((prev) => {
      const parent = prev[parentKey]
      if (!parent) return prev
      const next = { ...prev }
      const folders = items.filter((it) => it.type === 'folder').sort(naturalNameCompare)
      const files = items.filter((it) => it.type === 'file').sort(naturalNameCompare)
      for (const it of folders) {
        next[it.id] = {
          id: it.id, name: it.name, type: 'folder', parentId: parentKey,
          childIds: next[it.id]?.childIds ?? null, fileIds: next[it.id]?.fileIds ?? [],
          hasIndexWeb: Boolean(it.has_index_web),
        }
      }
      for (const it of files) {
        next[it.id] = { id: it.id, name: it.name, type: 'file', parentId: parentKey, childIds: [], fileIds: [], hasIndexWeb: false }
      }
      // 截断兜底：列表条数达到 limit 上限（1000）说明可能被截断——此时
      // 若结果里一个子目录都没有，不能断定无子目录，保留 childIds=null
      //（未知=可展开箭头），避免「·」死状态（见 caretStateOf 注释）。
      const possiblyTruncated = items.length >= LISTING_LIMIT_CAP
      const childIds: string[] | null = folders.length > 0
        ? folders.map((it) => it.id)
        : possiblyTruncated
          ? (parent.childIds ?? null)
          : []
      next[parentKey] = { ...parent, childIds, fileIds: files.map((it) => it.id) }
      return next
    })
  }, [])

  /** 懒加载某节点子目录（已加载则跳过；force=true 强制重取——「·」占位
   *  点击恢复 / 数据可能在别处变化）。同 key 并发请求经 inFlightRef 去重。 */
  const ensureLoaded = useCallback(
    async (key: string, force = false) => {
      if (!force && nodesRef.current[key]?.childIds != null) return
      if (inFlightRef.current.has(key)) return
      inFlightRef.current.add(key)
      setLoadingKeys((prev) => new Set(prev).add(key))
      setTreeError('')
      try {
        const items = await fetchChildren(key === ROOT_KEY ? null : key)
        registerListing(key, items)
      } catch (err) {
        setTreeError(err instanceof Error ? err.message : '目录加载失败')
      } finally {
        inFlightRef.current.delete(key)
        setLoadingKeys((prev) => {
          const n = new Set(prev)
          n.delete(key)
          return n
        })
      }
    },
    [fetchChildren, registerListing],
  )

  /** 展开祖先链（当前目录变化时高亮可达）。注意只展开**祖先**——当前目录
   *  自身是否展开由用户经 caret 控制（v1.6.1：单击目录名=进入，不做
   *  展开/收起），此处不得代劳。 */
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
    for (const c of chain.slice(0, -1)) {
      setExpanded((prev) => (prev.has(c) ? prev : new Set(prev).add(c)))
    }
    // 当前目录自身的子目录不自动加载（保持懒加载，展开时再取）。
    for (const ancestor of chain.slice(0, -1)) void ensureLoaded(ancestor)
  }, [currentKey, ensureLoaded])

  // ---- 树点击 → 驱动 FileBrowser 导航（folderNavSignal 受控信号） ----

  /** 包装 listItems：感知目录列表（非检索模式）→ 登记树节点 + 同步当前目录。 */
  const drivenListItems = useCallback(
    async (parentId: string | null, opts?: FileQueryOptions) => {
      const res = await listItemsRef.current(parentId, opts)
      const searching = isSearchQuery(opts)
      lastSearchRef.current = searching
      if (!searching) {
        const key = parentId == null ? ROOT_KEY : parentId
        if (nodesRef.current[key]) registerListing(key, res.items)
        setCurrentKey(key)
      }
      return res
    },
    [registerListing],
  )

  /** 树节点点击（目录名单击 = 进入；根节点回根）：向 FileBrowser 发
   *  folderNavSignal 受控信号——同实例内直接把面包屑切到根→目标完整链
   *  并加载目标目录，一次列表请求，无「先回根再逐段进入」的闪烁
   *  （v2.5 重构）。已在目标目录且中间列表不在检索模式时短路（避免
   *  无谓重载）。同时展开完整链（v2.6：**含目标自身**——点击目录后其
   *  子节点即展开呈现；只展开不收缩，收缩仍由用户点 caret 箭头），并
   *  触发目标自身子级懒加载。 */
  const selectFromTree = (key: string) => {
    if (key === currentKey && !lastSearchRef.current) return
    // 根→目标完整节点链（key=ROOT_KEY 时为空数组 = 回根）。
    const chain: string[] = []
    const seen = new Set<string>()
    let k: string | undefined = key
    while (k && k !== ROOT_KEY && !seen.has(k)) {
      seen.add(k)
      const node: TreeNode | undefined = nodesRef.current[k]
      if (!node) break
      chain.unshift(k)
      k = node.parentId
    }
    setCurrentKey(key)
    setFolderNavSignal({
      seq: ++folderNavSeqRef.current,
      path: chain.map((id) => ({ id, name: nodesRef.current[id]?.name ?? '' })),
    })
    // 展开完整链（含目标；Set.add 幂等 = 已展开的保持展开，不收缩）。
    setExpanded((prev) => {
      const n = new Set(prev)
      chain.forEach((c) => n.add(c))
      return n
    })
    // 目标自身子级懒加载（进入即展开其子节点；已加载则跳过）。
    if (key !== ROOT_KEY) void ensureLoaded(key)
  }

  // ---- 外部「打开文件」信号（树文件节点点击 → FileBrowser 查看弹窗） ----

  // 信号序列号：每次点击自增，保证 FileBrowser 侧 effect 依 seq 变化触发。
  const fileOpenSeqRef = useRef(0)
  const [fileOpenSignal, setFileOpenSignal] = useState<FileBrowserProps['fileOpenSignal']>(undefined)

  /** 拼装节点的命名空间相对路径段（不含根节点；含节点自身）。 */
  const nodeSegmentsOf = (key: string): string[] | null => {
    const node = nodesRef.current[key]
    if (!node) return null
    const segs: string[] = [node.name]
    const seen = new Set<string>([key])
    let k: string | undefined = node.parentId
    while (k && k !== ROOT_KEY && !seen.has(k)) {
      seen.add(k)
      const parent: TreeNode | undefined = nodesRef.current[k]
      if (!parent) return null
      segs.unshift(parent.name)
      k = parent.parentId
    }
    return segs
  }

  /**
   * 文件叶子点击：向 FileBrowser 发 fileOpenSignal 受控信号，打开其内部
   * 的查看弹窗（不再新窗口）；pathSegments 由包装器拼装随信号传去，弹窗
   * 内 by-path 路由（新窗口查看/编辑）按路径构建。
   */
  const openFileFromTree = (key: string) => {
    const node: TreeNode | undefined = nodesRef.current[key]
    if (!node || node.type !== 'file') return
    setFileOpenSignal({ fileId: node.id, seq: ++fileOpenSeqRef.current, pathSegments: nodeSegmentsOf(key) ?? undefined })
  }

  // ---- 默认打开方式（查看/编辑，openers 枚举体系）：偏好 + 集成可用性。 ----
  const [openPrefs, setOpenPrefs] = useState<OpenWithPrefs>({})
  const [ooEnabled, setOoEnabled] = useState(false)
  const [drawioEnabled, setDrawioEnabled] = useState(false)
  useEffect(() => {
    let alive = true
    void listOpenWith()
      .then((map) => { if (alive) setOpenPrefs(map) })
      .catch(() => { /* 偏好不可用（旧后端等）：按内置默认分发 */ })
    void onlyOfficeStatus().then((s) => { if (alive) setOoEnabled(s.enabled) })
    void drawioStatus().then((s) => { if (alive) setDrawioEnabled(s.enabled) })
    return () => { alive = false }
  }, [])

  /**
   * 右键菜单「查看/编辑/打开方式」（新窗口，by-path）：
   * method 缺省 = 生效默认方式（偏好合并内置默认）；等于内置默认时省略
   * ?open= 参数（保留 by-path 页按扩展名自动分发的细化行为）；集成未
   * 启用的方式（office/drawio）在菜单侧已被过滤，此处再兜底忽略。
   */
  const openFileWithFromTree = (key: string, kind: 'view' | 'edit', method?: string) => {
    const ns = browserProps.ns
    const segs = nodeSegmentsOf(key)
    const node = nodesRef.current[key]
    if (!ns || !segs || !node || node.type !== 'file') return
    const builtin = builtinOpenWith(extOf(node.name))
    const effective = effectiveOpenWithFor(node.name, openPrefs)
    let chosen = method ?? (kind === 'view' ? effective.view : effective.edit)
    if (chosen === 'none') return
    if (chosen === 'office' && !ooEnabled) return
    if (chosen === 'drawio' && !drawioEnabled) return
    const builtinMethod = kind === 'view' ? builtin.view : builtin.edit
    const url = new URL(
      `/${kind}/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${encodePathSegments(segs)}/`,
      window.location.origin,
    )
    if (chosen !== builtinMethod) url.searchParams.set('open', chosen)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  /** has_index_web 目录右键菜单「作为网页打开（新窗口）」：by-path 查看页
   *（查看页在目录路径上 resolve index.html，与文件列表菜单行为一致）。 */
  const openFolderAsWebsiteFromTree = (key: string) => {
    const ns = browserProps.ns
    const segs = nodeSegmentsOf(key)
    if (!ns || !segs) return
    const url = new URL(
      `/view/by-path/${ns.type}/${encodeURIComponent(ns.scope)}/${encodePathSegments(segs)}/`,
      window.location.origin,
    )
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  /** 展开/收起（仅经 caret 箭头触发）：未加载（或 force）时触发懒加载。 */
  const toggleExpand = (key: string, force = false) => {
    const node = nodesRef.current[key]
    if (!node) return
    if (expanded.has(key) && !force) {
      setExpanded((prev) => {
        const n = new Set(prev)
        n.delete(key)
        return n
      })
      return
    }
    setExpanded((prev) => new Set(prev).add(key))
    if (node.childIds == null || force) void ensureLoaded(key, force)
  }

  // 根目录子节点由 FileBrowser 首挂载的列表登记；树兜底自取一次，
  // 覆盖 FileBrowser 处于检索模式（列表不登记）时的树可用性。
  useEffect(() => {
    void ensureLoaded(ROOT_KEY)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 树根显示名随 treeRoot 变化同步（v2.6：宿主 spaces 异步加载后空间名
  // 才就位，挂载时可能是「空间」占位——仅更新根节点 name，不重置树状态）。
  useEffect(() => {
    setNodes((prev) => {
      const root = prev[ROOT_KEY]
      if (!root || root.name === treeRoot) return prev
      return { ...prev, [ROOT_KEY]: { ...root, name: treeRoot } }
    })
  }, [treeRoot])

  return (
    <div className="files-shell">
      {/* 全宽顶栏宿主：FileBrowser 工具行 portal 到此（44px，见 styles.css）。 */}
      <div className="files-topbar" ref={setToolbarHost} />
      <div className={`files-tree-layout${aside ? ' has-aside' : ''}`}>
        <FolderTreeNav
          rootLabel={treeRoot}
          nodes={nodes}
          expanded={expanded}
          loadingKeys={loadingKeys}
          currentKey={currentKey}
          onToggleExpand={toggleExpand}
          onSelect={selectFromTree}
          onSelectFile={openFileFromTree}
          onOpenFileWith={browserProps.ns ? openFileWithFromTree : undefined}
          onOpenFolderAsWebsite={browserProps.ns ? openFolderAsWebsiteFromTree : undefined}
          openPrefs={openPrefs}
          ooEnabled={ooEnabled}
          drawioEnabled={drawioEnabled}
          errorText={treeError}
        />
        <div className="files-tree-main">
          <FileBrowser
            {...browserProps}
            listItems={drivenListItems}
            /* 复制/移动弹窗目录树数据源：回传「未包装」的原始 listItems——
               drivenListItems 会登记左侧主树节点并同步当前目录高亮，弹窗
               展开目录若走它会把左侧主树也展开（联动 bug），故隔离。 */
            pickerListItems={browserProps.listItems}
            fileOpenSignal={fileOpenSignal}
            folderNavSignal={folderNavSignal}
            toolbarHost={toolbarHost}
          />
        </div>
        {aside && <div className="workspace-aside">{aside}</div>}
      </div>
    </div>
  )
}
