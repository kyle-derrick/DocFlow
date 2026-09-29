// 引用文件选择（v3.7）：文件列表模式（文件名 + 所属目录路径，支持搜索
// 过滤文件名与路径），替代目录树——更紧凑、搜索直达。
import { useEffect, useState } from 'react'
import { Check, FileText, FolderOpen } from 'lucide-react'
import { listFiles } from '../../api'
import type { FileItem } from '../../api'
import type { AIAttachFile } from './index'

export default function AIAttachTree({ zh, spaceId, rootFolderId, selected, onToggle }: {
  zh: boolean
  spaceId: string
  rootFolderId: string | null
  selected: AIAttachFile[]
  onToggle: (f: { fileId: string; fileName: string }) => void
}) {
  const [items, setItems] = useState<Array<{ file: FileItem; path: string }>>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let alive = true
    const collect = async (): Promise<Array<{ file: FileItem; path: string }>> => {
      const out: Array<{ file: FileItem; path: string }> = []
      const seen = new Set<string>()
      const walk = async (parentId: string | null, depth: number, prefix: string) => {
        if (depth > 3 || out.length > 300) return
        try {
          const res = await listFiles(parentId, { spaceId, limit: 200 })
          for (const f of res) {
            if (!alive) return
            if (seen.has(f.id)) continue
            seen.add(f.id)
            if (f.type === 'file') {
              out.push({ file: f, path: prefix + f.name })
            } else if (f.type === 'folder') {
              await walk(f.id, depth + 1, prefix + f.name + '/')
            }
          }
        } catch { /* 静默 */ }
      }
      await walk(rootFolderId ?? null, 0, '')
      return out
    }
    void collect().then((list) => { if (alive) { setItems(list); setLoading(false) } })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [spaceId])

  if (loading) return <div className="ai-attach-state muted">{zh ? '加载文件…' : 'Loading files…'}</div>
  if (items.length === 0) return <div className="ai-attach-state muted">{zh ? '空间内暂无文件' : 'No files in this space'}</div>
  return (
    <div className="ai-attach-filelist">
      {items.map(({ file, path }) => {
        const isSelected = selected.some((s) => s.fileId === file.id)
        const dir = path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : ''
        return (
          <button
            key={file.id}
            type="button"
            className={`ai-attach-item${isSelected ? ' selected' : ''}`}
            onClick={() => onToggle({ fileId: file.id, fileName: file.name })}
            title={path}
          >
            <FileText size={13} strokeWidth={2} aria-hidden="true" />
            <span className="ai-attach-name">{file.name}</span>
            {dir && <span className="ai-attach-dir"><FolderOpen size={10} strokeWidth={2} aria-hidden="true" />{dir}</span>}
            <Check size={13} strokeWidth={2} aria-hidden="true" className="check" />
          </button>
        )
      })}
    </div>
  )
}
