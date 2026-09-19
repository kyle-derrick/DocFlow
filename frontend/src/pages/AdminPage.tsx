// 管理设置页（仅 admin 角色）：顶部五类计数概览 + 按前缀分组的设置卡片，
// 行内编辑保存（bool 开关 / int 数字 / string 文本，按 value type 渲染）。
// 另含邀请管理与用户管理卡片：邀请创建（一次性注册链接展示/复制）、列表
// 状态与撤销；用户列表（q 前缀检索 + 分页）、禁用/启用（立即撤销其全部
// 会话）、改配额、改角色与重置密码（C6；删除以软禁用替代，见 openapi 取舍）。
// 非 admin（403）显示无权限页；保存成功后刷新列表并提示。
import { FormEvent, useEffect, useMemo, useState } from 'react'
import { NavLink, Navigate, useNavigate, useParams } from 'react-router-dom'
import { App as AntdApp, Button, Card, Checkbox, Input, InputNumber, Radio, Select, Switch, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  AdminStats,
  AdminUser,
  ApiError,
  AuditEntry,
  BackupStatus,
  BackupVerifyResult,
  Group,
  GroupMember,
  Invitation,
  QuarantineAction,
  QuarantineItem,
  SettingItem,
  SettingType,
  SettingValue,
  SmtpSettingsView,
  TlsCert,
  TlsMode,
  TlsStatus,
  UserSearchResult,
  adminAddGroupMember,
  adminCreateGroup,
  adminCreateInvitation,
  adminDeleteGroup,
  adminDownloadAuditCSV,
  adminGetBackupStatus,
  adminVerifyBackup,
  adminGetSettings,
  adminGetSmtpSettings,
  adminGetTls,
  adminListAuditLogs,
  adminListGroupMembers,
  adminListGroups,
  adminListQuarantine,
  adminPutTls,
  adminPutSmtpSettings,
  adminQuarantineAction,
  adminRemoveGroupMember,
  adminRunBackup,
  adminUpdateGroup,
  adminUploadTlsCert,
  adminGetStats,
  adminListInvitations,
  adminListUsers,
  adminPutSetting,
  adminResetUserPassword,
  adminRevokeInvitation,
  adminUpdateUser,
  currentUserId,
  searchUsers,
} from '../api'
import { Modal, confirmDialog, formatTime } from '../components/FileBrowser'
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

/** 概览统计卡片（key 对应 AdminStats 字段；format 缺省为数字直显）。 */
const statCards: Array<{ key: keyof AdminStats; label: string; format?: (v: number) => string }> = [
  { key: 'users', label: '用户' },
  { key: 'files', label: '文件' },
  { key: 'storage_bytes', label: '存储用量', format: formatBytes },
  { key: 'teams', label: '团队' },
  { key: 'groups', label: '用户组' },
  { key: 'shares', label: '分享' },
  { key: 'uploads', label: '上传会话' },
  { key: 'sessions', label: '登录会话' },
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
function TlsPanel({ onNotice }: { onNotice: (msg: string) => void }) {
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

  const current = tlsModeOptions.find((o) => o.value === mode)
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
          {current && mode !== 'http' && (
            <div className="setting-desc muted">
              {mode === 'auto'
                ? '要求：域名公网 DNS 指向本机、80/443 端口可达（ACME 挑战与重定向）。'
                : mode === 'internal'
                  ? '自签证书：浏览器将提示不受信（可信任导入 Caddy 根证书消除）；切换后请用 https:// 访问。'
                  : '使用已上传的自定义证书（下方上传区）；私钥仅存服务器（权限 600），不经数据库。'}
            </div>
          )}
          {/* 自定义证书：仅 custom（自定义证书）模式显示上传区——auto/internal
              无需用户证书；已上传过证书（status.cert 存在）时也显示摘要供查看。 */}
          {(mode === 'custom' || status.cert) && (
          <div className="tls-cert-box">
            <div className="setting-key">自定义证书（custom 模式）</div>
            <TlsCertInfo cert={status.cert} />
            {mode === 'custom' && (
            <div className="tls-cert-upload">
              <label className="field">
                <span>证书 PEM（.pem / .crt，可含中间证书链）</span>
                <input
                  type="file"
                  accept=".pem,.crt,.cer,text/plain"
                  onChange={(e) => setCertFile(e.target.files?.[0] ?? null)}
                />
              </label>
              <label className="field">
                <span>私钥 PEM（.key / .pem）</span>
                <input
                  type="file"
                  accept=".key,.pem,text/plain"
                  onChange={(e) => setKeyFile(e.target.files?.[0] ?? null)}
                />
              </label>
              <Button
                size="small"
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
          <Input readOnly value={oneTimeLink} onFocus={(e) => e.currentTarget.select()} />
          <Button size="small" onClick={() => void copyLink()}>
            {copied ? '已复制' : '复制链接'}
          </Button>
        </div>
      )}
      {oneTimeLink && <div className="setting-desc muted" style={{ marginBottom: 12 }}>该注册链接仅显示这一次，请立即复制发送给被邀请人（7 天内有效，仅可使用一次）</div>}
      <form className="team-create-row" onSubmit={(e) => void submit(e)}>
        <label className="field">
          <span>邮箱</span>
          <Input
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="teammate@example.com"
          />
        </label>
        <label className="field">
          <span>角色</span>
          <Select
            value={role}
            onChange={(v) => setRole(v === 'admin' ? 'admin' : 'user')}
            options={[
              { value: 'user', label: 'user' },
              { value: 'admin', label: 'admin' },
            ]}
          />
        </label>
        <Button type="primary" htmlType="submit" disabled={busy}>
          {busy ? '创建中…' : '创建邀请'}
        </Button>
      </form>
      <Table<Invitation>
        rowKey="id"
        dataSource={invitations}
        pagination={false}
        locale={{ emptyText: msg('noInvites') }}
        columns={[
          {
            title: '邮箱',
            dataIndex: 'email',
            key: 'email',
            render: (email: string, inv) => (
              <div>
                <div>{email}</div>
                {inv.accepted_at && <div className="setting-meta muted">已于 {formatTime(inv.accepted_at)} 接受</div>}
              </div>
            ),
          },
          { title: '角色', dataIndex: 'role', key: 'role', width: 80 },
          {
            title: '状态',
            key: 'status',
            width: 100,
            render: (_, inv) => <span className={invitationStatusBadge[inv.status]}>{invitationStatusText[inv.status]}</span>,
          },
          { title: '创建时间', key: 'created_at', width: 160, render: (_, inv) => formatTime(inv.created_at) },
          { title: '有效期至', key: 'expires_at', width: 160, render: (_, inv) => formatTime(inv.expires_at) },
          {
            title: '操作',
            key: 'actions',
            width: 90,
            render: (_, inv) =>
              inv.status === 'pending' ? (
                <Button size="small" danger disabled={revoking !== null} loading={revoking === inv.id} onClick={() => void revoke(inv.id)}>
                  撤销
                </Button>
              ) : null,
          },
        ]}
      />
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

/** 用户管理卡片（C6）：检索/分页列表 + 编辑弹窗（昵称/角色/状态/配额）+
 * 组归属管理 + 禁用/启用 + 重置密码。 */
function UsersPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [users, setUsers] = useState<AdminUser[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  const [total, setTotal] = useState(0)
  const [offset, setOffset] = useState(0)
  const [q, setQ] = useState('')
  const [query, setQuery] = useState('')
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  /** 弹窗状态：编辑用户 / 用户组归属 / 重置密码弹窗。 */
  const [editingUser, setEditingUser] = useState<AdminUser | null>(null)
  const [groupsUser, setGroupsUser] = useState<AdminUser | null>(null)
  const [resetUser, setResetUser] = useState<AdminUser | null>(null)
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

  const loadGroups = async () => {
    try {
      setGroups(await adminListGroups())
    } catch {
      // 组列表加载失败不阻塞用户列表（组操作入口仍可用，弹窗内重试）。
      setGroups([])
    }
  }

  useEffect(() => {
    void load(0, '')
    void loadGroups()
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

  /** 弹窗操作完成后的统一刷新（用户列表聚合 group_names + 组成员计数）。 */
  const refreshAll = async () => {
    await Promise.all([load(), loadGroups()])
  }

  const submitResetPassword = async (e: FormEvent, user: AdminUser) => {
    e.preventDefault()
    if (!resetUser || resetUser.id !== user.id || newPassword === '') return
    if (busy) return
    setBusy(true)
    onError('')
    try {
      await adminResetUserPassword(user.id, newPassword)
      setResetUser(null)
      setNewPassword('')
      onNotice(`已重置 ${user.username} 的密码（其全部登录会话已失效）`)
    } catch (err) {
      onError(err instanceof Error ? err.message : '重置密码失败')
    } finally {
      setBusy(false)
    }
  }

  const page = Math.floor(offset / pageLimit) + 1

  return (
    <div className="panel setting-group">
      <h3>{msg('usersTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        检索（用户名/邮箱/昵称前缀）与分页浏览全部用户；「编辑」弹窗可改昵称/角色/状态/配额，
        「组」管理其用户组归属。禁用账号立即撤销其全部登录会话，启用同时解除登录失败锁定；
        账号不支持删除——以禁用替代（保留其名下文件与审计记录）。
      </div>
      <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={(e) => void submitSearch(e)}>
        <label className="field">
          <span>检索（用户名 / 邮箱 / 昵称前缀）</span>
          <Input
            allowClear
            autoCapitalize="none"
            spellCheck={false}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="如：alice、alice@ 或昵称前缀"
          />
        </label>
        <Button type="primary" htmlType="submit">搜索</Button>
        {query && (
          <Button onClick={() => { setQ(''); setQuery(''); void load(0, '') }}>清除</Button>
        )}
      </form>
      <Table<AdminUser>
        rowKey="id"
        loading={loading}
        dataSource={users}
        scroll={{ x: 980 }}
        locale={{ emptyText: msg('noUsers') }}
        pagination={{
          total,
          current: page,
          pageSize: pageLimit,
          showSizeChanger: false,
          showTotal: (t) => `共 ${t} 人`,
          onChange: (p) => void load((p - 1) * pageLimit),
        }}
        columns={[
          {
            title: '用户',
            key: 'user',
            width: 240,
            render: (_, user) => (
              <div>
                <div>
                  {user.profile.nickname || user.username}
                  {user.id === selfId && <span className="badge" style={{ marginLeft: 6 }}>本人</span>}
                </div>
                <div className="setting-desc muted" title={user.email}>{user.username} · {user.email}</div>
                {(user.group_names?.length ?? 0) > 0 && (
                  <div style={{ marginTop: 2 }}>
                    {user.group_names!.map((name) => (
                      <span key={name} className="badge" style={{ marginLeft: 4 }} title="所属用户组">{name}</span>
                    ))}
                  </div>
                )}
              </div>
            ),
          },
          {
            title: '状态',
            key: 'status',
            width: 90,
            render: (_, user) => <span className={userStatusBadge[user.status]}>{userStatusText[user.status]}</span>,
          },
          {
            title: '角色',
            dataIndex: 'role',
            key: 'role',
            width: 80,
            render: (role: string) => <span className={`badge ${role === 'admin' ? 'role-owner' : ''}`}>{role}</span>,
          },
          {
            title: '存储用量',
            key: 'storage',
            width: 190,
            render: (_, user) => {
              const used = user.storage_used ?? 0
              const quotaPercent = user.storage_quota > 0 ? Math.min(100, Math.round((used * 100) / user.storage_quota)) : 0
              return (
                <span className="muted">
                  {formatBytes(used)} / {quotaToGib(user.storage_quota)} GiB（{quotaPercent}%）
                  {user.locked_until && ` · 锁定至 ${formatTime(user.locked_until)}`}
                </span>
              )
            },
          },
          {
            title: '注册时间',
            key: 'created_at',
            width: 150,
            render: (_, user) => <span className="muted">{formatTime(user.created_at)}</span>,
          },
          {
            title: '操作',
            key: 'actions',
            width: 270,
            fixed: 'right',
            render: (_, user) => {
              const isSelf = user.id === selfId
              return (
                <div className="table-actions">
                  <Button
                    size="small"
                    disabled={busy}
                    title="编辑昵称 / 角色 / 状态 / 配额"
                    onClick={() => setEditingUser(user)}
                  >
                    编辑
                  </Button>
                  <Button
                    size="small"
                    disabled={busy}
                    title="管理所属用户组（加入 / 移出）"
                    onClick={() => setGroupsUser(user)}
                  >
                    组
                  </Button>
                  {user.status === 'active' ? (
                    <Button
                      size="small"
                      danger
                      disabled={busy || isSelf}
                      title={isSelf ? '不可禁用自己的账号' : '禁用并撤销其全部会话'}
                      onClick={() => void update(user.id, { status: 'disabled' }, `已禁用 ${user.username}（其全部会话已失效）`)}
                    >
                      禁用
                    </Button>
                  ) : (
                    <Button
                      size="small"
                      disabled={busy}
                      title="启用并解除登录失败锁定"
                      onClick={() => void update(user.id, { status: 'active' }, `已启用 ${user.username}`)}
                    >
                      启用
                    </Button>
                  )}
                  <Button
                    size="small"
                    disabled={busy}
                    onClick={() => { setResetUser(user); setNewPassword('') }}
                  >
                    重置密码
                  </Button>
                </div>
              )
            },
          },
        ]}
      />
      {resetUser && (
        <Modal title={`重置密码：${resetUser.profile.nickname || resetUser.username}`} onClose={() => { setResetUser(null); setNewPassword('') }}>
          <form className="setting-edit" style={{ flexDirection: 'column', alignItems: 'stretch', gap: 12 }} onSubmit={(e) => void submitResetPassword(e, resetUser)}>
            <label className="field">
              <span>新密码（≥12 位，含大小写与数字；重置后其全部登录会话立即失效）</span>
              <Input.Password
                autoFocus
                required
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                placeholder="新密码（≥12 位，含大小写与数字）"
                autoComplete="new-password"
              />
            </label>
            <div className="setting-control" style={{ justifyContent: 'flex-end' }}>
              <Button type="primary" size="small" danger htmlType="submit" disabled={busy || newPassword === ''}>确认重置</Button>
              <Button size="small" disabled={busy} onClick={() => { setResetUser(null); setNewPassword('') }}>取消</Button>
            </div>
          </form>
        </Modal>
      )}
      {editingUser && (
        <UserEditModal
          user={editingUser}
          groups={groups}
          onClose={() => setEditingUser(null)}
          onError={onError}
          onNotice={onNotice}
          onChanged={refreshAll}
        />
      )}
      {groupsUser && (
        <UserGroupsModal
          user={groupsUser}
          groups={groups}
          onClose={() => setGroupsUser(null)}
          onError={onError}
          onNotice={onNotice}
          onChanged={refreshAll}
        />
      )}
    </div>
  )
}

function AuditPanel({ onError }: { onError: (msg: string) => void }) {
  const [filters, setFilters] = useState<import('../api').AuditFilters>({})
  const [data, setData] = useState<import('../api').AuditListResult | null>(null)
  const [history, setHistory] = useState<string[]>([])
  const [expanded, setExpanded] = useState<number[]>([])
  const load = async (cursor = '', push = false) => {
    try { setData(await adminListAuditLogs(filters, cursor)); if (push) setHistory((h) => [...h, cursor]) }
    catch (e) { onError(e instanceof Error ? e.message : '审计日志加载失败') }
  }
  useEffect(() => { void load() }, [])
  const field = (key: keyof import('../api').AuditFilters, placeholder: string, type = 'text') => (
    <Input
      type={type}
      placeholder={placeholder}
      allowClear
      value={filters[key] ?? ''}
      onChange={(e) => setFilters({ ...filters, [key]: e.target.value })}
    />
  )
  const columns: ColumnsType<AuditEntry> = [
    { title: '用户', dataIndex: 'user_id', key: 'user_id', width: 150, render: (v: string) => v || '系统/匿名' },
    { title: 'IP', dataIndex: 'ip', key: 'ip', width: 130, render: (v: string) => v || '-' },
    { title: '操作', dataIndex: 'action', key: 'action', width: 170 },
    { title: '资源', key: 'resource', render: (_, entry) => `${entry.resource_type} ${entry.resource_id}` },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 90,
      render: (v: string) => <span className={`badge ${v === 'success' ? 'available' : 'failed'}`}>{v}</span>,
    },
    { title: '时间', key: 'created_at', width: 160, render: (_, entry) => <span className="muted">{formatTime(entry.created_at)}</span> },
  ]
  return (
    <div className="panel setting-group audit-panel">
      <h3>审计日志</h3>
      <form className="audit-filters" onSubmit={(e) => { e.preventDefault(); setHistory([]); void load() }}>
        {field('action', 'Action')}
        {field('userId', '用户 UUID')}
        <Select
          allowClear
          placeholder="全部状态"
          value={filters.status || undefined}
          onChange={(v) => setFilters({ ...filters, status: v ?? '' })}
          options={[
            { value: 'success', label: 'success' },
            { value: 'failure', label: 'failure' },
          ]}
        />
        {field('resourceType', '资源类型')}
        {field('resourceId', '资源 ID')}
        {field('from', '开始时间', 'datetime-local')}
        {field('to', '结束时间', 'datetime-local')}
        <Button type="primary" htmlType="submit">筛选</Button>
        <Button onClick={() => void adminDownloadAuditCSV(filters)}>按当前筛选导出 CSV</Button>
      </form>
      {data && (
        <>
          <div className="setting-meta muted">共 {data.total} 条</div>
          <Table<AuditEntry>
            rowKey="id"
            columns={columns}
            dataSource={data.items}
            pagination={false}
            scroll={{ x: 900 }}
            expandable={{
              expandedRowKeys: expanded,
              onExpand: (open, entry) => setExpanded(open ? [entry.id] : []),
              expandedRowRender: (entry) => (
                <div>
                  <strong>User-Agent</strong>
                  <pre className="audit-detail-pre">{entry.user_agent || '-'}</pre>
                  <strong>Metadata</strong>
                  <pre className="audit-detail-pre">{entry.metadata || '{}'}</pre>
                </div>
              ),
            }}
            onRow={(entry) => ({
              onClick: () => setExpanded((keys) => (keys.includes(entry.id) ? keys.filter((k) => k !== entry.id) : [entry.id])),
              style: { cursor: 'pointer' },
            })}
          />
          <div className="pager">
            <Button size="small" disabled={history.length === 0} onClick={() => { const next = history.slice(0, -1); setHistory(next); void load(next[next.length - 1] ?? '') }}>上一页</Button>
            <Button size="small" disabled={!data.next_cursor} onClick={() => void load(data.next_cursor, true)}>下一页</Button>
          </div>
        </>
      )}
    </div>
  )
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
          <Table<(typeof status.files)[number]>
            rowKey="path"
            style={{ marginBottom: 12 }}
            pagination={false}
            dataSource={status.files}
            columns={[
              { title: '文件', dataIndex: 'path', key: 'path' },
              { title: '类型', dataIndex: 'type', key: 'type', width: 100 },
              { title: '大小', key: 'size', width: 110, render: (_, f) => formatBytes(f.size) },
              { title: 'sha256', key: 'sha256', width: 160, render: (_, f) => <span title={f.sha256} className="setting-value-mono">{f.sha256.slice(0, 16)}…</span> },
            ]}
          />
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
        <Button onClick={() => void run()}>运行备份（安全说明）</Button>
        <Button type="primary" disabled={verifying || !status?.enabled} loading={verifying} onClick={() => void verify()}>
          {verifying ? '校验中…' : '校验最近备份'}
        </Button>
      </div>
    </div>
  )
}

/** 邮件（SMTP）卡片：可编辑表单。GET/PUT /admin/settings/smtp —— 后端已
 * 支持运行时修改（DB 覆盖 → env 回退合并，保存即时生效：邮件发送处每次
 * 读库）；pass 留空 = 保持现值（任何读路径不回显，仅报 configured），
 * env 基线（.env 部署值）作对照展示，PUBLIC_BASE_URL 仍为 env-only。 */
function MailPanel({ onNotice, onError }: { onNotice: (m: string) => void; onError: (m: string) => void }) {
  const [view, setView] = useState<SmtpSettingsView | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')
  const [form, setForm] = useState<{ enabled: boolean; host: string; port: number | null; user: string; pass: string; from: string; tls_mode: string }>({
    enabled: false, host: '', port: 587, user: '', pass: '', from: '', tls_mode: 'auto',
  })

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

  if (loading) {
    return (
      <div className="panel setting-group">
        <h3>邮件（SMTP）</h3>
        <div className="empty">SMTP 配置加载中…</div>
      </div>
    )
  }

  return (
    <div className="panel setting-group">
      <h3>邮件（SMTP）</h3>
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
  const { modal } = AntdApp.useApp()
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
    const label = item.file_name ?? item.sha256.slice(0, 12)
    if (action === 'release') {
      const ok = await confirmDialog(modal, {
        title: '解除隔离',
        content: `确认解除隔离「${label}」？该内容将立即恢复为可下载状态。`,
        okText: '解除隔离',
        cancelText: '取消',
      })
      if (!ok) return
    }
    if (action === 'delete') {
      const ok = await confirmDialog(modal, {
        title: '删除隔离对象',
        content: `确认删除隔离对象「${label}」？将解除全部版本引用并物理删除，不可恢复。`,
        okText: '删除',
        danger: true,
        cancelText: '取消',
      })
      if (!ok) return
    }
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
        <Table<QuarantineItem>
          rowKey="sha256"
          pagination={false}
          dataSource={items}
          scroll={{ x: 900 }}
          columns={[
            {
              title: '文件 / SHA-256',
              key: 'object',
              render: (_, item) => (
                <div>
                  <div>{item.file_name ?? <span className="muted">（无引用文件）</span>}</div>
                  <div className="setting-desc muted" title={item.sha256}>{item.sha256.slice(0, 16)}… · {item.mime_type}</div>
                </div>
              ),
            },
            { title: '大小', key: 'size', width: 100, render: (_, item) => <span className="muted">{formatBytes(item.size)}</span> },
            { title: '引用', dataIndex: 'ref_count', key: 'ref_count', width: 70 },
            { title: '隔离时间', key: 'created_at', width: 150, render: (_, item) => <span className="muted">{formatTime(item.created_at)}</span> },
            {
              title: '操作',
              key: 'actions',
              width: 250,
              render: (_, item) => (
                <div className="table-actions">
                  <Button size="small" disabled={busy !== null} loading={busy === item.sha256 + 'rescan'} onClick={() => void act(item, 'rescan')}>
                    重扫
                  </Button>
                  <Button
                    size="small"
                    disabled={busy !== null || !confirmRelease}
                    title={confirmRelease ? '解除隔离（须确认）' : '先勾选下方确认框'}
                    loading={busy === item.sha256 + 'release'}
                    onClick={() => void act(item, 'release')}
                  >
                    解除隔离
                  </Button>
                  <Button size="small" danger disabled={busy !== null} loading={busy === item.sha256 + 'delete'} onClick={() => void act(item, 'delete')}>
                    删除
                  </Button>
                </div>
              ),
            },
          ]}
        />
      )}
      <Checkbox
        checked={confirmRelease}
        onChange={(e) => setConfirmRelease(e.target.checked)}
        style={{ marginTop: 8 }}
      >
        我确认该内容为误报，解除隔离后允许下载
      </Checkbox>
    </div>
  )
}

/** 成员选择器的展示名（昵称优先，回退 username）。 */
function searchResultLabel(user: UserSearchResult): string {
  return (user.nickname ?? user.profile?.nickname) || user.username
}

/** 用户搜索选择器（复用 /users/search，≥2 字符防误触全量枚举）：
 * antd Select 远程搜索模式（300ms 防抖检索见 effect）；选中后向上回调 user 对象。 */
function UserPickerField({ onSelect }: { onSelect: (user: UserSearchResult) => void }) {
  const [query, setQuery] = useState('')
  const [options, setOptions] = useState<UserSearchResult[]>([])
  const [searching, setSearching] = useState(false)
  const [picked, setPicked] = useState<UserSearchResult | null>(null)

  useEffect(() => {
    const q = query.trim()
    if (picked && searchResultLabel(picked) === q) return
    setPicked(null)
    if (q.length < 2) {
      setOptions([])
      return
    }
    setSearching(true)
    const timer = window.setTimeout(() => {
      void searchUsers(q)
        .then(setOptions)
        .catch(() => setOptions([]))
        .finally(() => setSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [query, picked])

  return (
    <div className="field">
      <span>用户（昵称 / 用户名 / 邮箱，至少 2 字）</span>
      <Select
        className="user-search-select"
        showSearch
        allowClear
        filterOption={false}
        value={picked ? picked.id : undefined}
        searchValue={query}
        loading={searching}
        placeholder="输入昵称、用户名或邮箱检索"
        notFoundContent={searching ? '搜索中…' : null}
        onSearch={(v) => setQuery(v)}
        onClear={() => {
          setQuery('')
          setPicked(null)
          setOptions([])
        }}
        onSelect={(value) => {
          const user = options.find((u) => u.id === value)
          if (!user) return
          setPicked(user)
          setQuery(searchResultLabel(user))
          setOptions([])
          onSelect(user)
        }}
        options={options.map((user) => ({
          value: user.id,
          label: (
            <span className="user-search-option">
              <strong>{searchResultLabel(user)}</strong>
              <span className="muted">{user.username} · {user.email}</span>
            </span>
          ),
        }))}
      />
    </div>
  )
}

/** 组成员管理弹窗：搜索添加成员 + 成员列表移除（操作后刷新组列表计数）。 */
function GroupMembersModal({
  group,
  onClose,
  onError,
  onNotice,
  onChanged,
}: {
  group: Group
  onClose: () => void
  onError: (msg: string) => void
  onNotice: (msg: string) => void
  onChanged: () => Promise<void> | void
}) {
  const { modal } = AntdApp.useApp()
  const [members, setMembers] = useState<GroupMember[] | null>(null)
  const [picked, setPicked] = useState<UserSearchResult | null>(null)
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      setMembers(await adminListGroupMembers(group.id))
    } catch (err) {
      onError(err instanceof Error ? err.message : '组成员加载失败')
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [group.id])

  const add = async () => {
    if (!picked || busy) return
    setBusy(true)
    try {
      await adminAddGroupMember(group.id, picked.id)
      onNotice(`已将 ${searchResultLabel(picked)} 加入「${group.name}」`)
      setPicked(null)
      await load()
      await onChanged()
    } catch (err) {
      onError(err instanceof Error ? err.message : '添加成员失败')
    } finally {
      setBusy(false)
    }
  }

  const remove = async (member: GroupMember) => {
    if (busy) return
    const ok = await confirmDialog(modal, {
      title: '移除组成员',
      content: `确定将 ${member.nickname || member.username || member.user_id.slice(0, 8)} 移出「${group.name}」？`,
      okText: '移除',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBusy(true)
    try {
      await adminRemoveGroupMember(group.id, member.user_id)
      onNotice('已移除组成员')
      await load()
      await onChanged()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        await load()
        await onChanged()
      } else {
        onError(err instanceof Error ? err.message : '移除成员失败')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`管理组成员：${group.name}`} onClose={onClose}>
      <div className="team-create-row" style={{ marginBottom: 12, alignItems: 'flex-end' }}>
        <UserPickerField onSelect={setPicked} />
        <Button type="primary" disabled={busy || !picked} loading={busy} onClick={() => void add()}>
          加入组
        </Button>
      </div>
      {picked && <div className="setting-desc muted" style={{ marginBottom: 8 }}>已选择：{searchResultLabel(picked)}（{picked.username}）</div>}
      {members === null ? (
        <div className="hint">加载中…</div>
      ) : members.length === 0 ? (
        <div className="empty">该组暂无成员</div>
      ) : (
        <ul className="member-list">
          {members.map((m) => (
            <li key={m.user_id} className="member-row">
              <div className="member-info">
                <span className="member-id" title={m.user_id}>{m.nickname || m.username || m.user_id}</span>
                {m.username && m.nickname && <span className="muted" style={{ marginLeft: 8 }}>{m.username}</span>}
              </div>
              <div className="member-side">
                <span className="muted member-time" title={`加入于 ${formatTime(m.joined_at)}`}>{formatTime(m.joined_at)}</span>
                <Button size="small" danger disabled={busy} onClick={() => void remove(m)}>移除</Button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </Modal>
  )
}

/** 用户组管理卡片：创建表单 + 组列表（antd Table：名称/描述/成员数/操作），
 * 成员管理与改名/描述编辑经弹窗完成；删除需确认（级联清成员关系）。 */
function GroupsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const { modal } = AntdApp.useApp()
  const [groups, setGroups] = useState<Group[] | null>(null)
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [busy, setBusy] = useState(false)
  const [membersGroup, setMembersGroup] = useState<Group | null>(null)
  const [editing, setEditing] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  const [editDesc, setEditDesc] = useState('')

  const load = async () => {
    try {
      setGroups(await adminListGroups())
      onError('')
    } catch (err) {
      onError(err instanceof Error ? err.message : '用户组加载失败')
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !name.trim()) return
    setBusy(true)
    try {
      await adminCreateGroup(name.trim(), desc.trim())
      setName('')
      setDesc('')
      onNotice('用户组已创建')
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '创建用户组失败')
    } finally {
      setBusy(false)
    }
  }

  const saveEdit = async (e: FormEvent) => {
    e.preventDefault()
    if (!editing || busy) return
    setBusy(true)
    try {
      await adminUpdateGroup(editing.id, { name: editName.trim(), description: editDesc.trim() })
      setEditing(null)
      onNotice('用户组已更新')
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '更新用户组失败')
    } finally {
      setBusy(false)
    }
  }

  const remove = async (g: Group) => {
    const ok = await confirmDialog(modal, {
      title: '删除用户组',
      content: `确定删除用户组「${g.name}」？其 ${g.member_count} 名成员的归属关系将被清除（用户本身不受影响）。`,
      okText: '删除',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBusy(true)
    try {
      await adminDeleteGroup(g.id)
      onNotice(`已删除用户组「${g.name}」`)
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '删除用户组失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>用户组</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        用户组为组织维度的人员集合（仅 admin 管理）：用于给用户打组织标签，
        不挂文件空间、不影响文件权限（协作空间请使用「团队」）。删除组会清除其成员归属，不会删除用户。
      </div>
      <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={(e) => void submit(e)}>
        <label className="field">
          <span>组名（≤100 字符，全局唯一）</span>
          <Input maxLength={100} value={name} onChange={(e) => setName(e.target.value)} placeholder="如：研发部" />
        </label>
        <label className="field">
          <span>描述（可选）</span>
          <Input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="组用途说明" />
        </label>
        <Button type="primary" htmlType="submit" disabled={busy || !name.trim()} loading={busy}>
          创建用户组
        </Button>
      </form>
      {groups === null ? (
        <div className="hint">加载中…</div>
      ) : (
        <Table<Group>
          rowKey="id"
          pagination={false}
          dataSource={groups}
          locale={{ emptyText: '尚未创建用户组' }}
          scroll={{ x: 760 }}
          columns={[
            { title: '名称', dataIndex: 'name', key: 'name' },
            { title: '描述', key: 'description', render: (_, g) => <span className="muted">{g.description || '—'}</span> },
            { title: '成员数', dataIndex: 'member_count', key: 'member_count', width: 90 },
            { title: '创建时间', key: 'created_at', width: 160, render: (_, g) => <span className="muted">{formatTime(g.created_at)}</span> },
            {
              title: '操作',
              key: 'actions',
              width: 200,
              render: (_, g) => (
                <div className="table-actions">
                  <Button size="small" disabled={busy} onClick={() => setMembersGroup(g)}>成员</Button>
                  <Button
                    size="small"
                    disabled={busy}
                    onClick={() => { setEditing(g); setEditName(g.name); setEditDesc(g.description) }}
                  >
                    编辑
                  </Button>
                  <Button size="small" danger disabled={busy} onClick={() => void remove(g)}>删除</Button>
                </div>
              ),
            },
          ]}
        />
      )}
      {membersGroup && (
        <GroupMembersModal
          group={membersGroup}
          onClose={() => setMembersGroup(null)}
          onError={onError}
          onNotice={onNotice}
          onChanged={load}
        />
      )}
      {editing && (
        <Modal title={`编辑用户组：${editing.name}`} onClose={() => setEditing(null)}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void saveEdit(e)}>
            <label className="field">
              <span>组名</span>
              <Input maxLength={100} required value={editName} onChange={(e) => setEditName(e.target.value)} />
            </label>
            <label className="field">
              <span>描述</span>
              <Input value={editDesc} onChange={(e) => setEditDesc(e.target.value)} placeholder="组用途说明（留空清除）" />
            </label>
            <div className="setting-control" style={{ marginTop: 8 }}>
              <Button type="primary" htmlType="submit" disabled={busy || !editName.trim()} loading={busy}>保存</Button>
              <Button disabled={busy} onClick={() => setEditing(null)}>取消</Button>
            </div>
          </form>
        </Modal>
      )}
    </div>
  )
}

/** 用户编辑弹窗（people 区行操作）：昵称 / 角色 / 状态 / 配额一次提交。 */
function UserEditModal({
  user,
  groups,
  onClose,
  onError,
  onNotice,
  onChanged,
}: {
  user: AdminUser
  groups: Group[]
  onClose: () => void
  onError: (msg: string) => void
  onNotice: (msg: string) => void
  onChanged: () => Promise<void> | void
}) {
  const [nickname, setNickname] = useState(user.profile.nickname ?? '')
  const [role, setRole] = useState<'user' | 'admin'>(user.role)
  const [status, setStatus] = useState<'active' | 'disabled'>(user.status === 'active' ? 'active' : 'disabled')
  const [quotaGib, setQuotaGib] = useState(quotaToGib(user.storage_quota))
  const [busy, setBusy] = useState(false)
  const selfId = currentUserId()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    const gib = Number(quotaGib)
    if (quotaGib === '' || !Number.isFinite(gib) || gib <= 0) {
      onError('请输入大于 0 的配额数字（GiB）')
      return
    }
    if (status === 'disabled' && user.id === selfId) {
      onError('不可禁用自己的账号')
      return
    }
    setBusy(true)
    try {
      await adminUpdateUser(user.id, {
        nickname,
        role,
        status,
        storageQuota: Math.round(gib * GIB),
      })
      onNotice(`已更新 ${user.username}`)
      onClose()
      await onChanged()
    } catch (err) {
      onError(err instanceof Error ? err.message : '更新失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`编辑用户：${user.profile.nickname || user.username}`} onClose={onClose}>
      <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void submit(e)}>
        <label className="field">
          <span>昵称（≤64 字符，留空清除）</span>
          <Input maxLength={64} value={nickname} onChange={(e) => setNickname(e.target.value)} />
        </label>
        <div className="team-create-row">
          <label className="field">
            <span>角色</span>
            <Select
              value={role}
              disabled={user.id === selfId}
              onChange={(v) => setRole(v === 'admin' ? 'admin' : 'user')}
              options={[
                { value: 'user', label: 'user' },
                { value: 'admin', label: 'admin' },
              ]}
            />
          </label>
          <label className="field">
            <span>状态（禁用立即撤销全部会话；启用解除锁定）</span>
            <Select
              value={status}
              disabled={user.id === selfId}
              onChange={(v) => setStatus(v === 'disabled' ? 'disabled' : 'active')}
              options={[
                { value: 'active', label: 'active（正常）' },
                { value: 'disabled', label: 'disabled（禁用）' },
              ]}
            />
          </label>
          <label className="field">
            <span>存储配额（GiB）</span>
            <InputNumber
              min={0.01}
              step={0.01}
              style={{ width: '100%' }}
              value={quotaGib === '' ? null : Number(quotaGib)}
              onChange={(v) => setQuotaGib(v === null || v === undefined ? '' : String(v))}
            />
          </label>
        </div>
        {groups.length > 0 && (
          <div className="setting-desc muted">
            所属组：{user.group_names?.length ? user.group_names.join('、') : '（无）'}
          </div>
        )}
        <div className="setting-control" style={{ marginTop: 8 }}>
          <Button type="primary" htmlType="submit" disabled={busy} loading={busy}>保存</Button>
          <Button disabled={busy} onClick={onClose}>取消</Button>
        </div>
      </form>
    </Modal>
  )
}

/** 用户组归属弹窗（people 区行操作「组」）：查看/移出所属组 + 加入新组。 */
function UserGroupsModal({
  user,
  groups,
  onClose,
  onError,
  onNotice,
  onChanged,
}: {
  user: AdminUser
  groups: Group[]
  onClose: () => void
  onError: (msg: string) => void
  onNotice: (msg: string) => void
  onChanged: () => Promise<void> | void
}) {
  const current = user.group_names ?? []
  const [picked, setPicked] = useState('')
  const [busy, setBusy] = useState(false)

  const add = async () => {
    if (!picked || busy) return
    setBusy(true)
    try {
      await adminAddGroupMember(picked, user.id)
      onNotice(`已将 ${user.username} 加入「${groups.find((g) => g.id === picked)?.name ?? picked}」`)
      setPicked('')
      await onChanged()
    } catch (err) {
      onError(err instanceof Error ? err.message : '加入组失败')
    } finally {
      setBusy(false)
    }
  }

  const remove = async (name: string) => {
    const group = groups.find((g) => g.name === name)
    if (!group || busy) return
    setBusy(true)
    try {
      await adminRemoveGroupMember(group.id, user.id)
      onNotice(`已将 ${user.username} 移出「${name}」`)
      await onChanged()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await onChanged()
      else onError(err instanceof Error ? err.message : '移出组失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal title={`所属用户组：${user.profile.nickname || user.username}`} onClose={onClose}>
      {current.length === 0 ? (
        <div className="empty">该用户尚未加入任何用户组</div>
      ) : (
        <ul className="member-list" style={{ marginBottom: 12 }}>
          {current.map((name) => {
            const group = groups.find((g) => g.name === name)
            return (
              <li key={name} className="member-row">
                <div className="member-info">
                  <span className="member-id">{name}</span>
                  {group && <span className="muted" style={{ marginLeft: 8 }}>{group.description || `${group.member_count} 名成员`}</span>}
                </div>
                <div className="member-side">
                  <Button size="small" danger disabled={busy || !group} onClick={() => void remove(name)}>移出</Button>
                </div>
              </li>
            )
          })}
        </ul>
      )}
      <div className="team-create-row" style={{ alignItems: 'flex-end' }}>
        <label className="field">
          <span>加入组</span>
          <Select
            value={picked || undefined}
            onChange={(v) => setPicked(v ?? '')}
            placeholder="选择用户组…"
            allowClear
            options={groups.filter((g) => !current.includes(g.name)).map((g) => ({
              value: g.id,
              label: `${g.name}（${g.member_count} 人）`,
            }))}
          />
        </label>
        <Button type="primary" disabled={busy || !picked} loading={busy} onClick={() => void add()}>
          加入组
        </Button>
      </div>
      {groups.length === 0 && <div className="setting-desc muted" style={{ marginTop: 8 }}>尚未创建任何用户组，可先在「用户组」页创建。</div>}
    </Modal>
  )
}

/** 概览页近期审计事件摘要（最近 5 条，点击跳转审计页查看全文）。 */
function RecentAuditPanel({ onError }: { onError: (msg: string) => void }) {
  const navigate = useNavigate()
  const [items, setItems] = useState<AuditEntry[] | null>(null)

  useEffect(() => {
    adminListAuditLogs({}, '', 5)
      .then((r) => setItems(r.items ?? []))
      .catch(() => onError('近期审计事件加载失败'))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return (
    <div className="panel setting-group">
      <div className="setting-row" style={{ borderBottom: 0, paddingBottom: 0 }}>
        <div className="setting-main">
          <h3 style={{ margin: 0 }}>近期审计事件</h3>
        </div>
        <div className="setting-control">
          <Button type="link" size="small" onClick={() => navigate('/admin/audit')}>查看全部 →</Button>
        </div>
      </div>
      {items === null ? (
        <div className="hint">加载中…</div>
      ) : items.length === 0 ? (
        <div className="empty">暂无审计事件</div>
      ) : (
        <ul className="member-list">
          {items.map((entry) => (
            <li key={entry.id} className="member-row">
              <div className="member-info">
                <span className="member-id">
                  {entry.action}
                  {entry.resource_type && <span className="muted" style={{ marginLeft: 8 }}>{entry.resource_type} {entry.resource_id?.slice(0, 8)}</span>}
                </span>
              </div>
              <div className="member-side">
                <span className={`badge ${entry.status === 'success' ? 'available' : 'failed'}`}>{entry.status}</span>
                <span className="muted member-time" title={entry.user_id ?? '系统/匿名'}>{formatTime(entry.created_at)}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

const adminSections = [
  ['overview', '概览'], ['people', '人员'], ['groups', '用户组'], ['audit', '审计日志'], ['security', '安全'],
  ['tls', 'TLS'], ['mail', '邮件'], ['backup', '备份'], ['system', '系统设置'],
] as const

export default function AdminPage() {
  const { section = 'overview' } = useParams()
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
  // 系统设置页：搜索框过滤键 + 分组折叠状态（prefix → 折叠；过滤时自动展开）。
  const [settingsQuery, setSettingsQuery] = useState('')
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({})

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

  /** 按键前缀分组（保持 Definitions 输出顺序），支持按 key/描述过滤（系统设置页搜索框）。 */
  const settingGroups = useMemo(() => {
    const needle = settingsQuery.trim().toLowerCase()
    const map = new Map<string, SettingItem[]>()
    for (const item of settings) {
      if (needle !== '' && !item.key.toLowerCase().includes(needle) && !item.description.toLowerCase().includes(needle)) {
        continue
      }
      const prefix = item.key.split('.')[0]
      const list = map.get(prefix) ?? []
      list.push(item)
      map.set(prefix, list)
    }
    return Array.from(map.entries())
  }, [settings, settingsQuery])

  const startEdit = (item: SettingItem) => {
    setEditingKey(item.key)
    setDraft(item.type === 'bool' ? Boolean(item.value) : String(item.value))
    setRowError('')
    setNotice('')
  }

  /** bool 设置行直开直关（不进编辑态）：切换即保存并刷新列表（含生效方式徽章）。 */
  const toggleBool = async (item: SettingItem) => {
    if (savingKey !== null) return
    setRowError('')
    setSavingKey(item.key)
    try {
      const normalized = await adminPutSetting(item.key, !item.value)
      setNotice(`已保存 ${item.key}（当前值：${normalized ? '开启' : '关闭'}）`)
      const refreshed = await adminGetSettings()
      setSettings(refreshed.settings ?? [])
      setSecrets(refreshed.secrets ?? {})
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) setRowError('无权限')
      else setRowError(err instanceof Error ? err.message : '保存失败')
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

  if (!adminSections.some(([key]) => key === section)) return <Navigate to="/admin/overview" replace />

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
    <div className="page wide-page admin-page">
      <aside className="section-sidebar"><h3>管理</h3>{adminSections.map(([key, label]) => <NavLink key={key} to={`/admin/${key}`} className={({ isActive }) => isActive ? 'active' : ''}>{label}</NavLink>)}</aside>
      <div className="section-content">
      <div className="page-head">
        <h2>{msg('adminTitle')}</h2>
        <Button type="text" onClick={() => { setNotice(''); void load() }}>{msg('refresh')}</Button>
      </div>

      {notice && <div className="banner ok">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}

      {section === 'overview' && !loading && stats && (
        <>
          <div className="stats-grid">
            {statCards.map(({ key, label, format }) => (
              <Card key={key} size="small">
                <div className="stat-value">{format ? format(Number(stats[key] ?? 0)) : stats[key]}</div>
                <div className="stat-label">{label}</div>
              </Card>
            ))}
          </div>
          <RecentAuditPanel onError={(m) => { setError(m); setNotice('') }} />
        </>
      )}

      {section === 'people' && !loading && !forbidden && (
        <UsersPanel
          onError={(msg) => { setError(msg); setNotice('') }}
          onNotice={(msg) => { setNotice(msg); setError('') }}
        />
      )}

      {section === 'groups' && !loading && !forbidden && (
        <GroupsPanel
          onError={(msg) => { setError(msg); setNotice('') }}
          onNotice={(msg) => { setNotice(msg); setError('') }}
        />
      )}

      {section === 'audit' && !loading && !forbidden && <AuditPanel onError={(msg) => { setError(msg); setNotice('') }} />}
      {section === 'backup' && !loading && !forbidden && <BackupPanel onError={(msg) => { setError(msg); setNotice('') }} onNotice={(msg) => { setNotice(msg); setError('') }} />}
      {section === 'security' && !loading && !forbidden && <QuarantinePanel onError={(msg) => { setError(msg); setNotice('') }} onNotice={(msg) => { setNotice(msg); setError('') }} />}
      {section === 'security' && !loading && !forbidden && <SecretsPanel secrets={secrets} />}
       {section === 'tls' && !loading && !forbidden && <TlsPanel onNotice={(msg) => { setNotice(msg); setError('') }} />}
      {section === 'mail' && !loading && !forbidden && <MailPanel onNotice={setNotice} onError={setError} />}

      {section === 'people' && !loading && !forbidden && (
        <InvitationsPanel
          onError={(msg) => { setError(msg); setNotice('') }}
          onNotice={(msg) => { setNotice(msg); setError('') }}
        />
      )}

      {section === 'system' && !loading && (
        <div className="panel setting-group" style={{ padding: '12px 16px' }}>
          <form className="team-create-row" style={{ marginBottom: 0 }} onSubmit={(e) => e.preventDefault()}>
            <label className="field" style={{ flex: 1 }}>
              <span>过滤设置键（按 key 或描述匹配；过滤时分组自动展开）</span>
              <Input
                allowClear
                autoCapitalize="none"
                spellCheck={false}
                value={settingsQuery}
                onChange={(e) => setSettingsQuery(e.target.value)}
                placeholder="如：upload、share.default 或“配额”"
              />
            </label>
            {settingsQuery && (
              <Button style={{ alignSelf: 'flex-end' }} onClick={() => setSettingsQuery('')}>清除</Button>
            )}
          </form>
        </div>
      )}

      {section === 'system' && !loading && settingGroups.length === 0 && (
        <div className="empty">没有匹配的设置项</div>
      )}

      {section === 'system' && !loading && settingGroups.map(([prefix, items]) => {
        const searching = settingsQuery.trim() !== ''
        const isCollapsed = !searching && collapsed[prefix]
        return (
        <div key={prefix} className="panel setting-group">
          <h3 style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <Button
              type="text"
              size="small"
              title={isCollapsed ? '展开分组' : '折叠分组'}
              onClick={() => setCollapsed((prev) => ({ ...prev, [prefix]: !isCollapsed }))}
            >
              {isCollapsed ? '▸' : '▾'}
            </Button>
            {groupTitles[prefix] ?? prefix}
            <span className="setting-meta muted">（{items.length} 项）</span>
          </h3>
          {!isCollapsed && items.map((item) => {
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
                        <span className="setting-bool">
                          <Switch size="small" checked={Boolean(draft)} onChange={(v) => setDraft(v)} />
                          <span>{draft ? '开启' : '关闭'}</span>
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
                        保存
                      </Button>
                      <Button
                        size="small"
                        disabled={savingKey !== null}
                        onClick={() => { setEditingKey(null); setRowError('') }}
                      >
                        取消
                      </Button>
                    </form>
                  ) : item.type === 'bool' ? (
                    <span className="setting-bool" title="bool 行直开直关：切换后立即保存">
                      <Switch
                        size="small"
                        checked={Boolean(item.value)}
                        loading={savingKey === item.key}
                        onChange={() => void toggleBool(item)}
                      />
                      <span>{savingKey === item.key ? '保存中…' : item.value ? '开启' : '关闭'}</span>
                    </span>
                  ) : (
                    <>
                      <span className="setting-value-mono">{formatSettingValue(item.value)}</span>
                      <Button size="small" onClick={() => startEdit(item)}>编辑</Button>
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
      </div>
    </div>
  )
}
