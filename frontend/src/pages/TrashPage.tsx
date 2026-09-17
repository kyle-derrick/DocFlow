import { useEffect, useState } from 'react'
import { FileItem, Team, batchRestoreFiles, listTeams, listTrash, purgeFile, restoreFile } from '../api'
import { describeBatchResults } from '../components/FileBrowser'
import SpaceSwitcher from '../components/SpaceSwitcher'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

/** 回收站：单项恢复/彻底删除 + 多选批量恢复（部分成功语义，逐项结果摘要）。 */
export default function TrashPage() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [items, setItems] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [batchBusy, setBatchBusy] = useState(false)
  const [teams, setTeams] = useState<Team[]>([])
  const [scope, setScope] = useState('personal')

  const load = async (nextScope = scope) => {
    setLoading(true)
    setError('')
    try {
      const list = await listTrash(nextScope === 'personal' ? 'personal' : 'team', nextScope === 'personal' ? '' : nextScope)
      list.sort((a, b) => a.name.localeCompare(b.name))
      setItems(list)
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('trashLoadFailed'))
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void listTeams().then(setTeams).catch(() => setTeams([]))
    void load('personal')
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
      setNotice(formatMessage(msg('batchRestorePrefix'), { summary: describeBatchResults(results) }))
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('batchRestoreFailed'))
    } finally {
      setBatchBusy(false)
    }
  }

  const handleRestore = async (item: FileItem) => {
    setError('')
    try {
      await restoreFile(item.id)
      setNotice(formatMessage(msg('restoreOk'), { name: item.name }))
      await load()
    } catch (err) {
      setNotice('')
      setError(`${formatMessage(msg('restoreFailedItem'), { name: item.name })}：${err instanceof Error ? err.message : msg('unknownError')}`)
    }
  }

  const handlePurge = async (item: FileItem) => {
    if (!window.confirm(formatMessage(msg('purgeConfirm'), { name: item.name }))) return
    setError('')
    try {
      await purgeFile(item.id)
      setNotice(formatMessage(msg('purgeOk'), { name: item.name }))
      await load()
    } catch (err) {
      setNotice('')
      setError(`${formatMessage(msg('purgeFailedItem'), { name: item.name })}：${err instanceof Error ? err.message : msg('unknownError')}`)
    }
  }

  return (
    <div className="page wide-page">
      <SpaceSwitcher />
      <div className="page-head">
        <h2>{msg('trash')}</h2>
        <button className="btn ghost" onClick={() => void load()}>{msg('refresh')}</button>
      </div>
      <div className="filter-bar">
        <label className="filter-item">回收站范围
          <select className="form-select" value={scope} onChange={(e) => { setScope(e.target.value); setSelected(new Set()); void load(e.target.value) }}>
            <option value="personal">我的文件</option>
            {teams.map((team) => <option key={team.id} value={team.id}>{team.name}</option>)}
          </select>
        </label>
      </div>

      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && !error && items.length === 0 && <div className="empty">{msg('trashEmpty')}</div>}

      {selected.size > 0 && (
        <div className="batch-bar">
          <span>{formatMessage(msg('selectedCount'), { n: selected.size })}</span>
          <button className="btn small" disabled={batchBusy} onClick={() => void handleBatchRestore()}>
            {msg('restoreSelected')}
          </button>
          <button className="btn ghost small" onClick={() => setSelected(new Set())}>{msg('clearSelection')}</button>
        </div>
      )}

      {items.length > 0 && (
        <table className="file-table">
          <thead>
            <tr>
              <th className="col-check">
                <input type="checkbox" checked={allSelected} onChange={toggleSelectAll} aria-label={msg('selectAll')} />
              </th>
              <th>{msg('name')}</th>
              <th>{msg('deletedAt')}</th>
              <th className="col-actions">{msg('actions')}</th>
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
                    aria-label={`${msg('selectItem')} ${item.name}`}
                  />
                </td>
                <td>
                  <span className="icon">{item.type === 'folder' ? '📁' : '📄'}</span>
                  {item.name}
                </td>
                <td className="muted">{item.deleted_at ? formatTime(item.deleted_at) : '-'}</td>
                <td className="col-actions">
                  <button className="btn small" onClick={() => void handleRestore(item)}>{msg('restore')}</button>
                  <button className="btn small danger" onClick={() => void handlePurge(item)}>{msg('purge')}</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
