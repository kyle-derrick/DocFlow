// 个人仪表盘（/dashboard，v1.1）：概览统计大数字卡片 + 最近文件列表 +
// 快捷键提示卡；admin 附全局统计卡组。统计口径为默认空间（owner 维度），
// 其他空间文件单列 space_files。
import { useEffect, useState } from 'react'
import { FileText } from 'lucide-react'
import { DashboardData, getDashboard } from '../api'
import { hotkeyDocs } from '../components/HotkeysHelp'
import { FileViewModal, formatQuota, formatTime } from '../components/FileBrowser'
import { MessageKey, t, useLocale } from '../i18n'

/** 个人统计卡片配置（值可为数字或字节数格式化）。 */
const PERSONAL_CARDS: Array<{ key: keyof Pick<DashboardData, 'files' | 'storage_bytes' | 'space_files' | 'shares' | 'uploads_7d'>; labelKey: MessageKey; bytes?: boolean }> = [
  { key: 'files', labelKey: 'statMyFiles' },
  { key: 'storage_bytes', labelKey: 'statStorage', bytes: true },
  { key: 'space_files', labelKey: 'statTeamFiles' },
  { key: 'shares', labelKey: 'statShares' },
  { key: 'uploads_7d', labelKey: 'statUploads7d' },
]

/** admin 全局统计卡片配置（复用 /admin/stats 字段）。 */
const ADMIN_CARDS: Array<{ key: keyof NonNullable<DashboardData['admin']>; labelKey: MessageKey }> = [
  { key: 'users', labelKey: 'statUsers' },
  { key: 'files', labelKey: 'statFiles' },
  { key: 'uploads', labelKey: 'statUploads' },
  { key: 'sessions', labelKey: 'statSessions' },
  { key: 'shares', labelKey: 'statShares' },
  { key: 'tokens', labelKey: 'statTokens' },
]

export default function DashboardPage() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [data, setData] = useState<DashboardData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  // 「最近文件」弹窗查看目标（v2.4：点击行内弹窗查看，不再新开窗口；
  // 复用文件页 FileViewModal 分发）。
  const [preview, setPreview] = useState<{ id: string; name: string } | null>(null)

  useEffect(() => {
    let alive = true
    void getDashboard()
      .then((d) => {
        if (alive) setData(d)
      })
      .catch((err) => {
        if (alive) setError(err instanceof Error ? err.message : msg('loadFailed'))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('overview')}</h2>
      </div>
      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}

      {!loading && data && (
        <>
          <h3 className="dash-section-title">{msg('personalStats')}</h3>
          <div className="stats-grid">
            {PERSONAL_CARDS.map(({ key, labelKey, bytes }) => (
              <div key={key} className="stat-card accent">
                <div className="stat-value">{bytes ? formatQuota(data[key], false) : data[key]}</div>
                <div className="stat-label">{msg(labelKey)}</div>
              </div>
            ))}
          </div>

          {data.admin && (
            <>
              <h3 className="dash-section-title">{msg('globalStats')}</h3>
              <div className="stats-grid">
                {ADMIN_CARDS.map(({ key, labelKey }) => (
                  <div key={key} className="stat-card">
                    <div className="stat-value">{data.admin?.[key] ?? 0}</div>
                    <div className="stat-label">{msg(labelKey)}</div>
                  </div>
                ))}
              </div>
            </>
          )}

          <h3 className="dash-section-title">{msg('recentFiles')}</h3>
          <div className="panel">
            {data.recent_files.length === 0 ? (
              <div className="empty">{msg('dashEmpty')}</div>
            ) : (
              <ul className="dash-recent">
                {data.recent_files.map((f) => (
                  <li key={f.id}>
                    <button type="button" className="dash-recent-item" onClick={() => setPreview({ id: f.id, name: f.name })} title={msg('goFiles')}>
                      <span className="icon"><FileText size={14} strokeWidth={2} aria-hidden="true" /></span>
                      <span className="dash-recent-name">{f.name}</span>
                      <span className="muted">{formatTime(f.updated_at)}</span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>

          <div className="panel">
            <h3>{msg('hotkeys')}</h3>
            <div className="hotkey-group">
              {hotkeyDocs(locale)[0].items.slice(0, 4).map((item) => (
                <div key={item.desc} className="hotkey-row">
                  <span className="hotkey-desc">{item.desc}</span>
                  <span className="hotkey-keys">
                    {item.keys.map((k) => (
                      <kbd key={k}>{k}</kbd>
                    ))}
                  </span>
                </div>
              ))}
            </div>
          </div>
        </>
      )}

      {/* 最近文件点击 = 弹窗查看（复用文件页查看弹窗分发，不新开窗口）。 */}
      {preview && <FileViewModal file={preview} onClose={() => setPreview(null)} />}
    </div>
  )
}
