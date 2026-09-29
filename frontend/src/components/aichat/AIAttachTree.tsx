// 引用文件的目录树浏览（v3.4：替代纯平铺列表——留空搜索时展示懒加载
// 目录树，文件页时树根 = 当前位置目录，随位置联动；文件节点点击即加入/
// 移除引用）。
import { useState } from 'react'
import { Check } from 'lucide-react'
import FileTreePanel from '../studio/FileTreePanel'
import type { FileItem } from '../../api'
import type { AIAttachFile } from './index'

export default function AIAttachTree({ zh, spaceId, rootFolderId, selected, onToggle }: {
  zh: boolean
  /** 浏览空间（缺省 = 助手工作目录/文件页位置空间）。 */
  spaceId: string
  /** 树根目录（文件页位置或工作目录；null = 空间根）。 */
  rootFolderId: string | null
  selected: AIAttachFile[]
  onToggle: (f: { fileId: string; fileName: string }) => void
}) {
  const [tick, setTick] = useState(0)
  if (!spaceId) {
    return <div className="ai-attach-state muted">{zh ? '空间加载中…' : 'Loading spaces…'}</div>
  }
  return (
    <div className="ai-attach-tree">
      <FileTreePanel
        key={`${spaceId}:${rootFolderId ?? 'root'}:${tick}`}
        zh={zh}
        spaceId={spaceId}
        rootId={rootFolderId}
        rootTitle={zh ? '当前位置' : 'Current location'}
        isRefFile={(f) => selected.some((s) => s.fileId === f.id)}
        onOpenFile={(f: FileItem) => onToggle({ fileId: f.id, fileName: f.name })}
        onRename={() => setTick((n) => n + 1)}
        className="ai-attach-tree-panel"
      />
      <div className="ai-attach-tree-hint muted">
        <Check size={11} strokeWidth={2} aria-hidden="true" />
        {zh ? '点击文件加入/移除引用；目录可展开' : 'Click a file to toggle reference; folders expand'}
      </div>
    </div>
  )
}
