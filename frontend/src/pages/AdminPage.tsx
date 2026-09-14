// 管理设置页（仅 admin 角色）：顶部五类计数概览 + 按前缀分组的设置卡片，
// 行内编辑保存（bool 开关 / int 数字 / string 文本，按 value type 渲染）。
// 另含邀请管理与用户管理卡片：邀请创建（一次性注册链接展示/复制）、列表
// 状态与撤销；用户列表（q 前缀检索 + 分页）、禁用/启用（立即撤销其全部
// 会话）、改配额、改角色与重置密码（C6；删除以软禁用替代，见 openapi 取舍）。
// 非 admin（403）显示无权限页；保存成功后刷新列表并提示。
import { FormEvent, useEffect, useMemo, useState } from 'react'
import {
  AdminStats,
  AdminUser,
  ApiError,
  BackupStatus,
  BackupVerifyResult,
  Invitation,
  QuarantineAction,
  QuarantineItem,
  SettingItem,
  SettingType,
  SettingValue,
  adminCreateInvitation,
  adminDownloadAuditCSV,
  adminGetBackupStatus,
  adminVerifyBackup,
  adminGetSettings,
  adminListAuditLogs,
  adminListQuarantine,
  adminQuarantineAction,
  adminRunBackup,
  adminGetStats,
  adminListInvitations,
  adminListUsers,
  adminPutSetting,
  adminResetUserPassword,
  adminRevokeInvitation,
  adminUpdateUser,
  currentUserId,
} from '../api'
import { formatTime } from '../components/FileBrowser'
import { MessageKey, t, useLocale } from '../i18n'

/** 设置键前缀 → 分组标题（未知前缀回退原样）。 */
const groupTitles: Record<string, string> = {
  site: '站点',
  upload: '上传',
  share: '分享',
  retention: '保留策略',
  security: '安全与限流',
  batch: '批量操作',
  folder: '目录',
  backup: '备份',
}

const typeText: Record<SettingType, string> = {
  bool: '布尔',
  int: '整数',
  string: '文本',
}

/** 设置生效方式徽章文案（effect 字段）。 */
const effectText: Record<string, string> = {
  immediate: '立即生效',
  new_session: '新会话生效',
  restart: '需重启生效',
}

/** 凭据状态卡的语义键 → 环境变量名展示（值绝不回显，仅展示配置状态）。 */
const secretLabels: Array<{ key: string; env: string; label: string }> = [
  { key: 'jwt_secret', env: 'JWT_SECRET', label: 'JWT 签名密钥' },
  { key: 'smtp_password', env: 'SMTP_PASS', label: 'SMTP 密码' },
  { key: 's3_secret_key', env: 'S3_SECRET_KEY', label: 'S3 密钥' },
  { key: 'onlyoffice_jwt_secret', env: 'ONLYOFFICE_JWT_SECRET', label: 'ONLYOFFICE JWT 密钥' },
]

const statCards: Array<{ key: keyof AdminStats; label: string }> = [
  { key: 'users', label: '用户' },
  { key: 'files', label: '文件' },
  { key: 'uploads', label: '上传会话' },
  { key: 'sessions', label: '登录会话' },
  { key: 'shares', label: '分享' },
  { key: 'tokens', label: 'API 令牌' },
]

/** 邀请派生状态的展示徽章 class（复用既有 badge 样式）。 */
const invitationStatusBadge: Record<Invitation['status'], string> = {
  pending: 'badge available',
  accepted: 'badge',
  expired: 'badge failed',
}

const invitationStatusText: Record<Invitation['status'], string> = {
  pending: '待接受',
  accepted: '已接受',
  expired: '已过期',
}

/** 邀请管理卡片：创建（一次性注册链接展示/复制）、列表状态与撤销。 */
function InvitationsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [invitations, setInvitations] = useState<Invitation[]>([])
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<'user' | 'admin'>('user')
  const [busy, setBusy] = useState(false)
  const [revoking, setRevoking] = useState<string | null>(null)
  /** 最近一次创建的一次性注册链接（明文仅创建响应返回一次，保存在内存）。 */
  const [oneTimeLink, setOneTimeLink] = useState('')
  const [copied, setCopied] = useState(false)

  const load = async () => {
    try {
      setInvitations(await adminListInvitations())
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('invitesLoadFailed'))
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
      const created = await adminCreateInvitation(email.trim(), role)
      setEmail('')
      if (created.accept_url) {
        // 拼成可分享的完整链接（当前站点 + 前端路由）。
        const link = new URL(created.accept_url, window.location.origin).toString()
        setOneTimeLink(link)
        setCopied(false)
        onNotice(`已创建给 ${created.email} 的邀请，请立即复制一次性注册链接`)
      } else {
        setOneTimeLink('')
        onNotice(`${created.email} 已有待接受的邀请，未重复创建`)
      }
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('inviteCreateFailed'))
    } finally {
      setBusy(false)
    }
  }

  const copyLink = async () => {
    try {
      await navigator.clipboard.writeText(oneTimeLink)
      setCopied(true)
    } catch {
      // 剪贴板不可用（如非安全上下文）：保留输入框展示，由管理员手动复制。
      setCopied(false)
    }
  }

  const revoke = async (id: string) => {
    if (revoking !== null) return
    setRevoking(id)
    try {
      await adminRevokeInvitation(id)
      onNotice('已撤销邀请')
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
      <h3>{msg('invitesTitle')}</h3>
      {oneTimeLink && (
        <div className="share-link" style={{ marginBottom: 12 }}>
          <input type="text" readOnly value={oneTimeLink} onFocus={(e) => e.currentTarget.select()} />
          <button className="btn small" type="button" onClick={() => void copyLink()}>
            {copied ? '已复制' : '复制链接'}
          </button>
        </div>
      )}
      {oneTimeLink && <div className="setting-desc muted" style={{ marginBottom: 12 }}>该注册链接仅显示这一次，请立即复制发送给被邀请人（7 天内有效，仅可使用一次）</div>}
      <form className="team-create-row" onSubmit={(e) => void submit(e)}>
        <label className="field">
          <span>邮箱</span>
          <input
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="teammate@example.com"
          />
        </label>
        <label className="field">
          <span>角色</span>
          <select value={role} onChange={(e) => setRole(e.target.value === 'admin' ? 'admin' : 'user')}>
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </label>
        <button className="btn primary" type="submit" disabled={busy}>
          {busy ? '创建中…' : '创建邀请'}
        </button>
      </form>
      {invitations.length === 0 ? (
        <div className="empty">{msg('noInvites')}</div>
      ) : (
        invitations.map((inv) => (
          <div key={inv.id} className="setting-row">
            <div className="setting-main">
              <div className="setting-key">{inv.email}</div>
              <div className="setting-meta muted">
                角色 {inv.role} · 创建于 {formatTime(inv.created_at)} · 有效期至 {formatTime(inv.expires_at)}
                {inv.accepted_at && ` · 已于 ${formatTime(inv.accepted_at)} 接受`}
              </div>
            </div>
            <div className="setting-control">
              <span className={invitationStatusBadge[inv.status]}>{invitationStatusText[inv.status]}</span>
              {inv.status === 'pending' && (
                <button
                  className="btn small danger"
                  disabled={revoking !== null}
                  onClick={() => void revoke(inv.id)}
                >
                  {revoking === inv.id ? '撤销中…' : '撤销'}
                </button>
              )}
            </div>
          </div>
        ))
      )}
    </div>
  )
}

function formatSettingValue(value: SettingValue): string {
  return String(value)
}

/** 用户状态徽标（locked 为登录失败锁定期，到期自动解锁或管理员启用解锁）。 */
const userStatusBadge: Record<AdminUser['status'], string> = {
  active: 'badge available',
  disabled: 'badge failed',
  locked: 'badge quarantined',
}

const userStatusText: Record<AdminUser['status'], string> = {
  active: '正常',
  disabled: '已禁用',
  locked: '已锁定',
}

/** GiB 数字与字节的互转（配额编辑用；后端存储字节）。 */
const GIB = 1 << 30
function quotaToGib(quota: number): string {
  return String(Math.round((quota / GIB) * 100) / 100)
}

/** 用户管理卡片（C6）：检索/分页列表 + 禁用启用/改配额/改角色/重置密码。 */
function UsersPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [users, setUsers] = useState<AdminUser[]>([])
  const [total, setTotal] = useState(0)
  const [offset, setOffset] = useState(0)
  const [q, setQ] = useState('')
  const [query, setQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  /** 行内编辑状态：配额草稿 / 重置密码草稿 / 状态流转中。 */
  const [editingQuota, setEditingQuota] = useState('')
  const [editingQuotaId, setEditingQuotaId] = useState<string | null>(null)
  const [resetPasswordId, setResetPasswordId] = useState<string | null>(null)
  const [newPassword, setNewPassword] = useState('')
  const pageLimit = 20
  const selfId = currentUserId()

  const load = async (nextOffset = offset, search = query) => {
    setLoading(true)
    try {
      const result = await adminListUsers(search, pageLimit, nextOffset)
      setUsers(result.users)
      setTotal(result.total)
      setOffset(nextOffset)
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : msg('usersLoadFailed'))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load(0, '')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const submitSearch = (e: FormEvent) => {
    e.preventDefault()
    setQuery(q)
    void load(0, q)
  }

  const update = async (id: string, opts: { status?: 'active' | 'disabled'; storageQuota?: number; role?: 'user' | 'admin' }, okMessage: string) => {
    if (busy) return
    setBusy(true)
    onError('')
    try {
      await adminUpdateUser(id, opts)
      onNotice(okMessage)
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '操作失败')
    } finally {
      setBusy(false)
    }
  }

  const submitQuota = async (e: FormEvent, user: AdminUser) => {
    e.preventDefault()
    if (editingQuotaId !== user.id) return
    const gib = Number(editingQuota)
    if (editingQuota === '' || !Number.isFinite(gib) || gib <= 0) {
      onError('请输入大于 0 的配额数字（GiB）')
      return
    }
    await update(user.id, { storageQuota: Math.round(gib * GIB) }, `已将 ${user.username} 的配额调整为 ${gib} GiB`)
    setEditingQuotaId(null)
  }

  const submitResetPassword = async (e: FormEvent, user: AdminUser) => {
    e.preventDefault()
    if (resetPasswordId !== user.id || newPassword === '') return
    if (busy) return
    setBusy(true)
    onError('')
    try {
      await adminResetUserPassword(user.id, newPassword)
      setResetPasswordId(null)
      setNewPassword('')
      onNotice(`已重置 ${user.username} 的密码（其全部登录会话已失效）`)
    } catch (err) {
      onError(err instanceof Error ? err.message : '重置密码失败')
    } finally {
      setBusy(false)
    }
  }

  const pages = Math.max(1, Math.ceil(total / pageLimit))
  const page = Math.floor(offset / pageLimit) + 1

  return (
    <div className="panel setting-group">
      <h3>{msg('usersTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        检索（用户名/邮箱前缀）与分页浏览全部用户；禁用账号立即撤销其全部登录会话，
        启用同时解除登录失败锁定。账号不支持删除——以禁用替代（保留其名下文件与审计记录）。
      </div>
      <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={(e) => void submitSearch(e)}>
        <label className="field">
          <span>检索（用户名 / 邮箱前缀）</span>
          <input
            type="text"
            autoCapitalize="none"
            spellCheck={false}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="如：alice 或 alice@"
          />
        </label>
        <button className="btn primary" type="submit">搜索</button>
        {query && (
          <button
            className="btn"
            type="button"
            onClick={() => { setQ(''); setQuery(''); void load(0, '') }}
          >
            清除
          </button>
        )}
      </form>
      {loading ? (
        <div className="hint">{msg('loading')}</div>
      ) : users.length === 0 ? (
        <div className="empty">{msg('noUsers')}</div>
      ) : (
        users.map((user) => {
          const isSelf = user.id === selfId
          const editing = editingQuotaId === user.id
          const resetting = resetPasswordId === user.id
          return (
            <div key={user.id} className="setting-row">
              <div className="setting-main">
                <div className="setting-key">
                  {user.profile.nickname || user.username}
                  <span className={userStatusBadge[user.status]} style={{ marginLeft: 8 }}>
                    {userStatusText[user.status]}
                  </span>
                  {user.role === 'admin' && <span className="badge role-owner" style={{ marginLeft: 4 }}>admin</span>}
                  {isSelf && <span className="badge" style={{ marginLeft: 4 }}>本人</span>}
                </div>
                <div className="setting-meta muted" title={user.email}>
                  {user.username} · {user.email} · 配额 {quotaToGib(user.storage_quota)} GiB
                  {user.locked_until && ` · 锁定至 ${formatTime(user.locked_until)}`}
                </div>
                <div className="setting-desc muted">注册于 {formatTime(user.created_at)}</div>
              </div>
              <div className="setting-control">
                {editing ? (
                  <form className="setting-edit" onSubmit={(e) => void submitQuota(e, user)}>
                    <input
                      type="number"
                      step="0.01"
                      min="0.01"
                      autoFocus
                      value={editingQuota}
                      onChange={(e) => setEditingQuota(e.target.value)}
                      title="新配额（GiB）"
                    />
                    <button type="submit" className="btn small primary" disabled={busy}>保存</button>
                    <button type="button" className="btn small" disabled={busy} onClick={() => setEditingQuotaId(null)}>取消</button>
                  </form>
                ) : (
                  <button
                    className="btn small"
                    disabled={busy}
                    onClick={() => { setEditingQuotaId(user.id); setEditingQuota(quotaToGib(user.storage_quota)) }}
                  >
                    改配额
                  </button>
                )}
                <select
                  value={user.role}
                  disabled={busy || isSelf}
                  title="角色（不可修改本人角色以防误操作）"
                  onChange={(e) => void update(user.id, { role: e.target.value === 'admin' ? 'admin' : 'user' }, `已将 ${user.username} 的角色调整为 ${e.target.value}`)}
                >
                  <option value="user">user</option>
                  <option value="admin">admin</option>
                </select>
                {user.status === 'active' ? (
                  <button
                    className="btn small danger"
                    disabled={busy || isSelf}
                    title={isSelf ? '不可禁用自己的账号' : '禁用并撤销其全部会话'}
                    onClick={() => void update(user.id, { status: 'disabled' }, `已禁用 ${user.username}（其全部会话已失效）`)}
                  >
                    禁用
                  </button>
                ) : (
                  <button
                    className="btn small"
                    disabled={busy}
                    title="启用并解除登录失败锁定"
                    onClick={() => void update(user.id, { status: 'active' }, `已启用 ${user.username}`)}
                  >
                    启用
                  </button>
                )}
                {resetting ? (
                  <form className="setting-edit" onSubmit={(e) => void submitResetPassword(e, user)}>
                    <input
                      type="password"
                      autoFocus
                      required
                      minLength={12}
                      value={newPassword}
                      onChange={(e) => setNewPassword(e.target.value)}
                      placeholder="新密码（≥12 位，含大小写与数字）"
                      autoComplete="new-password"
                    />
                    <button type="submit" className="btn small danger" disabled={busy || newPassword === ''}>确认</button>
                    <button
                      type="button"
                      className="btn small"
                      disabled={busy}
                      onClick={() => { setResetPasswordId(null); setNewPassword('') }}
                    >
                      取消
                    </button>
                  </form>
                ) : (
                  <button
                    className="btn small"
                    disabled={busy}
                    onClick={() => { setResetPasswordId(user.id); setNewPassword('') }}
                  >
                    重置密码
                  </button>
                )}
              </div>
            </div>
          )
        })
      )}
      {pages > 1 && (
        <div className="setting-control" style={{ marginTop: 12 }}>
          <button className="btn small" disabled={busy || page <= 1} onClick={() => void load((page - 2) * pageLimit)}>
            上一页
          </button>
          <span className="setting-desc muted" style={{ margin: '0 8px' }}>
            第 {page} / {pages} 页 · 共 {total} 人
          </span>
          <button className="btn small" disabled={busy || page >= pages} onClick={() => void load(page * pageLimit)}>
            下一页
          </button>
        </div>
      )}
    </div>
  )
}

function AuditPanel({ onError }: { onError: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [action, setAction] = useState('')
  const [userId, setUserId] = useState('')
  const [data, setData] = useState<{ items: import('../api').AuditEntry[]; next_cursor: string; total: number } | null>(null)
  const load = async (cursor = '') => { try { setData(await adminListAuditLogs(action, userId, cursor)) } catch (e) { onError(e instanceof Error ? e.message : msg('auditLoadFailed')) } }
  useEffect(() => { void load() }, [])
  return <div className="panel setting-group"><h3>{msg('auditTitle')}</h3><form className="team-create-row" onSubmit={(e) => { e.preventDefault(); void load() }}><input placeholder="操作类型" value={action} onChange={(e) => setAction(e.target.value)} /><input placeholder="用户 UUID" value={userId} onChange={(e) => setUserId(e.target.value)} /><button className="btn primary">筛选</button><button type="button" className="btn" onClick={() => void adminDownloadAuditCSV(action)}>下载 CSV</button></form>{data && <><div className="setting-meta muted">共 {data.total} 条</div><div className="table-scroll"><table><thead><tr><th>{msg('time')}</th><th>{msg('actionCol')}</th><th>资源</th><th>{msg('status')}</th></tr></thead><tbody>{data.items.map((e) => <tr key={e.id}><td>{formatTime(e.created_at)}</td><td>{e.action}</td><td>{e.resource_type} {e.resource_id}</td><td>{e.status}</td></tr>)}</tbody></table></div>{data.next_cursor && <button className="btn small" onClick={() => void load(data.next_cursor)}>下一页</button>}</>}</div>
}

/** 字节数的人类可读表示（备份文件/总大小展示）。 */
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let v = n
  let i = -1
  do {
    v /= 1024
    i++
  } while (v >= 1024 && i < units.length - 1)
  return `${Math.round(v * 100) / 100} ${units[i]}`
}

/** 备份管理卡片：最近备份状态（时间/是否验证/文件清单）与只读校验；
 * 执行保留 501 说明——服务进程不执行外部命令，由 scripts/backup.sh|ps1
 * 在部署机上完成（加密由运维层负责，如 LUKS/KMS）。
 */
function BackupPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [status, setStatus] = useState<BackupStatus | null>(null)
  const [verifying, setVerifying] = useState(false)
  const [verifyResult, setVerifyResult] = useState<BackupVerifyResult | null>(null)
  const load = async () => {
    try {
      setStatus(await adminGetBackupStatus())
    } catch (e) {
      onError(e instanceof Error ? e.message : msg('backupLoadFailed'))
    }
  }
  useEffect(() => { void load() }, [])
  const run = async () => {
    try {
      await adminRunBackup()
    } catch (e) {
      onNotice(
        e instanceof ApiError && e.status === 501
          ? '服务进程不会执行外部命令或接触密钥：请在部署机上运行 scripts/backup.sh 或 scripts/backup.ps1（支持 --verify 校验）'
          : e instanceof Error
            ? e.message
            : '备份执行失败',
      )
    }
  }
  const verify = async () => {
    if (verifying) return
    setVerifying(true)
    setVerifyResult(null)
    try {
      const r = await adminVerifyBackup()
      setVerifyResult(r)
      if (r.verified) onNotice(`校验通过：最近备份 ${r.files} 个文件 sha256 复核一致`)
      else onError(`校验未通过（${r.files} 个文件）：${r.errors?.join('；') ?? '未知错误'}`)
      // 校验标记写回备份目录后刷新状态（verified 徽章随之更新；目录只读时保持「未验证」）。
      await load()
    } catch (e) {
      onError(e instanceof Error ? e.message : '备份校验失败')
    } finally {
      setVerifying(false)
    }
  }
  const verifiedBadge = () => {
    if (!status || status.verified === null || status.verified === undefined) return <span className="badge">未验证</span>
    return status.verified ? <span className="badge available">已验证</span> : <span className="badge failed">校验未通过</span>
  }
  return (
    <div className="panel setting-group">
      <h3>{msg('backupTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        备份由 scripts/backup.sh 或 scripts/backup.ps1 在部署机执行（PostgreSQL + 对象存储目录 + 脱敏 .env 导出，
        manifest 含每文件 sha256/size）；服务进程不执行命令，此处仅展示状态与只读校验（sha256 复核，不执行恢复）。
        备份产物不加密，请由运维层对备份存储加密（如 LUKS / 云 KMS）。
      </div>
      {!status ? (
        <div className="empty">备份状态加载中…</div>
      ) : !status.enabled ? (
        <div className="empty">未配置 BACKUP_DIR</div>
      ) : !status.last_backup ? (
        <div className="empty">已配置 BACKUP_DIR，尚未发现备份</div>
      ) : (
        <>
          <div className="setting-row">
            <div className="setting-main">
              <div className="setting-key">
                {status.last_backup.name} {verifiedBadge()}
              </div>
              <div className="setting-meta muted">
                备份时间 {formatTime(status.last_backup.timestamp || status.last_backup.modified_at)} · 总大小 {formatBytes(status.last_backup.size)}
                {status.verified_at && ` · 最近校验 ${formatTime(status.verified_at)}`}
              </div>
              <div className="setting-desc muted">
                组件 {(status.last_backup.components ?? []).join(' / ') || '未知'} · 对象存储{' '}
                {status.last_backup.object_store === 'external' ? '外部托管（未打包，运维层负责）' : status.last_backup.object_store || '未知'}
              </div>
            </div>
          </div>
          <div className="table-scroll" style={{ marginBottom: 12 }}>
            <table>
              <thead>
                <tr><th>文件</th><th>类型</th><th>大小</th><th>sha256</th></tr>
              </thead>
              <tbody>
                {status.files.map((f) => (
                  <tr key={f.path}>
                    <td>{f.path}</td>
                    <td>{f.type}</td>
                    <td>{formatBytes(f.size)}</td>
                    <td title={f.sha256}>{f.sha256.slice(0, 16)}…</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
      {verifyResult && !verifyResult.verified && (
        <div className="setting-desc" style={{ marginBottom: 12 }}>
          {verifyResult.errors?.map((err) => (
            <div key={err} className="error-text">{err}</div>
          ))}
        </div>
      )}
      <div className="setting-control">
        <button className="btn" onClick={() => void run()}>运行备份（安全说明）</button>
        <button className="btn primary" disabled={verifying || !status?.enabled} onClick={() => void verify()}>
          {verifying ? '校验中…' : '校验最近备份'}
        </button>
      </div>
    </div>
  )
}

/** 凭据状态卡片（G6）：各密钥类 env 的已配置/未配置徽章（只读探针，不回显值）。 */
function SecretsPanel({ secrets }: { secrets: Record<string, boolean> }) {
  if (!secrets || Object.keys(secrets).length === 0) return null
  return (
    <div className="panel setting-group">
      <h3>凭据状态</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        密钥类凭据一律走环境变量（不入 system_settings）；此处仅展示各环境变量
        是否已配置（值不回显）。未配置的密钥在对应功能启用时将不可用。
      </div>
      {secretLabels.map(({ key, env, label }) => {
        const configured = Boolean(secrets[key])
        return (
          <div key={key} className="setting-row">
            <div className="setting-main">
              <div className="setting-key">
                {label} <code className="setting-desc muted">{env}</code>
              </div>
            </div>
            <div className="setting-control">
              <span className={configured ? 'badge available' : 'badge failed'}>
                {configured ? '已配置' : '未配置'}
              </span>
            </div>
          </div>
        )
      })}
    </div>
  )
}

/** 隔离区卡片（G6）：隔离 blob 列表 + rescan/release/delete 处置。
 * release 须显式勾选确认（服务端校验 confirm=true）；delete 为不可逆删除
 * （解除引用并删对象）。全部动作服务端写审计。
 */
function QuarantinePanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [items, setItems] = useState<QuarantineItem[] | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [confirmRelease, setConfirmRelease] = useState(false)

  const load = async () => {
    try {
      setItems(await adminListQuarantine())
    } catch (err) {
      onError(err instanceof Error ? err.message : '隔离区加载失败')
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const act = async (item: QuarantineItem, action: QuarantineAction) => {
    if (busy !== null) return
    if (action === 'release' && !confirmRelease) {
      onError('解除隔离前请先勾选「我确认该内容为误报」')
      return
    }
    if (action === 'release' && !window.confirm(`确认解除隔离「${item.file_name ?? item.sha256.slice(0, 12)}」？该内容将立即恢复为可下载状态。`)) return
    if (action === 'delete' && !window.confirm(`确认删除隔离对象「${item.file_name ?? item.sha256.slice(0, 12)}」？将解除全部版本引用并物理删除，不可恢复。`)) return
    setBusy(item.sha256 + action)
    onError('')
    try {
      const result = await adminQuarantineAction(item.sha256, action, { confirm: confirmRelease })
      if (action === 'rescan') {
        onNotice(result?.status === 'available' ? '重扫通过：对象已恢复可用' : '重扫未通过：对象仍处于隔离状态')
      } else if (action === 'release') {
        onNotice('已解除隔离（quarantine.release 已审计）')
        setConfirmRelease(false)
      } else {
        onNotice('已删除隔离对象（quarantine.delete 已审计）')
      }
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '操作失败')
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>隔离区</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        安全扫描未通过的内容对象（object_blobs status=quarantined）：重扫（重新入
        扫描，通过恢复可用）、解除隔离（需勾选确认，误报场景）、删除（解除全部版本
        引用并物理删除，不可恢复）。全部操作均记录审计。
      </div>
      {items === null ? (
        <div className="empty">隔离区加载中…</div>
      ) : items.length === 0 ? (
        <div className="empty">当前没有隔离中的内容对象</div>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr><th>文件 / SHA-256</th><th>大小</th><th>引用</th><th>隔离时间</th><th className="col-actions">操作</th></tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={item.sha256}>
                  <td>
                    <div>{item.file_name ?? <span className="muted">（无引用文件）</span>}</div>
                    <div className="setting-desc muted" title={item.sha256}>{item.sha256.slice(0, 16)}… · {item.mime_type}</div>
                  </td>
                  <td className="muted">{formatBytes(item.size)}</td>
                  <td className="muted">{item.ref_count}</td>
                  <td className="muted">{formatTime(item.created_at)}</td>
                  <td className="col-actions">
                    <button className="btn small" disabled={busy !== null} onClick={() => void act(item, 'rescan')}>
                      {busy === item.sha256 + 'rescan' ? '重扫中…' : '重扫'}
                    </button>
                    <button
                      className="btn small"
                      disabled={busy !== null || !confirmRelease}
                      title={confirmRelease ? '解除隔离（须确认）' : '先勾选下方确认框'}
                      onClick={() => void act(item, 'release')}
                    >
                      {busy === item.sha256 + 'release' ? '解除中…' : '解除隔离'}
                    </button>
                    <button className="btn small danger" disabled={busy !== null} onClick={() => void act(item, 'delete')}>
                      {busy === item.sha256 + 'delete' ? '删除中…' : '删除'}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <label className="setting-bool" style={{ marginTop: 8 }}>
        <input type="checkbox" checked={confirmRelease} onChange={(e) => setConfirmRelease(e.target.checked)} />
        <span>我确认该内容为误报，解除隔离后允许下载</span>
      </label>
    </div>
  )
}

export default function AdminPage() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [stats, setStats] = useState<AdminStats | null>(null)
  const [settings, setSettings] = useState<SettingItem[]>([])
  const [secrets, setSecrets] = useState<Record<string, boolean>>({})
  const [loading, setLoading] = useState(true)
  const [forbidden, setForbidden] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  // 行内编辑状态：当前编辑键 + 草稿（bool 直接存布尔，int/string 存字符串）。
  const [editingKey, setEditingKey] = useState<string | null>(null)
  const [draft, setDraft] = useState<string | boolean>('')
  const [savingKey, setSavingKey] = useState<string | null>(null)
  const [rowError, setRowError] = useState('')

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      const [result, st] = await Promise.all([adminGetSettings(), adminGetStats()])
      setSettings(result.settings ?? [])
      setSecrets(result.secrets ?? {})
      setStats(st)
      setForbidden(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        setForbidden(true)
        setError('')
      } else {
        setError(err instanceof Error ? err.message : msg('loadFailed'))
      }
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  /** 按键前缀分组（保持 Definitions 输出顺序）。 */
  const groups = useMemo(() => {
    const map = new Map<string, SettingItem[]>()
    for (const item of settings) {
      const prefix = item.key.split('.')[0]
      const list = map.get(prefix) ?? []
      list.push(item)
      map.set(prefix, list)
    }
    return Array.from(map.entries())
  }, [settings])

  const startEdit = (item: SettingItem) => {
    setEditingKey(item.key)
    setDraft(item.type === 'bool' ? Boolean(item.value) : String(item.value))
    setRowError('')
    setNotice('')
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
        setRowError('请输入整数')
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
      setNotice(`已保存 ${item.key}（当前值：${String(normalized)}）`)
      // 保存成功后刷新设置列表（含 updated_at/updated_by 与服务端归一化结果）。
      try {
        const refreshed = await adminGetSettings()
        setSettings(refreshed.settings ?? [])
        setSecrets(refreshed.secrets ?? {})
      } catch {
        // 列表刷新失败不打断，保留本地已保存状态
      }
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setRowError('无权限')
      else setRowError(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSavingKey(null)
    }
  }

  if (forbidden) {
    return (
      <div className="page">
        <div className="page-head">
          <h2>{msg('adminTitle')}</h2>
        </div>
        <div className="admin-forbidden">
          <h3>{msg('adminForbiddenTitle')}</h3>
          <p>{msg('adminForbiddenBody')}</p>
        </div>
      </div>
    )
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('adminTitle')}</h2>
        <button className="btn ghost" onClick={() => { setNotice(''); void load() }}>{msg('refresh')}</button>
      </div>

      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}

      {!loading && stats && (
        <div className="stats-grid">
          {statCards.map(({ key, label }) => (
            <div key={key} className="stat-card">
              <div className="stat-value">{stats[key]}</div>
              <div className="stat-label">{label}</div>
            </div>
          ))}
        </div>
      )}

      {!loading && !forbidden && (
        <UsersPanel
          onError={(msg) => { setError(msg); setNotice('') }}
          onNotice={(msg) => { setNotice(msg); setError('') }}
        />
      )}

      {!loading && !forbidden && <AuditPanel onError={(msg) => { setError(msg); setNotice('') }} />}
      {!loading && !forbidden && <BackupPanel onError={(msg) => { setError(msg); setNotice('') }} onNotice={(msg) => { setNotice(msg); setError('') }} />}
      {!loading && !forbidden && <QuarantinePanel onError={(msg) => { setError(msg); setNotice('') }} onNotice={(msg) => { setNotice(msg); setError('') }} />}
      {!loading && !forbidden && <SecretsPanel secrets={secrets} />}

      {!loading && !forbidden && (
        <InvitationsPanel
          onError={(msg) => { setError(msg); setNotice('') }}
          onNotice={(msg) => { setNotice(msg); setError('') }}
        />
      )}

      {!loading && groups.map(([prefix, items]) => (
        <div key={prefix} className="panel setting-group">
          <h3>{groupTitles[prefix] ?? prefix}</h3>
          {items.map((item) => {
            const editing = editingKey === item.key
            return (
              <div key={item.key} className="setting-row">
                <div className="setting-main">
                  <div className="setting-key">
                    {item.key}
                    {item.effect && (
                      <span className="badge" style={{ marginLeft: 8 }} title="变更生效方式">
                        {effectText[item.effect] ?? item.effect}
                      </span>
                    )}
                  </div>
                  <div className="setting-desc muted">{item.description}</div>
                  <div className="setting-meta muted">
                    类型 {typeText[item.type]} · 默认值 {formatSettingValue(item.default)}
                    {item.updated_at && ` · 更新于 ${formatTime(item.updated_at)}`}
                  </div>
                </div>
                <div className="setting-control">
                  {editing ? (
                    <form className="setting-edit" onSubmit={(e) => void handleSave(e, item)}>
                      {item.type === 'bool' ? (
                        <label className="setting-bool">
                          <input
                            type="checkbox"
                            checked={Boolean(draft)}
                            onChange={(e) => setDraft(e.target.checked)}
                          />
                          <span>{draft ? '开启' : '关闭'}</span>
                        </label>
                      ) : item.type === 'int' ? (
                        <input
                          type="number"
                          step={1}
                          autoFocus
                          value={String(draft)}
                          onChange={(e) => setDraft(e.target.value)}
                        />
                      ) : (
                        <input
                          type="text"
                          autoFocus
                          value={String(draft)}
                          onChange={(e) => setDraft(e.target.value)}
                        />
                      )}
                      <button
                        type="submit"
                        className="btn small primary"
                        disabled={savingKey !== null || (item.type === 'string' && String(draft).trim() === '')}
                      >
                        {savingKey === item.key ? '保存中…' : '保存'}
                      </button>
                      <button
                        type="button"
                        className="btn small"
                        disabled={savingKey !== null}
                        onClick={() => { setEditingKey(null); setRowError('') }}
                      >
                        取消
                      </button>
                    </form>
                  ) : (
                    <>
                      <span className={item.type === 'bool' ? `badge ${item.value ? 'available' : 'failed'}` : 'setting-value-mono'}>
                        {item.type === 'bool' ? (item.value ? '开启' : '关闭') : formatSettingValue(item.value)}
                      </span>
                      <button className="btn small" onClick={() => startEdit(item)}>编辑</button>
                    </>
                  )}
                </div>
                {editing && rowError && <div className="error-text setting-row-error">{rowError}</div>}
              </div>
            )
          })}
        </div>
      ))}
    </div>
  )
}
