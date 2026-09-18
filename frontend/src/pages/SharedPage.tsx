import { Fragment, useEffect, useState } from 'react'
import { App as AntdApp, Button } from 'antd'
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

/**
 * 我的分享：按 created_at 倒序列出我创建的分享（公开与私有）。
 * 可见性与文件名优先使用列表响应的后端字段（visibility/file_name）；
 * 明文 token 契约上仅公开分享创建时返回一次，取创建时暂存的前端内存态
 * （getShareMeta），刷新后不可再取（提示「创建时已展示」）。
 * 每条可展开「统计」：总访问次数 / 独立访客数 / 最近 20 条访问记录
 *（GET /shares/:id 详情端点，IP 已脱敏为前缀）。
 */
export default function SharedPage() {
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const [shares, setShares] = useState<ShareItem[]>([])
  const [names, setNames] = useState<Record<string, string>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [copiedId, setCopiedId] = useState('')
  // 统计展开状态：share id -> 详情（加载中为 'loading'）。
  const [statsOpen, setStatsOpen] = useState<Record<string, ShareDetail | 'loading' | 'error'>>({})

  const load = async () => {
    setLoading(true)
    setError('')
    setNotice('')
    try {
      const list = await listShares()
      setShares(list)
      // 文件名优先用后端 file_name 字段；缺失（旧后端）时才并发把 file_id
      // 解析为文件名（仅本人 owner 的文件可取到，失败回退短 ID）。
      const map: Record<string, string> = {}
      const missing = list.filter((s) => !s.file_name)
      const results = await Promise.allSettled(missing.map((s) => getFileMeta(s.file_id)))
      results.forEach((r, i) => {
        if (r.status === 'fulfilled') map[missing[i].file_id] = r.value.name
      })
      setNames(map)
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('sharedLoadFailed'))
      setShares([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const toggleStats = async (s: ShareItem) => {
    if (statsOpen[s.id]) {
      setStatsOpen((prev) => {
        const next = { ...prev }
        delete next[s.id]
        return next
      })
      return
    }
    setStatsOpen((prev) => ({ ...prev, [s.id]: 'loading' }))
    try {
      const detail = await getShareDetail(s.id)
      setStatsOpen((prev) => ({ ...prev, [s.id]: detail }))
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        // 分享刚被撤销等：收起并刷新列表。
        setStatsOpen((prev) => {
          const next = { ...prev }
          delete next[s.id]
          return next
        })
        await load()
        return
      }
      setStatsOpen((prev) => ({ ...prev, [s.id]: 'error' }))
    }
  }

  const handleRevoke = (s: ShareItem) => {
    antdModal.confirm({
      title: msg('revoke'),
      content: msg('revokeConfirm'),
      okText: msg('revoke'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await revokeShare(s.id)
          setNotice(msg('shareRevoked'))
          setStatsOpen((prev) => {
            const next = { ...prev }
            delete next[s.id]
            return next
          })
          await load()
        } catch (err) {
          setNotice('')
          setError(err instanceof Error ? err.message : msg('revokeFailed'))
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

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('sharedTitle')}</h2>
        <Button type="text" onClick={() => void load()}>{msg('refresh')}</Button>
      </div>

      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && !error && shares.length === 0 && <div className="empty">{msg('sharedEmpty')}</div>}

      {shares.length > 0 && (
        <table className="file-table share-table">
          <thead>
            <tr>
              <th>{msg('fileName')}</th>
              <th>{msg('permission')}</th>
              <th>{msg('visibility')}</th>
              <th>{msg('link')}</th>
              <th>{msg('downloadCount')}</th>
              <th>{msg('expiresAt')}</th>
              <th className="col-actions">{msg('actions')}</th>
            </tr>
          </thead>
          <tbody>
            {shares.map((s) => {
              const publicToken = getShareMeta(s.id)?.token
              const visibility = s.visibility
              const revoked = s.revoked_at !== null
              const expired = s.expires_at !== null && new Date(s.expires_at).getTime() <= Date.now()
              const invalid = revoked || expired
              const stats = statsOpen[s.id]
              return (
                <Fragment key={s.id}>
                  <tr className={invalid ? 'row-muted' : ''}>
                    <td title={s.file_id}>
                      {s.file_name ?? names[s.file_id] ?? `${s.file_id.slice(0, 8)}…`}
                    </td>
                    <td>{s.permission === 'download' ? msg('canDownload') : msg('viewOnly')}</td>
                    <td>
                      {visibility === 'public' ? (
                        <span className="badge">{msg('publicBadge')}</span>
                      ) : visibility === 'private' ? (
                        <span className="badge private">{msg('privateBadge')}</span>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td className="col-link">
                      {publicToken ? (
                        <Button
                          size="small"
                          title={`${window.location.origin}/s/${publicToken}`}
                          onClick={() => void copyLink(s, publicToken)}
                        >
                          {copiedId === s.id ? msg('copied') : msg('copyLink')}
                        </Button>
                      ) : visibility === 'public' ? (
                        <span className="muted">{msg('linkShownOnCreate')}</span>
                      ) : visibility === 'private' ? (
                        <span className="muted">{msg('grantedAccess')}</span>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td className="muted">
                      {s.download_count}
                      {s.max_downloads !== null ? ` / ${s.max_downloads}` : ''}
                    </td>
                    <td className="muted">
                      {revoked ? (
                        <span className="badge failed">{msg('revokedBadge')}</span>
                      ) : expired ? (
                        <span className="badge failed">{msg('expiredBadge')}</span>
                      ) : (
                        formatTime(s.expires_at as string)
                      )}
                    </td>
                    <td className="col-actions">
                      <Button size="small" onClick={() => void toggleStats(s)}>
                        {stats ? msg('hideStats') : msg('stats')}
                      </Button>
                      {!revoked && (
                        <Button size="small" danger onClick={() => handleRevoke(s)}>{msg('revoke')}</Button>
                      )}
                    </td>
                  </tr>
                  {stats && (
                    <tr className="share-stats-row">
                      <td colSpan={7}>
                        {stats === 'loading' ? (
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
                        )}
                      </td>
                    </tr>
                  )}
                </Fragment>
              )
            })}
          </tbody>
        </table>
      )}
    </div>
  )
}
