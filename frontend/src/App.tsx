import { ReactElement, useEffect, useRef, useState } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import {
  SearchResultItem,
  hasAccessToken,
  isAdmin,
  getMe,
  MeData,
  listNotifications,
  logout,
  markAllNotificationsRead,
  markNotificationRead,
  NotificationItem,
  refreshSession,
  searchFiles,
  SESSION_EXPIRED_EVENT,
  websocketToken,
} from './api'
import { formatTime } from './components/FileBrowser'
import HotkeysHelp from './components/HotkeysHelp'
import { OfflineBadge, UpdateToast } from './components/PwaStatus'
import { useHotkeys } from './useHotkeys'
import LoginPage from './pages/LoginPage'
import SsoPage from './pages/SsoPage'
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
import ExcalidrawPage from './pages/ExcalidrawPage'
import TextEditorPage from './pages/TextEditorPage'
import SettingsPage from './pages/SettingsPage'
import DashboardPage from './pages/DashboardPage'
import ViewerPage from './pages/ViewerPage'
import { EditByPathPage, ViewByPathPage } from './pages/ByPathPage'
import { messages, saveLocale, t, useLocale } from './i18n'

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
    let socket: WebSocket | null = null
    let fallbackTimer: number | null = null
    let connected = false
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
    try {
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const token = websocketToken()
       const wsURL = `${protocol}//${window.location.host}/api/v1/ws/notifications${token ? `?access_token=${encodeURIComponent(token)}` : ''}`
      socket = new WebSocket(wsURL)
      socket.onopen = () => { connected = true }
      socket.onmessage = () => { refresh() }
      socket.onerror = () => { connected = false }
      socket.onclose = () => {
        connected = false
        if (alive && fallbackTimer === null) fallbackTimer = window.setInterval(refresh, NOTIFICATION_POLL_INTERVAL)
      }
    } catch {
      fallbackTimer = window.setInterval(refresh, NOTIFICATION_POLL_INTERVAL)
    }
    return () => {
      alive = false
      socket?.close()
      if (fallbackTimer !== null) window.clearInterval(fallbackTimer)
      void connected
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

/** 检索输入防抖（毫秒）。 */
const SEARCH_DEBOUNCE_MS = 400

/** 检索下拉面板的条数上限。 */
const SEARCH_PANEL_LIMIT = 20

/** 高亮分段：text 中大小写不敏感包含 q 的子串标记 hit。 */
function highlightParts(text: string, q: string): Array<{ text: string; hit: boolean }> {
  if (!q) return [{ text, hit: false }]
  const lower = text.toLowerCase()
  const needle = q.toLowerCase()
  const parts: Array<{ text: string; hit: boolean }> = []
  let i = 0
  for (;;) {
    const idx = lower.indexOf(needle, i)
    if (idx < 0) break
    if (idx > i) parts.push({ text: text.slice(i, idx), hit: false })
    parts.push({ text: text.slice(idx, idx + needle.length), hit: true })
    i = idx + needle.length
  }
  if (i < text.length) parts.push({ text: text.slice(i), hit: false })
  return parts
}

/** snippet（ts_headline）的 [[..]] 标记转高亮分段。 */
function snippetParts(snippet: string): Array<{ text: string; hit: boolean }> {
  const parts: Array<{ text: string; hit: boolean }> = []
  let rest = snippet
  for (;;) {
    const start = rest.indexOf('[[')
    if (start < 0) break
    const end = rest.indexOf(']]', start + 2)
    if (end < 0) break
    if (start > 0) parts.push({ text: rest.slice(0, start), hit: false })
    parts.push({ text: rest.slice(start + 2, end), hit: true })
    rest = rest.slice(end + 2)
  }
  if (rest) parts.push({ text: rest, hit: false })
  return parts
}

function Highlight({ parts }: { parts: Array<{ text: string; hit: boolean }> }) {
  return (
    <>
      {parts.map((p, i) =>
        p.hit ? (
          <mark key={i} className="search-hit">{p.text}</mark>
        ) : (
          <span key={i}>{p.text}</span>
        ),
      )}
    </>
  )
}

/**
 * 顶栏全文搜索：输入防抖 400ms（回车立即）调 /search，下拉面板展示
 * 名称高亮/类型图标/内容片段；点击结果打开预览对话框（跳转限制：
 * 结果仅含 parent_id 无完整面包屑链，不定位到所在目录；文件夹结果
 * 引导前往文件页）。快捷键 '/' 聚焦本输入框（见 GlobalHotkeys）。
 */
function TopBarSearch() {
  const [q, setQ] = useState('')
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [results, setResults] = useState<SearchResultItem[]>([])
  const [error, setError] = useState('')
  const wrapRef = useRef<HTMLDivElement | null>(null)

  const doSearch = async (query: string) => {
    setLoading(true)
    setError('')
    try {
      setResults(await searchFiles(query, SEARCH_PANEL_LIMIT))
    } catch (err) {
      setError(err instanceof Error ? err.message : '搜索失败')
      setResults([])
    } finally {
      setLoading(false)
    }
  }

  // 防抖 400ms：清空输入即收起面板；回车时下方 onKeyDown 直接触发。
  useEffect(() => {
    const trimmed = q.trim()
    if (!trimmed) {
      setResults([])
      setError('')
      setOpen(false)
      return
    }
    setOpen(true)
    const timer = setTimeout(() => void doSearch(trimmed), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [q])

  // 点击面板外关闭。
  useEffect(() => {
    if (!open) return
    const onDocClick = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDocClick)
    return () => document.removeEventListener('mousedown', onDocClick)
  }, [open])

  const openViewer = (item: SearchResultItem) => {
    setOpen(false)
    if (item.type !== 'file') return
    const url = new URL(`/view/${item.id}`, window.location.origin)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  const onInputKey = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      const trimmed = q.trim()
      if (trimmed) void doSearch(trimmed)
    } else if (e.key === 'Escape') {
      setQ('')
      setOpen(false)
      ;(e.target as HTMLInputElement).blur()
    }
  }

  return (
    <div className="topbar-search" ref={wrapRef}>
      <input
        data-hotkey="search"
        type="search"
        value={q}
        placeholder="搜索文件…（按 / 聚焦）"
        aria-label="全文搜索"
        onChange={(e) => setQ(e.target.value)}
        onKeyDown={onInputKey}
        onFocus={() => {
          if (q.trim()) setOpen(true)
        }}
      />
      {open && (
        <div className="search-panel">
          {loading && <div className="search-empty">搜索中…</div>}
          {!loading && error && <div className="search-empty">{error}</div>}
          {!loading && !error && results.length === 0 && <div className="search-empty">没有匹配的文件</div>}
          {!loading && !error && results.length > 0 && (
            <div className="search-list">
              {results.map((r) => (
                <button key={r.id} className="search-item" onClick={() => openViewer(r)}>
                  <span className="search-item-name">
                    <span className="icon">{r.type === 'folder' ? '📁' : '📄'}</span>
                    <Highlight parts={highlightParts(r.name, q.trim())} />
                  </span>
                  {r.snippet && r.snippet !== r.name && (
                    <span className="search-snippet">
                      <Highlight parts={snippetParts(r.snippet)} />
                    </span>
                  )}
                  <span className="search-time muted">{formatTime(r.updated_at)}</span>
                </button>
              ))}
            </div>
          )}
          <div className="search-foot">
            名称与内容匹配；二进制文件仅名称。回车立即搜索。
          </div>
        </div>
      )}

    </div>
  )
}

/**
 * 全局快捷键（v1.1）：'/' 聚焦顶栏全文搜索框、g f/t/s/h 导航、'?' 帮助。
 * 编辑器页（/edit、/drawio，iframe 捕获键盘；/excalidraw 画布工具快捷键）
 * 禁用；弹窗打开时由 useHotkeys 统一跳过（Escape 由 HotkeysHelp 自行处理关闭）。
 */
function GlobalHotkeys() {
  const navigate = useNavigate()
  const location = useLocation()
  const [helpOpen, setHelpOpen] = useState(false)
  const editorPage =
    location.pathname.startsWith('/edit/') ||
    location.pathname.startsWith('/drawio/') ||
    location.pathname.startsWith('/excalidraw/')
  useHotkeys(
    {
      '/': () => {
        document.querySelector<HTMLElement>('[data-hotkey="search"]')?.focus()
      },
      'g f': () => navigate('/'),
      'g t': () => navigate('/teams'),
      'g s': () => navigate('/shared'),
      'g h': () => navigate('/trash'),
      '?': () => setHelpOpen(true),
    },
    !editorPage,
  )
  return helpOpen ? <HotkeysHelp onClose={() => setHelpOpen(false)} /> : null
}

function TopBar() {
  const navigate = useNavigate()
  const location = useLocation()
  const locale = useLocale()
  const msg = (key: keyof typeof messages['zh-CN']) => t(locale, key)
  const switchLocale = () => {
    const next = locale === 'zh-CN' ? 'en-US' : 'zh-CN'
    saveLocale(next)
    window.dispatchEvent(new Event('docflow:locale'))
  }
  // admin 探测：JWT 无 role 声明，降级为请求 /admin/stats（200/403）判定，
  // 结果按会话缓存（登录/登出后失效）；非 admin 隐藏「管理」入口。
  const [admin, setAdmin] = useState(false)
  const [me, setMe] = useState<MeData | null>(null)
  const [menuOpen, setMenuOpen] = useState(false)
  const userMenuRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    void getMe().then(setMe).catch(() => setMe(null))
  }, [])
  useEffect(() => {
    let alive = true
    void isAdmin().then((v) => {
      if (alive) setAdmin(v)
    })
    return () => {
      alive = false
    }
  }, [])
  // 用户菜单点击外部收起：capture 阶段监听，先于菜单内按钮的冒泡处理。
  useEffect(() => {
    if (!menuOpen) return
    const onDocClick = (e: MouseEvent) => {
      if (userMenuRef.current && !userMenuRef.current.contains(e.target as Node)) setMenuOpen(false)
    }
    document.addEventListener('click', onDocClick, true)
    return () => document.removeEventListener('click', onDocClick, true)
  }, [menuOpen])
  const handleLogout = async () => {
    await logout()
    navigate('/login', { replace: true })
  }
  return (
    <header className="topbar">
      <span className="brand">DocFlow</span>
      <nav className="nav">
        <Link to="/dashboard" className={location.pathname === '/dashboard' ? 'active' : ''}>{msg('overview')}</Link>
        <Link to="/" className={location.pathname === '/' ? 'active' : ''}>{msg('files')}</Link>
          <Link to="/shared" className={location.pathname === '/shared' ? 'active' : ''}>{msg('shared')}</Link>
      </nav>
      <TopBarSearch />
      <OfflineBadge />
      <NotificationBell />
      <button className="btn ghost language-btn" onClick={switchLocale} title={msg('language')}>🌐 {locale === 'zh-CN' ? '中' : 'EN'}</button>
      <div className="user-menu" ref={userMenuRef}>
        <button className="btn ghost user-menu-trigger" aria-expanded={menuOpen} onClick={() => setMenuOpen(!menuOpen)}><span className="avatar">{(me?.profile.nickname || me?.username || '?').slice(0, 1).toUpperCase()}</span>{me?.profile.nickname || me?.username || '用户'}<span className="user-menu-arrow" aria-hidden="true">▾</span></button>
        {menuOpen && <div className="user-menu-panel">
          <Link to="/settings/profile">资料</Link><Link to="/settings/security">设置</Link>
          {admin && <Link to="/admin/overview">管理</Link>}
          <button onClick={() => void handleLogout()}>{msg('logout')}</button>
        </div>}
      </div>
    </header>
  )
}

function RequireAuth({ children, bare = false }: { children: ReactElement; bare?: boolean }) {
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
  if (bare) return children
  return (
    <div className="app-shell">
      <TopBar />
      <GlobalHotkeys />
      <main className="content">{children}</main>
    </div>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      {/* SW 新版本提示：全局（含公开页），与登录态无关。 */}
      <UpdateToast />
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        {/* SSO 落地页：后端 OIDC 回调 302 到 /sso#access_token=...，读取后转首页。 */}
        <Route path="/sso" element={<SsoPage />} />
        {/* 邀请注册 / 忘记密码 / 密码重置：公开页面，无需登录，不包 RequireAuth。 */}
        <Route path="/register/:token" element={<RegisterPage />} />
        <Route path="/forgot" element={<ForgotPage />} />
        <Route path="/reset/:token" element={<ResetPage />} />
        {/* 公开分享页：无需登录，不包 RequireAuth。 */}
        <Route path="/s/:token" element={<SharePage />} />
        <Route path="/" element={<RequireAuth><FilesPage /></RequireAuth>} />
        {/* 个人仪表盘概览（v1.1）。 */}
        <Route path="/dashboard" element={<RequireAuth><DashboardPage /></RequireAuth>} />
        <Route path="/teams" element={<RequireAuth><TeamsPage /></RequireAuth>} />
        <Route path="/teams/:id" element={<RequireAuth><TeamSpacePage /></RequireAuth>} />
        <Route path="/shared" element={<RequireAuth><SharedPage /></RequireAuth>} />
        {/* 按路径访问（v1.1，登录）：resolve 现取 grant/file_id 后复用查看与
            编辑器分发；须置于 /view/:fileId、/edit/:fileId 之前匹配。 */}
        <Route path="/view/by-path/:nsType/:nsScope/*" element={<RequireAuth bare><ViewByPathPage /></RequireAuth>} />
        <Route path="/edit/by-path/:nsType/:nsScope/*" element={<RequireAuth bare><EditByPathPage /></RequireAuth>} />
        {/* 按文件类型分发的独立只读查看页；不挂 DocFlow 顶栏。 */}
        <Route path="/view/:fileId" element={<RequireAuth bare><ViewerPage /></RequireAuth>} />
        {/* ONLYOFFICE 在线编辑页（仅显式「编辑 Office」入口进入）。 */}
        <Route path="/edit/:fileId" element={<RequireAuth bare><EditorPage /></RequireAuth>} />
        {/* draw.io 图表编辑页（集成启用时由文件行「图表」按钮进入，iframe embed）。 */}
        <Route path="/drawio/:fileId" element={<RequireAuth bare><DrawioPage /></RequireAuth>} />
        {/* Excalidraw 白板编辑页（.excalidraw 文件行「白板」按钮进入；
            编辑器包经 React.lazy 动态加载独立 chunk）。 */}
        <Route path="/excalidraw/:fileId" element={<RequireAuth bare><ExcalidrawPage /></RequireAuth>} />
        {/* 文本与源码使用不带站点顶栏的独立编辑窗口。 */}
        <Route path="/text/:fileId" element={<RequireAuth bare><TextEditorPage kind="text" /></RequireAuth>} />
        <Route path="/markdown/:fileId" element={<RequireAuth bare><TextEditorPage kind="markdown" /></RequireAuth>} />
        <Route path="/code/:fileId" element={<RequireAuth bare><TextEditorPage kind="text" /></RequireAuth>} />
        <Route path="/admin" element={<Navigate to="/admin/overview" replace />} />
        <Route path="/admin/:section" element={<RequireAuth><AdminPage /></RequireAuth>} />
        <Route path="/trash" element={<RequireAuth><TrashPage /></RequireAuth>} />
        <Route path="/settings" element={<Navigate to="/settings/profile" replace />} />
        {/* 账户设置：登录会话与个人访问令牌（PAT）管理。 */}
        <Route path="/settings/:section" element={<RequireAuth><SettingsPage /></RequireAuth>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
