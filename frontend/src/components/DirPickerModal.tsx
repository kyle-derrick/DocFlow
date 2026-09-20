// 目录选择器弹窗（复制 / 移动的目标目录选择，替代手输 UUID）：
// - antd Modal + antd Tree：目录树懒加载（展开时经注入的 listChildren 拉取
//   并过滤 folder-only，复用文件列表的目录列举逻辑，同 FolderTreeNav）；
// - 单选目标目录（根目录可选），选中后底部显示路径面包屑；
// - 根节点空间切换（v1.6）：spaces 提供多个根（当前空间 + 我的其余空间），
//   经顶部 Select 切换——复制/移动可跨空间（后端 move/copy
//   继承目标作用域并做写权限校验）；未提供 spaces 时回退单空间（listChildren）；
// - onConfirm({ id, name, path })：id 为目标目录 UUID——根目录返回探测到的
//   根真实 ID（任一根级条目的 parent_id：空间根 UUID），
//   探测不到时为 ''（调用方按 batch/move 的「默认空间根」语义解释）；
// - excludeId：移动时从树中排除被移动项自身（移入更深层子目录由后端
//   INVALID_TARGET 兜底拒绝）。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { Key } from 'react'
import { Input, Modal as AntdModal, Select, Tree } from 'antd'
import type { DataNode } from 'antd/es/tree'
import { FileItem } from '../api'
import { useLocale } from '../i18n'

/** 树节点 key：根目录 pseudo-key（UUID 不会与之冲突）。 */
const ROOT_KEY = '__root__'

/** 可选空间（根节点）：当前空间 / 我的其余空间。 */
export interface PickerSpace {
  /** 稳定 key（空间内唯一，如 'current' / 'space:<uuid>'）。 */
  key: string
  /** 展示名（如「我的文件」「<空间名>」）。 */
  label: string
  /** 拉取该空间某目录的直接子项（parentId=null 表示空间根；内部过滤目录）。 */
  listChildren: (parentId: string | null) => Promise<FileItem[]>
  /**
   * 解析该空间根目录真实 UUID（v1.7）：空间根即使为空也能经
   * listSpaceFiles 响应的 parent_id 拿到；缺省回退「根级条目 parent_id」
   * 探测（默认空间根为空时探测失败 → ''，batch/move 按「默认空间根」缺省语义解释）。
   */
  resolveRootId?: () => Promise<string>
}

interface PickerNode {
  key: string
  title: string
  /** 相对根的路径段（不含根标签）。 */
  path: string[]
  /** 子目录 key 列表；null = 未加载（可展开触发懒加载）。 */
  childIds: string[] | null
}

/** 选择结果：id 为目标目录 UUID（根目录 = 探测到的根真实 ID，缺省 ''）。 */
export interface DirPickerTarget {
  id: string
  name: string
  path: string[]
  /** 选中目标所在空间（未提供 spaces 时为缺省空间）。 */
  spaceKey?: string
  spaceLabel?: string
}

export default function DirPickerModal({
  open,
  title,
  rootLabel,
  listChildren,
  spaces,
  excludeId,
  busy,
  errorText,
  onCancel,
  onConfirm,
}: {
  open: boolean
  title: string
  /** 根节点展示名（如「我的文件」/空间名）；未提供 spaces 时的单空间根标签。 */
  rootLabel: string
  /** 单空间模式的目录列举（parentId=null 表示根）；spaces 优先。 */
  listChildren: (parentId: string | null) => Promise<FileItem[]>
  /** 可切换的空间根列表（v1.6 跨空间复制/移动）；缺省回退单空间模式。 */
  spaces?: PickerSpace[]
  /** 从候选目标中排除的条目 ID（移动自身时排除自己）。 */
  excludeId?: string
  busy?: boolean
  /** 外部错误文案（上次确认失败原因）。 */
  errorText?: string
  onCancel: () => void
  onConfirm: (target: DirPickerTarget) => void
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  // 当前空间（spaces 缺省时为虚拟单空间，listChildren/rootLabel 即其数据源）。
  const spaceList = useMemo<PickerSpace[]>(() => spaces ?? [{
    key: 'default',
    label: rootLabel,
    listChildren,
  }], [spaces, rootLabel, listChildren])
  const [spaceKey, setSpaceKey] = useState(spaceList[0]?.key ?? 'default')
  const space = spaceList.find((s) => s.key === spaceKey) ?? spaceList[0]
  const [nodes, setNodes] = useState<Record<string, PickerNode>>(() => ({
    [ROOT_KEY]: { key: ROOT_KEY, title: rootLabel, path: [], childIds: null },
  }))
  const [expandedKeys, setExpandedKeys] = useState<Key[]>([ROOT_KEY])
  const [selectedKey, setSelectedKey] = useState('')
  const [loading, setLoading] = useState(false)
  const [loadError, setLoadError] = useState('')
  const [keyword, setKeyword] = useState('')
  // 根目录真实 ID：任一根级条目的 parent_id（默认空间根/空间根均为真实 UUID）。
  const rootIdRef = useRef('')
  // 已请求过子级的节点（懒加载去重；重开弹窗/切空间时清空）。
  const loadedKeysRef = useRef<Set<string>>(new Set())
  // nodes 最新引用（loadChildren 组装 childIds 用）。
  const nodesRef = useRef(nodes)
  nodesRef.current = nodes

  /** 拉取并登记某空间的某节点子目录（folder-only）。目标空间显式传参
   *  （v1.7 修复）：空间切换的 handleSpaceChange 在 setSpaceKey 提交前
   *  同步触发加载，若按 state 闭包取 space 会加载「旧空间」的目录树且
   *  根 ID 不被解析——确认时 target.id 指向错误空间。 */
  const loadInto = useCallback(
    async (target: PickerSpace, nodeKey: string) => {
      if (loadedKeysRef.current.has(nodeKey)) return
      loadedKeysRef.current.add(nodeKey)
      setLoading(true)
      setLoadError('')
      try {
        const items = await target.listChildren(nodeKey === ROOT_KEY ? null : nodeKey)
        if (nodeKey === ROOT_KEY) {
          // 根 ID 解析：优先注入的 resolveRootId（空间根为空也可靠），
          // 回退根级条目 parent_id 探测（默认空间根为空时留 ''，按默认根语义）。
          if (target.resolveRootId) {
            try {
              const rid = await target.resolveRootId()
              if (rid) rootIdRef.current = rid
            } catch {
              /* 解析失败走条目探测结果 */
            }
          }
          const rootFromChildren = items.find((it) => it.parent_id)?.parent_id
          if (rootFromChildren) rootIdRef.current = rootFromChildren
        }
        const parent = nodesRef.current[nodeKey]
        const parentPath = parent?.path ?? []
        const folders = items.filter((it) => it.type === 'folder' && it.id !== excludeId)
        setNodes((prev) => {
          const next = { ...prev }
          for (const f of folders) {
            // 保留已加载子级（重展开不丢状态）。
            next[f.id] = { key: f.id, title: f.name, path: [...parentPath, f.name], childIds: next[f.id]?.childIds ?? null }
          }
          next[nodeKey] = { ...(parent ?? { key: nodeKey, title: nodeKey, path: parentPath }), childIds: folders.map((f) => f.id) }
          return next
        })
      } catch (err) {
        loadedKeysRef.current.delete(nodeKey)
        setLoadError(err instanceof Error ? err.message : zh ? '目录加载失败' : 'Failed to load folders')
      } finally {
        setLoading(false)
      }
    },
    [excludeId, zh],
  )

  /** 当前空间版的懒加载（antd Tree loadData 用）。 */
  const loadChildren = useCallback((nodeKey: string) => loadInto(space, nodeKey), [loadInto, space])

  /** 重置到指定空间根并加载根级目录。 */
  const resetToSpace = useCallback((target: PickerSpace) => {
    setNodes({ [ROOT_KEY]: { key: ROOT_KEY, title: target.label, path: [], childIds: null } })
    setExpandedKeys([ROOT_KEY])
    setSelectedKey('')
    setLoadError('')
    setKeyword('')
    rootIdRef.current = ''
    loadedKeysRef.current = new Set()
  }, [])

  // 打开时重置到首个空间；关闭不卸载组件，重开须回到初始视图。
  useEffect(() => {
    if (!open) return
    const first = spaceList[0]
    if (first) {
      setSpaceKey(first.key)
      resetToSpace(first)
      void loadInto(first, ROOT_KEY)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  // 空间切换：重置树并以「目标空间」加载根级目录（显式传参，避免 setState
  // 未提交期间读到旧 space；根 ID 同步解析，见 loadInto）。
  const handleSpaceChange = (key: string) => {
    const target = spaceList.find((s) => s.key === key)
    if (!target || key === spaceKey) return
    setSpaceKey(key)
    resetToSpace(target)
    void loadInto(target, ROOT_KEY)
  }

  /** treeData（nodes → DataNode；未加载 childIds=null 的目录可展开懒加载）。 */
  const treeData = useMemo<DataNode[]>(() => {
    const build = (key: string): DataNode => {
      const n = nodes[key]
      const childIds = n?.childIds
      return {
        key,
        title: n?.title ?? key,
        isLeaf: childIds !== null && childIds.length === 0,
        children: childIds === null ? undefined : childIds.map(build),
      }
    }
    return [build(ROOT_KEY)]
  }, [nodes])

  // 关键字过滤：按目录名过滤（命中节点保留祖先链；空关键字显示全树）。
  const visibleTreeData = useMemo(() => {
    const kw = keyword.trim().toLowerCase()
    if (!kw) return treeData
    const filterNodes = (list: DataNode[]): DataNode[] => {
      const out: DataNode[] = []
      for (const node of list) {
        const children = node.children ? filterNodes(node.children) : []
        if (String(node.title).toLowerCase().includes(kw) || children.length > 0) {
          out.push({ ...node, children: children.length > 0 ? children : node.children })
        }
      }
      return out
    }
    return filterNodes(treeData)
  }, [treeData, keyword])

  const selectedNode = selectedKey
    ? (selectedKey === ROOT_KEY
      ? { key: ROOT_KEY, title: space?.label ?? rootLabel, path: [] as string[] }
      : nodes[selectedKey])
    : undefined
  const breadcrumb = selectedNode ? [space?.label ?? rootLabel, ...selectedNode.path].join(' / ') : ''

  const handleConfirm = () => {
    if (!selectedNode || !space) return
    onConfirm({
      id: selectedKey === ROOT_KEY ? rootIdRef.current : selectedKey,
      name: selectedNode.title,
      path: selectedNode.path,
      spaceKey: space.key,
      spaceLabel: space.label,
    })
  }

  return (
    <AntdModal
      open={open}
      centered
      width="min(560px, 92vw)"
      title={title}
      okText={busy ? (zh ? '处理中…' : 'Working…') : zh ? '确定' : 'OK'}
      cancelText={zh ? '取消' : 'Cancel'}
      okButtonProps={{ disabled: !selectedNode || busy, loading: busy }}
      onOk={handleConfirm}
      onCancel={onCancel}
      className="docflow-modal dir-picker-modal"
      styles={{ body: { overflow: 'auto', maxHeight: 'calc(80vh - 160px)' } }}
      /* 弹窗内部布局定制经官方 classNames 通道注入自有类（styles.css
         「antd Modal 薄封装」节），不再钩 .ant-modal-* 内部结构。 */
      classNames={{
        header: 'docflow-modal-header',
        title: 'docflow-modal-title',
        body: 'docflow-modal-body',
        close: 'docflow-modal-close',
      }}
    >
      <div className="dir-picker">
        {spaceList.length > 1 && (
          <div className="field dir-picker-space">
            <span>{zh ? '目标空间' : 'Target space'}</span>
            <Select
              value={spaceKey}
              onChange={handleSpaceChange}
              options={spaceList.map((s) => ({ value: s.key, label: s.label }))}
            />
          </div>
        )}
        <Input
          allowClear
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder={zh ? '按名称过滤目录…' : 'Filter folders…'}
        />
        <div className="dir-picker-tree">
          <Tree
            blockNode
            selectedKeys={selectedNode ? [selectedNode.key] : []}
            expandedKeys={expandedKeys}
            onExpand={(keys) => setExpandedKeys(keys)}
            loadData={(node) => loadChildren(String(node.key))}
            onSelect={(_, info) => {
              const key = String(info.node.key)
              setSelectedKey((prev) => (prev === key ? '' : key))
            }}
            treeData={visibleTreeData}
          />
        </div>
        <div className="dir-picker-path muted" title={breadcrumb}>
          {selectedNode
            ? (zh ? '目标：' : 'Target: ') + breadcrumb
            : (zh ? '选择目标目录（根目录可选，可切换空间）' : 'Pick a target folder (root selectable; switch space above)')}
        </div>
        {loading && <div className="hint">{zh ? '目录加载中…' : 'Loading…'}</div>}
        {(loadError || errorText) && <div className="error-text">{loadError || errorText}</div>}
      </div>
    </AntdModal>
  )
}
