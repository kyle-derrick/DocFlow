// 个人仪表盘（/dashboard，v1.1）：概览统计大数字卡片 + 最近文件列表 +
// 快捷键提示卡；admin 附全局统计卡组。统计口径为个人空间（owner 维度），
// 团队文件单列 team_files。
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { DashboardData, getDashboard } from '../api'
import { HOTKEY_DOCS } from '../components/HotkeysHelp'
import { formatTime } from '../components/FileBrowser'

/** 字节数人类可读格式（B/KB/MB/GB/TB，一位小数）。 */
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let value = n
  let i = -1
  do {
    value /= 1024
    i++
  } while (value >= 1024 && i < units.length - 1)
  return `${value.toFixed(1)} ${units[i]}`
}

/** 个人统计卡片配置（值可为数字或字节数格式化）。 */
const PERSONAL_CARDS: Array<{ key: keyof Pick<DashboardData, 'files' | 'storage_bytes' | 'team_files' | 'shares' | 'uploads_7d'>; label: string; bytes?: boolean }> = [
  { key: 'files', label: '我的文件' },
  { key: 'storage_bytes', label: '存储占用', bytes: true },
  { key: 'team_files', label: '团队空间文件' },
  { key: 'shares', label: '有效分享' },
  { key: 'uploads_7d', label: '近 7 天上传' },
]

/** admin 全局统计卡片配置（复用 /admin/stats 字段）。 */
const ADMIN_CARDS: Array<{ key: keyof NonNullable<DashboardData['admin']>; label: string }> = [
  { key: 'users', label: '用户' },
  { key: 'files', label: '文件' },
  { key: 'uploads', label: '上传会话' },
  { key: 'sessions', label: '登录会话' },
  { key: 'shares', label: '分享' },
  { key: 'tokens', label: 'API 令牌' },
]

export default function DashboardPage() {
  const navigate = useNavigate()
  const [data, setData] = useState<DashboardData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    void getDashboard()
      .then((d) => {
        if (alive) setData(d)
      })
      .catch((err) => {
        if (alive) setError(err instanceof Error ? err.message : '加载失败')
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => {
      alive = false
    }
  }, [])

  return (
    <div className="page">
      <div className="page-head">
        <h2>概览</h2>
      </div>
      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">加载中…</div>}

      {!loading && data && (
        <>
          <h3 className="dash-section-title">个人统计</h3>
          <div className="stats-grid">
            {PERSONAL_CARDS.map(({ key, label, bytes }) => (
              <div key={key} className="stat-card accent">
                <div className="stat-value">{bytes ? formatBytes(data[key]) : data[key]}</div>
                <div className="stat-label">{label}</div>
              </div>
            ))}
          </div>

          {data.admin && (
            <>
              <h3 className="dash-section-title">全局统计（管理员）</h3>
              <div className="stats-grid">
                {ADMIN_CARDS.map(({ key, label }) => (
                  <div key={key} className="stat-card">
                    <div className="stat-value">{data.admin?.[key] ?? 0}</div>
                    <div className="stat-label">{label}</div>
                  </div>
                ))}
              </div>
            </>
          )}

          <h3 className="dash-section-title">最近文件</h3>
          <div className="panel">
            {data.recent_files.length === 0 ? (
              <div className="empty">还没有文件，去文件页上传或新建</div>
            ) : (
              <ul className="dash-recent">
                {data.recent_files.map((f) => (
                  <li key={f.id}>
                    <button className="dash-recent-item" onClick={() => navigate('/')} title="前往文件页">
                      <span className="icon">📄</span>
                      <span className="dash-recent-name">{f.name}</span>
                      <span className="muted">{formatTime(f.updated_at)}</span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </div>

          <div className="panel">
            <h3>快捷键</h3>
            <div className="setting-desc muted" style={{ marginBottom: 10 }}>
              常用导航与操作；按 <kbd>?</kbd> 查看全部。
            </div>
            <div className="hotkey-group">
              {HOTKEY_DOCS[0].items.slice(0, 4).map((item) => (
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
    </div>
  )
}
