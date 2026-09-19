import { useCallback, useEffect, useMemo, useState } from 'react'
import type { Key, ReactNode } from 'react'
import { App as AntdApp, Button, Input, Select, Table } from 'antd'
import type { ColumnsType, TablePaginationConfig } from 'antd/es/table'
import { Search } from 'lucide-react'
import {
  ApiError,
  ShareDetail,
  ShareItem,
  getFileMeta,
  getShareMeta,
  getShareDetail,
  listShares,
  revokeShare,
} from '../api'
import { formatTime } from '../components/FileBrowser'
import { MessageKey, t, useLocale } from '../i18n'

/** 类型筛选（可见性）：全部 / 公开 / 私有。 */
type TypeFilter = 'all' | 'public' | 'private'
/** 状态筛选：全部 / 生效中 / 已删除 / 已过期。 */
type StatusFilter = 'all' | 'active' | 'revoked' | 'expired'

/**
 * 我的分享（v1.7.1）：antd Table + 服务端分页（page/page_size/total，
 * 后端 GET /shares）+ 服务端过滤（q 文件名 / visibility / status）。
 * - 行操作「删除」= revoke（删除即撤销，链接立即失效，措辞统一为删除）；
 * - 多选批量删除（仅生效中的分享可勾选，部分成功语义）；
 * - 链接列：本会话创建的公开分享（token 仅创建时返回一次，内存缓存
 *   getShareMeta）可直接点击新窗口打开分享页 /s/:token，也可复制；
 * - 行可展开「统计」：总访问次数 / 独立访客数 / 最近 20 条访问记录。
 */
export default function SharedPage() {
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const zh = locale === 'zh-CN'
  const [shares, setShares] = useState<ShareItem[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [copiedId, setCopiedId] = useState('')
  // 统计展开状态：share id -> 详情（加载中为 'loading'）。
  const [statsOpen, setStatsOpen] = useState<Record<string, ShareDetail | 'loading' | 'error'>>({})
  const [expandedKeys, setExpandedKeys] = useState<string[]>([])
  // 搜索输入（防抖后写入 query 触发服务端查询）+ 类型/状态筛选。
  const [queryInput, setQueryInput] = useState('')
  const [query, setQuery] = useState('')
  const [typeFilter, setTypeFilter] = useState<TypeFilter>('all')
  const [statusFilter, setStatusFilter] = useState<StatusFilter>('all')
  // 批量删除勾选（仅生效中的分享 id）。
  const [selected, setSelected] = useState<Key[]>([])
  const [batchBusy, setBatchBusy] = useState(false)

  const load = useCallback(async (p: number, ps: number, q: string, vis: TypeFilter, st: StatusFilter) => {
    setLoading(true)
    setError('')
    setNotice('')
    try {
      const res = await listShares({
        page: p,
        pageSize: ps,
        q: q || undefined,
        visibility: vis === 'all' ? undefined : vis,
        status: st === 'all' ? undefined : st,
      })
      setShares(res.shares)
      setTotal(res.total)
      // 文件名兜底：旧后端无 file_name 字段时并发解析（仅本人 owner 的
      // 文件可取到，失败回退短 ID）。
      if (res.shares.some((s) => !s.file_name)) {
        const missing = res.shares.filter((s) => !s.file_name)
        const results = await Promise.allSettled(missing.map((s) => getFileMeta(s.file_id)))
        setShares((prev) => prev.map((s) => {
          const i = missing.findIndex((m) => m.id === s.id)
          if (i < 0 || results[i].status !== 'fulfilled') return s
          return { ...s, file_name: results[i].value.name }
        }))
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('sharedLoadFailed'))
      setShares([])
      setTotal(0)
    } finally {
      setLoading(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [locale])

  useEffect(() => {
    void load(page, pageSize, query, typeFilter, statusFilter)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [page, pageSize, query, typeFilter, statusFilter])

  // 搜索输入防抖（500ms）→ 触发服务端查询（回第一页）。
  useEffect(() => {
    const timer = window.setTimeout(() => {
      setQuery((cur) => (cur === queryInput.trim() ? cur : queryInput.trim()))
      setPage((p) => (p === 1 ? p : 1))
    }, 500)
    return () => window.clearTimeout(timer)
  }, [queryInput])

  /** 分享状态：revoked / expired / active（删除优先）。 */
  const statusOf = (s: ShareItem): 'active' | 'revoked' | 'expired' => {
    if (s.revoked_at !== null) return 'revoked'
    if (s.expires_at !== null && new Date(s.expires_at).getTime() <= Date.now()) return 'expired'
    return 'active'
  }

  const reloadCurrent = () => void load(page, pageSize, query, typeFilter, statusFilter)

  const toggleStats = async (s: ShareItem, expanded: boolean) => {
    if (!expanded) {
      setExpandedKeys((prev) => prev.filter((k) => k !== s.id))
      setStatsOpen((prev) => {
        const next = { ...prev }
        delete next[s.id]
        return next
      })
      return
    }
    setExpandedKeys((prev) => (prev.includes(s.id) ? prev : [...prev, s.id]))
    setStatsOpen((prev) => ({ ...prev, [s.id]: 'loading' }))
    try {
      const detail = await getShareDetail(s.id)
      setStatsOpen((prev) => ({ ...prev, [s.id]: detail }))
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        // 分享刚被删除等：收起并刷新列表。
        setExpandedKeys((prev) => prev.filter((k) => k !== s.id))
        setStatsOpen((prev) => {
          const next = { ...prev }
          delete next[s.id]
          return next
        })
        reloadCurrent()
        return
      }
      setStatsOpen((prev) => ({ ...prev, [s.id]: 'error' }))
    }
  }

  /** 删除分享（= 撤销：链接立即失效，措辞统一为删除）。 */
  const handleDelete = (s: ShareItem) => {
    antdModal.confirm({
      title: msg('delete'),
      content: zh
        ? '确定删除该分享？删除后链接立即失效，不可恢复。'
        : 'Delete this share? The link becomes invalid immediately and cannot be recovered.',
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await revokeShare(s.id)
          setNotice(zh ? '分享已删除' : 'Share deleted')
          setStatsOpen((prev) => {
            const next = { ...prev }
            delete next[s.id]
            return next
          })
          setSelected((prev) => prev.filter((k) => k !== s.id))
          reloadCurrent()
        } catch (err) {
          setNotice('')
          setError(err instanceof Error ? err.message : zh ? '删除分享失败' : 'Failed to delete share')
        }
      },
    })
  }

  /** 批量删除勾选项（部分成功语义：逐项调用，失败项计数提示）。 */
  const handleBatchDelete = () => {
    const targets = selected.map(String)
    if (targets.length === 0) return
    antdModal.confirm({
      title: zh ? `批量删除 ${targets.length} 个分享` : `Delete ${targets.length} shares`,
      content: zh
        ? '确定删除全部勾选的分享？删除后链接立即失效，不可恢复。'
        : 'Delete all selected shares? Links become invalid immediately and cannot be recovered.',
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setBatchBusy(true)
        setError('')
        try {
          const results = await Promise.allSettled(targets.map((id) => revokeShare(id)))
          const failed = results.filter((r) => r.status === 'rejected').length
          setNotice(failed === 0
            ? (zh ? `已删除 ${targets.length} 个分享` : `Deleted ${targets.length} shares`)
            : (zh
              ? `已删除 ${targets.length - failed} 个，${failed} 个失败（列表已刷新，可重试剩余项）`
              : `${targets.length - failed} deleted, ${failed} failed`))
          setSelected([])
          reloadCurrent()
        } finally {
          setBatchBusy(false)
        }
      },
    })
  }

  const copyLink = async (s: ShareItem, token: string) => {
    const link = `${window.location.origin}/s/${token}`
    try {
      await navigator.clipboard.writeText(link)
      setCopiedId(s.id)
      setTimeout(() => setCopiedId((cur) => (cur === s.id ? '' : cur)), 2000)
    } catch {
      setError(msg('clipboardCopyFailed'))
    }
  }

  const columns: ColumnsType<ShareItem> = [
    {
      title: msg('fileName'),
      key: 'name',
      render: (_, s) => <span title={s.file_id}>{s.file_name ?? `${s.file_id.slice(0, 8)}…`}</span>,
    },
    {
      title: msg('permission'),
      key: 'permission',
      width: 96,
      render: (_, s) => (s.permission === 'download' ? msg('canDownload') : msg('viewOnly')),
    },
    {
      title: msg('visibility'),
      key: 'visibility',
      width: 80,
      render: (_, s) => s.visibility === 'public'
        ? <span className="badge">{msg('publicBadge')}</span>
        : s.visibility === 'private'
          ? <span className="badge private">{msg('privateBadge')}</span>
          : <span className="muted">—</span>,
    },
    {
      title: msg('link'),
      key: 'link',
      render: (_, s) => {
        const publicToken = getShareMeta(s.id)?.token
        if (publicToken) {
          return (
            <span className="share-link-actions">
              {/* 链接可点击：新窗口打开分享页 /s/:token。 */}
              <Button
                size="small"
                type="link"
                className="share-open-link"
                title={`${window.location.origin}/s/${publicToken}`}
                onClick={() => window.open(`/s/${publicToken}`, '_blank', 'noopener')}
              >
                {zh ? '打开 ↗' : 'Open ↗'}
              </Button>
              <Button
                size="small"
                title={`${window.location.origin}/s/${publicToken}`}
                onClick={() => void copyLink(s, publicToken)}
              >
                {copiedId === s.id ? msg('copied') : msg('copyLink')}
              </Button>
            </span>
          )
        }
        if (s.visibility === 'public') return <span className="muted">{msg('linkShownOnCreate')}</span>
        if (s.visibility === 'private') return <span className="muted">{msg('grantedAccess')}</span>
        return <span className="muted">—</span>
      },
    },
    {
      title: msg('downloadCount'),
      key: 'downloads',
      width: 110,
      render: (_, s) => (
        <span className="muted">
          {s.download_count}
          {s.max_downloads !== null ? ` / ${s.max_downloads}` : ''}
        </span>
      ),
    },
    {
      title: msg('expiresAt'),
      key: 'expires',
      width: 170,
      render: (_, s) => {
        const status = statusOf(s)
        if (status === 'revoked') return <span className="badge failed">{msg('revokedBadge')}</span>
        if (status === 'expired') return <span className="badge failed">{msg('expiredBadge')}</span>
        return <span className="muted">{formatTime(s.expires_at as string)}</span>
      },
    },
    {
      title: msg('actions'),
      key: 'actions',
      className: 'col-actions',
      width: 150,
      render: (_, s) => (
        <>
          <Button size="small" onClick={() => void toggleStats(s, !expandedKeys.includes(s.id))}>
            {statsOpen[s.id] ? msg('hideStats') : msg('stats')}
          </Button>{' '}
          {statusOf(s) === 'active' && (
            <Button size="small" danger onClick={() => handleDelete(s)}>{msg('delete')}</Button>
          )}
        </>
      ),
    },
  ]

  const pagination: TablePaginationConfig = {
    current: page,
    pageSize,
    total,
    showSizeChanger: true,
    pageSizeOptions: [10, 20, 50, 100],
    showTotal: (n) => (zh ? `共 ${n} 条` : `${n} total`),
    onChange: (p, ps) => {
      setPage(p)
      setPageSize(ps)
    },
  }

  const hasFilter = query !== '' || typeFilter !== 'all' || statusFilter !== 'all'

  const expandedContent = useMemo(() => {
    const map: Record<string, ReactNode> = {}
    for (const s of shares) {
      const stats = statsOpen[s.id]
      if (!stats) continue
      map[s.id] = stats === 'loading' ? (
        <span className="hint">{msg('statsLoading')}</span>
      ) : stats === 'error' ? (
        <span className="error-text">{msg('statsFailed')}</span>
      ) : (
        <div className="share-stats">
          <div className="share-stats-summary">
            <span>{msg('totalAccess')}：<strong>{stats.stats.total_access}</strong></span>
            <span>{msg('uniqueVisitors')}：<strong>{stats.stats.unique_visitors}</strong></span>
          </div>
          {stats.stats.recent.length > 0 ? (
            <table className="file-table share-stats-table">
              <thead>
                <tr>
                  <th>{msg('time')}</th>
                  <th>{msg('actionCol')}</th>
                  <th>{msg('ipPrefix')}</th>
                  <th>User-Agent</th>
                </tr>
              </thead>
              <tbody>
                {stats.stats.recent.map((r, i) => (
                  <tr key={i}>
                    <td className="muted">{formatTime(r.time)}</td>
                    <td>{r.action === 'download' ? msg('download') : msg('preview')}</td>
                    <td className="muted">{r.ip_prefix || '—'}</td>
                    <td className="muted share-stats-ua" title={r.user_agent}>{r.user_agent || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <p className="hint">{msg('noAccessRecords')}</p>
          )}
        </div>
      )
    }
    return map
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shares, statsOpen, locale])

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('sharedTitle')}</h2>
        <Button type="text" onClick={reloadCurrent}>{msg('refresh')}</Button>
      </div>

      {/* 工具行：搜索（文件名，服务端）+ 类型/状态筛选 + 批量删除（有勾选时可用）。 */}
      <div className="share-toolbar">
        <Input
          size="small"
          allowClear
          className="share-search"
          value={queryInput}
          onChange={(e) => setQueryInput(e.target.value)}
          placeholder={zh ? '搜索文件名…' : 'Search file name…'}
          prefix={<Search size={14} strokeWidth={2} aria-hidden="true" />}
        />
        <Select
          size="small"
          className="share-filter-select"
          value={typeFilter}
          onChange={(v) => { setTypeFilter(v); setPage(1) }}
          options={[
            { value: 'all', label: zh ? '全部类型' : 'All types' },
            { value: 'public', label: zh ? '公开链接' : 'Public' },
            { value: 'private', label: zh ? '私有分享' : 'Private' },
          ]}
        />
        <Select
          size="small"
          className="share-filter-select"
          value={statusFilter}
          onChange={(v) => { setStatusFilter(v); setPage(1) }}
          options={[
            { value: 'all', label: zh ? '全部状态' : 'All status' },
            { value: 'active', label: zh ? '生效中' : 'Active' },
            { value: 'revoked', label: zh ? '已删除' : 'Deleted' },
            { value: 'expired', label: zh ? '已过期' : 'Expired' },
          ]}
        />
        <Button
          size="small"
          danger
          disabled={selected.length === 0 || batchBusy}
          loading={batchBusy}
          onClick={handleBatchDelete}
        >
          {zh ? `批量删除${selected.length > 0 ? `（${selected.length}）` : ''}` : `Delete selected${selected.length > 0 ? ` (${selected.length})` : ''}`}
        </Button>
        {hasFilter && (
          <Button size="small" type="text" onClick={() => { setQueryInput(''); setQuery(''); setTypeFilter('all'); setStatusFilter('all'); setPage(1) }}>
            {zh ? '清除筛选' : 'Clear filters'}
          </Button>
        )}
      </div>

      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}

      <Table<ShareItem>
        rowKey="id"
        size="small"
        className="file-table share-table share-antd-table"
        columns={columns}
        dataSource={shares}
        loading={loading}
        pagination={pagination}
        rowClassName={(s) => (statusOf(s) !== 'active' ? 'row-muted' : '')}
        rowSelection={{
          selectedRowKeys: selected,
          onChange: (keys) => setSelected(keys),
          // 仅生效中的分享可勾选（批量删除语义恒对可删行）。
          getCheckboxProps: (s) => ({ disabled: statusOf(s) !== 'active' || batchBusy }),
        }}
        expandable={{
          expandedRowKeys: expandedKeys,
          onExpand: (_, s) => void toggleStats(s, !expandedKeys.includes(s.id)),
          expandedRowRender: (s) => expandedContent[s.id] ?? null,
          showExpandColumn: false,
        }}
        locale={{ emptyText: zh ? '暂无分享记录' : 'No shares yet' }}
      />
    </div>
  )
}
