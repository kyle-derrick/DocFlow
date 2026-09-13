import { ReactElement, useEffect, useState } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { hasAccessToken, isAdmin, logout, refreshSession, SESSION_EXPIRED_EVENT } from './api'
import LoginPage from './pages/LoginPage'
import FilesPage from './pages/FilesPage'
import TrashPage from './pages/TrashPage'
import SharePage from './pages/SharePage'
import TeamsPage from './pages/TeamsPage'
import TeamSpacePage from './pages/TeamSpacePage'
import SharedPage from './pages/SharedPage'
import AdminPage from './pages/AdminPage'
import EditorPage from './pages/EditorPage'

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
      </nav>
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
        {/* 公开分享页：无需登录，不包 RequireAuth。 */}
        <Route path="/s/:token" element={<SharePage />} />
        <Route path="/" element={<RequireAuth><FilesPage /></RequireAuth>} />
        <Route path="/teams" element={<RequireAuth><TeamsPage /></RequireAuth>} />
        <Route path="/teams/:id" element={<RequireAuth><TeamSpacePage /></RequireAuth>} />
        <Route path="/shared" element={<RequireAuth><SharedPage /></RequireAuth>} />
        {/* ONLYOFFICE 在线编辑页（集成启用时由文件行「编辑」按钮进入）。 */}
        <Route path="/edit/:fileId" element={<RequireAuth><EditorPage /></RequireAuth>} />
        <Route path="/admin" element={<RequireAuth><AdminPage /></RequireAuth>} />
        <Route path="/trash" element={<RequireAuth><TrashPage /></RequireAuth>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
