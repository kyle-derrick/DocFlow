import { useEffect, useState } from 'react'
import { FileItem, listTrash, purgeFile, restoreFile } from '../api'

function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

export default function TrashPage() {
  const [items, setItems] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

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

      {items.length > 0 && (
        <table className="file-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>删除时间</th>
              <th className="col-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.id}>
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
