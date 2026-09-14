// 账户设置页（/settings）：个人资料、外观、通知偏好、两步验证、Webhook、
// 登录会话与个人访问令牌（PAT）卡片。
// - 个人资料（C21a）：昵称/部门/职位/电话/简介/语言/时区编辑（PATCH /me），
//   附存储用量/配额展示（软删文件计入已用）。
// - 登录会话：活跃会话列表（IP/UA/最后活跃）、撤销单个、「撤销全部并登出」
//   ——服务端撤销全部时含当前会话，成功后前端清空令牌并跳转登录页。
// - 个人访问令牌：创建对话框（名称/有效期）→ 一次性明文展示+复制，
//   列表含最近使用时间与撤销。
// - 两步验证（v2 TOTP）：setup（secret 文本+复制，不渲染二维码）→ 确认
//   → 一次性恢复码列表+复制全部；已启用可凭密码禁用。
// - Webhook（v1.1）：注册回调 URL（事件多选）→ 一次性 secret 展示+复制，
//   列表含事件徽标/投递状态/失败计数/启停开关与删除。
// 会话列表不标记「当前会话」（实现取舍：当前会话由 refresh cookie 识别，
// 凭据哈希不出服务端）。
import { FormEvent, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  ApiError,
  ApiTokenItem,
  MeData,
  NotificationEventType,
  PROFILE_LANGUAGES,
  SessionItem,
  TotpSetup,
  TotpStatus,
  WebhookItem,
  beginTotpSetup,
  confirmTotpSetup,
  createToken,
  createWebhook,
  deleteWebhook,
  disableTotp,
  getMe,
  getTotpStatus,
  listNotificationPreferences,
  listSessions,
  listTokens,
  listWebhooks,
  logout,
  revokeAllSessions,
  revokeSession,
  revokeToken,
  updateMe,
  updateToken,
  updateNotificationPreference,
  updateWebhook,
} from '../api'
import { formatTime } from '../components/FileBrowser'
import { THEME_ACCENTS, ThemeAccent, ThemeMode, ThemePreference, loadTheme, saveTheme } from '../theme'
import { MessageKey, saveLocale, t, useLocale } from '../i18n'

/** 通知事件类型的中文标签与说明（顺序即设置页展示顺序）。 */
const NOTIFICATION_TYPE_META: Array<{ type: NotificationEventType; label: string; desc: string }> = [
  { type: 'upload.completed', label: '上传完成', desc: '我的上传完成（校验与安全扫描通过）' },
  { type: 'upload.quarantined', label: '上传隔离提醒', desc: '我的上传未通过安全扫描被隔离' },
  { type: 'share.accessed', label: '分享被下载', desc: '我的公开/私有分享文件被下载' },
  { type: 'file.updated', label: '团队文件更新', desc: '团队文件被其他成员更新新版本' },
  { type: 'file.version.deleted', label: '文件版本被删除', desc: '我的团队文件历史版本被其他成员删除（当前版本不受影响）' },
  { type: 'quota.warning', label: '配额用量警告', desc: '存储用量超过配额的 80%（上传成功后触发）' },
]

/** 字节数的人类可读格式化（GiB/MiB/KB，配额展示用）。 */
function formatBytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(2)} GiB`
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(2)} MiB`
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(2)} KiB`
  return `${n} B`
}

/** 个人资料卡片（C21a）：档案字段编辑 + 存储用量/配额展示。 */
function ProfilePanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [me, setMe] = useState<MeData | null>(null)
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  // 表单草稿（文本字段 null → ''；language/timezone 恒有值）。
  const [nickname, setNickname] = useState('')
  const [department, setDepartment] = useState('')
  const [position, setPosition] = useState('')
  const [phone, setPhone] = useState('')
  const [bio, setBio] = useState('')
  const [language, setLanguage] = useState('zh-CN')
  const [timezone, setTimezone] = useState('Asia/Shanghai')

  const load = async () => {
    setLoading(true)
    try {
      const data = await getMe()
      setMe(data)
      setNickname(data.profile.nickname ?? '')
      setDepartment(data.profile.department ?? '')
      setPosition(data.profile.position ?? '')
      setPhone(data.profile.phone ?? '')
      setBio(data.profile.bio ?? '')
      setLanguage(data.profile.language)
      setTimezone(data.profile.timezone)
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('profileLoadFailed'))
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
      const updated = await updateMe({
        nickname,
        department,
        position,
        phone,
        bio,
        language,
        timezone: timezone.trim(),
      })
      setMe(updated)
      if (language === 'zh-CN' || language === 'en-US') {
        saveLocale(language)
        window.dispatchEvent(new Event('docflow:locale'))
      }
      onNotice(msg('profileSaved'))
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setBusy(false)
    }
  }

  if (loading) {
    return (
      <div className="panel setting-group">
        <h3>{msg('profileTitle')}</h3>
        <div className="hint">{msg('loading')}</div>
      </div>
    )
  }

  const used = me?.storage.used ?? 0
  const quota = me?.storage.quota ?? 0
  const percent = quota > 0 ? Math.min(100, Math.round((used * 100) / quota)) : 0

  return (
    <div className="panel setting-group">
      <h3>个人资料</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        昵称、部门、职位、电话与简介仅用于展示；语言与时区为界面偏好。
        存储用量含回收站（软删除）文件——彻底删除后才释放配额。
      </div>
      <div className="setting-row">
        <div className="setting-main">
          <div className="setting-key">
            {me?.profile.nickname || me?.username || '用户'}
            <span className="badge" style={{ marginLeft: 8 }}>{me?.username}</span>
          </div>
          <div className="setting-desc muted">
            {me?.email} · 注册于 {me ? formatTime(me.created_at) : ''}
          </div>
          <div className="setting-desc muted">
            存储用量 {formatBytes(used)} / {formatBytes(quota)}（{percent}%）
            {percent >= 80 && ' · 接近配额上限，可清理回收站释放空间'}
          </div>
        </div>
      </div>
      <form className="team-create-row" style={{ marginTop: 12 }} onSubmit={(e) => void submit(e)}>
        <label className="field">
          <span>昵称（≤64 字符）</span>
          <input type="text" maxLength={64} value={nickname} onChange={(e) => setNickname(e.target.value)} placeholder="展示名称" />
        </label>
        <label className="field">
          <span>部门（≤128 字符）</span>
          <input type="text" maxLength={128} value={department} onChange={(e) => setDepartment(e.target.value)} placeholder="如：工程部" />
        </label>
        <label className="field">
          <span>职位（≤128 字符）</span>
          <input type="text" maxLength={128} value={position} onChange={(e) => setPosition(e.target.value)} placeholder="如：工程师" />
        </label>
        <label className="field">
          <span>电话（≤32 字符）</span>
          <input type="text" maxLength={32} value={phone} onChange={(e) => setPhone(e.target.value)} placeholder="13800000000" />
        </label>
        <label className="field">
          <span>语言</span>
          <select value={language} onChange={(e) => setLanguage(e.target.value)}>
            {PROFILE_LANGUAGES.map((l) => (
              <option key={l.value} value={l.value}>{l.label}</option>
            ))}
          </select>
        </label>
        <label className="field">
          <span>时区（IANA 名称）</span>
          <input type="text" maxLength={64} required value={timezone} onChange={(e) => setTimezone(e.target.value)} placeholder="Asia/Shanghai" />
        </label>
        <label className="field">
          <span>简介（≤512 字符）</span>
          <textarea rows={3} maxLength={512} value={bio} onChange={(e) => setBio(e.target.value)} placeholder="个人简介" />
        </label>
        <button className="btn primary" type="submit" disabled={busy || timezone.trim() === ''}>
          {busy ? '保存中…' : '保存资料'}
        </button>
      </form>
    </div>
  )
}

/** 会话行的 UA 简述（截断展示，完整值经 title 提示）。 */
function uaSummary(ua: string): string {
  if (!ua) return '未知设备'
  return ua.length > 64 ? `${ua.slice(0, 64)}…` : ua
}

/** 登录会话卡片。 */
function SessionsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
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
      onError(err instanceof Error ? err.message : msg('sessionsLoadFailed'))
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
      <h3>{msg('sessionsTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        当前登录的全部活跃设备/浏览器。撤销全部时会包含当前会话，随后需重新登录。
      </div>
      {loading ? (
        <div className="hint">{msg('loading')}</div>
      ) : sessions.length === 0 ? (
        <div className="empty">{msg('noSessions')}</div>
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
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [tokens, setTokens] = useState<ApiTokenItem[]>([])
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [name, setName] = useState('')
  const [expiry, setExpiry] = useState(30)
  const [busy, setBusy] = useState(false)
  const [revoking, setRevoking] = useState<string | null>(null)
  const [editing, setEditing] = useState<string | null>(null)
  const [editName, setEditName] = useState('')
  const [editScopes, setEditScopes] = useState<string[]>([])
  /** 最近一次创建的一次性明文（仅展示一次，保存在内存）。 */
  const [oneTimeToken, setOneTimeToken] = useState('')
  const [copied, setCopied] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      setTokens(await listTokens())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('patLoadFailed'))
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
      onError(err instanceof Error ? err.message : msg('patCreateFailed'))
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

  const saveEdit = async (id: string) => {
    try { const updated = await updateToken(id, { name: editName, scopes: editScopes }); setTokens((prev) => prev.map((t) => t.id === id ? updated : t)); setEditing(null); onNotice('令牌已更新') }
    catch (err) { onError(err instanceof Error ? err.message : '更新令牌失败') }
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
      <h3>{msg('patTitle')}</h3>
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
        <div className="hint">{msg('loading')}</div>
      ) : tokens.length === 0 ? (
        <div className="empty">{msg('noTokens')}</div>
      ) : (
        tokens.map((t) => (
          <div key={t.id} className="setting-row">
            <div className="setting-main">
              {editing === t.id ? (
                <div className="team-create-row">
                  <input value={editName} onChange={(e) => setEditName(e.target.value)} />
                  {['files:read', 'files:write'].map((scope) => <label key={scope} className="check-item"><input type="checkbox" checked={editScopes.includes(scope)} onChange={(e) => setEditScopes(e.target.checked ? [...editScopes, scope] : editScopes.filter((s) => s !== scope))} /> {scope}</label>)}
                </div>
              ) : <div className="setting-key">{t.name}</div>}
              <div className="setting-meta muted">
                {t.prefix}… · 创建于 {formatTime(t.created_at)}
                {t.last_used_at ? ` · 最近使用 ${formatTime(t.last_used_at)}` : ' · 未使用'}
                {t.expires_at && ` · 有效期至 ${formatTime(t.expires_at)}`}
              </div>
            </div>
            <div className="setting-control">
              {editing === t.id ? <><button className="btn small primary" onClick={() => void saveEdit(t.id)}>保存</button><button className="btn small" onClick={() => setEditing(null)}>取消</button></> : <button className="btn small" onClick={() => { setEditing(t.id); setEditName(t.name); setEditScopes(t.scopes ?? []) }}>修改</button>}
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

/** 复制文本到剪贴板（不可用时返回 false，由调用方保留展示）。 */
async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

/**
 * 两步验证（TOTP）卡片：
 * - 未启用：开始设置 → 展示 secret/otpauth URL（文本+复制，不渲染二维码——
 *   设计限制，认证器手动录入/导入）→ 输入 6 位码确认 → 一次性恢复码列表+复制全部；
 * - 已启用：状态（启用时间）+ 禁用（密码确认对话框）。
 */
function TotpPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [status, setStatus] = useState<TotpStatus | null>(null)
  const [loading, setLoading] = useState(true)
  /** setup 阶段数据（secret/otpauth_url）；null 表示不在 setup 流程中。 */
  const [setup, setSetup] = useState<TotpSetup | null>(null)
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  /** confirm 成功后的一次性恢复码（仅展示一次，内存态）。 */
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([])
  const [copiedSecret, setCopiedSecret] = useState(false)
  const [copiedUrl, setCopiedUrl] = useState(false)
  const [copiedAll, setCopiedAll] = useState(false)
  /** 禁用对话框（密码确认）。 */
  const [showDisable, setShowDisable] = useState(false)
  const [disablePassword, setDisablePassword] = useState('')

  const load = async () => {
    setLoading(true)
    try {
      setStatus(await getTotpStatus())
      onError('')
    } catch (err) {
      // 旧后端（无 TOTP 端点）404：视为未启用而非报错。
      if (err instanceof ApiError && err.status === 404) setStatus({ enabled: false, confirmed_at: null })
      else onError(err instanceof Error ? err.message : msg('totpLoadFailed'))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const startSetup = async () => {
    if (busy) return
    setBusy(true)
    onError('')
    try {
      setSetup(await beginTotpSetup())
      setCode('')
      setCopiedSecret(false)
      setCopiedUrl(false)
    } catch (err) {
      onError(err instanceof Error ? err.message : '开始设置失败')
    } finally {
      setBusy(false)
    }
  }

  const submitConfirm = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !/^\d{6}$/.test(code.trim())) return
    setBusy(true)
    onError('')
    try {
      const codes = await confirmTotpSetup(code.trim())
      setRecoveryCodes(codes)
      setCopiedAll(false)
      setSetup(null)
      setCode('')
      onNotice('两步验证已启用，请立即保存一次性恢复码')
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '确认失败')
    } finally {
      setBusy(false)
    }
  }

  const submitDisable = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || disablePassword === '') return
    setBusy(true)
    onError('')
    try {
      await disableTotp(disablePassword)
      setShowDisable(false)
      setDisablePassword('')
      setRecoveryCodes([])
      onNotice('已禁用两步验证')
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        setShowDisable(false)
        await load()
      } else {
        onError(err instanceof Error ? err.message : '禁用失败')
      }
    } finally {
      setBusy(false)
    }
  }

  const enabled = status?.enabled ?? false

  return (
    <div className="panel setting-group">
      <h3>{msg('totpTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        登录时在密码之外要求认证器（TOTP）6 位验证码；丢失认证器可用一次性恢复码登录。
      </div>
      {loading ? (
        <div className="hint">{msg('loading')}</div>
      ) : (
        <div className="setting-row">
          <div className="setting-main">
            <div className="setting-key">
              状态
              <span className={`badge ${enabled ? 'available' : 'failed'}`} style={{ marginLeft: 8 }}>
                {enabled ? '已启用' : '未启用'}
              </span>
            </div>
            <div className="setting-desc muted">
              {enabled && status?.confirmed_at
                ? `启用于 ${formatTime(status.confirmed_at)}；登录须输入认证器 6 位码或恢复码。`
                : '启用后登录需第二因子（认证器 6 位码或一次性恢复码）。'}
            </div>
          </div>
          <div className="setting-control">
            {!enabled && !setup && (
              <button className="btn primary" disabled={busy} onClick={() => void startSetup()}>
                {busy ? '生成中…' : '开始设置'}
              </button>
            )}
            {enabled && !showDisable && (
              <button className="btn danger" disabled={busy} onClick={() => setShowDisable(true)}>
                禁用
              </button>
            )}
          </div>
        </div>
      )}
      {setup && (
        <div style={{ marginTop: 12 }}>
          <div className="setting-desc muted" style={{ marginBottom: 8 }}>
            在认证器（如 Google Authenticator）中手动录入以下密钥或导入 otpauth 链接（本页不渲染二维码），
            然后输入认证器显示的 6 位码完成启用。
          </div>
          <div className="share-link" style={{ marginBottom: 8 }}>
            <input type="text" readOnly value={setup.secret} onFocus={(e) => e.currentTarget.select()} />
            <button
              className="btn small"
              type="button"
              onClick={() => {
                void copyText(setup.secret).then(setCopiedSecret)
              }}
            >
              {copiedSecret ? '已复制' : '复制密钥'}
            </button>
          </div>
          <div className="share-link" style={{ marginBottom: 12 }}>
            <input type="text" readOnly value={setup.otpauth_url} onFocus={(e) => e.currentTarget.select()} />
            <button
              className="btn small"
              type="button"
              onClick={() => {
                void copyText(setup.otpauth_url).then(setCopiedUrl)
              }}
            >
              {copiedUrl ? '已复制' : '复制链接'}
            </button>
          </div>
          <form className="team-create-row" onSubmit={(e) => void submitConfirm(e)}>
            <label className="field">
              <span>认证器 6 位验证码</span>
              <input
                type="text"
                inputMode="numeric"
                required
                pattern="\d{6}"
                maxLength={6}
                value={code}
                onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
                placeholder="123456"
                autoComplete="one-time-code"
              />
            </label>
            <button className="btn primary" type="submit" disabled={busy || !/^\d{6}$/.test(code)}>
              {busy ? '验证中…' : '确认启用'}
            </button>
            <button
              className="btn"
              type="button"
              disabled={busy}
              onClick={() => {
                setSetup(null)
                setCode('')
              }}
            >
              取消
            </button>
          </form>
        </div>
      )}
      {recoveryCodes.length > 0 && (
        <div style={{ marginTop: 12 }}>
          <div className="setting-desc muted" style={{ marginBottom: 8 }}>
            以下 10 个恢复码仅显示这一次（服务端只存哈希）：每个可用一次，用于丢失认证器时登录。
            请立即复制保存到安全位置。
          </div>
          <div className="recovery-codes">
            {recoveryCodes.map((c) => (
              <code key={c}>{c}</code>
            ))}
          </div>
          <div style={{ marginTop: 8 }}>
            <button
              className="btn small"
              type="button"
              onClick={() => {
                void copyText(recoveryCodes.join('\n')).then(setCopiedAll)
              }}
            >
              {copiedAll ? '已复制全部' : '复制全部'}
            </button>
          </div>
        </div>
      )}
      {showDisable && (
        <form className="team-create-row" style={{ marginTop: 12 }} onSubmit={(e) => void submitDisable(e)}>
          <label className="field">
            <span>确认密码</span>
            <input
              type="password"
              required
              value={disablePassword}
              onChange={(e) => setDisablePassword(e.target.value)}
              placeholder="输入登录密码以确认禁用"
              autoComplete="current-password"
            />
          </label>
          <button className="btn danger" type="submit" disabled={busy || disablePassword === ''}>
            {busy ? '禁用中…' : '确认禁用'}
          </button>
          <button
            className="btn"
            type="button"
            disabled={busy}
            onClick={() => {
              setShowDisable(false)
              setDisablePassword('')
            }}
          >
            取消
          </button>
        </form>
      )}
    </div>
  )
}

/** 明暗模式选项（seg 按钮）。 */
const THEME_MODES: Array<{ value: ThemeMode; label: string }> = [
  { value: 'dark', label: '深色' },
  { value: 'light', label: '浅色' },
  { value: 'system', label: '跟随系统' },
]

/** 外观卡片（v1.1）：accent 色五选一 + 明暗模式（含跟随系统），本地即时生效。 */
function AppearancePanel() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [accent, setAccent] = useState<ThemeAccent>(() => loadTheme().accent)
  const [mode, setMode] = useState<ThemeMode>(() => loadTheme().mode)

  const update = (next: Partial<ThemePreference>) => {
    const merged: ThemePreference = { accent, mode, ...next }
    setAccent(merged.accent)
    setMode(merged.mode)
    saveTheme(merged)
  }

  return (
    <div className="panel setting-group">
      <h3>{msg('appearance')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        主题色影响按钮、链接与强调元素；明暗模式即时生效，偏好仅保存在本浏览器。
      </div>
      <div className="setting-main">
        <div className="setting-key">主题色</div>
        <div className="setting-desc muted" style={{ marginBottom: 20 }}>五选一，默认靛蓝。</div>
        <div className="theme-swatches">
          {THEME_ACCENTS.map((t) => (
            <button
              key={t.value}
              type="button"
              className={`theme-swatch${accent === t.value ? ' active' : ''}`}
              style={{ background: t.color }}
              title={t.label}
              aria-label={`主题色：${t.label}`}
              aria-pressed={accent === t.value}
              onClick={() => update({ accent: t.value })}
            >
              <span className="theme-swatch-name">{t.label}</span>
            </button>
          ))}
        </div>
      </div>
      <div className="setting-main" style={{ marginTop: 16 }}>
        <div className="setting-key">明暗模式</div>
        <div className="setting-desc muted" style={{ marginBottom: 8 }}>跟随系统时按操作系统偏好实时切换。</div>
        <div className="seg-group" style={{ maxWidth: 360 }}>
          {THEME_MODES.map((m) => (
            <button
              key={m.value}
              type="button"
              className={`seg${mode === m.value ? ' active' : ''}`}
              onClick={() => update({ mode: m.value })}
            >
              {m.label}
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}

/** Webhook URL 的截断展示（完整值经 title 提示）。 */
function urlSummary(url: string): string {
  return url.length > 56 ? `${url.slice(0, 56)}…` : url
}

/** 最近投递状态摘要（HTTP 状态码 + 时间；0=传输层失败，null=从未投递）。 */
function deliverySummary(w: WebhookItem): string {
  if (w.last_delivered_at && w.last_status !== null) {
    const code = w.last_status === 0 ? '传输失败' : `HTTP ${w.last_status}`
    return `最近投递 ${code} · ${formatTime(w.last_delivered_at)}`
  }
  return '尚未投递'
}

/** Webhook 卡片（v1.1）：注册回调 URL 接收通知事件推送（HMAC 签名）。 */
function WebhooksPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [hooks, setHooks] = useState<WebhookItem[]>([])
  const [loading, setLoading] = useState(true)
  const [showCreate, setShowCreate] = useState(false)
  const [url, setUrl] = useState('')
  const [events, setEvents] = useState<Record<string, boolean>>({})
  const [busy, setBusy] = useState(false)
  const [toggling, setToggling] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<string | null>(null)
  /** 最近一次创建的一次性 secret（仅展示一次，保存在内存）。 */
  const [oneTimeSecret, setOneTimeSecret] = useState('')
  const [secretCopied, setSecretCopied] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      setHooks(await listWebhooks())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('webhookLoadFailed'))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const selectedEvents = (): NotificationEventType[] =>
    NOTIFICATION_TYPE_META.filter((m) => events[m.type]).map((m) => m.type)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    onError('')
    try {
      const created = await createWebhook(url.trim(), selectedEvents())
      setUrl('')
      setEvents({})
      setShowCreate(false)
      setOneTimeSecret(created.secret)
      setSecretCopied(false)
      onNotice('Webhook 已创建，请立即复制一次性 secret（接收方验签用）')
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('webhookCreateFailed'))
    } finally {
      setBusy(false)
    }
  }

  const copySecret = async () => {
    try {
      await navigator.clipboard.writeText(oneTimeSecret)
      setSecretCopied(true)
    } catch {
      // 剪贴板不可用（如非安全上下文）：保留输入框展示，由用户手动复制。
      setSecretCopied(false)
    }
  }

  const toggle = async (id: string, enabled: boolean) => {
    if (toggling !== null) return
    setToggling(id)
    try {
      const updated = await updateWebhook(id, enabled)
      setHooks((prev) => prev.map((w) => (w.id === updated.id ? updated : w)))
      onNotice(`已${updated.enabled ? '启用' : '停用'}该 Webhook`)
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '更新失败')
    } finally {
      setToggling(null)
    }
  }

  const remove = async (id: string) => {
    if (deleting !== null) return
    setDeleting(id)
    try {
      await deleteWebhook(id)
      onNotice('已删除 Webhook')
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '删除失败')
    } finally {
      setDeleting(null)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>{msg('webhookTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        事件发生时向回调 URL POST JSON（X-DocFlow-Signature 头含 HMAC-SHA256 签名，以创建时下发的一次性
        secret 验签）；受「通知偏好」同一开关控制。连续 10 次投递失败将自动停用。
      </div>
      {oneTimeSecret && (
        <>
          <div className="share-link" style={{ marginBottom: 8 }}>
            <input type="text" readOnly value={oneTimeSecret} onFocus={(e) => e.currentTarget.select()} />
            <button className="btn small" type="button" onClick={() => void copySecret()}>
              {secretCopied ? '已复制' : '复制 secret'}
            </button>
          </div>
          <div className="setting-desc muted" style={{ marginBottom: 12 }}>
            该签名 secret 仅显示这一次，请立即复制保存；接收方以其复算 HMAC 验签，关闭后无法再次查看。
          </div>
        </>
      )}
      {showCreate && (
        <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={(e) => void submit(e)}>
          <label className="field">
            <span>回调 URL（http/https）</span>
            <input
              type="url"
              required
              maxLength={2048}
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com/webhook"
            />
          </label>
          <label className="field">
            <span>订阅事件（至少一个）</span>
            <span className="check-list" style={{ display: 'inline-flex', marginBottom: 0 }}>
              {NOTIFICATION_TYPE_META.map((meta) => (
                <label key={meta.type} className="check-item">
                  <input
                    type="checkbox"
                    checked={!!events[meta.type]}
                    onChange={(e) => setEvents((prev) => ({ ...prev, [meta.type]: e.target.checked }))}
                  />
                  {meta.label}
                </label>
              ))}
            </span>
          </label>
          <button
            className="btn primary"
            type="submit"
            disabled={busy || url.trim() === '' || selectedEvents().length === 0}
          >
            {busy ? '创建中…' : '创建'}
          </button>
          <button
            className="btn"
            type="button"
            disabled={busy}
            onClick={() => {
              setShowCreate(false)
              setUrl('')
              setEvents({})
            }}
          >
            取消
          </button>
        </form>
      )}
      {loading ? (
        <div className="hint">{msg('loading')}</div>
      ) : hooks.length === 0 ? (
        <div className="empty">{msg('noWebhooks')}</div>
      ) : (
        hooks.map((w) => (
          <div key={w.id} className="setting-row">
            <div className="setting-main">
              <div className="setting-key" title={w.url}>
                {urlSummary(w.url)}
              </div>
              <div className="event-badges">
                {w.events.map((e) => (
                  <span key={e} className="badge">
                    {NOTIFICATION_TYPE_META.find((m) => m.type === e)?.label ?? e}
                  </span>
                ))}
              </div>
              <div className="setting-desc muted">
                {deliverySummary(w)} · 连续失败 {w.failure_count} 次 · 创建于 {formatTime(w.created_at)}
              </div>
            </div>
            <div className="setting-control">
              <label className="setting-bool">
                <input
                  type="checkbox"
                  checked={w.enabled}
                  disabled={toggling !== null}
                  onChange={(e) => void toggle(w.id, e.target.checked)}
                />
                {toggling === w.id ? '处理中…' : w.enabled ? '启用' : '停用'}
              </label>
              <button
                className="btn small danger"
                disabled={deleting !== null}
                onClick={() => void remove(w.id)}
              >
                {deleting === w.id ? '删除中…' : '删除'}
              </button>
            </div>
          </div>
        ))
      )}
      {!showCreate && (
        <div style={{ marginTop: 12 }}>
          <button
            className="btn primary"
            onClick={() => {
              setOneTimeSecret('')
              setShowCreate(true)
            }}
          >
            注册 Webhook
          </button>
        </div>
      )}
    </div>
  )
}

/** 通知偏好卡片：各事件类型开关（无记录 = 默认开启；关闭后不再生成该类通知）。 */
function NotificationsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
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
      onError(err instanceof Error ? err.message : msg('notifPrefsLoadFailed'))
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
      onError(err instanceof Error ? err.message : msg('notifPrefsUpdateFailed'))
    } finally {
      setSaving(null)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>{msg('notifPrefsTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        站内通知的事件类型开关；关闭后对应事件不再生成通知（默认全部开启）。
      </div>
      {loading ? (
        <div className="hint">{msg('loading')}</div>
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
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('settings')}</h2>
      </div>
      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      <ProfilePanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />
      <AppearancePanel />
      <NotificationsPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />
      <TotpPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />
      <WebhooksPanel
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
