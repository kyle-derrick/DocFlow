import { useEffect, useState } from 'react'
import { FileItem, batchRestoreFiles, listTrash, purgeFile, restoreFile } from '../api'
import { describeBatchResults } from '../components/FileBrowser'

function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

/** 回收站：单项恢复/彻底删除 + 多选批量恢复（部分成功语义，逐项结果摘要）。 */
export default function TrashPage() {
  const [items, setItems] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [batchBusy, setBatchBusy] = useState(false)

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      const list = await listTrash()
      list.sort((a, b) => a.name.localeCompare(b.name))
      setItems(list)
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载回收站失败')
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const allSelected = items.length > 0 && items.every((item) => selected.has(item.id))
  const toggleSelectAll = () => {
    setSelected(allSelected ? new Set() : new Set(items.map((item) => item.id)))
  }

  const handleBatchRestore = async () => {
    const ids = [...selected]
    if (ids.length === 0) return
    setBatchBusy(true)
    setError('')
    setNotice('')
    try {
      const results = await batchRestoreFiles(ids)
      setNotice(`批量恢复：${describeBatchResults(results)}`)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : '批量恢复失败')
    } finally {
      setBatchBusy(false)
    }
  }

  const handleRestore = async (item: FileItem) => {
    setError('')
    try {
      await restoreFile(item.id)
      setNotice(`已恢复「${item.name}」`)
      await load()
    } catch (err) {
      setNotice('')
      setError(`恢复「${item.name}」失败：${err instanceof Error ? err.message : '未知错误'}`)
    }
  }

  const handlePurge = async (item: FileItem) => {
    if (!window.confirm(`彻底删除「${item.name}」？此操作不可恢复。`)) return
    setError('')
    try {
      await purgeFile(item.id)
      setNotice(`已彻底删除「${item.name}」`)
      await load()
    } catch (err) {
      setNotice('')
      setError(`彻底删除「${item.name}」失败：${err instanceof Error ? err.message : '未知错误'}`)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>回收站</h2>
        <button className="btn ghost" onClick={() => void load()}>刷新</button>
      </div>

      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading && <div className="hint">加载中…</div>}
      {!loading && !error && items.length === 0 && <div className="empty">回收站为空</div>}

      {selected.size > 0 && (
        <div className="batch-bar">
          <span>已选 {selected.size} 项</span>
          <button className="btn small" disabled={batchBusy} onClick={() => void handleBatchRestore()}>
            恢复所选
          </button>
          <button className="btn ghost small" onClick={() => setSelected(new Set())}>取消选择</button>
        </div>
      )}

      {items.length > 0 && (
        <table className="file-table">
          <thead>
            <tr>
              <th className="col-check">
                <input type="checkbox" checked={allSelected} onChange={toggleSelectAll} aria-label="全选" />
              </th>
              <th>名称</th>
              <th>删除时间</th>
              <th className="col-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.id} className={selected.has(item.id) ? 'selected' : ''}>
                <td className="col-check">
                  <input
                    type="checkbox"
                    checked={selected.has(item.id)}
                    onChange={() => toggleSelect(item.id)}
                    aria-label={`选择 ${item.name}`}
                  />
                </td>
                <td>
                  <span className="icon">{item.type === 'folder' ? '📁' : '📄'}</span>
                  {item.name}
                </td>
                <td className="muted">{item.deleted_at ? formatTime(item.deleted_at) : '-'}</td>
                <td className="col-actions">
                  <button className="btn small" onClick={() => void handleRestore(item)}>恢复</button>
                  <button className="btn small danger" onClick={() => void handlePurge(item)}>彻底删除</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
