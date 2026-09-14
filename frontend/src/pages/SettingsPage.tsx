// 账户设置页（/settings）：登录会话与个人访问令牌（PAT）两个卡片。
// - 登录会话：活跃会话列表（IP/UA/最后活跃）、撤销单个、「撤销全部并登出」
//   ——服务端撤销全部时含当前会话，成功后前端清空令牌并跳转登录页。
// - 个人访问令牌：创建对话框（名称/有效期）→ 一次性明文展示+复制，
//   列表含最近使用时间与撤销。
// 会话列表不标记「当前会话」（实现取舍：当前会话由 refresh cookie 识别，
// 凭据哈希不出服务端）。
import { FormEvent, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ApiError,
  ApiTokenItem,
  NotificationEventType,
  SessionItem,
  createToken,
  listNotificationPreferences,
  listSessions,
  listTokens,
  logout,
  revokeAllSessions,
  revokeSession,
  revokeToken,
  updateNotificationPreference,
} from '../api'
import { formatTime } from '../components/FileBrowser'

/** 通知事件类型的中文标签与说明（顺序即设置页展示顺序）。 */
const NOTIFICATION_TYPE_META: Array<{ type: NotificationEventType; label: string; desc: string }> = [
  { type: 'upload.completed', label: '上传完成', desc: '我的上传完成（校验与安全扫描通过）' },
  { type: 'upload.quarantined', label: '上传隔离提醒', desc: '我的上传未通过安全扫描被隔离' },
  { type: 'share.accessed', label: '分享被下载', desc: '我的公开/私有分享文件被下载' },
  { type: 'file.updated', label: '团队文件更新', desc: '团队文件被其他成员更新新版本' },
]

/** 会话行的 UA 简述（截断展示，完整值经 title 提示）。 */
function uaSummary(ua: string): string {
  if (!ua) return '未知设备'
  return ua.length > 64 ? `${ua.slice(0, 64)}…` : ua
}

/** 登录会话卡片。 */
function SessionsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const navigate = useNavigate()
  const [sessions, setSessions] = useState<SessionItem[]>([])
  const [loading, setLoading] = useState(true)
  const [revoking, setRevoking] = useState<string | null>(null)
  const [revokingAll, setRevokingAll] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      setSessions(await listSessions())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : '会话列表加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const revokeOne = async (id: string) => {
    if (revoking !== null) return
    setRevoking(id)
    try {
      await revokeSession(id)
      onNotice('已撤销该设备的会话')
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '撤销失败')
    } finally {
      setRevoking(null)
    }
  }

  // 撤销全部（含当前）：成功后本会话已失效，清空本地令牌并回登录页。
  const revokeAll = async () => {
    if (revokingAll) return
    setRevokingAll(true)
    try {
      await revokeAllSessions()
      await logout()
      navigate('/login', { replace: true })
    } catch (err) {
      onError(err instanceof Error ? err.message : '撤销全部失败')
      setRevokingAll(false)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>登录会话</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        当前登录的全部活跃设备/浏览器。撤销全部时会包含当前会话，随后需重新登录。
      </div>
      {loading ? (
        <div className="hint">加载中…</div>
      ) : sessions.length === 0 ? (
        <div className="empty">暂无活跃会话</div>
      ) : (
        sessions.map((s) => (
          <div key={s.id} className="setting-row">
            <div className="setting-main">
              <div className="setting-key">{s.ip ?? '未知 IP'}</div>
              <div className="setting-meta muted" title={s.user_agent}>
                {uaSummary(s.user_agent)} · 最后活跃 {formatTime(s.last_active_at)}
              </div>
              <div className="setting-desc muted">
                登录于 {formatTime(s.created_at)} · 有效期至 {formatTime(s.expires_at)}
              </div>
            </div>
            <div className="setting-control">
              <button
                className="btn small danger"
                disabled={revoking !== null || revokingAll}
                onClick={() => void revokeOne(s.id)}
              >
                {revoking === s.id ? '撤销中…' : '撤销'}
              </button>
            </div>
          </div>
        ))
      )}
      <div style={{ marginTop: 12 }}>
        <button className="btn danger" disabled={revokingAll || loading || sessions.length === 0} onClick={() => void revokeAll()}>
          {revokingAll ? '撤销中…' : '撤销全部会话并登出'}
        </button>
      </div>
    </div>
  )
}

/** PAT 创建的有效期选项（天；0 = 永久）。 */
const EXPIRY_OPTIONS: Array<{ value: number; label: string }> = [
  { value: 30, label: '30 天' },
  { value: 90, label: '90 天' },
  { value: 365, label: '365 天' },
  { value: 0, label: '永久（不推荐）' },
]

/** 个人访问令牌卡片：创建对话框（一次性明文+复制）、列表与撤销。 */
function TokensPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [tokens, setTokens] = useState<ApiTokenItem[]>([])
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [name, setName] = useState('')
  const [expiry, setExpiry] = useState(30)
  const [busy, setBusy] = useState(false)
  const [revoking, setRevoking] = useState<string | null>(null)
  /** 最近一次创建的一次性明文（仅展示一次，保存在内存）。 */
  const [oneTimeToken, setOneTimeToken] = useState('')
  const [copied, setCopied] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      setTokens(await listTokens())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : '令牌列表加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    onError('')
    try {
      const created = await createToken(name.trim(), expiry)
      setName('')
      setShowCreate(false)
      setOneTimeToken(created.token)
      setCopied(false)
      onNotice(`已创建令牌「${created.name}」，请立即复制一次性明文`)
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '创建令牌失败')
    } finally {
      setBusy(false)
    }
  }

  const copyToken = async () => {
    try {
      await navigator.clipboard.writeText(oneTimeToken)
      setCopied(true)
    } catch {
      // 剪贴板不可用（如非安全上下文）：保留输入框展示，由用户手动复制。
      setCopied(false)
    }
  }

  const revoke = async (id: string) => {
    if (revoking !== null) return
    setRevoking(id)
    try {
      await revokeToken(id)
      onNotice('已撤销令牌')
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '撤销失败')
    } finally {
      setRevoking(null)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>个人访问令牌</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        供脚本/CI 等以 Bearer dfpat_… 直接调用 API；令牌仅创建时可见一次，撤销立即失效。
      </div>
      {oneTimeToken && (
        <>
          <div className="share-link" style={{ marginBottom: 8 }}>
            <input type="text" readOnly value={oneTimeToken} onFocus={(e) => e.currentTarget.select()} />
            <button className="btn small" type="button" onClick={() => void copyToken()}>
              {copied ? '已复制' : '复制令牌'}
            </button>
          </div>
          <div className="setting-desc muted" style={{ marginBottom: 12 }}>
            该令牌明文仅显示这一次，请立即复制保存；关闭后无法再次查看。
          </div>
        </>
      )}
      {showCreate && (
        <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={(e) => void submit(e)}>
          <label className="field">
            <span>名称</span>
            <input
              type="text"
              required
              maxLength={100}
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="如：备份脚本"
            />
          </label>
          <label className="field">
            <span>有效期</span>
            <select value={expiry} onChange={(e) => setExpiry(Number(e.target.value))}>
              {EXPIRY_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
          </label>
          <button className="btn primary" type="submit" disabled={busy || name.trim() === ''}>
            {busy ? '创建中…' : '创建'}
          </button>
          <button className="btn" type="button" disabled={busy} onClick={() => { setShowCreate(false); setName('') }}>
            取消
          </button>
        </form>
      )}
      {loading ? (
        <div className="hint">加载中…</div>
      ) : tokens.length === 0 ? (
        <div className="empty">暂无令牌</div>
      ) : (
        tokens.map((t) => (
          <div key={t.id} className="setting-row">
            <div className="setting-main">
              <div className="setting-key">{t.name}</div>
              <div className="setting-meta muted">
                {t.prefix}… · 创建于 {formatTime(t.created_at)}
                {t.last_used_at ? ` · 最近使用 ${formatTime(t.last_used_at)}` : ' · 未使用'}
                {t.expires_at && ` · 有效期至 ${formatTime(t.expires_at)}`}
              </div>
            </div>
            <div className="setting-control">
              {t.expires_at && new Date(t.expires_at).getTime() < Date.now() ? (
                <span className="badge failed">已过期</span>
              ) : (
                <span className="badge available">有效</span>
              )}
              <button
                className="btn small danger"
                disabled={revoking !== null}
                onClick={() => void revoke(t.id)}
              >
                {revoking === t.id ? '撤销中…' : '撤销'}
              </button>
            </div>
          </div>
        ))
      )}
      {!showCreate && (
        <div style={{ marginTop: 12 }}>
          <button className="btn primary" onClick={() => { setOneTimeToken(''); setShowCreate(true) }}>
            创建令牌
          </button>
        </div>
      )}
    </div>
  )
}

/** 通知偏好卡片：各事件类型开关（无记录 = 默认开启；关闭后不再生成该类通知）。 */
function NotificationsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [prefs, setPrefs] = useState<Record<string, boolean>>({})
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState<string | null>(null)

  const load = async () => {
    setLoading(true)
    try {
      const list = await listNotificationPreferences()
      const next: Record<string, boolean> = {}
      for (const p of list) next[p.event_type] = p.enabled
      setPrefs(next)
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : '通知偏好加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const toggle = async (type: NotificationEventType, enabled: boolean) => {
    if (saving !== null) return
    setSaving(type)
    try {
      const updated = await updateNotificationPreference(type, enabled)
      setPrefs((prev) => ({ ...prev, [updated.event_type]: updated.enabled }))
      onNotice(`已${updated.enabled ? '开启' : '关闭'}「${NOTIFICATION_TYPE_META.find((m) => m.type === updated.event_type)?.label ?? updated.event_type}」通知`)
    } catch (err) {
      onError(err instanceof Error ? err.message : '通知偏好更新失败')
    } finally {
      setSaving(null)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>通知偏好</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        站内通知的事件类型开关；关闭后对应事件不再生成通知（默认全部开启）。
      </div>
      {loading ? (
        <div className="hint">加载中…</div>
      ) : (
        NOTIFICATION_TYPE_META.map((meta) => (
          <div key={meta.type} className="setting-row">
            <div className="setting-main">
              <div className="setting-key">{meta.label}</div>
              <div className="setting-desc muted">{meta.desc}</div>
            </div>
            <div className="setting-control">
              <label className="setting-bool">
                <input
                  type="checkbox"
                  checked={prefs[meta.type] ?? true}
                  disabled={saving !== null}
                  onChange={(e) => void toggle(meta.type, e.target.checked)}
                />
                {saving === meta.type ? '保存中…' : prefs[meta.type] ?? true ? '开启' : '关闭'}
              </label>
            </div>
          </div>
        ))
      )}
    </div>
  )
}

export default function SettingsPage() {
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  return (
    <div className="page">
      <div className="page-head">
        <h2>设置</h2>
      </div>
      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      <NotificationsPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />
      <SessionsPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />
      <TokensPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />
    </div>
  )
}
