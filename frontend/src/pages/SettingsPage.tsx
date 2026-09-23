// 账户设置页（/settings，v2.3 重分配）：
// - 外观（主题色/明暗/界面语言）、打开方式、账号安全（TOTP 两步验证 +
//   登录会话）、通知偏好、开发者（MCP/Webhook/PAT）；
// - admin 附加：邮件配置（SMTP）、TLS（HTTPS 运行时切换）、系统设置
//   （system_settings 全量键：中文名 + key 直显 + 按类型渲染，行内编辑）；
// - 原「资料」tab 已删除：资料编辑并入右上角「个人信息」弹窗（见 App.tsx）。
// - 个人访问令牌（PAT）：创建对话框（名称/有效期）→ 一次性明文展示+复制，
//   列表含最近使用时间与撤销。
// - 两步验证（v2 TOTP）：setup（二维码 + secret 文本+复制）→ 确认
//   → 一次性恢复码列表+复制全部；已启用可凭密码禁用。
// - Webhook（v1.1）：注册回调 URL（事件多选）→ 一次性 secret 展示+复制，
//   列表含事件徽标/投递状态/失败计数/启停开关与删除。
// 会话列表不标记「当前会话」（实现取舍：当前会话由 refresh cookie 识别，
// 凭据哈希不出服务端）。
import { FormEvent, useEffect, useMemo, useState } from 'react'
import { NavLink, Navigate, useNavigate, useParams } from 'react-router-dom'
import { Alert, Button, Input, InputNumber, Modal as AntdModal, QRCode, Radio, Segmented, Select, Switch, Upload } from 'antd'
import {
  ApiError,
  ApiTokenItem,
  AdminSettingsResult,
  MeData,
  NotificationEventType,
  OpenWithPrefs,
  SessionItem,
  SettingItem,
  SettingType,
  SettingValue,
  SmtpSettingsView,
  TotpSetup,
  TotpStatus,
  TlsCert,
  TlsMode,
  TlsStatus,
  WebhookItem,
  adminGetSettings,
  adminGetSmtpSettings,
  adminGetTls,
  adminPutSetting,
  adminPutSmtpSettings,
  adminPutTls,
  adminTestSmtp,
  adminUploadTlsCert,
  beginTotpSetup,
  confirmEmailChange,
  confirmTotpSetup,
  createToken,
  createWebhook,
  deleteOpenWith,
  deleteWebhook,
  disableTotp,
  getMe,
  getTotpStatus,
  isAdmin,
  listNotificationPreferences,
  listOpenWith,
  listSessions,
  listTokens,
  listWebhooks,
  logout,
  requestEmailChange,
  revokeAllSessions,
  revokeSession,
  revokeToken,
  setOpenWith,
  updateMe,
  updateToken,
  updateNotificationPreference,
  updateWebhook,
  listWebDAVTokens,
  createWebDAVToken,
  revokeWebDAVToken,
  WebDAVToken,
  getAgentSettings,
  putAgentSettings,
} from '../api'
import {
  ALL_EDIT_METHODS,
  ALL_VIEW_METHODS,
  BUILTIN_OPENWITH_EXTS,
  EditMethod,
  ViewMethod,
  builtinOpenWith,
  editMethodLabel,
  viewMethodLabel,
} from '../openers'
import { formatQuota, formatTime, Modal } from '../components/FileBrowser'
import AIPersonalPanel from '../components/settings/AIPersonalPanel'
import { useAIEnabled } from '../aiFeature'
import { THEME_ACCENTS, ThemeAccent, ThemeMode, ThemePreference, THEME_EVENT, loadTheme, saveTheme } from '../theme'
import { MessageKey, saveLocale, t, useLocale } from '../i18n'

/** 通知事件类型的中文标签与说明（顺序即设置页展示顺序）。 */
const NOTIFICATION_TYPE_META: Array<{ type: NotificationEventType; label: string; desc: string }> = [
  { type: 'upload.completed', label: '上传完成', desc: '我的上传完成（校验与安全扫描通过）' },
  { type: 'upload.quarantined', label: '上传隔离提醒', desc: '我的上传未通过安全扫描被隔离' },
  { type: 'share.accessed', label: '分享被下载', desc: '我的公开/私有分享文件被下载' },
  { type: 'file.updated', label: '空间文件更新', desc: '空间文件被其他成员更新新版本' },
  { type: 'file.version.deleted', label: '文件版本被删除', desc: '我的空间文件历史版本被其他成员删除（当前版本不受影响）' },
  { type: 'quota.warning', label: '配额用量警告', desc: '存储用量超过配额的 80%（上传成功后触发）' },
]

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
              <Button
                size="small"
                danger
                disabled={revoking !== null || revokingAll}
                onClick={() => void revokeOne(s.id)}
              >
                {revoking === s.id ? '撤销中…' : '撤销'}
              </Button>
            </div>
          </div>
        ))
      )}
      <div style={{ marginTop: 12 }}>
        <Button danger disabled={revokingAll || loading || sessions.length === 0} onClick={() => void revokeAll()}>
          {revokingAll ? '撤销中…' : '撤销全部会话并登出'}
        </Button>
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
  // AI 未启用时 ai:chat scope 选项隐藏（与全站 AI 入口显隐一致）。
  const aiOn = useAIEnabled()
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
            <Input readOnly value={oneTimeToken} onFocus={(e) => e.currentTarget.select()} />
            <Button size="small" onClick={() => void copyToken()}>
              {copied ? '已复制' : '复制令牌'}
            </Button>
          </div>
          <div className="setting-desc muted" style={{ marginBottom: 12 }}>
            该令牌明文仅显示这一次，请立即复制保存；关闭后无法再次查看。
          </div>
        </>
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
                  <Input allowClear value={editName} onChange={(e) => setEditName(e.target.value)} />
                  {(aiOn ? ['files:read', 'files:write', 'ai:chat'] : ['files:read', 'files:write']).map((scope) => (
                    <label key={scope} className="check-item" title={scope}>
                      <input type="checkbox" checked={editScopes.includes(scope)} onChange={(e) => setEditScopes(e.target.checked ? [...editScopes, scope] : editScopes.filter((s) => s !== scope))} />
                      {scope === 'files:read' ? '文件只读（files:read）' : scope === 'files:write' ? '文件读写（files:write）' : 'AI 对话（ai:chat）'}
                    </label>
                  ))}
                </div>
              ) : <div className="setting-key">{t.name}</div>}
              <div className="setting-meta muted">
                {t.prefix}… · 创建于 {formatTime(t.created_at)}
                {t.last_used_at ? ` · 最近使用 ${formatTime(t.last_used_at)}` : ' · 未使用'}
                {t.expires_at && ` · 有效期至 ${formatTime(t.expires_at)}`}
              </div>
            </div>
            <div className="setting-control">
              {editing === t.id ? <><Button size="small" type="primary" onClick={() => void saveEdit(t.id)}>保存</Button><Button size="small" onClick={() => setEditing(null)}>取消</Button></> : <Button size="small" onClick={() => { setEditing(t.id); setEditName(t.name); setEditScopes(t.scopes ?? []) }}>修改</Button>}
              {t.expires_at && new Date(t.expires_at).getTime() < Date.now() ? (
                <span className="badge failed">已过期</span>
              ) : (
                <span className="badge available">有效</span>
              )}
              <Button
                size="small"
                danger
                disabled={revoking !== null}
                onClick={() => void revoke(t.id)}
              >
                {revoking === t.id ? '撤销中…' : '撤销'}
              </Button>
            </div>
          </div>
        ))
      )}
      <div style={{ marginTop: 12 }}>
        <Button type="primary" onClick={() => { setOneTimeToken(''); setShowCreate(true) }}>
          {msg('createTokenBtn')}
        </Button>
      </div>

      {/* 创建令牌弹窗（v2.2：平铺表单弹窗化，列表上只留「创建令牌」按钮）。 */}
      {showCreate && (
        <Modal title="创建个人访问令牌" onClose={() => { if (!busy) { setShowCreate(false); setName('') } }}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void submit(e)}>
            <label className="field">
              <span>名称（≤100 字符，如：备份脚本）</span>
              <Input
                autoFocus
                required
                maxLength={100}
                allowClear
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="如：备份脚本"
              />
            </label>
            <label className="field">
              <span>有效期</span>
              <Select
                value={expiry}
                onChange={(v) => setExpiry(v)}
                options={EXPIRY_OPTIONS.map((opt) => ({ value: opt.value, label: opt.label }))}
              />
            </label>
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button type="primary" htmlType="submit" disabled={busy || name.trim() === ''}>
                {busy ? '创建中…' : '创建'}
              </Button>
              <Button disabled={busy} onClick={() => { setShowCreate(false); setName('') }}>
                取消
              </Button>
            </div>
          </form>
        </Modal>
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
 * - 未启用：开始设置 → 展示二维码（otpauth URL，antd QRCode 白底黑码）+
 *   手动密钥/otpauth URL 文本+复制 → 输入 6 位码确认 → 一次性恢复码列表
 *   +复制全部；
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
              <Button type="primary" disabled={busy} onClick={() => void startSetup()}>
                {busy ? '生成中…' : '开始设置'}
              </Button>
            )}
            {enabled && !showDisable && (
              <Button danger disabled={busy} onClick={() => setShowDisable(true)}>
                禁用
              </Button>
            )}
          </div>
        </div>
      )}
      {setup && (
        <div style={{ marginTop: 12 }} className="totp-setup">
          <div className="setting-desc muted" style={{ marginBottom: 8 }}>
            用认证器（Google Authenticator / Microsoft Authenticator / 1Password 等）扫描下方二维码，
            或手动录入密钥 / 导入 otpauth 链接，然后输入认证器显示的 6 位码完成启用。
          </div>
          <div className="totp-qr-wrap">
            <QRCode
              value={setup.otpauth_url}
              size={176}
              errorLevel="M"
              bgColor="#ffffff"
              color="#000000"
              aria-label="TOTP 绑定二维码（otpauth 链接）"
            />
          </div>
          <div className="share-link" style={{ marginBottom: 8, marginTop: 12 }}>
            <Input readOnly value={setup.secret} onFocus={(e) => e.currentTarget.select()} aria-label="TOTP 手动密钥" />
            <Button
              size="small"
              onClick={() => {
                void copyText(setup.secret).then(setCopiedSecret)
              }}
            >
              {copiedSecret ? '已复制' : '复制密钥'}
            </Button>
          </div>
          <div className="setting-desc muted" style={{ marginBottom: 8 }}>
            手动密钥（Base32）：认证器「无法扫描」时选择「输入设置密钥」，账号名即你的邮箱。
          </div>
          <div className="share-link" style={{ marginBottom: 12 }}>
            <Input readOnly value={setup.otpauth_url} onFocus={(e) => e.currentTarget.select()} />
            <Button
              size="small"
              onClick={() => {
                void copyText(setup.otpauth_url).then(setCopiedUrl)
              }}
            >
              {copiedUrl ? '已复制' : '复制链接'}
            </Button>
          </div>
          <form className="team-create-row" onSubmit={(e) => void submitConfirm(e)}>
            <label className="field">
              <span>认证器 6 位验证码</span>
              <Input
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
            <Button type="primary" htmlType="submit" disabled={busy || !/^\d{6}$/.test(code)}>
              {busy ? '验证中…' : '确认启用'}
            </Button>
            <Button
              disabled={busy}
              onClick={() => {
                setSetup(null)
                setCode('')
              }}
            >
              取消
            </Button>
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
            <Button
              size="small"
              onClick={() => {
                void copyText(recoveryCodes.join('\n')).then(setCopiedAll)
              }}
            >
              {copiedAll ? '已复制全部' : '复制全部'}
            </Button>
          </div>
        </div>
      )}
      {showDisable && (
        <form className="team-create-row" style={{ marginTop: 12 }} onSubmit={(e) => void submitDisable(e)}>
          <label className="field">
            <span>确认密码</span>
            <Input.Password
              required
              value={disablePassword}
              onChange={(e) => setDisablePassword(e.target.value)}
              placeholder="输入登录密码以确认禁用"
              autoComplete="current-password"
            />
          </label>
          <Button danger htmlType="submit" disabled={busy || disablePassword === ''}>
            {busy ? '禁用中…' : '确认禁用'}
          </Button>
          <Button
            disabled={busy}
            onClick={() => {
              setShowDisable(false)
              setDisablePassword('')
            }}
          >
            取消
          </Button>
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

/**
 * 换绑邮箱面板（账号安全，v2.4 整改项 14）：展示当前绑定邮箱 +
 * 「换绑邮箱」两段式弹窗——① 验证当前密码并填新邮箱（请求投递 6 位验证码
 * 到新邮箱，10 分钟有效）；② 提交验证码确认换绑。成功后刷新 /me。
 */
function EmailChangePanel({ onNotice }: { onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const [me, setMe] = useState<MeData | null>(null)
  const [open, setOpen] = useState(false)
  // 弹窗两段状态。
  const [step, setStep] = useState<'form' | 'code'>('form')
  const [password, setPassword] = useState('')
  const [newEmail, setNewEmail] = useState('')
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    void getMe().then((data) => { if (alive) setMe(data) }).catch(() => { if (alive) setMe(null) })
    return () => { alive = false }
  }, [])

  const openDialog = () => {
    setStep('form')
    setPassword('')
    setNewEmail('')
    setCode('')
    setError('')
    setOpen(true)
  }

  /** 第一段：验证密码 → 请求验证码（投递到新邮箱）。 */
  const requestCode = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    const email = newEmail.trim()
    if (!password || !email) return
    if (email === (me?.email ?? '')) {
      setError(zh ? '新邮箱须与当前邮箱不同' : 'New email must differ from the current one')
      return
    }
    setBusy(true)
    setError('')
    try {
      await requestEmailChange(password, email)
      setStep('code')
    } catch (err) {
      setError(err instanceof Error ? err.message : zh ? '验证码发送失败' : 'Failed to send code')
    } finally {
      setBusy(false)
    }
  }

  /** 第二段：提交验证码完成换绑。 */
  const confirmChange = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || code.trim() === '') return
    setBusy(true)
    setError('')
    try {
      const r = await confirmEmailChange(code.trim())
      setOpen(false)
      setMe((prev) => (prev ? { ...prev, email: r.email } : prev))
      onNotice(zh ? `绑定邮箱已更换为 ${r.email}` : `Email changed to ${r.email}`)
      window.dispatchEvent(new Event('docflow:me'))
    } catch (err) {
      setError(err instanceof Error ? err.message : zh ? '换绑失败' : 'Failed to change email')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>{zh ? '绑定邮箱' : 'Email'}</h3>
      <div className="setting-row" style={{ borderBottom: 'none', paddingBottom: 0 }}>
        <div className="setting-main">
          <span className="setting-key" style={{ wordBreak: 'break-all' }}>{me?.email ?? '…'}</span>
          <div className="setting-desc">{zh ? '登录标识与通知投递地址；更换需验证当前密码并经新邮箱验证码确认' : 'Login identifier and notification address'}</div>
        </div>
        <div className="setting-control">
          <Button size="small" onClick={openDialog}>{zh ? '换绑邮箱' : 'Change email'}</Button>
        </div>
      </div>

      <AntdModal
        open={open}
        centered
        footer={null}
        width="min(480px, 92vw)"
        title={zh ? '换绑邮箱' : 'Change email'}
        onCancel={() => { if (!busy) setOpen(false) }}
      >
        {step === 'form' ? (
          <form onSubmit={(e) => void requestCode(e)}>
            <p className="hint" style={{ marginTop: 0 }}>
              {zh ? '第一步：验证当前密码并填写新邮箱，验证码将投递到新邮箱（10 分钟内有效）。' : 'Step 1: verify your password and enter the new email.'}
            </p>
            <label className="field">
              <span>{zh ? '当前密码' : 'Current password'}</span>
              <Input.Password
                autoFocus
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
              />
            </label>
            <label className="field">
              <span>{zh ? '新邮箱' : 'New email'}</span>
              <Input
                required
                type="email"
                allowClear
                value={newEmail}
                onChange={(e) => setNewEmail(e.target.value)}
                placeholder="you@example.com"
              />
            </label>
            {error && <div className="error-text">{error}</div>}
            <div className="modal-actions">
              <Button disabled={busy} onClick={() => setOpen(false)}>{zh ? '取消' : 'Cancel'}</Button>
              <Button type="primary" htmlType="submit" disabled={busy || !password || !newEmail.trim()}>
                {busy ? (zh ? '发送中…' : 'Sending…') : (zh ? '发送验证码' : 'Send code')}
              </Button>
            </div>
          </form>
        ) : (
          <form onSubmit={(e) => void confirmChange(e)}>
            <p className="hint" style={{ marginTop: 0 }}>
              {zh ? `验证码已发送到 ${newEmail.trim()}，请查收邮件并输入 6 位验证码完成换绑。` : `Code sent to ${newEmail.trim()}.`}
            </p>
            <label className="field">
              <span>{zh ? '邮件验证码' : 'Verification code'}</span>
              <Input
                autoFocus
                allowClear
                maxLength={6}
                value={code}
                onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
                placeholder="000000"
                style={{ letterSpacing: 2, fontKerning: 'none' }}
              />
            </label>
            {error && <div className="error-text">{error}</div>}
            <div className="modal-actions">
              <Button disabled={busy} onClick={() => { setStep('form'); setError('') }}>{zh ? '上一步' : 'Back'}</Button>
              <Button type="primary" htmlType="submit" disabled={busy || code.trim() === ''}>
                {busy ? (zh ? '确认中…' : 'Confirming…') : (zh ? '确认换绑' : 'Confirm')}
              </Button>
            </div>
          </form>
        )}
      </AntdModal>
    </div>
  )
}

/** 外观卡片（v1.1）：accent 色五选一 + 明暗模式（含跟随系统）+ 界面语言，
 * 本地即时生效。与顶栏「外观快捷入口」共享 theme.ts 存储（saveTheme 经
 * THEME_EVENT 广播，两处状态实时同步）。 */
function AppearancePanel() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [accent, setAccent] = useState<ThemeAccent>(() => loadTheme().accent)
  const [mode, setMode] = useState<ThemeMode>(() => loadTheme().mode)

  // 顶栏外观快捷入口改动主题时同步本面板（共享 docflow.theme 存储）。
  useEffect(() => {
    const onTheme = () => {
      const cur = loadTheme()
      setAccent(cur.accent)
      setMode(cur.mode)
    }
    window.addEventListener(THEME_EVENT, onTheme)
    return () => window.removeEventListener(THEME_EVENT, onTheme)
  }, [])

  // v2.5 防漂移：update 一律基于 localStorage 现值合并（而非本地 state）——
  // THEME_EVENT 监听的 setState 异步生效，快速跨入口连续操作时本地 state
  // 可能仍是旧 accent，用它合并会把用户已选主题覆盖回旧值（主题自动漂移
  // 的竞态根因）；本地 state 仅作 UI 展示（事件同步）。
  const update = (next: Partial<ThemePreference>) => {
    saveTheme({ ...loadTheme(), ...next })
  }

  return (
    <div className="panel setting-group">
      <h3>{msg('appearance')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        主题色影响按钮、链接与强调元素；明暗模式即时生效，偏好仅保存在本浏览器。
        顶栏右侧的外观快捷入口与本面板状态实时同步。
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
        <div style={{ maxWidth: 360 }}>
          <Segmented
            value={mode}
            onChange={(v) => update({ mode: v as ThemeMode })}
            options={THEME_MODES.map((m) => ({ value: m.value, label: m.label }))}
          />
        </div>
      </div>
      <div className="setting-main" style={{ marginTop: 16 }}>
        <div className="setting-key">界面语言</div>
        <div className="setting-desc muted" style={{ marginBottom: 8 }}>同时写入个人资料的语言偏好字段。</div>
        <div style={{ maxWidth: 360 }}>
          <Segmented
            value={locale}
            onChange={(v) => {
              const next = v === 'en-US' ? 'en-US' : 'zh-CN'
              saveLocale(next)
              window.dispatchEvent(new Event('docflow:locale'))
              void updateMe({ language: next }).catch(() => { /* 资料字段写入失败不阻塞界面语言切换 */ })
            }}
            options={[
              { value: 'zh-CN', label: '简体中文' },
              { value: 'en-US', label: 'English' },
            ]}
          />
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
            <Input readOnly value={oneTimeSecret} onFocus={(e) => e.currentTarget.select()} />
            <Button size="small" onClick={() => void copySecret()}>
              {secretCopied ? '已复制' : '复制 secret'}
            </Button>
          </div>
          <div className="setting-desc muted" style={{ marginBottom: 12 }}>
            该签名 secret 仅显示这一次，请立即复制保存；接收方以其复算 HMAC 验签，关闭后无法再次查看。
          </div>
        </>
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
              <Button
                size="small"
                danger
                disabled={deleting !== null}
                onClick={() => void remove(w.id)}
              >
                {deleting === w.id ? '删除中…' : '删除'}
              </Button>
            </div>
          </div>
        ))
      )}
      <div style={{ marginTop: 12 }}>
        <Button
          type="primary"
          onClick={() => {
            setOneTimeSecret('')
            setShowCreate(true)
          }}
        >
          注册 Webhook
        </Button>
      </div>

      {/* 注册 Webhook 弹窗（v2.2：平铺表单弹窗化）。 */}
      {showCreate && (
        <Modal
          title="注册 Webhook"
          onClose={() => {
            if (!busy) { setShowCreate(false); setUrl(''); setEvents({}) }
          }}
        >
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void submit(e)}>
            <label className="field">
              <span>回调 URL（http/https）</span>
              <Input
                autoFocus
                type="url"
                required
                maxLength={2048}
                allowClear
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://example.com/webhook"
              />
            </label>
            <label className="field">
              <span>订阅事件（至少一个）</span>
              <span className="check-list" style={{ display: 'inline-flex', marginBottom: 0 }}>
                {NOTIFICATION_TYPE_META.map((meta) => (
                  <label key={meta.type} className="check-item" title={meta.type}>
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
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button
                type="primary"
                htmlType="submit"
                disabled={busy || url.trim() === '' || selectedEvents().length === 0}
              >
                {busy ? '创建中…' : '创建'}
              </Button>
              <Button
                disabled={busy}
                onClick={() => {
                  setShowCreate(false)
                  setUrl('')
                  setEvents({})
                }}
              >
                取消
              </Button>
            </div>
          </form>
        </Modal>
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

/** 打开方式卡片（自文件页迁移）：按扩展名管理默认 {查看, 编辑} 双方式。
 * - 表格预填全部内置默认扩展名（txt/md/html/…/docx/drawio/excalidraw/
 *   xmind/pdf 等，见 openers.BUILTIN_OPENWITH_EXTS）+「其他（默认）」说明行
 *   + 用户自定义扩展名（高亮「已覆盖」）；
 * - v2.2：下拉列出**全枚举**（查看 raw/office/drawio/excalidraw/xmind/
 *   richtext；编辑 text/office/drawio/excalidraw/richtext +「不支持」），
 *   由用户自选（后端按同一白名单落库，不再按扩展名过滤）；内置行修改即
 *   创建覆盖，两字段均与内置一致时自动删除覆盖恢复内置；
 * - 「添加」行在面板顶部：输入扩展名 + 两个下拉（随输入给出内置默认）。 */
function OpenWithPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [prefs, setPrefs] = useState<OpenWithPrefs>({})
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  // 「添加」行草稿：扩展名 + 查看/编辑方式（空串 = 跟随内置默认）。
  const [newExt, setNewExt] = useState('')
  const [newView, setNewView] = useState<ViewMethod | ''>('')
  const [newEdit, setNewEdit] = useState<EditMethod | ''>('')

  const load = async () => {
    setLoading(true)
    try {
      setPrefs(await listOpenWith())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : '打开方式偏好加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 行内下拉修改：合并既有覆盖后整体 upsert；两字段均与内置一致时改回
   *「删除覆盖」恢复内置（内置值即显式保存的兜底语义）。 */
  const saveField = async (ext: string, field: 'view' | 'edit', value: ViewMethod | EditMethod) => {
    if (busy) return
    const builtin = builtinOpenWith(ext)
    setBusy(true)
    onError('')
    try {
      const merged: { view?: ViewMethod; edit?: EditMethod } = { ...prefs[ext], [field]: value }
      const viewEq = (merged.view ?? builtin.view) === builtin.view
      const editEq = (merged.edit ?? builtin.edit) === builtin.edit
      if (viewEq && editEq) {
        // 与内置默认一致：删除覆盖行（若存在）恢复内置。
        if (prefs[ext]) {
          await deleteOpenWith(ext)
          setPrefs((prev) => {
            const next = { ...prev }
            delete next[ext]
            return next
          })
          onNotice(`已恢复 .${ext} 的内置默认打开方式`)
        }
      } else {
        await setOpenWith(ext, merged)
        setPrefs((prev) => ({ ...prev, [ext]: merged }))
      }
    } catch (err) {
      onError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  /** 重置：删除该扩展偏好，恢复内置默认。 */
  const resetExt = async (ext: string) => {
    if (busy) return
    setBusy(true)
    onError('')
    try {
      await deleteOpenWith(ext)
      setPrefs((prev) => {
        const next = { ...prev }
        delete next[ext]
        return next
      })
      onNotice(`已恢复 .${ext} 的内置默认打开方式`)
    } catch (err) {
      onError(err instanceof Error ? err.message : '重置失败')
    } finally {
      setBusy(false)
    }
  }

  /** 添加：扩展名规范化（去点小写，1..16 位 [a-z0-9]）+ 校验后保存；
   * 方式取下拉所选，未显式选择时回退该扩展的内置默认（编辑含「不支持」）。 */
  const submitAdd = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    const ext = newExt.trim().toLowerCase().replace(/^\.+/, '')
    if (!/^[a-z0-9]{1,16}$/.test(ext)) {
      onError('扩展名须为 1..16 位字母/数字（可带前导点）')
      return
    }
    if (prefs[ext]) {
      onError(`.${ext} 已在列表中，可直接在行内修改`)
      return
    }
    setBusy(true)
    onError('')
    try {
      const builtin = builtinOpenWith(ext)
      const merged: { view?: ViewMethod; edit?: EditMethod } = {
        view: newView || builtin.view,
        edit: newEdit || builtin.edit,
      }
      await setOpenWith(ext, merged)
      setPrefs((prev) => ({ ...prev, [ext]: merged }))
      setNewExt('')
      setNewView('')
      setNewEdit('')
      onNotice(`已添加 .${ext} 的默认打开方式`)
    } catch (err) {
      onError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  // 表格行 = 内置默认扩展名 ∪ 用户覆盖中不属于内置的自定义扩展名。
  const builtinExts = BUILTIN_OPENWITH_EXTS as readonly string[]
  const customExts = Object.keys(prefs).filter((e) => !builtinExts.includes(e)).sort((a, b) => a.localeCompare(b))
  const rowExts = [...builtinExts, ...customExts]
  // 「其他（默认）」行：未列出扩展名的内置兜底（raw / 二进制不可编辑）。
  const fallbackBuiltin = builtinOpenWith('')

  // 全枚举选项（用户自选，不再按扩展名过滤）：查看 6 项；编辑 5 项 + none。
  const viewSelectOptions = (ext: string) =>
    ALL_VIEW_METHODS.map((m) => ({ value: m, label: viewMethodLabel(m, true, ext || undefined) }))
  const editSelectOptions: Array<{ value: EditMethod; label: string }> = [
    ...ALL_EDIT_METHODS.map((m) => ({ value: m, label: editMethodLabel(m, true) })),
    { value: 'none', label: '不支持（none）' },
  ]

  // 「添加」行草稿扩展名（决定下拉默认值的内置默认口径）。
  const draftExt = newExt.trim().toLowerCase().replace(/^\.+/, '')

  return (
    <div className="panel setting-group">
      <h3>打开方式</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        按扩展名管理默认的查看 / 编辑方式。下表预填全部内置默认（未覆盖行直接显示内置值，
        修改即创建覆盖、改回内置值自动恢复默认）；标「已覆盖」的行是你显式保存过的配置，
        可重置恢复内置。下拉列出全部方式由你自选（不按扩展名过滤，如 .docx 也可强制用
        富文本查看）；文件右键菜单的「打开方式」选择不再改写这里的配置。
      </div>
      {/* 「添加」行（v2.2 移至面板顶部）：扩展名 + 查看/编辑方式（默认取该扩展内置）。 */}
      <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={(e) => void submitAdd(e)}>
        <label className="field">
          <span>扩展名</span>
          <Input
            allowClear
            maxLength={17}
            value={newExt}
            onChange={(e) => {
              setNewExt(e.target.value)
              setNewView('')
              setNewEdit('')
            }}
            placeholder="如 docx / md / drawio"
            aria-label="扩展名"
          />
        </label>
        <label className="field">
          <span>查看方式</span>
          <Select
            value={newView || (draftExt ? builtinOpenWith(draftExt).view : '')}
            onChange={(v) => setNewView(v as ViewMethod)}
            disabled={!draftExt || busy}
            options={viewSelectOptions(draftExt)}
            style={{ minWidth: 140 }}
            aria-label="查看方式"
          />
        </label>
        <label className="field">
          <span>编辑方式</span>
          <Select
            value={newEdit || (draftExt ? builtinOpenWith(draftExt).edit : '')}
            onChange={(v) => setNewEdit(v as EditMethod)}
            disabled={!draftExt || busy}
            options={editSelectOptions}
            style={{ minWidth: 140 }}
            aria-label="编辑方式"
          />
        </label>
        <Button type="primary" htmlType="submit" disabled={busy || !/^[a-z0-9]{1,16}$/.test(draftExt)}>
          {busy ? '保存中…' : '添加'}
        </Button>
      </form>
      {loading ? (
        <div className="hint">加载中…</div>
      ) : (
        <table className="file-table openwith-table">
          <thead>
            <tr>
              <th>扩展名</th>
              <th>查看方式</th>
              <th>编辑方式</th>
              <th>状态</th>
              <th className="col-actions">操作</th>
            </tr>
          </thead>
          <tbody>
            {rowExts.map((ext) => {
              const builtin = builtinOpenWith(ext)
              const overridden = Boolean(prefs[ext])
              const currentView = prefs[ext]?.view ?? builtin.view
              const currentEdit = prefs[ext]?.edit ?? builtin.edit
              return (
                <tr key={ext} className={overridden ? 'openwith-overridden' : undefined}>
                  <td><code>.{ext}</code></td>
                  <td>
                    <Select
                      size="small"
                      disabled={busy}
                      value={currentView}
                      onChange={(v) => void saveField(ext, 'view', v as ViewMethod)}
                      options={viewSelectOptions(ext)}
                      style={{ minWidth: 140 }}
                    />
                  </td>
                  <td>
                    <Select
                      size="small"
                      disabled={busy}
                      value={currentEdit}
                      onChange={(v) => void saveField(ext, 'edit', v as EditMethod)}
                      options={editSelectOptions}
                      style={{ minWidth: 140 }}
                    />
                  </td>
                  <td>
                    {overridden
                      ? <span className="badge openwith-badge">已覆盖</span>
                      : <span className="muted">内置默认</span>}
                  </td>
                  <td className="col-actions">
                    {overridden && (
                      <Button size="small" disabled={busy} onClick={() => void resetExt(ext)}>
                        重置
                      </Button>
                    )}
                  </td>
                </tr>
              )
            })}
            <tr>
              <td><span className="muted">其他（默认）</span></td>
              <td className="muted">{viewMethodLabel(fallbackBuiltin.view, true)}</td>
              <td className="muted">不支持</td>
              <td className="muted">内置兜底</td>
              <td className="col-actions" />
            </tr>
          </tbody>
        </table>
      )}
    </div>
  )
}

/** MCP 服务卡片（v1.7）：DocFlow 已内置 MCP 服务端（POST /mcp，JSON-RPC 2.0，
 * PAT Bearer 鉴权）。本卡片展示接入端点与客户端配置示例（Claude Desktop /
 * Cursor / Cline），凭据经下方「个人访问令牌」创建（建议限定 files:read /
 * files:write scope 最小授权）。服务随部署常开，无独立开关；按 PAT 可控
 * 撤销即等效停用。 */
function McpPanel({ onNotice }: { onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const endpoint = `${window.location.origin}/mcp`
  const clientConfig = JSON.stringify(
    {
      mcpServers: {
        docflow: {
          type: 'http',
          url: endpoint,
          headers: { Authorization: 'Bearer dfpat_用你的令牌替换' },
        },
      },
    },
    null,
    2,
  )
  const [copied, setCopied] = useState<'url' | 'json' | ''>('')
  const copy = async (kind: 'url' | 'json', text: string) => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(kind)
      setTimeout(() => setCopied(''), 2000)
    } catch {
      onNotice(zh ? '复制失败，请手动选择复制' : 'Copy failed — select manually')
    }
  }
  return (
    <div className="panel setting-group">
      <h3>MCP{zh ? ' 服务（AI 接入）' : ' service (AI access)'}</h3>
      <div className="setting-desc muted">
        {zh ? (
          <>DocFlow 已内置 MCP（Model Context Protocol）服务端：在 Claude Desktop / Cursor / Cline 等
            AI 客户端接入后，AI 可直接操作你的文件（浏览 / 读写 / 版本管理 / 分享 / 全文检索，共 21 个工具）。
            权限与你的账号完全一致，写入走完整上传管线（病毒扫描 / 配额 / 黑名单）。</>
        ) : (
          <>DocFlow ships a built-in MCP (Model Context Protocol) server: connect from Claude Desktop,
            Cursor, Cline and let AI operate your files (browse / read / write / versions / shares / search,
            21 tools). Permissions mirror your account; writes go through the full upload pipeline.</>
        )}
      </div>
      <div className="field" style={{ marginTop: 10 }}>
        <span>{zh ? '服务端点（POST /mcp，JSON-RPC 2.0，限流 60 次/分钟/IP）' : 'Endpoint (POST /mcp, JSON-RPC 2.0, 60 req/min/IP)'}</span>
        <div className="share-link">
          <Input readOnly value={endpoint} onFocus={(e) => e.currentTarget.select()} />
          <Button onClick={() => void copy('url', endpoint)}>
            {copied === 'url' ? (zh ? '已复制 ✓' : 'Copied ✓') : zh ? '复制' : 'Copy'}
          </Button>
        </div>
      </div>
      <div className="field" style={{ marginTop: 10 }}>
        <span>
          {zh ? '客户端配置示例（先在本页「个人访问令牌」创建 PAT，建议限定 files:read 或 files:write scope）'
            : 'Client config (create a PAT below first; scope it to files:read or files:write)'}
        </span>
        <pre className="mcp-config-sample">{clientConfig}</pre>
        <Button size="small" style={{ marginTop: 6 }} onClick={() => void copy('json', clientConfig)}>
          {copied === 'json' ? (zh ? '已复制 ✓' : 'Copied ✓') : zh ? '复制配置' : 'Copy config'}
        </Button>
      </div>
    </div>
  )
}

// ---- admin 专属面板（v2.3 由管理页迁入设置页：邮件配置 / TLS / 系统设置） ----

/** TLS 模式选项（值与后端 caddytls.Mode 对齐）。 */
const tlsModeOptions: Array<{ value: TlsMode; label: string; desc: string }> = [
  { value: 'http', label: 'HTTP（明文）', desc: '仅限本地/内网验证；127.0.0.1 等无域名场景' },
  { value: 'auto', label: 'HTTPS（自动证书）', desc: '公网域名 DNS 指向本机，自动签发受信证书（Let\u0027s Encrypt）' },
  { value: 'internal', label: 'HTTPS（自签）', desc: '内网域名或 IP 可用，流量加密但浏览器会提示不受信' },
  { value: 'custom', label: 'HTTPS（自定义证书）', desc: '已有企业/自购证书：上传 PEM 证书+私钥，受信且无需公网 DNS' },
]

/** 证书摘要展示（CN / SAN / 有效期；未上传时提示先上传）。 */
function TlsCertInfo({ cert }: { cert: TlsCert | null | undefined }) {
  if (!cert) {
    return <div className="setting-desc muted">尚未上传证书——选择「自定义证书」模式前请先在下方上传 PEM 证书与私钥</div>
  }
  return (
    <div className="setting-desc">
      <div>CN <code className="setting-value-mono">{cert.cn || '（无 CN，以 SAN 为准）'}</code></div>
      {cert.dns_names && cert.dns_names.length > 0 && (
        <div className="muted">SAN：{cert.dns_names.join('、')}</div>
      )}
      <div className="muted">有效期：{cert.not_before} ~ {cert.not_after}</div>
    </div>
  )
}

/** HTTPS 运行时切换卡片：模式选择 + 域名，保存后经 Caddy admin API 热下发
 * （立即生效，无需重启容器；caddy 拒绝时原子回退）。未托管（managed=false）
 * 时降级为提示。切换到 HTTPS 后提示 COOKIE_SECURE 联动。custom 模式附
 * 证书上传（multipart cert/key，后端解析校验并落盘共享卷）与摘要展示。 */
export function TlsPanel({ onNotice }: { onNotice: (msg: string) => void }) {
  const [status, setStatus] = useState<TlsStatus | null>(null)
  const [mode, setMode] = useState<TlsMode>('http')
  const [domain, setDomain] = useState('')
  const [saving, setSaving] = useState(false)
  const [rowError, setRowError] = useState('')
  // 证书上传：文件选择 + 上传中标记。
  const [certFile, setCertFile] = useState<File | null>(null)
  const [keyFile, setKeyFile] = useState<File | null>(null)
  const [uploading, setUploading] = useState(false)

  useEffect(() => {
    adminGetTls()
      .then((st) => {
        setStatus(st)
        setMode(st.mode)
        setDomain(st.domain)
      })
      .catch(() => setStatus(null))
  }, [])

  const uploadCert = async () => {
    if (!certFile || !keyFile || uploading) return
    setUploading(true)
    setRowError('')
    try {
      const st = await adminUploadTlsCert(certFile, keyFile)
      setStatus(st)
      setCertFile(null)
      setKeyFile(null)
      onNotice(`证书已上传（CN：${st.cert?.cn ?? '未知'}，到期 ${st.cert?.not_after ?? '?'}）；如需启用请在上方选择「自定义证书」并保存`)
    } catch (err) {
      setRowError(err instanceof Error ? err.message : '证书上传失败')
    } finally {
      setUploading(false)
    }
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setRowError('')
    setSaving(true)
    try {
      const st = await adminPutTls(mode, domain.trim())
      setStatus(st)
      const notice =
        st.mode === 'http'
          ? '已切换为 HTTP 明文模式'
          : `HTTPS 已生效（${st.mode === 'auto' ? '自动证书' : st.mode === 'internal' ? '自签证书' : '自定义证书'}：${st.domain || '默认'}）`
      onNotice(`${notice}。若 .env 的 COOKIE_SECURE 与当前模式不符，请调整后重启 backend。`)
    } catch (err) {
      setRowError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>HTTPS / TLS</h3>
      {status === null ? (
        <div className="hint">TLS 状态加载失败</div>
      ) : !status.managed ? (
        <div className="setting-desc muted">
          当前部署未接入运行时切换（CADDY_ADMIN_ADDR 未配置）。TLS 由部署配置决定：
          .env 设置 APP_DOMAIN 为域名时入口自动启用 HTTPS（ACME 自动签发），
          未设置时为 HTTP 明文（本地验证）。
        </div>
      ) : (
        <form className="setting-edit" onSubmit={submit} style={{ flexDirection: 'column', alignItems: 'stretch', gap: 8 }}>
          <div className="setting-row" style={{ width: '100%' }}>
            <div className="setting-main">
              <div className="setting-key">当前模式</div>
              <div className="setting-desc muted">
                {status.mode === 'http'
                  ? 'HTTP 明文'
                  : status.mode === 'auto'
                    ? `HTTPS 自动证书${status.domain ? `（${status.domain}）` : ''}`
                    : status.mode === 'internal'
                      ? `HTTPS 自签${status.domain ? `（${status.domain}）` : ''}`
                      : `HTTPS 自定义证书${status.domain ? `（${status.domain}）` : ''}`}
              </div>
            </div>
          </div>
          <Radio.Group
            value={mode}
            onChange={(e) => setMode(e.target.value as TlsMode)}
            style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 6 }}
          >
            {tlsModeOptions.map((opt) => (
              <Radio key={opt.value} value={opt.value}>
                {opt.label}
                <span className="muted" style={{ marginLeft: 6 }}>{opt.desc}</span>
              </Radio>
            ))}
          </Radio.Group>
          {mode !== 'http' && (
            <label className="field">
              <span>站点域名或 IP</span>
              <Input
                placeholder="如 docflow.example.com / 192.168.1.10"
                value={domain}
                onChange={(e) => setDomain(e.target.value)}
              />
            </label>
          )}
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <Button type="primary" size="small" htmlType="submit" loading={saving}>
              {saving ? '下发中…' : '保存并立即生效'}
            </Button>
            {rowError && <span className="badge failed">{rowError}</span>}
          </div>
          {(mode === 'custom' || status.cert) && (
            <div className="tls-cert-box">
              <div className="setting-key">自定义证书（custom 模式）</div>
              <TlsCertInfo cert={status.cert} />
              {mode === 'custom' && (
                <div className="tls-cert-upload">
                  <label className="field">
                    <span>证书 PEM（.pem / .crt，可含中间证书链）</span>
                    <Upload
                      accept=".pem,.crt,.cer"
                      maxCount={1}
                      showUploadList={false}
                      beforeUpload={(file) => {
                        setCertFile(file)
                        return false
                      }}
                    >
                      <Button size="small">
                        {certFile ? `已选择：${certFile.name}` : '选择证书文件'}
                      </Button>
                    </Upload>
                  </label>
                  <label className="field">
                    <span>私钥 PEM（.key / .pem）</span>
                    <Upload
                      accept=".key,.pem"
                      maxCount={1}
                      showUploadList={false}
                      beforeUpload={(file) => {
                        setKeyFile(file)
                        return false
                      }}
                    >
                      <Button size="small">
                        {keyFile ? `已选择：${keyFile.name}` : '选择私钥文件'}
                      </Button>
                    </Upload>
                  </label>
                  <Button
                    size="small"
                    type="primary"
                    disabled={!certFile || !keyFile || uploading}
                    loading={uploading}
                    onClick={() => void uploadCert()}
                  >
                    {uploading ? '上传校验中…' : '上传证书'}
                  </Button>
                  <span className="setting-desc muted" style={{ marginLeft: 8 }}>
                    服务端校验 PEM 可解析且私钥匹配后原子落盘（替换旧证书）
                  </span>
                </div>
              )}
            </div>
          )}
        </form>
      )}
    </div>
  )
}

/** 邮件（SMTP）卡片：可编辑表单。GET/PUT /admin/settings/smtp —— 后端已
 * 支持运行时修改（DB 覆盖 → env 回退合并，保存即时生效：邮件发送处每次
 * 读库）；pass 留空 = 保持现值（任何读路径不回显，仅报 configured），
 * env 基线（.env 部署值）作对照展示，PUBLIC_BASE_URL 仍为 env-only。 */
export function MailPanel({ onNotice, onError }: { onNotice: (m: string) => void; onError: (m: string) => void }) {
  const [view, setView] = useState<SmtpSettingsView | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')
  const [form, setForm] = useState<{ enabled: boolean; host: string; port: number | null; user: string; pass: string; from: string; tls_mode: string }>({
    enabled: false, host: '', port: 587, user: '', pass: '', from: '', tls_mode: 'auto',
  })
  // 测试邮件：收件邮箱 + 发送中标记 + 最近一次结果（Alert 展示，含错误详情）。
  const [testTo, setTestTo] = useState('')
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string } | null>(null)

  const load = async () => {
    setLoading(true)
    try {
      const v = await adminGetSmtpSettings()
      setView(v)
      setForm({
        enabled: v.enabled, host: v.host, port: v.port, user: v.user, pass: '',
        from: v.from, tls_mode: v.tls_mode || 'auto',
      })
    } catch (err) {
      onError(err instanceof Error ? err.message : 'SMTP 配置加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const handleSave = async (e: FormEvent) => {
    e.preventDefault()
    const host = form.host.trim()
    const from = form.from.trim()
    const port = form.port ?? 0
    if (form.enabled && (!host || !from)) {
      setFormError('启用 SMTP 时服务器地址与发件人必填')
      return
    }
    if (port < 1 || port > 65535) {
      setFormError('端口须为 1-65535')
      return
    }
    setSaving(true)
    setFormError('')
    try {
      const v = await adminPutSmtpSettings({
        enabled: form.enabled, host, port, user: form.user.trim(), pass: form.pass, from, tls_mode: form.tls_mode,
      })
      setView(v)
      setForm((prev) => ({ ...prev, pass: '' }))
      onNotice('SMTP 配置已保存（即时生效）')
    } catch (err) {
      setFormError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  /** 发送测试邮件（POST /admin/settings/smtp/test）：用当前**生效**配置投递；
   * 注意表单未保存的草稿不生效——提示先保存。失败错误详情经 Alert 展示。 */
  const sendTest = async () => {
    const to = testTo.trim()
    if (to === '' || testing) return
    setTesting(true)
    setTestResult(null)
    try {
      const r = await adminTestSmtp(to)
      setTestResult({ ok: true, message: r.message || `测试邮件已发送至 ${to}` })
    } catch (err) {
      setTestResult({ ok: false, message: err instanceof Error ? err.message : '发送失败' })
    } finally {
      setTesting(false)
    }
  }

  if (loading) {
    return (
      <div className="panel setting-group">
        <h3>邮件配置（SMTP）</h3>
        <div className="empty">SMTP 配置加载中…</div>
      </div>
    )
  }

  return (
    <div className="panel setting-group">
      <h3>邮件配置（SMTP）</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        邮件通道（邀请注册、密码重置、通知副本）在此保存即时生效：表单值即为
        生效配置（输入框占位符提示 .env 基线值）；密码留空表示保持现值
        （不回显）；未启用时使用日志通道（链接输出到 backend 日志，不发送邮件）。
      </div>
      {view && (
        <form className="smtp-form" onSubmit={handleSave}>
          <div className="setting-row" style={{ borderBottom: 0, paddingBottom: 0 }}>
            <div className="setting-main">
              <div className="setting-key">通道状态 <code className="setting-desc muted">smtp.enabled</code></div>
              <div className="setting-desc muted">启用时经 SMTP 投递；未启用为 Noop 日志通道（env 基线：{view.env.enabled ? '启用' : '未启用'}）</div>
            </div>
            <div className="setting-control">
              <span className="setting-bool">
                <Switch size="small" checked={form.enabled} onChange={(v) => setForm((prev) => ({ ...prev, enabled: v }))} />
                <span>{form.enabled ? '开启' : '关闭'}</span>
              </span>
            </div>
          </div>
          <div className="smtp-grid">
            <label className="field">
              <span>服务器地址（SMTP_HOST）</span>
              <Input
                value={form.host}
                onChange={(e) => setForm((prev) => ({ ...prev, host: e.target.value }))}
                placeholder={view.env.host || '如 smtp.example.com'}
              />
            </label>
            <label className="field">
              <span>端口（SMTP_PORT）</span>
              <InputNumber
                min={1}
                max={65535}
                value={form.port}
                onChange={(v) => setForm((prev) => ({ ...prev, port: v }))}
                placeholder={String(view.env.port || 587)}
                style={{ width: '100%' }}
              />
            </label>
            <label className="field">
              <span>加密方式（smtp.tls_mode）</span>
              <Select
                value={form.tls_mode}
                onChange={(v) => setForm((prev) => ({ ...prev, tls_mode: v }))}
                options={[
                  { value: 'auto', label: 'auto（STARTTLS 自动协商，默认）' },
                  { value: 'ssl', label: 'ssl（隐式 TLS / SMTPS，465 常见）' },
                  { value: 'none', label: 'none（不协商，仅内网中继）' },
                ]}
              />
            </label>
            <label className="field">
              <span>发件人（SMTP_FROM，启用时必填）</span>
              <Input
                value={form.from}
                onChange={(e) => setForm((prev) => ({ ...prev, from: e.target.value }))}
                placeholder={view.env.from || '如 docflow@example.com'}
              />
            </label>
            <label className="field">
              <span>认证用户名（SMTP_USER，空 = 匿名投递）</span>
              <Input
                value={form.user}
                onChange={(e) => setForm((prev) => ({ ...prev, user: e.target.value }))}
                placeholder={view.env.user || '匿名投递'}
              />
            </label>
            <label className="field">
              <span>认证密码（SMTP_PASS，留空保持现值）</span>
              <Input.Password
                value={form.pass}
                onChange={(e) => setForm((prev) => ({ ...prev, pass: e.target.value }))}
                placeholder={view.password_configured ? '已配置（留空保持不变）' : '未配置'}
                autoComplete="new-password"
              />
            </label>
          </div>
          {formError && <div className="error-text">{formError}</div>}
          <div className="modal-actions" style={{ marginTop: 4 }}>
            <Button disabled={saving} onClick={() => void load()}>重置</Button>
            <Button type="primary" htmlType="submit" loading={saving}>
              {saving ? '保存中…' : '保存（即时生效）'}
            </Button>
          </div>
        </form>
      )}
      <div className="setting-row" style={{ marginTop: 12 }}>
        <div className="setting-main">
          <div className="setting-key">站点地址 <code className="setting-desc muted">PUBLIC_BASE_URL</code></div>
          <div className="setting-desc muted">邮件内邀请/重置链接的前缀（env-only，不可在此修改）；为空时链接退化为相对路径（仅日志可见）</div>
        </div>
        <div className="setting-control">
          {view?.public_base_url ? <span className="setting-value-mono">{view.public_base_url}</span> : <span className="badge">未设置</span>}
        </div>
      </div>
      {/* 发送测试邮件：用当前生效配置投递一封测试邮件；错误详情回显。
          v2.6 双线修复：上方「站点地址」setting-row 自带 border-bottom，
          本块不再叠加 borderTop（此前两条分隔线相距 12px）。 */}
      <div className="panel-inner" style={{ marginTop: 12, paddingTop: 12 }}>
        <div className="setting-key" style={{ marginBottom: 4 }}>发送测试邮件</div>
        <div className="setting-desc muted" style={{ marginBottom: 8 }}>
          用当前生效配置（DB 覆盖 → env 回退）投递一封测试邮件验证连通性；
          表单修改后须先「保存（即时生效）」再测试。失败时下方展示服务端错误详情（网络/认证/拒收等）。
        </div>
        <div className="team-create-row" style={{ marginBottom: testResult ? 8 : 0 }}>
          <label className="field" style={{ flex: 1 }}>
            <span>收件邮箱</span>
            <Input
              type="email"
              allowClear
              value={testTo}
              onChange={(e) => setTestTo(e.target.value)}
              placeholder="you@example.com"
              onPressEnter={() => void sendTest()}
            />
          </label>
          <Button
            type="primary"
            loading={testing}
            disabled={testing || testTo.trim() === ''}
            onClick={() => void sendTest()}
          >
            {testing ? '发送中…' : '发送测试邮件'}
          </Button>
        </div>
        {testResult && (
          <Alert
            type={testResult.ok ? 'success' : 'error'}
            showIcon
            closable
            message={testResult.ok ? '发送成功' : '发送失败'}
            description={testResult.message}
            onClose={() => setTestResult(null)}
          />
        )}
      </div>
    </div>
  )
}

/** 设置键前缀 → 分组标题（v2.3 补 audit/space；未知前缀回退原样）。
 * v2.7（反馈 14）：分组标题中英双语，随界面语言切换；补齐 ai/webdav/
 * collab/agent 前缀（此前这些分组直接裸显前缀 key）。 */
const groupTitles: Record<string, { zh: string; en: string }> = {
  ai: { zh: 'AI 检索与联网搜索', en: 'AI retrieval & web search' },
  site: { zh: '站点', en: 'Site' },
  upload: { zh: '上传', en: 'Upload' },
  share: { zh: '分享', en: 'Share' },
  retention: { zh: '保留策略', en: 'Retention' },
  security: { zh: '安全与限流', en: 'Security & rate limits' },
  batch: { zh: '批量操作', en: 'Batch operations' },
  folder: { zh: '目录', en: 'Folders' },
  backup: { zh: '备份', en: 'Backup' },
  audit: { zh: '审计', en: 'Audit' },
  space: { zh: '空间', en: 'Spaces' },
  webdav: { zh: 'WebDAV', en: 'WebDAV' },
  collab: { zh: '实时协作', en: 'Realtime collaboration' },
  agent: { zh: 'AI 创作舱（Agent）', en: 'AI agent studio' },
}

/** 值类型徽章文案（typeText，随界面语言切换）。 */
const typeText: Record<SettingType, { zh: string; en: string }> = {
  bool: { zh: '布尔', en: 'Boolean' },
  int: { zh: '整数', en: 'Integer' },
  string: { zh: '文本', en: 'Text' },
}

/** 设置生效方式徽章文案（effect 字段，随界面语言切换）。 */
const effectText: Record<string, { zh: string; en: string }> = {
  immediate: { zh: '立即生效', en: 'Immediate' },
  new_session: { zh: '新会话生效', en: 'New session' },
  restart: { zh: '需重启生效', en: 'Restart required' },
}

/** 字节量设置键（配额/大小上限类，值以字节存储；v2.6 展示层统一人类可读）。 */
const QUOTA_BYTE_KEYS = new Set([
  'upload.default_quota',
  'space.default_quota',
  'space.max_quota',
  'upload.max_file_size',
  'agent.max_memory_bytes',
])

/** 本面板不展示的键前缀（v2.9 反馈 9/10 去重）：ai.* 属「AI 设置」面板、
 * agent.* 属「AI 创作舱」面板、security.* 与 webdav.*（防爆破/限流/扫描
 * 策略/WebDAV 平台开关）属「安全与访问」面板——同一键保持单一编辑入口，
 * 避免双入口漂移；过滤在渲染层做（读取后 filter，仍一次拉全量）。 */
const HIDDEN_SETTING_PREFIXES = new Set(['ai', 'agent', 'security', 'webdav'])

/** 全部内置设置键的双语名称与简短说明（v2.7 反馈 14：此前仅部分键有
 * 中文名、其余裸显 key，且不随界面语言切换）。清单与后端 settings
 * Definitions 一一对应（admin settings 端点只输出内置键，smtp 与 ai 的
 * 运行时行不进此列表）；未收录的 key 回退显示原 key（不硬造）。
 * dzh/den 为简短说明：优先于后端 description 展示（后端描述中英混杂）。
 * 导出（v2.8）：供「配置总览」面板（ConfigOverviewPanel）复用分组索引。 */
export const SETTING_KEY_META: Record<string, { zh: string; en: string; dzh: string; den: string }> = {
  // ---- ai.rag.*（RAG 检索） ----
  'ai.rag.mode': { zh: 'RAG 检索模式', en: 'RAG mode', dzh: 'keyword（关键词）或 hybrid（混合检索）', den: 'keyword or hybrid' },
  'ai.rag.vector_enabled': { zh: '启用向量检索', en: 'Vector RAG enabled', dzh: '开启后问答检索额外走向量召回（需 Qdrant）', den: 'Enable optional vector retrieval (requires Qdrant)' },
  'ai.rag.qdrant_url': { zh: 'Qdrant 服务地址', en: 'Qdrant URL', dzh: '向量库访问地址（容器内网名或服务地址）', den: 'Qdrant vector store address' },
  'ai.rag.collection_prefix': { zh: '向量集合前缀', en: 'Collection prefix', dzh: 'Qdrant 集合名前缀', den: 'Qdrant collection name prefix' },
  'ai.rag.embedding_provider': { zh: '向量化提供方', en: 'Embedding provider', dzh: '生成向量的服务提供方', den: 'Provider for embedding vectors' },
  'ai.rag.embedding_model': { zh: '向量化模型', en: 'Embedding model', dzh: '向量化使用的模型名', den: 'Model used for embeddings' },
  'ai.rag.top_k': { zh: '向量召回条数', en: 'Vector top K', dzh: '向量检索每次召回的片段数上限', den: 'Max chunks fetched per vector query' },
  'ai.rag.chunk_size': { zh: 'RAG 分块大小', en: 'RAG chunk size', dzh: '文档切分为片段的目标大小（字符）', den: 'Target chunk size in characters' },
  'ai.rag.chunk_overlap': { zh: 'RAG 分块重叠', en: 'RAG chunk overlap', dzh: '相邻片段重叠字符数（提高边界连续性）', den: 'Overlap between adjacent chunks' },
  // ---- ai.search.*（联网搜索） ----
  'ai.search.provider': { zh: '联网搜索提供方', en: 'Web search provider', dzh: '空 = 关闭；可选 searxng 或 tavily', den: 'Empty (disabled), searxng or tavily' },
  'ai.search.searxng_url': { zh: 'SearXNG 地址', en: 'SearXNG URL', dzh: 'SearXNG 基地址（需启用 JSON API）', den: 'SearXNG base URL (JSON API enabled)' },
  'ai.search.max_results': { zh: '搜索结果条数上限', en: 'Search max results', dzh: '每次联网搜索返回的结果数上限', den: 'Max results per web search query' },
  // ---- upload.* ----
  'upload.max_versions_per_file': { zh: '每文件版本数上限', en: 'Max versions per file', dzh: '覆盖上传后按版本号裁剪历史版本', den: 'Trim history versions above this count after overwrites' },
  'upload.version_retention_days': { zh: '版本保留时间窗', en: 'Version retention window', dzh: '窗口内的版本不因数量裁剪删除；0 = 不启用', den: 'Versions inside the window survive count trims; 0 = disabled' },
  'upload.blocked_extensions': { zh: '上传扩展名黑名单', en: 'Blocked extensions', dzh: '逗号分隔（如 exe,bat,sh）；空 = 不拦截', den: 'Comma-separated (e.g. exe,bat,sh); empty = allow all' },
  'upload.max_file_size': { zh: '单文件上传大小上限', en: 'Max file size', dzh: '单文件上传的字节上限', den: 'Per-file upload size limit in bytes' },
  'upload.default_quota': { zh: '新用户默认存储配额', en: 'Default user quota', dzh: '仅对新创建用户生效；存量用户经管理端调整', den: 'Applies to newly created users only' },
  'upload.max_concurrent_uploads_per_user': { zh: '每用户并发上传上限', en: 'Concurrent uploads per user', dzh: '非终态上传会话达到上限时新建返回 429', den: 'New sessions get 429 when active sessions hit the cap' },
  // ---- share.* ----
  'share.default_expiry_hours': { zh: '分享默认有效期', en: 'Default share expiry', dzh: '新建公开分享的默认有效时长（小时）', den: 'Default validity for new public shares (hours)' },
  'share.default_watermark': { zh: '分享默认启用水印', en: 'Default watermark', dzh: '创建分享未显式指定水印时采用', den: 'Applied when share creation omits watermark flag' },
  'share.watermark_text': { zh: '水印默认模板', en: 'Watermark template', dzh: '支持 {email}/{date}/{name} 占位符', den: 'Supports {email}/{date}/{name} placeholders' },
  'share.public_enabled': { zh: '允许公开分享', en: 'Public shares allowed', dzh: '关闭后不允许创建公开分享链接', den: 'When off, public share links cannot be created' },
  // ---- retention.* ----
  'retention.trash_days': { zh: '回收站保留天数', en: 'Trash retention days', dzh: '软删除超过该天数后由后台任务彻底删除', den: 'Background job purges soft-deleted items beyond this' },
  'retention.access_events_days': { zh: '访问事件保留天数', en: 'Access event retention', dzh: '文件访问事件超过该天数后删除', den: 'Access events older than this are deleted' },
  // ---- security.* ----
  'security.rate_limit_per_minute': { zh: '认证 API 每分钟限流', en: 'Auth API rate limit', dzh: '每分钟请求上限（须重启生效：限流器启动时装配）', den: 'Requests per minute (restart required: limiter built at startup)' },
  'security.login_max_retries': { zh: '登录失败锁定阈值', en: 'Login lockout threshold', dzh: '同一用户名+IP 连续失败达到阈值后锁定，覆盖登录与 WebDAV', den: 'Lock after N consecutive failures per username+IP; covers login and WebDAV' },
  'security.login_lock_minutes': { zh: '登录锁定时长', en: 'Login lockout duration', dzh: '触发锁定后的锁定分钟数，到期自动解除', den: 'Lockout duration in minutes; lifts automatically on expiry' },
  'security.scan_quarantine_policy': { zh: '扫描失败处理策略', en: 'Scan failure policy', dzh: 'quarantine（隔离）或 reject（拒绝）', den: 'quarantine or reject' },
  // ---- batch / folder ----
  'batch.max_items': { zh: '批量操作单次上限', en: 'Batch max items', dzh: '批量操作单次可处理的最大项目数', den: 'Max items per batch operation' },
  'folder.max_depth': { zh: '目录最大深度', en: 'Max folder depth', dzh: '根为 1；创建/移动超过上限拒绝', den: 'Root is 1; deeper create/move is rejected' },
  // ---- backup.* ----
  'backup.enabled': { zh: '启用备份任务', en: 'Backup enabled', dzh: '是否启用定时备份任务（须重启生效）', den: 'Enable scheduled backup jobs (restart required)' },
  'backup.retention_days': { zh: '备份保留天数', en: 'Backup retention days', dzh: '备份文件超过该天数后清理（须重启生效）', den: 'Backups older than this are pruned (restart required)' },
  'backup.encryption_required': { zh: '要求备份加密', en: 'Backup encryption required', dzh: '开启后未加密的备份将被拒绝（须重启生效）', den: 'Reject unencrypted backups when on (restart required)' },
  'backup.last_verify': { zh: '最近备份校验时间', en: 'Last backup verify', dzh: '最近一次备份校验的时间戳（须重启生效）', den: 'Timestamp of last backup verification (restart required)' },
  // ---- audit / space ----
  'audit.retention_days': { zh: '审计日志保留期', en: 'Audit retention days', dzh: '0 = 永久保留；后台任务每日清理过期记录', den: '0 = keep forever; daily job prunes expired records' },
  'space.default_quota': { zh: '新空间默认配额', en: 'Default space quota', dzh: '新建空间的初始配额；0 = 不限', den: 'Initial quota for new spaces; 0 = unlimited' },
  'space.max_quota': { zh: '空间配额上限', en: 'Max space quota', dzh: 'owner/admin 调整配额不得超过；0 = 不限', den: 'Cap for owner/admin quota changes; 0 = unlimited' },
  'space.max_per_user': { zh: '每用户空间数上限', en: 'Max spaces per user', dzh: 'owner 维度计数，含默认空间', den: 'Counted per owner, including the default space' },
  // ---- webdav / collab ----
  'webdav.enabled': { zh: '启用 WebDAV 访问', en: 'WebDAV enabled', dzh: '开启后可经 /webdav 以个人令牌挂载文件', den: 'Mount files via /webdav with personal tokens' },
  'collab.enabled': { zh: '启用富文本实时协作', en: 'Rich-text collaboration', dzh: '协作 WebSocket 房间（关闭时端点 404）', den: 'Collab WebSocket rooms (endpoint 404s when off)' },
  // ---- agent.* ----
  'agent.enabled': { zh: '启用 Docker Agent 创作舱', en: 'Agent studio enabled', dzh: '关闭时 API 不可用且不启动容器', den: 'API disabled and no containers when off' },
  'agent.runtime': { zh: 'Agent 运行时', en: 'Agent runtime', dzh: '当前仅支持 docker（须重启生效）', den: 'Currently docker only (restart required)' },
  'agent.allowed_images': { zh: 'Agent 镜像白名单', en: 'Allowed agent images', dzh: '逗号分隔的容器镜像列表', den: 'Comma-separated container image list' },
  'agent.max_concurrent': { zh: 'Agent 最大并发任务数', en: 'Agent max concurrency', dzh: '同时运行的 Agent 任务上限', den: 'Max concurrently running agent tasks' },
  'agent.default_timeout_seconds': { zh: 'Agent 默认超时（秒）', en: 'Agent default timeout', dzh: '单个任务的默认超时秒数', den: 'Default per-task timeout in seconds' },
  'agent.max_cpu': { zh: 'Agent 最大 CPU 数', en: 'Agent max CPU', dzh: '单容器可用 CPU 上限', den: 'CPU limit per container' },
  'agent.max_memory_bytes': { zh: 'Agent 最大内存', en: 'Agent max memory', dzh: '单容器内存上限（字节）', den: 'Memory limit per container (bytes)' },
  'agent.network_mode': { zh: 'Agent 网络模式', en: 'Agent network mode', dzh: 'none 或 restricted（受限出网）', den: 'none or restricted' },
  'agent.mcp_callback_base_url': { zh: '受限 MCP 回调基地址', en: 'MCP callback base URL', dzh: '不含凭据的回调地址前缀', den: 'Credential-free callback URL prefix' },
  'agent.allow_ai': { zh: '允许 Agent 调用平台 AI', en: 'Agent AI over IPC', dzh: '容器保持断网，经 IPC socket 调用平台默认对话模型', den: 'Containers stay offline; platform AI reached over an IPC socket' },
  'agent.ai_max_calls': { zh: 'Agent 单任务 AI 调用上限', en: 'Agent AI call limit', dzh: '单个任务经 IPC 调用平台 AI 的次数上限（超出 429）', den: 'Per-task platform AI calls over IPC (429 beyond)' },
  'agent.sync_mode': { zh: 'Agent 产物同步模式', en: 'Agent sync mode', dzh: 'git（按变更清单同步，推荐）或 scan（全量扫描）', den: 'git (change-list based, recommended) or scan (full scan)' },
}

/**
 * 系统设置面板（v2.3 自管理页迁入，仅 admin）：system_settings 内置键的
 * 通用入口，按前缀分组（分组标题中文化）、key 直显 + 中文名 + 说明，值按
 * 类型渲染（bool 开关直开直关 / int 数字 / string 文本），行内编辑保存；
 * 顶部搜索框按 key/中文名/描述过滤，分组可折叠。
 * v2.9 去重：ai.* / agent.* / security.* / webdav.* 不在此展示（分属
 * 「AI 设置」「AI 创作舱」「安全与访问」面板，见 HIDDEN_SETTING_PREFIXES），
 * 面板顶部以 notice 指引对应位置。
 */
export function SystemSettingsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  // v2.7（反馈 14）：名称/说明/分组标题随界面语言切换（SETTING_KEY_META）。
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const [result, setResult] = useState<AdminSettingsResult | null>(null)
  const [loading, setLoading] = useState(true)
  const [forbidden, setForbidden] = useState(false)
  // 行内编辑状态：当前编辑键 + 草稿（bool 直接存布尔，int/string 存字符串）。
  const [editingKey, setEditingKey] = useState<string | null>(null)
  const [draft, setDraft] = useState<string | boolean>('')
  const [savingKey, setSavingKey] = useState<string | null>(null)
  const [rowError, setRowError] = useState('')
  // 搜索框过滤键 + 分组折叠状态（prefix → 折叠；过滤时自动展开）。
  const [settingsQuery, setSettingsQuery] = useState('')
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})

  const load = async () => {
    setLoading(true)
    try {
      setResult(await adminGetSettings())
      setForbidden(false)
      onError('')
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setForbidden(true)
      else onError(err instanceof Error ? err.message : (zh ? '系统设置加载失败' : 'Failed to load system settings'))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const settings = result?.settings ?? []

  /** 按键取当前语言的名称；未收录回退空串（行内仅显示原 key，不硬造）。 */
  const keyLabel = (key: string) => {
    const meta = SETTING_KEY_META[key]
    return meta ? (zh ? meta.zh : meta.en) : ''
  }

  /** 按键取当前语言的简短说明；未收录回退后端 description。 */
  const keyDesc = (key: string, fallback: string) => {
    const meta = SETTING_KEY_META[key]
    return meta ? (zh ? meta.dzh : meta.den) : fallback
  }

  /** 按键前缀分组（保持 Definitions 输出顺序），支持按 key/名称/说明过滤
   *（双语名称均参与匹配，中英文界面过滤行为一致）；ai、agent、security、
   * webdav 四类前缀已分属专属面板，在此渲染层排除（见 HIDDEN_SETTING_PREFIXES）。 */
  const settingGroups = useMemo(() => {
    const needle = settingsQuery.trim().toLowerCase()
    const map = new Map<string, SettingItem[]>()
    for (const item of settings) {
      if (HIDDEN_SETTING_PREFIXES.has(item.key.split('.')[0])) continue
      const meta = SETTING_KEY_META[item.key]
      if (needle !== '') {
        const haystack = meta
          ? `${item.key} ${meta.zh} ${meta.en} ${meta.dzh} ${meta.den}`.toLowerCase()
          : `${item.key} ${item.description}`.toLowerCase()
        if (!haystack.includes(needle)) continue
      }
      const prefix = item.key.split('.')[0]
      const list = map.get(prefix) ?? []
      list.push(item)
      map.set(prefix, list)
    }
    return Array.from(map.entries())
  }, [settings, settingsQuery])

  /** bool 设置行直开直关（不进编辑态）：切换即保存并刷新列表。 */
  const toggleBool = async (item: SettingItem) => {
    if (savingKey !== null) return
    setRowError('')
    setSavingKey(item.key)
    try {
      const normalized = await adminPutSetting(item.key, !item.value)
      onNotice(zh ? `已保存 ${item.key}（当前值：${normalized ? '开启' : '关闭'}）` : `Saved ${item.key} (now ${normalized ? 'on' : 'off'})`)
      const refreshed = await adminGetSettings()
      setResult(refreshed)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setRowError(zh ? '无权限' : 'Forbidden')
      else setRowError(err instanceof Error ? err.message : (zh ? '保存失败' : 'Save failed'))
    } finally {
      setSavingKey(null)
    }
  }

  const handleSave = async (e: FormEvent, item: SettingItem) => {
    e.preventDefault()
    if (editingKey !== item.key) return
    setRowError('')
    let value: SettingValue
    if (item.type === 'bool') {
      value = Boolean(draft)
    } else if (item.type === 'int') {
      const parsed = Number(draft)
      if (draft === '' || !Number.isInteger(parsed)) {
        setRowError(zh ? '请输入整数' : 'Enter an integer')
        return
      }
      value = parsed
    } else {
      value = String(draft).trim()
    }
    setSavingKey(item.key)
    try {
      const normalized = await adminPutSetting(item.key, value)
      setEditingKey(null)
      onNotice(zh ? `已保存 ${item.key}（当前值：${String(normalized)}）` : `Saved ${item.key} (value: ${String(normalized)})`)
      try {
        setResult(await adminGetSettings())
      } catch {
        // 列表刷新失败不打断，保留本地已保存状态
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setRowError(zh ? '无权限' : 'Forbidden')
      else if (err instanceof ApiError && err.status === 400) setRowError(zh ? '取值超出允许范围' : 'Value out of allowed range')
      else setRowError(err instanceof Error ? err.message : (zh ? '保存失败' : 'Save failed'))
    } finally {
      setSavingKey(null)
    }
  }

  if (forbidden) {
    return (
      <div className="panel setting-group">
        <h3>{zh ? '系统设置' : 'System settings'}</h3>
        <div className="empty">{zh ? '仅系统管理员可访问' : 'Administrators only'}</div>
      </div>
    )
  }

  return (
    <>
      <div className="panel setting-group" style={{ padding: '12px 16px' }}>
        {/* v2.9 去重指引：被排除的键分组不再出现，告知对应修改入口。 */}
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message={zh ? '部分配置已移至对应面板（此处不再重复展示）' : 'Some settings live in their own panels (not listed here)'}
          description={zh
            ? 'AI 检索与联网搜索（ai.*）请前往「AI 设置」；Agent 创作舱（agent.*）请前往「AI 创作舱」；登录防爆破、认证限流、扫描策略与 WebDAV 平台开关（security.* / webdav.*）请前往「安全与访问」。'
            : 'AI retrieval & web search (ai.*) → "AI settings"; agent keys (agent.*) → "AI agent studio"; login anti-bruteforce, auth rate limit, scan policy and the WebDAV platform toggle (security.* / webdav.*) → "Security & access".'}
        />
        <form className="team-create-row" style={{ marginBottom: 0 }} onSubmit={(e) => e.preventDefault()}>
          <label className="field" style={{ flex: 1 }}>
            <span>{zh ? '过滤设置键（按 key / 名称 / 说明匹配；过滤时分组自动展开）' : 'Filter setting keys (by key / name / description; groups auto-expand while filtering)'}</span>
            <Input
              allowClear
              autoCapitalize="none"
              spellCheck={false}
              value={settingsQuery}
              onChange={(e) => setSettingsQuery(e.target.value)}
              placeholder={zh ? '如：upload、space.max_quota 或“配额”' : 'e.g. upload, space.max_quota or "quota"'}
            />
          </label>
          {settingsQuery && (
            <Button style={{ alignSelf: 'flex-end' }} onClick={() => setSettingsQuery('')}>{zh ? '清除' : 'Clear'}</Button>
          )}
        </form>
      </div>

      {loading && <div className="hint">{zh ? '加载中…' : 'Loading…'}</div>}
      {!loading && settingGroups.length === 0 && <div className="empty">{zh ? '没有匹配的设置项' : 'No matching settings'}</div>}

      {settingGroups.map(([prefix, items]) => {
        const searching = settingsQuery.trim() !== ''
        const isCollapsed = !searching && collapsed[prefix]
        const groupTitle = groupTitles[prefix]
        return (
          <div key={prefix} className="panel setting-group">
            <h3 style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <Button
                type="text"
                size="small"
                title={isCollapsed ? (zh ? '展开分组' : 'Expand group') : (zh ? '折叠分组' : 'Collapse group')}
                onClick={() => setCollapsed((prev) => ({ ...prev, [prefix]: !isCollapsed }))}
              >
                {isCollapsed ? '▸' : '▾'}
              </Button>
              {groupTitle ? (zh ? groupTitle.zh : groupTitle.en) : prefix}
              <span className="setting-meta muted">{zh ? `（${items.length} 项）` : `(${items.length})`}</span>
            </h3>
            {!isCollapsed && items.map((item) => {
              const editing = editingKey === item.key
              const label = keyLabel(item.key)
              return (
                <div key={item.key} className="setting-row">
                  <div className="setting-main">
                    <div className="setting-key">
                      {label && <span>{label}</span>}
                      <code className="setting-key-code" title={item.key}>{item.key}</code>
                      {item.effect && (
                        <span className="badge" style={{ marginLeft: 8 }} title={zh ? '变更生效方式' : 'How changes take effect'}>
                          {(() => {
                            const text = effectText[item.effect]
                            return text ? (zh ? text.zh : text.en) : item.effect
                          })()}
                        </span>
                      )}
                    </div>
                    <div className="setting-desc muted">{keyDesc(item.key, item.description)}</div>
                    <div className="setting-meta muted">
                      {zh ? '类型' : 'Type'} {(() => { const t = typeText[item.type]; return t ? (zh ? t.zh : t.en) : item.type })()}
                      {' · '}{zh ? '默认值' : 'Default'} {QUOTA_BYTE_KEYS.has(item.key) ? formatQuota(Number(item.default)) : String(item.default)}
                      {item.updated_at && ` · ${zh ? '更新于' : 'updated'} ${formatTime(item.updated_at)}`}
                    </div>
                  </div>
                  <div className="setting-control">
                    {editing ? (
                      <form className="setting-edit" onSubmit={(e) => void handleSave(e, item)}>
                        {item.type === 'bool' ? (
                          <span className="setting-bool">
                            <Switch size="small" checked={Boolean(draft)} onChange={(v) => setDraft(v)} />
                            <span>{draft ? (zh ? '开启' : 'On') : (zh ? '关闭' : 'Off')}</span>
                          </span>
                        ) : item.type === 'int' ? (
                          <InputNumber
                            step={1}
                            autoFocus
                            value={draft === '' ? null : Number(draft)}
                            onChange={(v) => setDraft(v === null || v === undefined ? '' : String(v))}
                          />
                        ) : (
                          <Input
                            autoFocus
                            style={{ width: 220 }}
                            value={String(draft)}
                            onChange={(e) => setDraft(e.target.value)}
                          />
                        )}
                        <Button
                          type="primary"
                          size="small"
                          htmlType="submit"
                          disabled={savingKey !== null || (item.type === 'string' && String(draft).trim() === '')}
                          loading={savingKey === item.key}
                        >
                          {zh ? '保存' : 'Save'}
                        </Button>
                        <Button
                          size="small"
                          disabled={savingKey !== null}
                          onClick={() => { setEditingKey(null); setRowError('') }}
                        >
                          {zh ? '取消' : 'Cancel'}
                        </Button>
                      </form>
                    ) : item.type === 'bool' ? (
                      <span className="setting-bool" title={zh ? 'bool 行直开直关：切换后立即保存' : 'Boolean rows save immediately on toggle'}>
                        <Switch
                          size="small"
                          checked={Boolean(item.value)}
                          loading={savingKey === item.key}
                          onChange={() => void toggleBool(item)}
                        />
                        <span>{savingKey === item.key ? (zh ? '保存中…' : 'Saving…') : item.value ? (zh ? '开启' : 'On') : (zh ? '关闭' : 'Off')}</span>
                      </span>
                    ) : (
                      <>
                        {/* 字节量设置键（配额/大小上限，v2.6）：值显示人类可读
                            大小（formatQuota 口径），title 悬浮原始字节数；编辑
                            仍输入原始整数（字节）。 */}
                        <span className="setting-value-mono" title={QUOTA_BYTE_KEYS.has(item.key) ? (zh ? `${String(item.value)} 字节` : `${String(item.value)} bytes`) : undefined}>
                          {QUOTA_BYTE_KEYS.has(item.key) ? formatQuota(Number(item.value)) : String(item.value)}
                        </span>
                        <Button size="small" onClick={() => {
                          setEditingKey(item.key)
                          setDraft(item.type === 'bool' ? Boolean(item.value) : String(item.value))
                          setRowError('')
                        }}>{zh ? '编辑' : 'Edit'}</Button>
                      </>
                    )}
                  </div>
                  {editing && rowError && <div className="error-text setting-row-error">{rowError}</div>}
                </div>
              )
            })}
          </div>
        )
      })}
    </>
  )
}

/** 设置页分区（v2.x 个人/平台分离）：设置页只保留**个人**配置——外观/
 * 打开方式/账号安全/通知/开发者（PAT/Webhook/MCP）/WebDAV 个人令牌。
 * 平台级配置（AI 全局/创作舱/邮件/TLS/系统设置）全部迁往「平台管理」
 *（AdminPage /admin/platform），仅管理员可见。 */
const baseSettingsSections = [['appearance', '外观'], ['openers', '打开方式'], ['security', '账号安全'], ['notifications', '通知'], ['developer', '开发者'], ['webdav', 'WebDAV'], ['aipersonal', 'AI 个人配置']] as const
export function AgentPanel({ onError, onNotice }: { onError: (m: string) => void; onNotice: (m: string) => void }) { const [cfg,setCfg]=useState<Record<string,unknown>|null>(null); const [busy,setBusy]=useState(false); useEffect(()=>{void getAgentSettings().then(setCfg).catch(e=>onError(e instanceof Error?e.message:'Agent 配置加载失败'))},[]); if(!cfg)return <div className="panel setting-group"><h3>AI 创作舱</h3><div className="hint">加载中…</div></div>; const save=async(enabled:boolean)=>{setBusy(true);try{const next=await putAgentSettings({enabled});setCfg(next);onNotice(enabled?'Agent 已开启':'Agent 已关闭')}catch(e){onError(e instanceof Error?e.message:'保存失败')}finally{setBusy(false)}}; return <div className="panel setting-group"><h3>Docker Agent 创作舱</h3><div className="setting-desc muted">安全边界：关闭时 API 不可用且不启动容器；任务会先创建目录快照；Agent 产物必须经过差异预览和用户确认后才会写回平台，Docker runtime 按需启用。</div><div className="setting-row"><div className="setting-main"><div className="setting-key">Agent 开关</div><div className="setting-desc muted">默认关闭；开启后仍需配置镜像白名单。</div></div><div className="setting-control"><Switch checked={Boolean(cfg.enabled)} disabled={busy} onChange={(v)=>void save(v)} /></div></div></div> }
export function WebDAVPanel({ onError, onNotice }: { onError: (m: string) => void; onNotice: (m: string) => void }) { const [items,setItems]=useState<WebDAVToken[]>([]); const [name,setName]=useState(''); const [token,setToken]=useState(''); const load=async()=>{try{setItems(await listWebDAVTokens())}catch(e){onError(e instanceof Error?e.message:'加载失败')}}; useEffect(()=>{void load()},[]); const create=async()=>{try{const x=await createWebDAVToken(name,90);setToken(x.token);setName('');await load();onNotice('令牌已创建，请立即复制') }catch(e){onError(e instanceof Error?e.message:'创建失败')}}; return <div className="panel setting-group"><h3>WebDAV 文件挂载（个人令牌）</h3><div className="setting-desc muted">URL：{window.location.origin}/webdav；Windows 映射网络驱动器，Linux 使用 davfs2，macOS 使用 Finder“连接服务器”。WebDAV 使用 Basic Auth：用户名为平台邮箱/用户名，密码为下方一次性令牌；服务端开关由管理员在「平台管理 → 平台设置」配置。</div>{token&&<div className="share-link"><Input readOnly value={token}/><Button onClick={()=>void navigator.clipboard.writeText(token)}>复制令牌</Button></div>} {items.map(x=><div className="setting-row" key={x.id}><div className="setting-main"><b>{x.name}</b><div className="muted">最后使用：{x.last_used_at||'未使用'} · 过期：{x.expires_at||'永不过期'}</div></div><Button danger onClick={()=>void revokeWebDAVToken(x.id).then(load)}>吊销</Button></div>)}<div className="team-create-row"><Input placeholder="令牌名称（如：我的电脑）" style={{ maxWidth: 240 }} value={name} onChange={e=>setName(e.target.value)}/><Button type="primary" disabled={!name.trim()} onClick={()=>void create()}>创建令牌</Button></div></div>}

export default function SettingsPage() {
  const { section = 'appearance' } = useParams()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  // admin 探测（与顶栏「管理」入口同法）：admin 专属分区（邮件/TLS/系统设置）。
  // v2.5 三态（null = 探测中）：isAdmin() 异步返回前不重定向——旧实现首渲染
  // 恒为 false，直接访问 /settings/mail 等分区会在探测完成前被 Navigate 弹回
  // 「外观」（「点邮件配置/TLS/系统设置跳到外观页」的根因）。
  const [admin, setAdmin] = useState<boolean | null>(null)
  useEffect(() => {
    let alive = true
    void isAdmin().then((v) => { if (alive) setAdmin(v) })
    return () => { alive = false }
  }, [])
  // 探测中：渲染骨架（不重定向、不闪错分区），探测完成即出正确分区。
  if (admin === null) {
    return (
      <div className="page section-page">
        <aside className="section-sidebar"><h3>{msg('settings')}</h3></aside>
        <div className="section-content">
          <div className="page-head"><h2>{msg('settings')}</h2></div>
          <div className="hint">{msg('loading')}</div>
        </div>
      </div>
    )
  }
  // 个人/平台分离（v2.x）：设置页不再区分 admin 分区——平台级配置全部
  // 位于「平台管理」（/admin/platform）；此处仅个人分区，admin 探测仅用于
  // 旧地址（/settings/mail|tls|system|ai|agent）平滑弹回个人「外观」。
  const sections = baseSettingsSections
  if (!sections.some(([key]) => key === section)) {
    return <Navigate to="/settings/appearance" replace />
  }
  return (
    <div className="page section-page">
      <aside className="section-sidebar"><h3>设置</h3>{sections.map(([key, label]) => <NavLink key={key} to={`/settings/${key}`} className={({ isActive }) => isActive ? 'active' : ''}>{label}</NavLink>)}</aside>
      <div className="section-content">
      <div className="page-head">
        <h2>{msg('settings')}</h2>
      </div>
      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {section === 'webdav' && <WebDAVPanel onError={(m) => { setError(m); setNotice('') }} onNotice={(m) => { setNotice(m); setError('') }} />}
      {section === 'aipersonal' && <AIPersonalPanel onError={(m) => { setError(m); setNotice('') }} onNotice={(m) => { setNotice(m); setError('') }} />}
      {section === 'appearance' && <AppearancePanel />}
      {section === 'openers' && <OpenWithPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />}
      {section === 'notifications' && <NotificationsPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />}
      {section === 'security' && (
        <>
          <TotpPanel
            onError={(msg) => { setError(msg); setNotice('') }}
            onNotice={(msg) => { setNotice(msg); setError('') }}
          />
          <EmailChangePanel onNotice={(msg) => { setNotice(msg); setError('') }} />
        </>
      )}
      {section === 'developer' && (
        <div className="panel setting-group" style={{ padding: '12px 16px' }}>
          <div className="setting-desc muted" style={{ marginBottom: 0 }}>
            开发者工具：个人访问令牌（PAT，供脚本/CI 以 Bearer dfpat_… 调用 API）与
            Webhook（事件发生时向回调 URL 推送 HMAC 签名的 JSON）。两者均为本人维度，
            一次性凭据仅在创建时展示一次。
          </div>
        </div>
      )}
      {section === 'developer' && (
        <McpPanel onNotice={(m) => { setNotice(m); setError('') }} />
      )}
      {section === 'developer' && <WebhooksPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />}
      {section === 'security' && <SessionsPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />}
      {section === 'developer' && <TokensPanel
        onError={(msg) => { setError(msg); setNotice('') }}
        onNotice={(msg) => { setNotice(msg); setError('') }}
      />}
      </div>
    </div>
  )
}
