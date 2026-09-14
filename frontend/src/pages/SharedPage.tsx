import { useEffect, useState } from 'react'
import { ShareItem, getFileMeta, getShareMeta, listShares, revokeShare } from '../api'
import { formatTime } from '../components/FileBrowser'

/**
 * 我的分享：按 created_at 倒序列出我创建的分享（公开与私有）。
 * 可见性与文件名优先使用列表响应的后端字段（visibility/file_name）；
 * 明文 token 契约上仅公开分享创建时返回一次，取创建时暂存的前端内存态
 * （getShareMeta），刷新后不可再取（提示「创建时已展示」）。
 */
export default function SharedPage() {
  const [shares, setShares] = useState<ShareItem[]>([])
  const [names, setNames] = useState<Record<string, string>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [copiedId, setCopiedId] = useState('')

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

  const handleRevoke = async (s: ShareItem) => {
    if (!window.confirm('确定撤销该分享？撤销后立即失效，不可恢复。')) return
    setError('')
    try {
      await revokeShare(s.id)
      setNotice('分享已撤销')
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
              return (
                <tr key={s.id} className={invalid ? 'row-muted' : ''}>
                  <td title={s.file_id}>{s.file_name ?? names[s.file_id] ?? `${s.file_id.slice(0, 8)}…`}</td>
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
                    {!revoked && (
                      <button className="btn small danger" onClick={() => void handleRevoke(s)}>撤销</button>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </div>
  )
}
