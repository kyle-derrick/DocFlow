// 管理设置页（仅 admin 角色）：顶部五类计数概览 + 按前缀分组的设置卡片，
// 行内编辑保存（bool 开关 / int 数字 / string 文本，按 value type 渲染）。
// 另含邀请管理卡片：创建邀请（一次性注册链接展示/复制）、列表状态与撤销。
// 非 admin（403）显示无权限页；保存成功后刷新设置列表并提示。
import { FormEvent, useEffect, useMemo, useState } from 'react'
import {
  AdminStats,
  ApiError,
  Invitation,
  SettingItem,
  SettingType,
  SettingValue,
  adminCreateInvitation,
  adminGetSettings,
  adminGetStats,
  adminListInvitations,
  adminPutSetting,
  adminRevokeInvitation,
} from '../api'
import { formatTime } from '../components/FileBrowser'

/** 设置键前缀 → 分组标题（未知前缀回退原样）。 */
const groupTitles: Record<string, string> = {
  site: '站点',
  upload: '上传',
  share: '分享',
  retention: '保留策略',
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
