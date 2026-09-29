// 目录快照差异的树形视图（v3.5：替代 JSON 平铺）：
// 输入 SnapshotDiff（added/removed/changed/unchanged 路径数组），构建目录树，
// 变更路径按状态着色（绿=新增 / 红=删除 / 橙=修改；unchanged 折叠默认隐藏），
// 支持按状态过滤与路径搜索。
import { useMemo, useState } from 'react'
import { FileDiff, FileMinus, FilePlus, Folder, FolderOpen } from 'lucide-react'
import type { SnapshotDiff } from '../api'

type Status = 'added' | 'removed' | 'changed'

interface DiffNode {
  name: string
  path: string
  isFolder: boolean
  status?: Status
  children: DiffNode[]
}

/** 路径数组 → 目录树（目录继承子级状态：仅当全部子级同状态时着色；
 *  混合状态目录不着色）。 */
function buildTree(paths: Array<{ path: string; status: Status }>): DiffNode[] {
  const root: DiffNode = { name: '', path: '', isFolder: true, children: [] }
  const folderByPath = new Map<string, DiffNode>([['', root]])
  const ensureFolder = (dirPath: string): DiffNode => {
    const hit = folderByPath.get(dirPath)
    if (hit) return hit
    const idx = dirPath.lastIndexOf('/')
    const parentPath = idx >= 0 ? dirPath.slice(0, idx) : ''
    const name = idx >= 0 ? dirPath.slice(idx + 1) : dirPath
    const node: DiffNode = { name, path: dirPath, isFolder: true, children: [] }
    ensureFolder(parentPath).children.push(node)
    folderByPath.set(dirPath, node)
    return node
  }
  // 先目录后文件（同层目录在前）。
  const sorted = [...paths].sort((a, b) => a.path.split('/').length - b.path.split('/').length || a.path.localeCompare(b.path, 'zh-Hans-CN'))
  for (const { path, status } of sorted) {
    const idx = path.lastIndexOf('/')
    const parent = ensureFolder(idx >= 0 ? path.slice(0, idx) : '')
    parent.children.push({ name: idx >= 0 ? path.slice(idx + 1) : path, path, isFolder: false, status, children: [] })
  }
  // 目录状态归一（全部子树同状态才继承）。
  const resolve = (n: DiffNode): Status | undefined => {
    if (!n.isFolder) return n.status
    const childStatuses = n.children.map(resolve).filter(Boolean) as Status[]
    const uniform = childStatuses.length === n.children.length && childStatuses.length > 0
      && childStatuses.every((s) => s === childStatuses[0])
    n.status = uniform ? childStatuses[0] : undefined
    return n.status
  }
  resolve(root)
  const sortRec = (n: DiffNode) => {
    n.children.sort((a, b) => (a.isFolder === b.isFolder ? a.name.localeCompare(b.name, 'zh-Hans-CN') : a.isFolder ? -1 : 1))
    n.children.forEach(sortRec)
  }
  sortRec(root)
  return root.children
}

const STATUS_META: Record<Status, { cls: string; icon: typeof FilePlus; label: string; labelEn: string }> = {
  added: { cls: 'sdiff-add', icon: FilePlus, label: '新增', labelEn: 'Added' },
  removed: { cls: 'sdiff-del', icon: FileMinus, label: '删除', labelEn: 'Removed' },
  changed: { cls: 'sdiff-mod', icon: FileDiff, label: '修改', labelEn: 'Changed' },
}

function TreeRow({ node, depth, filter, zh }: { node: DiffNode; depth: number; filter: Status | 'all'; zh: boolean }) {
  const [open, setOpen] = useState(depth < 2)
  const meta = node.status ? STATUS_META[node.status] : null
  const Icon = node.isFolder ? (open ? FolderOpen : Folder) : (meta?.icon ?? FileDiff)
  const visibleChildren = node.children.filter((c) => {
    if (filter === 'all') return true
    // 过滤模式：仅保留含目标状态的子树。
    const hasStatus = (n: DiffNode): boolean => (n.status === filter) || n.children.some(hasStatus)
    return hasStatus(c)
  })
  if (filter !== 'all' && !node.isFolder && node.status !== filter) return null
  return (
    <div className="sdiff-row-wrap">
      <div
        className={`sdiff-row${meta ? ` ${meta.cls}` : ''}`}
        style={{ paddingLeft: 6 + depth * 14 }}
        onClick={() => node.isFolder && setOpen((v) => !v)}
        role={node.isFolder ? 'button' : undefined}
      >
        {node.isFolder && <span className="sdiff-caret" aria-hidden="true">{open ? '▾' : '▸'}</span>}
        {!node.isFolder && <span className="sdiff-caret" aria-hidden="true" />}
        <Icon size={13} strokeWidth={2} aria-hidden="true" />
        <span className="sdiff-name" title={node.path}>{node.name}</span>
        {meta && <span className={`sdiff-badge ${meta.cls}`}>{zh ? meta.label : meta.labelEn}</span>}
      </div>
      {node.isFolder && open && visibleChildren.map((c) => (
        <TreeRow key={c.path} node={c} depth={depth + 1} filter={filter} zh={zh} />
      ))}
    </div>
  )
}

export default function SnapshotDiffView({ diff, zh }: { diff: SnapshotDiff; zh: boolean }) {
  const [filter, setFilter] = useState<Status | 'all'>('all')
  const [query, setQuery] = useState('')
  const counts = { added: diff.added.length, removed: diff.removed.length, changed: diff.changed.length }
  const paths = useMemo<Array<{ path: string; status: Status }>>(() => [
    ...diff.added.map((path: string) => ({ path, status: 'added' as Status })),
    ...diff.removed.map((path: string) => ({ path, status: 'removed' as Status })),
    ...diff.changed.map((path: string) => ({ path, status: 'changed' as Status })),
  ].filter(({ path }) => !query.trim() || path.toLowerCase().includes(query.trim().toLowerCase())), [diff, query])
  const tree = useMemo(() => buildTree(paths), [paths])
  const total = counts.added + counts.removed + counts.changed
  return (
    <div className="sdiff">
      <div className="sdiff-toolbar">
        <input
          className="sdiff-search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={zh ? '搜索路径…' : 'Filter paths…'}
        />
        {(['all', 'added', 'removed', 'changed'] as const).map((f) => (
          <button
            key={f}
            type="button"
            className={`sdiff-filter${filter === f ? ' on' : ''}${f !== 'all' ? ` sdiff-${f === 'added' ? 'add' : f === 'removed' ? 'del' : 'mod'}` : ''}`}
            onClick={() => setFilter(f)}
          >
            {f === 'all'
              ? (zh ? `全部 ${total}` : `All ${total}`)
              : `${zh ? STATUS_META[f].label : STATUS_META[f].labelEn} ${counts[f]}`}
          </button>
        ))}
      </div>
      <div className="sdiff-tree">
        {tree.length === 0 && <div className="sdiff-empty muted">{zh ? '没有匹配的差异' : 'No matching changes'}</div>}
        {tree.map((n) => <TreeRow key={n.path} node={n} depth={0} filter={filter} zh={zh} />)}
      </div>
    </div>
  )
}
