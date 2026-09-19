// 回收站弹窗（v1.5 布局整改：原 /trash 整页路由删除，改为文件页工具栏
// 「回收站」按钮打开的 Modal）：范围下拉（个人 / 各团队）+ 多选批量恢复
// + 单项恢复 / 彻底删除 + 一键清空（逐项 purge，部分成功语义）。
// 尺寸：width min(920px, 92vw)，body 定高 70vh 内部滚动。
import { useCallback, useEffect, useState } from 'react'
import { FileText, Folder } from 'lucide-react'
import { App as AntdApp, Button, Modal as AntdModal, Select } from 'antd'
import { FileItem, Team, batchRestoreFiles, listTeams, listTrash, purgeFile, restoreFile } from '../api'
import { describeBatchResults } from './FileBrowser'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false })
}

export default function TrashModal({
  open,
  onClose,
  onChanged,
}: {
  open: boolean
  onClose: () => void
  /** 恢复/清空等操作成功后回调（文件页刷新当前目录用）。 */
  onChanged?: () => void
}) {
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const [items, setItems] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [batchBusy, setBatchBusy] = useState(false)
  const [teams, setTeams] = useState<Team[]>([])
  const [scope, setScope] = useState('personal')

  const load = useCallback(
    async (nextScope = scope) => {
      setLoading(true)
      setError('')
      try {
        const list = await listTrash(nextScope === 'personal' ? 'personal' : 'team', nextScope === 'personal' ? '' : nextScope)
        list.sort((a, b) => a.name.localeCompare(b.name))
        setItems(list)
        setSelected(new Set())
      } catch (err) {
        setError(err instanceof Error ? err.message : msg('trashLoadFailed'))
        setItems([])
      } finally {
        setLoading(false)
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [locale],
  )

  // 打开时加载（关闭期间不轮询）；首次打开拉取团队列表。
  useEffect(() => {
    if (!open) return
    void listTeams().then(setTeams).catch(() => setTeams([]))
    void load('personal')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

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
      onChanged?.()
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
      onChanged?.()
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
          onChanged?.()
          await load()
        } catch (err) {
          setNotice('')
          setError(`${formatMessage(msg('purgeFailedItem'), { name: item.name })}：${err instanceof Error ? err.message : msg('unknownError')}`)
        }
      },
    })
  }

  /** 清空回收站（当前范围）：逐项 purge，部分成功语义（失败明细摘要）。 */
  const handlePurgeAll = () => {
    if (items.length === 0 || batchBusy) return
    antdModal.confirm({
      title: locale === 'zh-CN' ? '清空回收站' : 'Empty trash',
      content: locale === 'zh-CN'
        ? `确定彻底删除当前范围内的 ${items.length} 项？此操作不可恢复。`
        : `Permanently delete ${items.length} items in the current scope? This cannot be undone.`,
      okText: msg('purge'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setBatchBusy(true)
        setError('')
        setNotice('')
        let ok = 0
        const failures: string[] = []
        for (const item of items) {
          try {
            await purgeFile(item.id)
            ok++
          } catch {
            failures.push(item.name)
          }
        }
        setBatchBusy(false)
        if (ok > 0) {
          setNotice(locale === 'zh-CN' ? `清空完成：彻底删除 ${ok} 项${failures.length > 0 ? `，${failures.length} 项失败` : ''}` : `Emptied ${ok} items${failures.length > 0 ? `, ${failures.length} failed` : ''}`)
          onChanged?.()
        }
        if (failures.length > 0) {
          setError(`${locale === 'zh-CN' ? '失败明细' : 'Failures'}：${failures.slice(0, 10).join('；')}`)
        }
        await load()
      },
    })
  }

  return (
    <AntdModal
      open={open}
      onCancel={onClose}
      footer={null}
      title={msg('trash')}
      width="min(920px, 92vw)"
      className="docflow-modal trash-modal"
      styles={{ body: { height: '70vh', maxHeight: '70vh', overflow: 'auto', paddingTop: 12 } }}
      /* 弹窗内部布局定制经官方 classNames 通道注入自有类（styles.css
         「antd Modal 薄封装」节），不再钩 .ant-modal-* 内部结构。 */
      classNames={{
        header: 'docflow-modal-header',
        title: 'docflow-modal-title',
        body: 'docflow-modal-body',
        close: 'docflow-modal-close',
      }}
    >
      <div className="trash-modal-toolbar toolbar-mini">
        <label className="filter-item">
          <span>{locale === 'zh-CN' ? '范围' : 'Scope'}</span>
          <Select
            size="small"
            value={scope}
            onChange={(v) => { setScope(v); setSelected(new Set()); void load(v) }}
            options={[
              { value: 'personal', label: locale === 'zh-CN' ? '我的文件' : 'My files' },
              ...teams.map((team) => ({ value: team.id, label: team.name })),
            ]}
          />
        </label>
        <Button size="small" onClick={() => void load()}>{msg('refresh')}</Button>
        <span className="toolbar-spacer" />
        <Button size="small" danger disabled={batchBusy || items.length === 0} onClick={handlePurgeAll}>
          {locale === 'zh-CN' ? '清空回收站' : 'Empty trash'}
        </Button>
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
    </AntdModal>
  )
}
