import { ReactElement, useEffect, useRef, useState } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { ArrowUpDown, Bell, ChevronDown, FileText, Folder, Palette, Search, Sparkles } from 'lucide-react'
import { Badge, Button, Dropdown, Input, Popover, Segmented, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import {
  MeData,
  SearchResultItem,
  hasAccessToken,
  isAdmin,
  getMe,
  listNotifications,
  logout,
  markAllNotificationsRead,
  markNotificationRead,
  NotificationItem,
  refreshSession,
  searchFiles,
  SESSION_EXPIRED_EVENT,
  updateMe,
  websocketToken,
} from './api'
import { formatQuota, formatTime, installModalSweepAutoheal, Modal, sweepModalLayer } from './components/FileBrowser'
import HotkeysHelp from './components/HotkeysHelp'
import { OfflineBadge, UpdateToast } from './components/PwaStatus'
import AIAssistant, { AIAssistantButton, openAIAssistant } from './components/AIAssistant'
import { useAIEnabled } from './aiFeature'
import { useHotkeys } from './useHotkeys'
import {
  clearFinishedUploadTasks,
  phaseText,
  requestUploadCancel,
  uploadPhaseActive,
  useUploadTasks,
} from './uploadTasks'
import { THEME_ACCENTS, ThemeMode, loadTheme, saveTheme } from './theme'
import LoginPage from './pages/LoginPage'
import SsoPage from './pages/SsoPage'
import RegisterPage from './pages/RegisterPage'
import ForgotPage from './pages/ForgotPage'
import ResetPage from './pages/ResetPage'
import FilesPage from './pages/FilesPage'
import SharePage from './pages/SharePage'
import SpacesPage from './pages/SpacesPage'
import JoinSpacePage from './pages/JoinSpacePage'
import SharedPage from './pages/SharedPage'
import AdminPage from './pages/AdminPage'
import DrawioPage from './pages/DrawioPage'
import ExcalidrawPage from './pages/ExcalidrawPage'
import TextEditorPage from './pages/TextEditorPage'
import DfdocEditorPage from './pages/DfdocEditorPage'
import SettingsPage from './pages/SettingsPage'
import DashboardPage from './pages/DashboardPage'
import ViewerPage from './pages/ViewerPage'
import { EditDispatchPage } from './pages/ViewerPage'
import StudioPage from './pages/StudioPage'
import { EditByPathPage, ViewByPathPage } from './pages/ByPathPage'
import { messages, saveLocale, t, useLocale } from './i18n'

/** 铃铛未读数轮询间隔（毫秒）。 */
const NOTIFICATION_POLL_INTERVAL = 15_000

/** 通知下拉面板展示的条数上限。 */
const NOTIFICATION_PANEL_LIMIT = 20

/**
 * 顶栏通知铃铛（antd Badge + Popover）：未读数徽标（页面加载拉取 + 15s 轮询
 * 失败回退 + 打开面板手动刷新），面板展示最近 20 条（标题/时间/已读态）；
 * 「全部已读」一键清理，点击条目标记已读并关闭面板（v1.0 跳转简化为关闭） */
function NotificationBell() {
  const [open, setOpen] = useState(false)
  const [unread, setUnread] = useState(0)
  const [items, setItems] = useState<NotificationItem[]>([])
  const [loading, setLoading] = useState(false)

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

  const panel = (
    <div className="notif-panel notif-panel-popover">
      <div className="notif-head">
        <span>通知</span>
        <div className="notif-head-actions">
          <Button size="small" type="text" disabled={loading} onClick={() => void refreshPanel()}>
            {loading ? '刷新中…' : '刷新'}
          </Button>
          <Button size="small" disabled={loading || unread === 0} onClick={() => void markAll()}>
            全部已读
          </Button>
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
  )

  return (
    <Popover
      trigger="click"
      placement="bottomRight"
      arrow={false}
      content={panel}
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (next) void refreshPanel()
      }}
    >
      <Tooltip title="站内通知" mouseEnterDelay={0.5}>
        <Button type="text" className="bell-btn" aria-label={`站内通知（${unread} 条未读）`}>
          <Badge count={unread} size="small" offset={[2, -2]}>
            <Bell size={16} strokeWidth={2} aria-hidden="true" />
          </Badge>
        </Button>
      </Tooltip>
    </Popover>
  )
}

/** 检索输入防抖（毫秒）。 */
const SEARCH_DEBOUNCE_MS = 400

/**
 * 顶栏「传输」入口（v2.6：上传任务从文件页工具栏上移）：进行中任务数
 * Badge（完成转静默 = 无徽标），点击弹窗查看任务列表（可取消进行中 /
 * 清空已完成）。任务状态在模块级 store（uploadTasks.ts），切页面/切
 * 空间不丢；与「消息」通知铃铛并排。
 */
function UploadTasksBell() {
  const tasks = useUploadTasks()
  const [open, setOpen] = useState(false)
  const activeCount = tasks.filter((r) => uploadPhaseActive(r.phase)).length
  return (
    <>
      <Tooltip title={`传输任务${activeCount > 0 ? `（${activeCount} 个进行中）` : ''}`} mouseEnterDelay={0.5}>
        <Button
          type="text"
          className="bell-btn"
          aria-label={`传输任务（${activeCount} 个进行中）`}
          onClick={() => setOpen(true)}
        >
          <Badge count={activeCount} size="small" offset={[2, -2]}>
            <ArrowUpDown size={16} strokeWidth={2} aria-hidden="true" />
          </Badge>
        </Button>
      </Tooltip>
      {open && (
        <Modal title="传输任务" onClose={() => setOpen(false)}>
          {tasks.length === 0 ? (
            <p className="hint">暂无传输任务。</p>
          ) : (
            <>
              <div className="upload-list">
                {tasks.map((row) => (
                  <div key={row.key} className="upload-row">
                    <span className="upload-name">{row.name}</span>
                    <span className={`badge ${row.phase}`}>{phaseText[row.phase]}</span>
                    {row.error && <span className="error-text">{row.error}</span>}
                    {uploadPhaseActive(row.phase) && (
                      <Button size="small" onClick={() => requestUploadCancel(row.key)}>
                        取消
                      </Button>
                    )}
                  </div>
                ))}
              </div>
              <div className="modal-actions">
                <Button
                  disabled={activeCount === tasks.length}
                  onClick={() => clearFinishedUploadTasks()}
                >
                  清空已完成
                </Button>
                <Button onClick={() => setOpen(false)}>关闭</Button>
              </div>
            </>
          )}
        </Modal>
      )}
    </>
  )
}

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
  // 「问 AI」入口（AI 未启用不渲染）：携带当前输入打开 AI 抽屉并以检索
  // 增强模式提问（流式回答 + 引用文件列表）。
  const locale = useLocale()
  const aiOn = useAIEnabled()
  const zh = locale === 'zh-CN'
  const askAI = () => {
    const question = q.trim()
    setOpen(false)
    if (question) openAIAssistant(undefined, question)
    else openAIAssistant()
  }

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
      {/* 与全站 antd Input 风格统一：allowClear + 前置搜索图标（紧凑胶囊
          样式已退役，视觉与文件页工具栏搜索一致）。 */}
      <Input
        data-hotkey="search"
        allowClear
        value={q}
        placeholder="搜索文件…（按 / 聚焦）"
        aria-label="全文搜索"
        prefix={<Search size={14} strokeWidth={2} aria-hidden="true" />}
        onChange={(e) => setQ(e.target.value)}
        onKeyDown={onInputKey}
        onFocus={() => {
          if (q.trim()) setOpen(true)
        }}
      />
      {/* AI 入口去重（顶栏仅保留 AIAssistantButton 图标）：「问 AI」按钮
          移入搜索面板底部作为次级操作，携带当前输入转 AI 抽屉问答。 */}
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
                    <span className="icon">{r.type === 'folder'
                      ? <Folder size={14} strokeWidth={2} aria-hidden="true" />
                      : <FileText size={14} strokeWidth={2} aria-hidden="true" />}</span>
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
            {aiOn && (
              <Button size="small" type="link" className="search-ask-ai-link" onClick={askAI}>
                <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
                {zh ? '用 AI 回答（引用文件）' : 'Ask AI (with sources)'}
              </Button>
            )}
          </div>
        </div>
      )}

    </div>
  )
}

/**
 * 全局快捷键（v1.1）：'/' 聚焦顶栏全文搜索框、g f/t/s 导航、'?' 帮助。
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
      'g t': () => navigate('/spaces'),
      'g s': () => navigate('/shared'),
      '?': () => setHelpOpen(true),
    },
    !editorPage,
  )
  return helpOpen ? <HotkeysHelp onClose={() => setHelpOpen(false)} /> : null
}

/**
 * 「个人信息」独立弹窗（v2.3 资料编辑并入）：头像/姓名/邮箱/账号信息只读
 * 展示 + 可编辑资料字段（显示名/部门/职位/电话/简介/时区，PATCH /me）。
 * 设置页「资料」tab 已删除，本弹窗是唯一的资料编辑入口。
 */
function ProfileInfoModal({ onClose }: { onClose: () => void }) {
  const [me, setMe] = useState<MeData | null>(null)
  const [nickname, setNickname] = useState('')
  const [department, setDepartment] = useState('')
  const [position, setPosition] = useState('')
  const [phone, setPhone] = useState('')
  const [bio, setBio] = useState('')
  const [timezone, setTimezone] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  useEffect(() => {
    let alive = true
    void getMe()
      .then((data) => {
        if (!alive) return
        setMe(data)
        setNickname(data.profile.nickname ?? '')
        setDepartment(data.profile.department ?? '')
        setPosition(data.profile.position ?? '')
        setPhone(data.profile.phone ?? '')
        setBio(data.profile.bio ?? '')
        setTimezone(data.profile.timezone ?? '')
      })
      .catch(() => { if (alive) setError('个人信息加载失败') })
    return () => { alive = false }
  }, [])
  const save = async () => {
    if (busy || !me) return
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const updated = await updateMe({
        nickname,
        department,
        position,
        phone,
        bio,
        timezone: timezone.trim(),
      })
      setMe(updated)
      setNotice('资料已保存')
      window.dispatchEvent(new Event('docflow:me'))
    } catch (err) {
      setError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }
  const display = me?.profile.nickname || me?.username || '用户'
  return (
    <Modal title="个人信息" onClose={onClose}>
      {error && !me && <div className="error-text">{error}</div>}
      {me && (
        <div className="profile-info-modal">
          <div className="profile-info-head">
            <span className="avatar">{display.slice(0, 1).toUpperCase()}</span>
            <div>
              <div className="profile-info-name">{display}</div>
              <div className="muted profile-info-sub">
                {me.username} · {me.role === 'admin' ? '管理员' : '普通用户'}
                {me.status !== 'active' && ` · 状态异常（${me.status}）`}
              </div>
            </div>
          </div>
          {/* 账号信息（只读）。 */}
          <dl className="profile-info-list">
            <div><dt>邮箱</dt><dd title={me.email}>{me.email}</dd></div>
            <div><dt>用户名</dt><dd>{me.username}</dd></div>
            <div><dt>账号 ID</dt><dd className="setting-value-mono" title={me.id}>{me.id}</dd></div>
            <div><dt>注册时间</dt><dd>{formatTime(me.created_at)}</dd></div>
            <div><dt>存储用量</dt><dd>{formatQuota(me.storage.used, false)} / {me.storage.quota > 0 ? formatQuota(me.storage.quota) : '不限'}（配额）</dd></div>
          </dl>
          {/* 资料编辑（原设置页「资料」tab 并入）。 */}
          <div className="profile-edit-fields">
            <label className="field">
              <span>显示名（昵称，≤64 字符）</span>
              <Input
                allowClear
                maxLength={64}
                value={nickname}
                onChange={(e) => setNickname(e.target.value)}
                placeholder="展示名称（留空显示用户名）"
              />
            </label>
            <div className="team-create-row">
              <label className="field">
                <span>部门</span>
                <Input allowClear maxLength={128} value={department} onChange={(e) => setDepartment(e.target.value)} placeholder="如：工程部" />
              </label>
              <label className="field">
                <span>职位</span>
                <Input allowClear maxLength={128} value={position} onChange={(e) => setPosition(e.target.value)} placeholder="如：工程师" />
              </label>
            </div>
            <div className="team-create-row">
              <label className="field">
                <span>电话</span>
                <Input allowClear maxLength={32} value={phone} onChange={(e) => setPhone(e.target.value)} placeholder="13800000000" />
              </label>
              <label className="field">
                <span>时区（IANA 名称）</span>
                <Input allowClear maxLength={64} value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder="Asia/Shanghai" />
              </label>
            </div>
            <label className="field">
              <span>简介（≤512 字符）</span>
              <Input.TextArea rows={3} maxLength={512} value={bio} onChange={(e) => setBio(e.target.value)} placeholder="个人简介" />
            </label>
          </div>
          {error && <div className="error-text">{error}</div>}
          {notice && <div className="banner ok" style={{ margin: '8px 0 0' }}>{notice}</div>}
          <div className="setting-control" style={{ marginTop: 12, justifyContent: 'flex-end' }}>
            <Button type="primary" disabled={busy || timezone.trim() === ''} loading={busy} onClick={() => void save()}>
              {busy ? '保存中…' : '保存资料'}
            </Button>
            <Button disabled={busy} onClick={onClose}>关闭</Button>
          </div>
          <p className="hint" style={{ marginBottom: 0 }}>邮箱与用户名为账号标识，不可在此修改；界面语言与主题见右上角「外观」入口。</p>
        </div>
      )}
    </Modal>
  )
}

/**
 * 外观快捷入口（v2.2）：Popover 内明暗切换 + accent 色板 + 语言，紧凑单列。
 * 与设置页「外观」面板共享 theme.ts 偏好（saveTheme 广播 docflow:theme，
 * 双向实时同步）；语言沿用 docflow:locale 事件。
 */
function AppearanceQuickEntry({ locale }: { locale: 'zh-CN' | 'en-US' }) {
  const zh = locale === 'zh-CN'
  const [open, setOpen] = useState(false)
  const [accent, setAccent] = useState(() => loadTheme().accent)
  const [mode, setMode] = useState<ThemeMode>(() => loadTheme().mode)
  // 设置页外观面板改动主题时同步本入口。
  useEffect(() => {
    const onTheme = () => {
      const cur = loadTheme()
      setAccent(cur.accent)
      setMode(cur.mode)
    }
    window.addEventListener('docflow:theme', onTheme)
    return () => window.removeEventListener('docflow:theme', onTheme)
  }, [])
  // v2.5 防漂移：update 一律基于 localStorage 现值合并（而非本地 state）——
  // THEME_EVENT 监听的 setState 异步生效，快速跨入口连续操作时本地 state
  // 可能仍是旧 accent，用它合并会把用户已选主题覆盖回旧值（主题自动漂移
  // 的竞态根因）；本地 state 仅作 UI 展示（事件同步）。
  const update = (next: { accent?: typeof accent; mode?: ThemeMode }) => {
    saveTheme({ ...loadTheme(), ...next })
  }
  const switchLocale = (next: 'zh-CN' | 'en-US') => {
    saveLocale(next)
    window.dispatchEvent(new Event('docflow:locale'))
  }
  const panel = (
    <div className="appearance-pop">
      <div className="appearance-pop-row">
        <span className="appearance-pop-label">{zh ? '明暗' : 'Theme'}</span>
        <Segmented
          size="small"
          value={mode}
          onChange={(v) => update({ mode: v as ThemeMode })}
          options={[
            { value: 'dark', label: zh ? '深色' : 'Dark' },
            { value: 'light', label: zh ? '浅色' : 'Light' },
            { value: 'system', label: zh ? '系统' : 'Auto' },
          ]}
        />
      </div>
      <div className="appearance-pop-row">
        <span className="appearance-pop-label">{zh ? '主题色' : 'Accent'}</span>
        <div className="appearance-pop-swatches">
          {THEME_ACCENTS.map((a) => (
            <button
              key={a.value}
              type="button"
              className={`theme-swatch${accent === a.value ? ' active' : ''}`}
              style={{ background: a.color }}
              title={a.label}
              aria-label={`${zh ? '主题色' : 'Accent'}：${a.label}`}
              aria-pressed={accent === a.value}
              onClick={() => update({ accent: a.value })}
            />
          ))}
        </div>
      </div>
      <div className="appearance-pop-row">
        <span className="appearance-pop-label">{zh ? '语言' : 'Language'}</span>
        <Segmented
          size="small"
          value={locale}
          onChange={(v) => switchLocale(v === 'en-US' ? 'en-US' : 'zh-CN')}
          options={[
            { value: 'zh-CN', label: '中文' },
            { value: 'en-US', label: 'EN' },
          ]}
        />
      </div>
      <div className="appearance-pop-foot muted">{zh ? '完整外观设置见 设置 → 外观' : 'Full options in Settings → Appearance'}</div>
    </div>
  )
  return (
    <Popover trigger="click" placement="bottomRight" arrow={false} open={open} onOpenChange={setOpen} content={panel}>
      <Tooltip title={zh ? '外观：明暗 / 主题色 / 语言' : 'Appearance'} mouseEnterDelay={0.5}>
        <Button type="text" className="appearance-entry" aria-label={zh ? '外观' : 'Appearance'}>
          <Palette size={15} strokeWidth={2} aria-hidden="true" />
        </Button>
      </Tooltip>
    </Popover>
  )
}

function TopBar() {
  const navigate = useNavigate()
  const location = useLocation()
  const locale = useLocale()
  const msg = (key: keyof typeof messages['zh-CN']) => t(locale, key)
  const aiOn = useAIEnabled()
  // admin 探测：JWT 无 role 声明，降级为请求 /admin/stats（200/403）判定，
  // 结果按会话缓存（登录/登出后失效）；非 admin 隐藏「管理」入口。
  const [admin, setAdmin] = useState(false)
  const [me, setMe] = useState<MeData | null>(null)
  // 「个人信息」独立弹窗（v2.2 用户下拉重组：profile → Modal 而非跳设置页）。
  const [profileOpen, setProfileOpen] = useState(false)
  useEffect(() => {
    void getMe().then(setMe).catch(() => setMe(null))
  }, [])
  // 资料在别处（如个人信息弹窗）更新后刷新顶栏显示名。
  useEffect(() => {
    const onMe = () => { void getMe().then(setMe).catch(() => undefined) }
    window.addEventListener('docflow:me', onMe)
    return () => window.removeEventListener('docflow:me', onMe)
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
  const handleLogout = async () => {
    await logout()
    navigate('/login', { replace: true })
  }
  // 用户菜单（v2.2 重组）：个人信息（独立弹窗）/ 设置（设置页）/ 管理
  //（admin，管理后台 /admin）/ 分隔线 / 退出登录（底部，危险项）。
  // 快捷键帮助（?）属设置性质入口，仍由全局 ? 键与 HotkeysHelp 提供。
  const userMenuItems: MenuProps['items'] = [
    { key: 'profile', label: '个人信息' },
    { key: 'settings', label: msg('settings') },
    ...(admin ? [{ key: 'admin', label: msg('admin') }] : []),
    { type: 'divider' },
    { key: 'logout', label: msg('logout'), danger: true },
  ]
  const onUserMenuClick: MenuProps['onClick'] = ({ key }) => {
    if (key === 'logout') {
      void handleLogout()
      return
    }
    if (key === 'profile') {
      setProfileOpen(true)
      return
    }
    const target = key === 'settings' ? '/settings' : key === 'admin' ? '/admin' : null
    if (target) navigate(target)
  }
  return (
    <header className="topbar">
      <span className="brand">DocFlow</span>
      <nav className="nav">
        <Link to="/dashboard" className={location.pathname === '/dashboard' ? 'active' : ''}>{msg('overview')}</Link>
        <Link to="/" className={location.pathname === '/' ? 'active' : ''}>{msg('files')}</Link>
        {/* 空间管理页（v2.0 统一空间模型）：我的空间卡片 + 成员/用户组/配额/解散/转让入口。 */}
        <Link to="/spaces" className={location.pathname.startsWith('/spaces') ? 'active' : ''}>{msg('teams')}</Link>
        <Link to="/shared" className={location.pathname === '/shared' ? 'active' : ''}>{msg('shared')}</Link>
        {/* AI 创作空间（AI 启用时显示）：独立创作页（文件快速访问 + AI 对话/
            智能体任务聚合），顶栏直达。 */}
        {aiOn && (
          <Link to="/studio" className={location.pathname.startsWith('/studio') ? 'active' : ''}>
            {locale === 'zh-CN' ? 'AI 创作' : 'AI Studio'}
          </Link>
        )}
      </nav>
      <TopBarSearch />
      <OfflineBadge />
      {/* AI 助手入口（AI 能力第一版）：Drawer 侧边栏全局可用。 */}
      <AIAssistantButton />
      {/* 传输任务入口（v2.6：与「消息」通知并排；任务状态全局 store）。 */}
      <UploadTasksBell />
      <NotificationBell />
      {/* 外观快捷入口（v2.2）：明暗 + accent 色板 + 语言（原独立语言按钮并入）。 */}
      <AppearanceQuickEntry locale={locale} />
      <Dropdown menu={{ items: userMenuItems, onClick: onUserMenuClick }} trigger={['click']} placement="bottomRight">
        <Button type="text" className="user-menu-trigger" title={zhName(me)}>
          <span className="avatar">{(me?.profile.nickname || me?.username || '?').slice(0, 1).toUpperCase()}</span>
          {me?.profile.nickname || me?.username || '用户'}
          {/* v2.2：头像旁下拉箭头（表达可展开）。 */}
          <ChevronDown size={13} strokeWidth={2} aria-hidden="true" className="user-menu-caret" />
        </Button>
      </Dropdown>
      {profileOpen && <ProfileInfoModal onClose={() => setProfileOpen(false)} />}
    </header>
  )
}

/** 用户菜单触发按钮的 tooltip 文本。 */
function zhName(me: MeData | null): string {
  return me ? `${me.profile.nickname || me.username}（${me.username}）` : '账户菜单'
}

function RequireAuth({ children, bare = false }: { children: ReactElement; bare?: boolean }) {
  const navigate = useNavigate()
  const location = useLocation()
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
      {/* AI 助手 Drawer（全局挂载，顶栏 Sparkles 打开）。 */}
      <AIAssistant />
      {/* key=pathname：路由切换时重挂载 .content，触发 180ms 淡入过渡
          （styles.css @keyframes content-fadein）。 */}
      <main className="content" key={location.pathname}>{children}</main>
    </div>
  )
}

/** 路由切换时清扫弹层残留（遮罩卡死自愈一环，见 sweepModalLayer）。 */
function ModalSweepOnNavigate() {
  const location = useLocation()
  useEffect(() => { sweepModalLayer() }, [location.pathname])
  return null
}

export default function App() {
  // 全局弹层自愈（v2.x 偶现卡死兜底）：窗口聚焦/切回前台清扫残留遮罩；
  // 路由切换（AppShell 内 useLocation）亦触发一次。
  useEffect(() => installModalSweepAutoheal(), [])
  return (
    <BrowserRouter>
      {/* SW 新版本提示：全局（含公开页），与登录态无关。 */}
      <UpdateToast />
      <ModalSweepOnNavigate />
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
        {/* 文件页别名单路由（/files?space=<id> 切换空间；与 / 同一页面）。 */}
        <Route path="/files" element={<RequireAuth><FilesPage /></RequireAuth>} />
        {/* 个人仪表盘概览（v1.1）。 */}
        <Route path="/dashboard" element={<RequireAuth><DashboardPage /></RequireAuth>} />
        {/* 空间管理页（v2.0）：我的空间卡片；空间内文件统一由 /files?space= 承载。 */}
        <Route path="/spaces" element={<RequireAuth><SpacesPage /></RequireAuth>} />
        {/* 接受空间邀请落地页（/spaces/join/:token，一次性链接）。 */}
        <Route path="/spaces/join/:token" element={<RequireAuth><JoinSpacePage /></RequireAuth>} />
        <Route path="/shared" element={<RequireAuth><SharedPage /></RequireAuth>} />
        {/* 按路径访问（v1.1，登录）：resolve 现取 grant/file_id 后复用查看与
            编辑器分发；须置于 /view/:fileId、/edit/:fileId 之前匹配。 */}
        <Route path="/view/by-path/:nsType/:nsScope/*" element={<RequireAuth bare><ViewByPathPage /></RequireAuth>} />
        <Route path="/edit/by-path/:nsType/:nsScope/*" element={<RequireAuth bare><EditByPathPage /></RequireAuth>} />
        {/* 按文件类型分发的独立只读查看页；不挂 DocFlow 顶栏。 */}
        <Route path="/view/:fileId" element={<RequireAuth bare><ViewerPage /></RequireAuth>} />
        {/* UUID 直链编辑分发页：按扩展名（及 ?open= 偏好）分发各编辑器——
            仅显式「编辑 Office」等合法 force 才进入 OnlyOffice；此前固定渲染
            EditorPage 导致 routeFor 回退 UUID 直链时 txt/md/drawio 全进 OnlyOffice。 */}
        <Route path="/edit/:fileId" element={<RequireAuth bare><EditDispatchPage /></RequireAuth>} />
        {/* draw.io 图表编辑页（集成启用时由文件行「图表」按钮进入，iframe embed）。 */}
        <Route path="/drawio/:fileId" element={<RequireAuth bare><DrawioPage /></RequireAuth>} />
        {/* Excalidraw 白板编辑页（.excalidraw 文件行「白板」按钮进入；
            编辑器包经 React.lazy 动态加载独立 chunk）。 */}
        <Route path="/excalidraw/:fileId" element={<RequireAuth bare><ExcalidrawPage /></RequireAuth>} />
        {/* 文本与源码使用不带站点顶栏的独立编辑窗口。 */}
        <Route path="/text/:fileId" element={<RequireAuth bare><TextEditorPage kind="text" /></RequireAuth>} />
        <Route path="/markdown/:fileId" element={<RequireAuth bare><TextEditorPage kind="markdown" /></RequireAuth>} />
        <Route path="/code/:fileId" element={<RequireAuth bare><TextEditorPage kind="text" /></RequireAuth>} />
        {/* .dfdoc 富文本文档（Tiptap JSON）：编辑/查看同编辑器，独立窗口。 */}
        <Route path="/dfdoc/:fileId" element={<RequireAuth bare><DfdocEditorPage /></RequireAuth>} />
        {/* AI 创作空间：文件快速访问 + AI 对话/智能体任务聚合（AI 启用）。 */}
        <Route path="/studio" element={<RequireAuth><StudioPage /></RequireAuth>} />
        <Route path="/admin" element={<Navigate to="/admin/overview" replace />} />
        <Route path="/admin/:section" element={<RequireAuth><AdminPage /></RequireAuth>} />
        {/* 回收站已弹窗化（文件页工具栏按钮，见 TrashModal），整页路由删除；
            旧地址 /trash 落入通配重定向回文件页。 */}
        {/* 账户设置（v2.3「资料」tab 删除——资料编辑并入右上角「个人信息」弹窗；
            /settings 缺省重定向到「外观」）。 */}
        <Route path="/settings" element={<Navigate to="/settings/appearance" replace />} />
        {/* 账户设置：登录会话与个人访问令牌（PAT）管理。 */}
        <Route path="/settings/:section" element={<RequireAuth><SettingsPage /></RequireAuth>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </BrowserRouter>
  )
}
