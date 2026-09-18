import { useEffect, useState } from 'react'
import { FileText, Folder } from 'lucide-react'
import { App as AntdApp, Button, Select } from 'antd'
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
  const { modal: antdModal } = AntdApp.useApp()
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

  const handlePurge = (item: FileItem) => {
    antdModal.confirm({
      title: msg('purge'),
      content: formatMessage(msg('purgeConfirm'), { name: item.name }),
      okText: msg('purge'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await purgeFile(item.id)
          setNotice(formatMessage(msg('purgeOk'), { name: item.name }))
          await load()
        } catch (err) {
          setNotice('')
          setError(`${formatMessage(msg('purgeFailedItem'), { name: item.name })}：${err instanceof Error ? err.message : msg('unknownError')}`)
        }
      },
    })
  }

  return (
    <div className="page wide-page">
      <SpaceSwitcher />
      <div className="page-head">
        <h2>{msg('trash')}</h2>
        <Button type="text" onClick={() => void load()}>{msg('refresh')}</Button>
      </div>
      <div className="filter-bar">
        <label className="filter-item">回收站范围
          <Select
            className="trash-scope-select"
            value={scope}
            onChange={(v) => { setScope(v); setSelected(new Set()); void load(v) }}
            options={[
              { value: 'personal', label: '我的文件' },
              ...teams.map((team) => ({ value: team.id, label: team.name })),
            ]}
          />
        </label>
      </div>

      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && !error && items.length === 0 && <div className="empty">{msg('trashEmpty')}</div>}

      {selected.size > 0 && (
        <div className="batch-bar">
          <span>{formatMessage(msg('selectedCount'), { n: selected.size })}</span>
          <Button size="small" disabled={batchBusy} onClick={() => void handleBatchRestore()}>
            {msg('restoreSelected')}
          </Button>
          <Button type="text" size="small" onClick={() => setSelected(new Set())}>{msg('clearSelection')}</Button>
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
                  <span className="icon">{item.type === 'folder'
                    ? <Folder size={14} strokeWidth={2} aria-hidden="true" />
                    : <FileText size={14} strokeWidth={2} aria-hidden="true" />}</span>
                  {item.name}
                </td>
                <td className="muted">{item.deleted_at ? formatTime(item.deleted_at) : '-'}</td>
                <td className="col-actions">
                  <Button size="small" onClick={() => void handleRestore(item)}>{msg('restore')}</Button>
                  <Button size="small" danger onClick={() => handlePurge(item)}>{msg('purge')}</Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}
