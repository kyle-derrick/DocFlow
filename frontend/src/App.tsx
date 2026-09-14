import { ReactElement, useEffect, useRef, useState } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { hasAccessToken, isAdmin, listNotifications, logout, markAllNotificationsRead, markNotificationRead, NotificationItem, refreshSession, SESSION_EXPIRED_EVENT } from './api'
import { formatTime } from './components/FileBrowser'
import LoginPage from './pages/LoginPage'
import RegisterPage from './pages/RegisterPage'
import ForgotPage from './pages/ForgotPage'
import ResetPage from './pages/ResetPage'
import FilesPage from './pages/FilesPage'
import TrashPage from './pages/TrashPage'
import SharePage from './pages/SharePage'
import TeamsPage from './pages/TeamsPage'
import TeamSpacePage from './pages/TeamSpacePage'
import SharedPage from './pages/SharedPage'
import AdminPage from './pages/AdminPage'
import EditorPage from './pages/EditorPage'
import DrawioPage from './pages/DrawioPage'
import SettingsPage from './pages/SettingsPage'

/** 铃铛未读数轮询间隔（毫秒）。 */
const NOTIFICATION_POLL_INTERVAL = 15_000

/** 通知下拉面板展示的条数上限。 */
const NOTIFICATION_PANEL_LIMIT = 20

/**
 * 顶栏通知铃铛：未读数徽标（页面加载拉取 + 15s 轮询 + 打开面板手动刷新），
 * 下拉面板展示最近 20 条（标题/时间/已读态）；「全部已读」一键清理，
 * 点击条目标记已读并关闭面板（v1.0 跳转简化为关闭）。
 */
function NotificationBell() {
  const [open, setOpen] = useState(false)
  const [unread, setUnread] = useState(0)
  const [items, setItems] = useState<NotificationItem[]>([])
  const [loading, setLoading] = useState(false)
  const wrapRef = useRef<HTMLDivElement | null>(null)

  // 未读数轮询：加载即拉取，此后每 15s 刷新；面板打开时由 loadPanel 拉取。
  useEffect(() => {
    let alive = true
    const refresh = () => {
      void listNotifications({ limit: NOTIFICATION_PANEL_LIMIT })
        .then((r) => {
          if (alive) {
            setUnread(r.unread_count)
            setItems(r.items ?? [])
          }
        })
        .catch(() => {
          /* 会话过期由 authFetch 广播；轮询失败静默，下轮重试 */
        })
    }
    refresh()
    const timer = setInterval(refresh, NOTIFICATION_POLL_INTERVAL)
    return () => {
      alive = false
      clearInterval(timer)
    }
  }, [])

  // 点击面板外关闭。
  useEffect(() => {
    if (!open) return
    const onDocClick = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDocClick)
    return () => document.removeEventListener('mousedown', onDocClick)
  }, [open])

  const refreshPanel = async () => {
    setLoading(true)
    try {
      const r = await listNotifications({ limit: NOTIFICATION_PANEL_LIMIT })
      setUnread(r.unread_count)
      setItems(r.items ?? [])
    } catch {
      /* 刷新失败保持现状 */
    } finally {
      setLoading(false)
    }
  }

  const toggle = async () => {
    const next = !open
    setOpen(next)
    if (next) await refreshPanel()
  }

  const markOne = async (id: string, isRead: boolean) => {
    setOpen(false)
    if (isRead) return
    try {
      await markNotificationRead(id)
    } catch {
      /* 已读/不存在：幂等 */
    }
    await refreshPanel()
  }

  const markAll = async () => {
    try {
      await markAllNotificationsRead()
    } catch {
      /* 失败保持现状 */
    }
    await refreshPanel()
  }

  return (
    <div className="bell-wrap" ref={wrapRef}>
      <button
        className="btn ghost bell-btn"
        onClick={() => void toggle()}
        title="站内通知"
        aria-label={`站内通知（${unread} 条未读）`}
      >
        🔔
        {unread > 0 && <span className="bell-badge">{unread > 99 ? '99+' : unread}</span>}
      </button>
      {open && (
        <div className="notif-panel">
          <div className="notif-head">
            <span>通知</span>
            <div className="notif-head-actions">
              <button className="btn small ghost" disabled={loading} onClick={() => void refreshPanel()}>
                {loading ? '刷新中…' : '刷新'}
              </button>
              <button className="btn small" disabled={loading || unread === 0} onClick={() => void markAll()}>
                全部已读
              </button>
            </div>
          </div>
          <div className="notif-list">
            {items.length === 0 ? (
              <div className="notif-empty">暂无通知</div>
            ) : (
              items.map((n) => (
                <button key={n.id} className={`notif-item${n.is_read ? '' : ' unread'}`} onClick={() => void markOne(n.id, n.is_read)}>
                  <span className="notif-title">
                    {!n.is_read && <span className="notif-dot" />}
                    {n.title}
                  </span>
                  <span className="notif-time muted">{formatTime(n.created_at)}</span>
                </button>
              ))
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function TopBar() {
  const navigate = useNavigate()
  const location = useLocation()
  // admin 探测：JWT 无 role 声明，降级为请求 /admin/stats（200/403）判定，
  // 结果按会话缓存（登录/登出后失效）；非 admin 隐藏「管理」入口。
  const [admin, setAdmin] = useState(false)
  useEffect(() => {
    let alive = true
    void isAdmin().then((v) => {
      if (alive) setAdmin(v)
    })
    return () => {
      alive = false
    }
  }, [])
  const handleLogout = async () => {
    await logout()
    navigate('/login', { replace: true })
  }
  return (
    <header className="topbar">
      <span className="brand">DocFlow</span>
      <nav className="nav">
        <Link to="/" className={location.pathname === '/' ? 'active' : ''}>文件</Link>
        <Link to="/trash" className={location.pathname === '/trash' ? 'active' : ''}>回收站</Link>
        {admin && <Link to="/admin" className={location.pathname === '/admin' ? 'active' : ''}>管理</Link>}
        <Link to="/settings" className={location.pathname === '/settings' ? 'active' : ''}>设置</Link>
      </nav>
      <NotificationBell />
      <button className="btn ghost" onClick={handleLogout}>退出登录</button>
    </header>
  )
}

function RequireAuth({ children }: { children: ReactElement }) {
  const navigate = useNavigate()
  // access_token 仅存内存：页面刷新后为空，先用 refresh cookie 静默续期恢复
  // 会话（api.ts 头注释约定的行为），失败再跳登录页。
  const [authed, setAuthed] = useState(hasAccessToken())
  useEffect(() => {
    const onExpired = () => navigate('/login', { replace: true })
    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired)
    return () => window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired)
  }, [navigate])
  useEffect(() => {
    if (authed) return
    let alive = true
    void refreshSession().then((ok) => {
      if (!alive) return
      if (ok) setAuthed(true)
      else navigate('/login', { replace: true })
    })
    return () => {
      alive = false
    }
  }, [authed, navigate])
  if (!authed) return null
  return (
    <div className="app-shell">
      <TopBar />
      <main className="content">{children}</main>
    </div>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        {/* 邀请注册 / 忘记密码 / 密码重置：公开页面，无需登录，不包 RequireAuth。 */}
        <Route path="/register/:token" element={<RegisterPage />} />
        <Route path="/forgot" element={<ForgotPage />} />
        <Route path="/reset/:token" element={<ResetPage />} />
        {/* 公开分享页：无需登录，不包 RequireAuth。 */}
        <Route path="/s/:token" element={<SharePage />} />
        <Route path="/" element={<RequireAuth><FilesPage /></RequireAuth>} />
        <Route path="/teams" element={<RequireAuth><TeamsPage /></RequireAuth>} />
        <Route path="/teams/:id" element={<RequireAuth><TeamSpacePage /></RequireAuth>} />
        <Route path="/shared" element={<RequireAuth><SharedPage /></RequireAuth>} />
        {/* ONLYOFFICE 在线编辑页（集成启用时由文件行「编辑」按钮进入）。 */}
        <Route path="/edit/:fileId" element={<RequireAuth><EditorPage /></RequireAuth>} />
        {/* draw.io 图表编辑页（集成启用时由文件行「图表」按钮进入，iframe embed）。 */}
        <Route path="/drawio/:fileId" element={<RequireAuth><DrawioPage /></RequireAuth>} />
        <Route path="/admin" element={<RequireAuth><AdminPage /></RequireAuth>} />
        <Route path="/trash" element={<RequireAuth><TrashPage /></RequireAuth>} />
        {/* 账户设置：登录会话与个人访问令牌（PAT）管理。 */}
        <Route path="/settings" element={<RequireAuth><SettingsPage /></RequireAuth>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
