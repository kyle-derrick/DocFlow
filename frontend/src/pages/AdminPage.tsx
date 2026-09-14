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
  Invitation,
  SettingItem,
  SettingType,
  SettingValue,
  adminCreateInvitation,
  adminDownloadAuditCSV,
  adminGetBackupStatus,
  adminGetSettings,
  adminListAuditLogs,
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
      onError(err instanceof Error ? err.message : '邀请列表加载失败')
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
      onError(err instanceof Error ? err.message : '创建邀请失败')
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
      <h3>邀请管理</h3>
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
        <div className="empty">暂无邀请记录</div>
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
      onError(err instanceof Error ? err.message : '用户列表加载失败')
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
      <h3>用户管理</h3>
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
        <div className="hint">加载中…</div>
      ) : users.length === 0 ? (
        <div className="empty">没有匹配的用户</div>
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
  const [action, setAction] = useState('')
  const [userId, setUserId] = useState('')
  const [data, setData] = useState<{ items: import('../api').AuditEntry[]; next_cursor: string; total: number } | null>(null)
  const load = async (cursor = '') => { try { setData(await adminListAuditLogs(action, userId, cursor)) } catch (e) { onError(e instanceof Error ? e.message : '审计日志加载失败') } }
  useEffect(() => { void load() }, [])
  return <div className="panel setting-group"><h3>审计日志</h3><form className="team-create-row" onSubmit={(e) => { e.preventDefault(); void load() }}><input placeholder="操作类型" value={action} onChange={(e) => setAction(e.target.value)} /><input placeholder="用户 UUID" value={userId} onChange={(e) => setUserId(e.target.value)} /><button className="btn primary">筛选</button><button type="button" className="btn" onClick={() => void adminDownloadAuditCSV(action)}>下载 CSV</button></form>{data && <><div className="setting-meta muted">共 {data.total} 条</div><div className="table-scroll"><table><thead><tr><th>时间</th><th>操作</th><th>资源</th><th>状态</th></tr></thead><tbody>{data.items.map((e) => <tr key={e.id}><td>{formatTime(e.created_at)}</td><td>{e.action}</td><td>{e.resource_type} {e.resource_id}</td><td>{e.status}</td></tr>)}</tbody></table></div>{data.next_cursor && <button className="btn small" onClick={() => void load(data.next_cursor)}>下一页</button>}</>}</div>
}

function BackupPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [status, setStatus] = useState<import('../api').BackupStatus | null>(null)
  useEffect(() => { void adminGetBackupStatus().then(setStatus).catch((e) => onError(e instanceof Error ? e.message : '备份状态加载失败')) }, [])
  const run = async () => { try { await adminRunBackup() } catch (e) { onNotice(e instanceof ApiError && e.status === 501 ? '为避免服务进程执行系统命令，备份请通过 scripts/backup.sh 或 backup.ps1 在受控环境运行' : (e instanceof Error ? e.message : '备份执行失败')) } }
  return <div className="panel setting-group"><h3>备份</h3><p className="setting-desc muted">服务端不会执行命令；状态仅扫描 BACKUP_DIR 中最近的清单。请在受控运维环境运行备份脚本。</p>{status?.latest ? <div className="setting-meta">最近备份：{status.latest.name} · {formatTime(status.latest.modified_at)}</div> : <div className="setting-meta muted">{status?.configured ? '未发现备份' : '未配置 BACKUP_DIR'}</div>}<button className="btn" onClick={() => void run()}>运行备份（安全说明）</button></div>
}

export default function AdminPage() {
  const [stats, setStats] = useState<AdminStats | null>(null)
  const [settings, setSettings] = useState<SettingItem[]>([])
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
      const [list, st] = await Promise.all([adminGetSettings(), adminGetStats()])
      setSettings(list)
      setStats(st)
      setForbidden(false)
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) {
        setForbidden(true)
        setError('')
      } else {
        setError(err instanceof Error ? err.message : '加载失败')
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
        setSettings(await adminGetSettings())
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
          <h2>管理设置</h2>
        </div>
        <div className="admin-forbidden">
          <h3>无访问权限</h3>
          <p>该页面仅系统管理员（admin 角色）可访问。</p>
        </div>
      </div>
    )
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>管理设置</h2>
        <button className="btn ghost" onClick={() => { setNotice(''); void load() }}>刷新</button>
      </div>

      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">加载中…</div>}

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
                  <div className="setting-key">{item.key}</div>
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
