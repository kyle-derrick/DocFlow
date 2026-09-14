import { Fragment, useEffect, useState } from 'react'
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

/**
 * 我的分享：按 created_at 倒序列出我创建的分享（公开与私有）。
 * 可见性与文件名优先使用列表响应的后端字段（visibility/file_name）；
 * 明文 token 契约上仅公开分享创建时返回一次，取创建时暂存的前端内存态
 * （getShareMeta），刷新后不可再取（提示「创建时已展示」）。
 * 每条可展开「统计」：总访问次数 / 独立访客数 / 最近 20 条访问记录
 *（GET /shares/:id 详情端点，IP 已脱敏为前缀）。
 */
export default function SharedPage() {
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
      setError(err instanceof Error ? err.message : '加载分享失败')
      setShares([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
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

  const handleRevoke = async (s: ShareItem) => {
    if (!window.confirm('确定撤销该分享？撤销后立即失效，不可恢复。')) return
    setError('')
    try {
      await revokeShare(s.id)
      setNotice('分享已撤销')
      setStatsOpen((prev) => {
        const next = { ...prev }
        delete next[s.id]
        return next
      })
      await load()
    } catch (err) {
      setNotice('')
      setError(err instanceof Error ? err.message : '撤销失败')
    }
  }

  const copyLink = async (s: ShareItem, token: string) => {
    const link = `${window.location.origin}/s/${token}`
    try {
      await navigator.clipboard.writeText(link)
      setCopiedId(s.id)
      setTimeout(() => setCopiedId((cur) => (cur === s.id ? '' : cur)), 2000)
    } catch {
      setError('复制失败，请手动复制')
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>我的分享</h2>
        <button className="btn ghost" onClick={() => void load()}>刷新</button>
      </div>

      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading && <div className="hint">加载中…</div>}
      {!loading && !error && shares.length === 0 && <div className="empty">暂无分享记录</div>}

      {shares.length > 0 && (
        <table className="file-table share-table">
          <thead>
            <tr>
              <th>文件名</th>
              <th>权限</th>
              <th>可见性</th>
              <th>链接</th>
              <th>下载次数</th>
              <th>过期时间</th>
              <th className="col-actions">操作</th>
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
                      {s.has_password && <span className="badge" title="受密码保护"> 🔒</span>}
                      {s.watermark_enabled !== false && <span className="badge" title="水印已开启"> ◍</span>}
                    </td>
                    <td>{s.permission === 'download' ? '可下载' : '仅查看'}</td>
                    <td>
                      {visibility === 'public' ? (
                        <span className="badge">公开</span>
                      ) : visibility === 'private' ? (
                        <span className="badge private">私有</span>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td className="col-link">
                      {publicToken ? (
                        <button
                          className="btn small"
                          title={`${window.location.origin}/s/${publicToken}`}
                          onClick={() => void copyLink(s, publicToken)}
                        >
                          {copiedId === s.id ? '已复制 ✓' : '复制链接'}
                        </button>
                      ) : visibility === 'public' ? (
                        <span className="muted">链接创建时已展示</span>
                      ) : visibility === 'private' ? (
                        <span className="muted">授权用户/团队访问</span>
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
                        <span className="badge failed">已撤销</span>
                      ) : expired ? (
                        <span className="badge failed">已过期</span>
                      ) : (
                        formatTime(s.expires_at as string)
                      )}
                    </td>
                    <td className="col-actions">
                      <button className="btn small" onClick={() => void toggleStats(s)}>
                        {stats ? '收起统计' : '统计'}
                      </button>
                      {!revoked && (
                        <button className="btn small danger" onClick={() => void handleRevoke(s)}>撤销</button>
                      )}
                    </td>
                  </tr>
                  {stats && (
                    <tr className="share-stats-row">
                      <td colSpan={7}>
                        {stats === 'loading' ? (
                          <span className="hint">统计加载中…</span>
                        ) : stats === 'error' ? (
                          <span className="error-text">统计加载失败，请重试</span>
                        ) : (
                          <div className="share-stats">
                            <div className="share-stats-summary">
                              <span>总访问：<strong>{stats.stats.total_access}</strong></span>
                              <span>独立访客：<strong>{stats.stats.unique_visitors}</strong></span>
                              <span className="muted">（公开下载/预览成功计入；IP 以哈希存储，仅展示脱敏前缀）</span>
                            </div>
                            {stats.stats.recent.length > 0 ? (
                              <table className="file-table share-stats-table">
                                <thead>
                                  <tr>
                                    <th>时间</th>
                                    <th>动作</th>
                                    <th>IP 前缀</th>
                                    <th>User-Agent</th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {stats.stats.recent.map((r, i) => (
                                    <tr key={i}>
                                      <td className="muted">{formatTime(r.time)}</td>
                                      <td>{r.action === 'download' ? '下载' : '预览'}</td>
                                      <td className="muted">{r.ip_prefix || '—'}</td>
                                      <td className="muted share-stats-ua" title={r.user_agent}>{r.user_agent || '—'}</td>
                                    </tr>
                                  ))}
                                </tbody>
                              </table>
                            ) : (
                              <p className="hint">暂无访问记录</p>
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
