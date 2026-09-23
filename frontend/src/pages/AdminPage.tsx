// 管理页（仅 admin 角色，v2.3 重分配）：概览 / 人员与组（合并：左组树 +
// 右成员表 + 批量跨组移动/移出组/改角色/禁用 + 组 CRUD + 邀请记录弹窗）/
// 空间（含「已解散」筛选与彻底删除；v3.1 归并：space.* 策略键自「系统
// 设置」迁入本页顶部）/ 审计日志（含保留期设置，audit.* 唯一入口）/
// 威胁防护（隔离区 + 扫描 + 凭据状态）/ 备份（v3.1 归并：backup.* 策略键
// 自「系统设置」迁入本页顶部）。TLS / 邮件配置 / 系统设置已迁至
// 「设置」页（admin 可见）；邀请管理并入「人员与组」右上角弹窗。
import { FormEvent, Key, Suspense, lazy, useEffect, useMemo, useState } from 'react'
import { NavLink, Navigate, useNavigate, useParams } from 'react-router-dom'
import { App as AntdApp, Button, Card, Checkbox, Input, InputNumber, Menu, Segmented, Select, Table, Tag } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import type { MenuProps } from 'antd'
import { Edit3, Trash2, Users } from 'lucide-react'
import {
  AdminStats,
  AdminSpaceItem,
  AdminUser,
  ApiError,
  AuditEntry,
  BackupStatus,
  BackupVerifyResult,
  Group,
  Invitation,
  QuarantineAction,
  QuarantineItem,
  UserSearchResult,
  adminAddGroupMember,
  adminCreateGroup,
  adminCreateInvitation,
  adminDeleteGroup,
  adminDownloadAuditCSV,
  adminGetBackupStatus,
  adminVerifyBackup,
  adminGetSettings,
  adminListAuditLogs,
  adminListGroups,
  adminListQuarantine,
  adminListSpaces,
  adminPurgeSpace,
  adminQuarantineAction,
  adminRemoveGroupMember,
  adminResendInvitation,
  adminRunBackup,
  adminDeleteSpace,
  adminUpdateGroup,
  adminUpdateSpace,
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
import { Modal, confirmDialog, formatQuota, formatTime, promptViaModal } from '../components/FileBrowser'
import QuotaInput from '../components/QuotaInput'
import { MessageKey, t, useLocale } from '../i18n'
/* 平台设置分区（个人/平台分离）：平台级面板自 SettingsPage 导出复用。 */
// 平台设置五面板懒加载（v2.7 页面偶现卡死治理）：AISettingsPanel 与
// SettingsPage 导出的四面板体量可观（SettingsPage 100KB+ 源码及其依赖），
// 直 import 会全部进入管理页首屏 chunk，tag 切换前也常驻渲染压力。改
// React.lazy 后 vite 自动分包，进入对应 tag 才拉取并挂载（Suspense 兜底
// loading）；SettingsPage 的面板为命名导出，经 then 映射为 default。
const AISettingsPanel = lazy(() => import('../components/AISettingsPanel'))
const AgentPanel = lazy(() => import('../components/AgentPanel'))
const ConfigOverviewPanel = lazy(() => import('../components/ConfigOverviewPanel'))
const MailPanel = lazy(() => import('./SettingsPage').then((m) => ({ default: m.MailPanel })))
const TlsPanel = lazy(() => import('./SettingsPage').then((m) => ({ default: m.TlsPanel })))
const SystemSettingsPanel = lazy(() => import('./SettingsPage').then((m) => ({ default: m.SystemSettingsPanel })))
// v3.1 归并：space.* / backup.* 键组卡片（迁入「空间」「备份」页，复用
// 系统设置面板的控件化渲染），同样经 SettingsPage 命名导出懒加载。
const SystemSettingKeysCard = lazy(() => import('./SettingsPage').then((m) => ({ default: m.SystemSettingKeysCard })))
const SecurityPanel = lazy(() => import('../components/SecurityPanel'))

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
  { key: 'storage_bytes', label: '存储用量', format: (v) => formatQuota(v, false) },
  { key: 'spaces', label: '空间' },
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

/** 邀请管理卡片：创建（弹窗，一次性注册链接展示/复制）、列表（邮箱检索 +
 * 状态筛选 + 多选批量撤销）与撤销。 */
function InvitationsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const { modal: antdModal } = AntdApp.useApp()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [invitations, setInvitations] = useState<Invitation[] | null>(null)
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<'user' | 'admin'>('user')
  const [busy, setBusy] = useState(false)
  const [revoking, setRevoking] = useState<string | null>(null)
  /** 创建邀请弹窗开关（v2.2：平铺表单弹窗化）。 */
  const [createOpen, setCreateOpen] = useState(false)
  /** 列筛选：邮箱关键词 + 状态。 */
  const [query, setQuery] = useState('')
  const [statusFilter, setStatusFilter] = useState<Invitation['status'] | ''>('')
  /** 批量撤销勾选（仅待接受可撤）。 */
  const [selected, setSelected] = useState<Key[]>([])
  const [batchBusy, setBatchBusy] = useState(false)
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
      setCreateOpen(false)
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
      setSelected((prev) => prev.filter((k) => k !== id))
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '撤销失败')
    } finally {
      setRevoking(null)
    }
  }

  /** 重发邀请（v2.3）：撤销旧 token 并生成新一次性链接（展示+复制），
   * best-effort 补发邮件；已接受 410 / 不存在 404 由后端校验。 */
  const resend = async (inv: Invitation) => {
    if (revoking !== null || inv.status !== 'pending') return
    setRevoking(inv.id)
    onError('')
    try {
      const created = await adminResendInvitation(inv.id)
      if (created.accept_url) {
        const link = new URL(created.accept_url, window.location.origin).toString()
        setOneTimeLink(link)
        setCopied(false)
        onNotice(`已重发给 ${created.email} 的新邀请链接（旧链接已失效），请立即复制发送`)
      } else {
        onError('重发未返回新链接，请重试')
      }
      setSelected((prev) => prev.filter((k) => k !== inv.id))
      await load()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load()
      else onError(err instanceof Error ? err.message : '重发失败')
    } finally {
      setRevoking(null)
    }
  }

  /** 批量撤销（逐项调用，部分成功语义）。 */
  const batchRevoke = async () => {
    const ids = selected.map(String).filter((id) => invitations?.find((i) => i.id === id)?.status === 'pending')
    if (ids.length === 0) return
    const ok = await confirmDialog(antdModal, {
      title: '批量撤销邀请',
      content: `确定撤销选中的 ${ids.length} 条待接受邀请？对应注册链接立即失效。`,
      okText: '撤销',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBatchBusy(true)
    onError('')
    try {
      const results = await Promise.allSettled(ids.map((id) => adminRevokeInvitation(id)))
      const failed = results.filter((r) => r.status === 'rejected').length
      if (failed > 0) onError(`批量撤销：成功 ${ids.length - failed} 条，失败 ${failed} 条`)
      else onNotice(`已撤销 ${ids.length} 条邀请`)
      setSelected([])
      await load()
    } finally {
      setBatchBusy(false)
    }
  }

  // 列筛选（前端过滤：邮箱包含 + 状态相等）。
  const needle = query.trim().toLowerCase()
  const visible = (invitations ?? []).filter((inv) =>
    (needle === '' || inv.email.toLowerCase().includes(needle))
    && (statusFilter === '' || inv.status === statusFilter))
  const pendingSelected = selected.filter((id) => invitations?.find((i) => i.id === id)?.status === 'pending').length

  return (
    <div className="panel setting-group">
      <h3>{msg('invitesTitle')}</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        一次性注册邀请链接（7 天内有效、仅可用一次）：发送给未注册的同事完成注册，
        角色决定其注册后的系统角色。已注册用户无需邀请——可在「用户组」或空间内直接添加。
      </div>
      {oneTimeLink && (
        <div className="share-link" style={{ marginBottom: 12 }}>
          <Input readOnly value={oneTimeLink} onFocus={(e) => e.currentTarget.select()} />
          <Button size="small" onClick={() => void copyLink()}>
            {copied ? '已复制' : '复制链接'}
          </Button>
        </div>
      )}
      {oneTimeLink && <div className="setting-desc muted" style={{ marginBottom: 12 }}>该注册链接仅显示这一次，请立即复制发送给被邀请人（7 天内有效，仅可使用一次）</div>}
      {/* 工具行：新建入口 + 邮箱检索 + 状态筛选 + 批量撤销。 */}
      <div className="member-list-toolbar" style={{ marginBottom: 12 }}>
        <Button type="primary" onClick={() => { setCreateOpen(true); setEmail('') }}>
          创建邀请
        </Button>
        <Input
          allowClear
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="按邮箱检索…"
          style={{ width: 200 }}
          aria-label="按邮箱检索"
        />
        <Select
          allowClear
          value={statusFilter || undefined}
          onChange={(v) => setStatusFilter((v ?? '') as Invitation['status'] | '')}
          placeholder="全部状态"
          options={[
            { value: 'pending', label: '待接受' },
            { value: 'accepted', label: '已接受' },
            { value: 'expired', label: '已过期' },
          ]}
          style={{ width: 120 }}
        />
        {selected.length > 0 && pendingSelected > 0 && (
          <Button danger disabled={batchBusy || revoking !== null} loading={batchBusy} onClick={() => void batchRevoke()}>
            批量撤销（{pendingSelected}）
          </Button>
        )}
      </div>
      <Table<Invitation>
        rowKey="id"
        loading={invitations === null}
        dataSource={visible}
        pagination={false}
        locale={{ emptyText: query || statusFilter ? '没有匹配的邀请' : msg('noInvites') }}
        rowSelection={{
          selectedRowKeys: selected,
          onChange: setSelected,
          getCheckboxProps: (inv) => ({ disabled: inv.status !== 'pending' || batchBusy || revoking !== null }),
        }}
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
          {
            title: '角色',
            dataIndex: 'role',
            key: 'role',
            width: 100,
            render: (r: string) => (
              <Tag color={r === 'admin' ? 'purple' : 'default'}>{r === 'admin' ? '管理员（admin）' : '普通用户（user）'}</Tag>
            ),
          },
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
            width: 150,
            render: (_, inv) =>
              inv.status === 'pending' ? (
                <>
                  <Button size="small" disabled={revoking !== null || batchBusy} loading={revoking === inv.id} onClick={() => void resend(inv)}>
                    重发
                  </Button>
                  {' '}
                  <Button size="small" danger disabled={revoking !== null || batchBusy} onClick={() => void revoke(inv.id)}>
                    撤销
                  </Button>
                </>
              ) : null,
          },
        ]}
      />
      {/* 创建邀请弹窗（v2.2：平铺表单弹窗化）。 */}
      {createOpen && (
        <Modal title="创建注册邀请" onClose={() => { if (!busy) { setCreateOpen(false); setEmail('') } }}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void submit(e)}>
            <label className="field">
              <span>邮箱（发给未注册的同事）</span>
              <Input
                autoFocus
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="teammate@example.com"
              />
            </label>
            <label className="field">
              <span>角色（注册成功后的系统角色）</span>
              <Select
                value={role}
                onChange={(v) => setRole(v === 'admin' ? 'admin' : 'user')}
                options={[
                  { value: 'user', label: '普通用户（user）' },
                  { value: 'admin', label: '管理员（admin）' },
                ]}
              />
            </label>
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button type="primary" htmlType="submit" disabled={busy || email.trim() === ''}>
                {busy ? '创建中…' : '创建邀请'}
              </Button>
              <Button disabled={busy} onClick={() => { setCreateOpen(false); setEmail('') }}>
                取消
              </Button>
            </div>
          </form>
        </Modal>
      )}
    </div>
  )
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

/** 人员与组（v2.3 合并页 /admin/people，原「人员」+「用户组」两个独立面板
 * 合并删除）：左侧组树（全部用户 + 各用户组，组节点内联改名/删除）+ 右侧
 * 成员表（点组过滤成员；组视图附「加入成员」与行内「移出本组」）；
 * 批量操作：跨组移动（多选用户 → 目标组多选，移动=移出其余组并加入目标）、
 * 移出当前组、改角色/禁用/启用（原有）；组 CRUD 收顶部按钮；邀请记录经
 * 右上「邀请记录」按钮弹窗（创建 / 重发 / 撤销），不再单独占管理导航位。 */
function PeopleAndGroupsPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const { modal: antdModal } = AntdApp.useApp()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [users, setUsers] = useState<AdminUser[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  const [total, setTotal] = useState(0)
  const [offset, setOffset] = useState(0)
  const [q, setQ] = useState('')
  const [query, setQuery] = useState('')
  /** 列筛选（当前页客户端过滤）：状态 + 角色。 */
  const [statusFilter, setStatusFilter] = useState<AdminUser['status'] | ''>('')
  const [roleFilter, setRoleFilter] = useState<'user' | 'admin' | ''>('')
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  /** 左侧组树选中（'' = 全部用户；否则组 ID）。 */
  const [selectedGroup, setSelectedGroup] = useState('')
  /** 弹窗状态：编辑用户 / 重置密码 / 新建组 / 编辑组 / 邀请记录。 */
  const [editingUser, setEditingUser] = useState<AdminUser | null>(null)
  const [resetUser, setResetUser] = useState<AdminUser | null>(null)
  const [newPassword, setNewPassword] = useState('')
  const [invitesOpen, setInvitesOpen] = useState(false)
  /** 批量操作：勾选行 + 批量角色下拉 + 跨组移动目标（组多选）。 */
  const [selected, setSelected] = useState<Key[]>([])
  const [batchRole, setBatchRole] = useState<'user' | 'admin'>('user')
  const [batchBusy, setBatchBusy] = useState(false)
  const [moveTargets, setMoveTargets] = useState<string[]>([])
  const [moveBusy, setMoveBusy] = useState(false)
  /** 组视图「加入成员」单选（远程搜索结果）。 */
  const [addGroupPick, setAddGroupPick] = useState<UserSearchResult | null>(null)
  /** 新建用户组弹窗（v2.2 平铺表单弹窗化）：名/描述 + 初始成员多选远程搜索。 */
  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [memberIds, setMemberIds] = useState<string[]>([])
  const [memberQuery, setMemberQuery] = useState('')
  const [memberOptions, setMemberOptions] = useState<UserSearchResult[]>([])
  const [memberSearching, setMemberSearching] = useState(false)
  const memberNameRef = useMemo(() => new Map<string, string>(), [])
  /** 编辑组弹窗（改名/描述）。 */
  const [editing, setEditing] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  const [editDesc, setEditDesc] = useState('')
  const pageLimit = 50
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
      // 组列表加载失败不阻塞用户列表（组树仍可重试）。
      setGroups([])
    }
  }

  useEffect(() => {
    void load(0, '')
    void loadGroups()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 新建组初始成员远程搜索（防抖 300ms，≥2 字）。
  useEffect(() => {
    const queryText = memberQuery.trim()
    if (queryText.length < 2) {
      setMemberOptions([])
      return
    }
    const timer = window.setTimeout(() => {
      setMemberSearching(true)
      void searchUsers(queryText)
        .then((found) => {
          setMemberOptions(found)
          for (const u of found) memberNameRef.set(u.id, (u.nickname ?? u.profile?.nickname) || u.username)
        })
        .catch(() => setMemberOptions([]))
        .finally(() => setMemberSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [memberQuery, memberNameRef])

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

  /** 批量操作（逐项调用现有端点，部分成功语义）：kind=role 改角色、
   *  disable 禁用（并撤销其全部会话）、enable 启用（解除锁定）。 */
  const runBatch = async (kind: 'role' | 'disable' | 'enable') => {
    const rows = users.filter((u) => selected.includes(u.id))
    const targets = kind === 'disable' ? rows.filter((u) => u.id !== selfId) : rows
    if (targets.length === 0) return
    const actionText = kind === 'role'
      ? `将 ${targets.length} 名用户的角色改为「${batchRole === 'admin' ? '管理员' : '普通用户'}」`
      : kind === 'disable'
        ? `禁用 ${targets.length} 名用户（立即撤销其全部登录会话）`
        : `启用 ${targets.length} 名用户（同时解除登录失败锁定）`
    const ok = await confirmDialog(antdModal, {
      title: kind === 'role' ? '批量改角色' : kind === 'disable' ? '批量禁用' : '批量启用',
      content: `确定${actionText}？`,
      okText: kind === 'role' ? '改角色' : kind === 'disable' ? '禁用' : '启用',
      danger: kind === 'disable',
      cancelText: '取消',
    })
    if (!ok) return
    setBatchBusy(true)
    onError('')
    try {
      const calls = targets.map((u) =>
        kind === 'role'
          ? adminUpdateUser(u.id, { role: batchRole })
          : adminUpdateUser(u.id, { status: kind === 'disable' ? 'disabled' : 'active' }))
      const results = await Promise.allSettled(calls)
      const failed = results.filter((r) => r.status === 'rejected').length
      if (failed > 0) onError(`批量操作：成功 ${targets.length - failed} 项，失败 ${failed} 项`)
      else onNotice(`批量操作完成：成功 ${targets.length} 项`)
      setSelected([])
      await refreshAll()
    } finally {
      setBatchBusy(false)
    }
  }

  /** 批量跨组移动（v2.3）：多选用户 → 目标组多选；移动语义 = 移出其余组
   * 并加入全部目标组（逐项调用成员增删端点，部分成功汇总）。 */
  const batchMove = async () => {
    const rows = users.filter((u) => selected.includes(u.id))
    if (rows.length === 0 || moveTargets.length === 0 || moveBusy) return
    const targetNames = moveTargets.map((id) => groups.find((g) => g.id === id)?.name ?? id.slice(0, 8)).join('、')
    const ok = await confirmDialog(antdModal, {
      title: '跨组移动',
      content: `将选中的 ${rows.length} 名用户移出其现有用户组并加入「${targetNames}」？`,
      okText: '移动',
      cancelText: '取消',
    })
    if (!ok) return
    setMoveBusy(true)
    onError('')
    let done = 0
    let failed = 0
    try {
      for (const u of rows) {
        // 移出全部现有组 → 加入目标组。
        const outCalls = (u.group_names ?? [])
          .map((gname) => groups.find((g) => g.name === gname))
          .filter((g): g is Group => Boolean(g) && !moveTargets.includes(g!.id))
          .map((g) => adminRemoveGroupMember(g.id, u.id))
        const inCalls = moveTargets.map((gid) => adminAddGroupMember(gid, u.id))
        const results = await Promise.allSettled([...outCalls, ...inCalls])
        if (results.some((r) => r.status === 'rejected')) failed++
        else done++
      }
      if (failed > 0) onError(`跨组移动：成功 ${done} 人，失败 ${failed} 人`)
      else onNotice(`已将 ${done} 名用户移动到目标组`)
      setSelected([])
      setMoveTargets([])
      await refreshAll()
    } finally {
      setMoveBusy(false)
    }
  }

  /** 批量移出当前组（组视图）：把选中用户移出正在查看的组。 */
  const batchRemoveFromGroup = async () => {
    const current = groups.find((g) => g.id === selectedGroup)
    const rows = users.filter((u) => selected.includes(u.id) && (u.group_names ?? []).includes(current?.name ?? '\u0000'))
    if (!current || rows.length === 0 || batchBusy) return
    const ok = await confirmDialog(antdModal, {
      title: '移出用户组',
      content: `将选中的 ${rows.length} 名用户移出「${current.name}」？（其他组归属不受影响）`,
      okText: '移出',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBatchBusy(true)
    onError('')
    try {
      const results = await Promise.allSettled(rows.map((u) => adminRemoveGroupMember(current.id, u.id)))
      const failed = results.filter((r) => r.status === 'rejected').length
      if (failed > 0) onError(`移出组：成功 ${rows.length - failed} 人，失败 ${failed} 人`)
      else onNotice(`已将 ${rows.length} 名用户移出「${current.name}」`)
      setSelected([])
      await refreshAll()
    } finally {
      setBatchBusy(false)
    }
  }

  /** 组视图行内移出单个用户。 */
  const removeFromGroup = async (user: AdminUser) => {
    const current = groups.find((g) => g.id === selectedGroup)
    if (!current || busy) return
    const ok = await confirmDialog(antdModal, {
      title: '移出用户组',
      content: `确定将 ${user.profile.nickname || user.username} 移出「${current.name}」？`,
      okText: '移出',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBusy(true)
    try {
      await adminRemoveGroupMember(current.id, user.id)
      onNotice(`已移出「${current.name}」`)
      await refreshAll()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await refreshAll()
      else onError(err instanceof Error ? err.message : '移出组失败')
    } finally {
      setBusy(false)
    }
  }

  /** 组视图「加入成员」：把远程搜索选中的用户加入当前组。 */
  const addUserToGroup = async () => {
    const current = groups.find((g) => g.id === selectedGroup)
    if (!current || !addGroupPick || busy) return
    setBusy(true)
    try {
      await adminAddGroupMember(current.id, addGroupPick.id)
      onNotice(`已将 ${searchResultLabel(addGroupPick)} 加入「${current.name}」`)
      setAddGroupPick(null)
      await refreshAll()
    } catch (err) {
      onError(err instanceof Error ? err.message : '加入组失败')
    } finally {
      setBusy(false)
    }
  }

  /** 新建用户组：创建后逐个加入勾选的初始成员（部分成功语义）。 */
  const submitCreateGroup = async (e: FormEvent) => {
    e.preventDefault()
    if (busy || !name.trim()) return
    setBusy(true)
    try {
      const created = await adminCreateGroup(name.trim(), desc.trim())
      const failedNames: string[] = []
      for (const uid of memberIds) {
        try { await adminAddGroupMember(created.id, uid) } catch { failedNames.push(memberNameRef.get(uid) ?? uid.slice(0, 8)) }
      }
      if (failedNames.length > 0) onError(`用户组已创建，但 ${failedNames.length} 名初始成员加入失败：${failedNames.join('、')}`)
      else onNotice(memberIds.length > 0 ? `用户组已创建并加入 ${memberIds.length} 名成员` : '用户组已创建')
      setName('')
      setDesc('')
      setMemberIds([])
      setMemberQuery('')
      setMemberOptions([])
      setCreateOpen(false)
      await loadGroups()
    } catch (err) {
      onError(err instanceof Error ? err.message : '创建用户组失败')
    } finally {
      setBusy(false)
    }
  }

  /** 编辑组保存（改名/描述）。 */
  const saveGroupEdit = async (e: FormEvent) => {
    e.preventDefault()
    if (!editing || busy) return
    setBusy(true)
    try {
      await adminUpdateGroup(editing.id, { name: editName.trim(), description: editDesc.trim() })
      setEditing(null)
      onNotice('用户组已更新')
      await loadGroups()
    } catch (err) {
      onError(err instanceof Error ? err.message : '更新用户组失败')
    } finally {
      setBusy(false)
    }
  }

  /** 删除组（级联清成员关系，不影响用户本身）。 */
  const removeGroup = async (g: Group) => {
    const ok = await confirmDialog(antdModal, {
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
      if (selectedGroup === g.id) setSelectedGroup('')
      await refreshAll()
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await refreshAll()
      else onError(err instanceof Error ? err.message : '删除用户组失败')
    } finally {
      setBusy(false)
    }
  }

  /** 弹窗/批量操作完成后的统一刷新（用户列表聚合 group_names + 组成员计数）。 */
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
  const currentGroup = groups.find((g) => g.id === selectedGroup) ?? null
  // 当前页客户端筛选（状态/角色 + 左侧组树选中组过滤）。
  const visibleUsers = users.filter((u) =>
    (statusFilter === '' || u.status === statusFilter)
    && (roleFilter === '' || u.role === roleFilter)
    && (!currentGroup || (u.group_names ?? []).includes(currentGroup.name)))

  return (
    <div className="panel setting-group people-groups">
      <h3>人员与组</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        左侧选择「全部用户」或某个用户组过滤右侧成员表；勾选多行可批量改角色/禁用/启用、
        跨组移动（移出其余组并加入目标组）或移出当前组。组的新建/改名/删除收在顶部按钮；
        邀请记录（创建/重发/撤销）经右上按钮弹窗。禁用账号立即撤销其全部登录会话；
        账号不支持删除——以禁用替代（保留其名下文件与审计记录）。
      </div>
      {/* 顶部操作行：组 CRUD + 邀请记录 + 检索/筛选。 */}
      <form className="member-list-toolbar" style={{ marginBottom: 12 }} onSubmit={(e) => void submitSearch(e)}>
        <Button type="primary" onClick={() => { setCreateOpen(true); setName(''); setDesc(''); setMemberIds([]) }}>
          新建用户组
        </Button>
        <Button onClick={() => setInvitesOpen(true)}>邀请记录</Button>
        <Input
          allowClear
          autoCapitalize="none"
          spellCheck={false}
          style={{ width: 220 }}
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="检索：用户名 / 邮箱 / 昵称前缀"
          aria-label="检索用户"
        />
        <Select
          allowClear
          value={statusFilter || undefined}
          onChange={(v) => setStatusFilter((v ?? '') as AdminUser['status'] | '')}
          placeholder="全部状态"
          style={{ width: 120 }}
          options={[
            { value: 'active', label: '正常' },
            { value: 'disabled', label: '已禁用' },
            { value: 'locked', label: '已锁定' },
          ]}
        />
        <Select
          allowClear
          value={roleFilter || undefined}
          onChange={(v) => setRoleFilter((v ?? '') as 'user' | 'admin' | '')}
          placeholder="全部角色"
          style={{ width: 140 }}
          options={[
            { value: 'admin', label: '管理员（admin）' },
            { value: 'user', label: '普通用户（user）' },
          ]}
        />
        <Button htmlType="submit">搜索</Button>
        {(query || statusFilter || roleFilter) && (
          <Button onClick={() => { setQ(''); setQuery(''); setStatusFilter(''); setRoleFilter(''); void load(0, '') }}>清除</Button>
        )}
      </form>
      <div className="people-groups-layout">
        {/* 左侧组树（v2.5 改 antd Menu 原生风格，与右侧 antd Table 协调）：
            全部用户 + 各用户组；组节点行尾 Edit3/Trash2 图标按钮（改名/删除），
            旧「改」「删」文字按钮退役；外框去除（间距与右侧对齐）。 */}
        <aside className="people-group-tree">
          <Menu
            mode="inline"
            className="people-group-menu"
            selectedKeys={[selectedGroup === '' ? '__all__' : selectedGroup]}
            onClick={({ key }) => {
              const next = key === '__all__' ? '' : String(key)
              setSelectedGroup(next)
              setSelected([])
            }}
            items={[
              {
                key: '__all__',
                icon: <Users size={14} strokeWidth={2} aria-hidden="true" />,
                label: (
                  <span className="people-group-label">
                    <span className="people-group-name">全部用户</span>
                    <span className="muted people-group-count">{total}</span>
                  </span>
                ),
              },
              ...groups.map((g) => ({
                key: g.id,
                label: (
                  <span className="people-group-label" title={g.description || g.name}>
                    <span className="people-group-name">{g.name}</span>
                    <span className="people-group-side">
                      <span className="muted people-group-count">{g.member_count}</span>
                      <Button
                        type="text"
                        size="small"
                        className="people-group-action"
                        title="改名 / 描述"
                        aria-label={`改名：${g.name}`}
                        onClick={(e) => { e.stopPropagation(); setEditing(g); setEditName(g.name); setEditDesc(g.description) }}
                      >
                        <Edit3 size={13} strokeWidth={2} aria-hidden="true" />
                      </Button>
                      <Button
                        type="text"
                        size="small"
                        danger
                        className="people-group-action"
                        title="删除组（成员关系清除，用户不受影响）"
                        aria-label={`删除组：${g.name}`}
                        onClick={(e) => { e.stopPropagation(); void removeGroup(g) }}
                      >
                        <Trash2 size={13} strokeWidth={2} aria-hidden="true" />
                      </Button>
                    </span>
                  </span>
                ),
              })),
            ] satisfies MenuProps['items']}
          />
          {groups.length === 0 && <p className="hint" style={{ margin: '4px 12px' }}>尚未创建用户组</p>}
        </aside>
        {/* 右侧成员表（按组过滤）。 */}
        <div className="people-group-main">
          {/* 组视图：加入成员（远程搜索单选）。 */}
          {currentGroup && (
            <div className="team-create-row" style={{ marginBottom: 12, alignItems: 'flex-end' }}>
              <UserPickerField onSelect={setAddGroupPick} />
              <Button type="primary" disabled={busy || !addGroupPick} loading={busy} onClick={() => void addUserToGroup()}>
                加入「{currentGroup.name}」
              </Button>
            </div>
          )}
          {/* 批量操作栏（勾选后出现）：改角色/禁用/启用 + 跨组移动 + 移出当前组。 */}
          {selected.length > 0 && (
            <div className="member-list-toolbar" style={{ marginBottom: 12 }}>
              <span className="setting-meta muted">已选 {selected.length} 项</span>
              <Select
                value={batchRole}
                onChange={(v) => setBatchRole(v === 'admin' ? 'admin' : 'user')}
                options={[
                  { value: 'user', label: '普通用户（user）' },
                  { value: 'admin', label: '管理员（admin）' },
                ]}
                style={{ width: 170 }}
              />
              <Button disabled={batchBusy} loading={batchBusy} onClick={() => void runBatch('role')}>批量改角色</Button>
              <Button danger disabled={batchBusy} loading={batchBusy} onClick={() => void runBatch('disable')}>批量禁用</Button>
              <Button disabled={batchBusy} loading={batchBusy} onClick={() => void runBatch('enable')}>批量启用</Button>
              <Select
                mode="multiple"
                allowClear
                value={moveTargets}
                onChange={setMoveTargets}
                placeholder="移动到组（可多选）…"
                style={{ minWidth: 220 }}
                options={groups.map((g) => ({ value: g.id, label: g.name }))}
              />
              <Button disabled={moveBusy || moveTargets.length === 0} loading={moveBusy} onClick={() => void batchMove()}>
                跨组移动
              </Button>
              {currentGroup && (
                <Button danger disabled={batchBusy} onClick={() => void batchRemoveFromGroup()}>
                  移出「{currentGroup.name}」
                </Button>
              )}
            </div>
          )}
          <Table<AdminUser>
            rowKey="id"
            loading={loading}
            dataSource={visibleUsers}
            scroll={{ x: 980 }}
            locale={{ emptyText: query || statusFilter || roleFilter || currentGroup ? '没有匹配的用户' : msg('noUsers') }}
            rowSelection={{
              selectedRowKeys: selected,
              onChange: setSelected,
              getCheckboxProps: () => ({ disabled: busy || batchBusy || moveBusy }),
            }}
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
                        {user.group_names!.map((gname) => (
                          <span key={gname} className="badge" style={{ marginLeft: 4 }} title="所属用户组">{gname}</span>
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
                width: 110,
                render: (role: string) => (
                  <Tag color={role === 'admin' ? 'purple' : 'default'}>{role === 'admin' ? '管理员（admin）' : '普通用户（user）'}</Tag>
                ),
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
                      {formatQuota(used, false)} / {formatQuota(user.storage_quota)}（{quotaPercent}%）
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
                      {currentGroup && (user.group_names ?? []).includes(currentGroup.name) && (
                        <Button
                          size="small"
                          disabled={busy}
                          title={`移出「${currentGroup.name}」`}
                          onClick={() => void removeFromGroup(user)}
                        >
                          移出本组
                        </Button>
                      )}
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
        </div>
      </div>
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
      {/* 新建用户组弹窗：名称/描述 + 搜索勾选初始成员。 */}
      {createOpen && (
        <Modal title="新建用户组" onClose={() => { if (!busy) { setCreateOpen(false); setName(''); setDesc(''); setMemberIds([]) } }}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void submitCreateGroup(e)}>
            <label className="field">
              <span>组名（≤100 字符，全局唯一）</span>
              <Input autoFocus maxLength={100} value={name} onChange={(e) => setName(e.target.value)} placeholder="如：研发部" />
            </label>
            <label className="field">
              <span>描述（可选）</span>
              <Input maxLength={200} value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="组用途说明" />
            </label>
            <div className="field">
              <span>初始成员（可选，搜索昵称/用户名/邮箱，至少 2 字）</span>
              <Select
                mode="multiple"
                showSearch
                allowClear
                filterOption={false}
                value={memberIds}
                loading={memberSearching}
                placeholder="输入昵称、用户名或邮箱检索并勾选"
                notFoundContent={memberSearching ? '搜索中…' : null}
                onSearch={setMemberQuery}
                onChange={setMemberIds}
                options={[
                  ...memberOptions.map((u) => {
                    const nickname = (u.nickname ?? u.profile?.nickname) || u.username
                    return {
                      value: u.id,
                      label: (
                        <span className="user-search-option">
                          <strong>{nickname}</strong>
                          <span className="muted">{u.username} · {u.email}</span>
                        </span>
                      ),
                    }
                  }),
                  ...memberIds
                    .filter((id) => !memberOptions.some((u) => u.id === id))
                    .map((id) => ({ value: id, label: memberNameRef.get(id) ?? id })),
                ]}
              />
            </div>
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button type="primary" htmlType="submit" disabled={busy || !name.trim()} loading={busy}>
                创建
              </Button>
              <Button disabled={busy} onClick={() => { setCreateOpen(false); setName(''); setDesc(''); setMemberIds([]) }}>
                取消
              </Button>
            </div>
          </form>
        </Modal>
      )}
      {/* 编辑组弹窗（改名/描述）。 */}
      {editing && (
        <Modal title={`编辑用户组：${editing.name}`} onClose={() => setEditing(null)}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={(e) => void saveGroupEdit(e)}>
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
      {/* 邀请记录弹窗（v2.3 并入人员页，不再单独占管理导航位）。 */}
      {invitesOpen && (
        <Modal wide title="邀请记录" onClose={() => setInvitesOpen(false)}>
          <InvitationsPanel onError={onError} onNotice={onNotice} />
        </Modal>
      )}
    </div>
  )
}

function AuditPanel({
  onError,
  retentionDays,
  onSaveRetention,
}: {
  onError: (msg: string) => void
  /** audit.retention_days 当前值（null = 未加载）。 */
  retentionDays: number | null
  /** 保存保留期（0 = 永久）；成功后由宿主刷新设置。 */
  onSaveRetention: (days: number) => Promise<void>
}) {
  const [filters, setFilters] = useState<import('../api').AuditFilters>({})
  const [data, setData] = useState<import('../api').AuditListResult | null>(null)
  const [loading, setLoading] = useState(false)
  const [history, setHistory] = useState<string[]>([])
  const [expanded, setExpanded] = useState<number[]>([])
  // 保留期草稿（服务端 audit.retention_days；0 = 永久）。
  const [retentionDraft, setRetentionDraft] = useState<number | null>(retentionDays)
  const [retentionBusy, setRetentionBusy] = useState(false)
  useEffect(() => { setRetentionDraft(retentionDays) }, [retentionDays])
  const saveRetention = async () => {
    if (retentionBusy || retentionDraft === null || retentionDraft < 0) return
    setRetentionBusy(true)
    try {
      await onSaveRetention(retentionDraft)
    } finally {
      setRetentionBusy(false)
    }
  }
  const load = async (cursor = '', push = false) => {
    setLoading(true)
    try { setData(await adminListAuditLogs(filters, cursor)); if (push) setHistory((h) => [...h, cursor]) }
    catch (e) { onError(e instanceof Error ? e.message : '审计日志加载失败') }
    finally { setLoading(false) }
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
    { title: '操作', dataIndex: 'action', key: 'action', width: 170, render: (v: string) => <span title={v}>{auditActionText(v)}</span> },
    { title: '资源', key: 'resource', render: (_, entry) => `${entry.resource_type} ${entry.resource_id}` },
    {
      title: '状态',
      dataIndex: 'status',
      key: 'status',
      width: 90,
      render: (v: string) => (
        <span className={`badge ${v === 'success' ? 'available' : 'failed'}`} title={v}>
          {v === 'success' ? '成功' : v === 'failure' ? '失败' : v}
        </span>
      ),
    },
    { title: '时间', key: 'created_at', width: 160, render: (_, entry) => <span className="muted">{formatTime(entry.created_at)}</span> },
  ]
  return (
    <div className="panel setting-group audit-panel">
      <h3>审计日志</h3>
      {/* 保留期设置（audit.retention_days；0 = 永久，后台清理任务每日删过期）。 */}
      <div className="setting-row">
        <div className="setting-main">
          <div className="setting-key">
            审计日志保留期 <code className="setting-desc muted">audit.retention_days</code>
          </div>
          <div className="setting-desc muted">
            后台定时清理任务删除超过保留期的审计记录；0 = 永久保留。修改即时生效。
          </div>
        </div>
        <div className="setting-control">
          <InputNumber
            min={0}
            max={3650}
            step={1}
            precision={0}
            style={{ width: 110 }}
            value={retentionDraft}
            onChange={(v) => setRetentionDraft(v === null || v === undefined ? null : v)}
            addonAfter="天"
            aria-label="审计日志保留天数"
          />
          <Button
            size="small"
            type="primary"
            loading={retentionBusy}
            disabled={retentionBusy || retentionDraft === null || retentionDraft < 0 || retentionDraft === retentionDays}
            onClick={() => void saveRetention()}
          >
            保存
          </Button>
        </div>
      </div>
      <form className="audit-filters" onSubmit={(e) => { e.preventDefault(); setHistory([]); void load() }}>
        {field('action', '操作（如 auth.login）')}
        {field('userId', '用户 UUID')}
        <Select
          allowClear
          placeholder="全部状态"
          value={filters.status || undefined}
          onChange={(v) => setFilters({ ...filters, status: v ?? '' })}
          options={[
            { value: 'success', label: '成功（success）' },
            { value: 'failure', label: '失败（failure）' },
          ]}
        />
        {field('resourceType', '资源类型')}
        {field('resourceId', '资源 ID')}
        {field('from', '开始时间', 'datetime-local')}
        {field('to', '结束时间', 'datetime-local')}
        <Button type="primary" htmlType="submit" loading={loading}>筛选</Button>
        <Button onClick={() => void adminDownloadAuditCSV(filters)}>按当前筛选导出 CSV</Button>
      </form>
      <Table<AuditEntry>
        rowKey="id"
        columns={columns}
        dataSource={data?.items ?? []}
        loading={loading && data === null}
        pagination={false}
        scroll={{ x: 900 }}
        locale={{ emptyText: loading ? '加载中…' : '没有匹配的审计记录' }}
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
      {data && (
        <>
          <div className="setting-meta muted">共 {data.total} 条</div>
          <div className="pager">
            <Button size="small" disabled={history.length === 0 || loading} onClick={() => { const next = history.slice(0, -1); setHistory(next); void load(next[next.length - 1] ?? '') }}>上一页</Button>
            <Button size="small" disabled={!data.next_cursor || loading} onClick={() => void load(data.next_cursor, true)}>下一页</Button>
          </div>
        </>
      )}
    </div>
  )
}

/** 常见审计动作 → 中文标签（键名与后端 internal/audit 动作常量对齐；
 * 未命中回退原 action 串，原值经 title 提示）。 */
const AUDIT_ACTION_LABELS: Record<string, string> = {
  login_success: '登录成功', login_failure: '登录失败',
  upload_complete: '上传完成', purge: '彻底删除',
  share_create: '创建分享', share_revoke: '撤销分享', public_download: '分享下载',
  'version.create': '创建新版本', 'version.restore': '回滚版本', 'file.version.delete': '删除历史版本',
  'file.copy': '复制文件', 'file.convert_markdown': '转换为 Markdown',
  'settings.update': '系统设置变更',
  'janitor.upload': '清理·上传残留', 'janitor.blob': '清理·孤立对象', 'janitor.trash': '清理·回收站过期',
  'onlyoffice.save': 'Office 保存', 'onlyoffice.cleanup': 'Office 会话清理',
  'invite.create': '创建邀请', 'invite.revoke': '撤销邀请', 'invite.accept': '接受邀请',
  'auth.password_reset': '密码重置',
  'token.create': '创建令牌', 'token.revoke': '撤销令牌',
  'webhook.create': '创建 Webhook', 'webhook.delete': '删除 Webhook',
  'totp.enable': '启用两步验证', 'totp.disable': '禁用两步验证',
  'oidc.provision': 'SSO 开通账号',
  'user.update': '更新用户', 'user.reset_password': '管理员重置密码',
  'backup.verify': '校验备份',
  'quarantine.rescan': '重扫隔离对象', 'quarantine.release': '解除隔离', 'quarantine.delete': '删除隔离对象',
  'group.create': '创建用户组', 'group.update': '更新用户组', 'group.delete': '删除用户组',
  'group.member.add': '组成员加入', 'group.member.remove': '组成员移出',
  'space.delete': '解散空间', 'space.leave': '离开空间', 'space.owner_transfer': '转让空间所有权',
  'space.admin_update': '管理员改空间', 'space.group.add': '空间加用户组', 'space.group.update': '空间改组角色', 'space.group.remove': '空间移除用户组',
  'acl.update': '目录权限变更',
  'tls.update': 'TLS 模式切换', 'tls.cert_upload': '上传自定义证书',
}

function auditActionText(action: string): string {
  return AUDIT_ACTION_LABELS[action] ?? action
}

/** 字节数的人类可读表示（备份文件/总大小展示；v2.6 统一 formatQuota 口径
 *  B→KiB/MiB/GiB/TiB、1 位小数）。 */
function formatBytes(n: number): string {
  return formatQuota(n, false)
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

/** 凭据状态卡片（G6）：各密钥类 env 的已配置/未配置徽章（只读探针，不回显值）。
 * v2.2 自查结论：本卡为**环境态只读信息**——密钥值从不离开服务端（任何 API 都
 * 不回显），故保持只读；每项补充「在哪改」的指引：SMTP 密码可在「邮件」面板
 * 运行时修改，其余须改部署 .env 后重启 backend。 */
function SecretsPanel({ secrets }: { secrets: Record<string, boolean> }) {
  if (!secrets || Object.keys(secrets).length === 0) return null
  /** 各密钥的配置指引（值只读，但告诉管理员在哪改）。 */
  const hints: Record<string, string> = {
    jwt_secret: '修改：部署 .env 的 JWT_SECRET（重启 backend 生效；变更会使全部现有会话与 PAT 失效）',
    smtp_password: '修改：可在「设置 → 邮件配置」运行时设置（保存即时生效），或 .env 的 SMTP_PASS',
    s3_secret_key: '修改：部署 .env 的 S3_SECRET_KEY（STORAGE_DRIVER=s3 时必需；重启生效）',
    onlyoffice_jwt_secret: '修改：部署 .env 的 ONLYOFFICE_JWT_SECRET（须与 DocumentServer 一致；重启生效）',
  }
  return (
    <div className="panel setting-group">
      <h3>凭据状态</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        密钥类凭据一律走环境变量（不入 system_settings），此处为**只读探针**：仅展示各环境变量
        是否已配置，值绝不回显（服务端任何接口都不返回密钥明文）。未配置的密钥在对应功能
        启用时将不可用。
      </div>
      {secretLabels.map(({ key, env, label }) => {
        const configured = Boolean(secrets[key])
        return (
          <div key={key} className="setting-row">
            <div className="setting-main">
              <div className="setting-key">
                {label} <code className="setting-desc muted">{env}</code>
              </div>
              {hints[key] && <div className="setting-desc muted">{hints[key]}</div>}
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
  /** v2.2：文件名/SHA 检索（前端过滤）+ 多选批量重扫/删除。 */
  const [query, setQuery] = useState('')
  const [selected, setSelected] = useState<Key[]>([])
  const [batchBusy, setBatchBusy] = useState(false)

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
      setSelected((prev) => prev.filter((k) => k !== item.sha256))
      await load()
    } catch (err) {
      onError(err instanceof Error ? err.message : '操作失败')
    } finally {
      setBusy(null)
    }
  }

  /** 批量重扫/删除（逐项调用，部分成功语义；删除为不可逆，双重确认）。 */
  const runBatch = async (action: 'rescan' | 'delete') => {
    const shaList = selected.map(String)
    if (shaList.length === 0 || batchBusy) return
    const ok = await confirmDialog(modal, {
      title: action === 'rescan' ? '批量重扫' : '批量删除隔离对象',
      content: action === 'rescan'
        ? `对选中的 ${shaList.length} 个对象重新执行安全扫描？通过者恢复可用。`
        : `确认删除选中的 ${shaList.length} 个隔离对象？将解除全部版本引用并物理删除，不可恢复。`,
      okText: action === 'rescan' ? '重扫' : '删除',
      danger: action === 'delete',
      cancelText: '取消',
    })
    if (!ok) return
    setBatchBusy(true)
    onError('')
    try {
      const results = await Promise.allSettled(shaList.map((sha) => adminQuarantineAction(sha, action, { confirm: true })))
      const failed = results.filter((r) => r.status === 'rejected').length
      if (failed > 0) onError(`批量${action === 'rescan' ? '重扫' : '删除'}：成功 ${shaList.length - failed} 项，失败 ${failed} 项`)
      else onNotice(`批量${action === 'rescan' ? '重扫' : '删除'}完成：${shaList.length} 项`)
      setSelected([])
      await load()
    } finally {
      setBatchBusy(false)
    }
  }

  const needle = query.trim().toLowerCase()
  const visible = (items ?? []).filter((it) =>
    needle === ''
    || (it.file_name ?? '').toLowerCase().includes(needle)
    || it.sha256.toLowerCase().includes(needle))

  return (
    <div className="panel setting-group">
      <h3>隔离区</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        安全扫描未通过的内容对象（object_blobs status=quarantined）：重扫（重新入
        扫描，通过恢复可用）、解除隔离（需勾选确认，误报场景）、删除（解除全部版本
        引用并物理删除，不可恢复）。全部操作均记录审计。
      </div>
      <div className="member-list-toolbar" style={{ marginBottom: 12 }}>
        <Input
          allowClear
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="按文件名 / SHA-256 检索…"
          style={{ width: 240 }}
          aria-label="隔离区检索"
        />
        {selected.length > 0 && (
          <>
            <Button disabled={batchBusy || busy !== null} loading={batchBusy} onClick={() => void runBatch('rescan')}>
              批量重扫（{selected.length}）
            </Button>
            <Button danger disabled={batchBusy || busy !== null} loading={batchBusy} onClick={() => void runBatch('delete')}>
              批量删除（{selected.length}）
            </Button>
          </>
        )}
      </div>
      {items === null ? (
        <div className="empty">隔离区加载中…</div>
      ) : visible.length === 0 ? (
        <div className="empty">{needle ? '没有匹配的隔离对象' : '当前没有隔离中的内容对象'}</div>
      ) : (
        <Table<QuarantineItem>
          rowKey="sha256"
          pagination={false}
          dataSource={visible}
          scroll={{ x: 900 }}
          rowSelection={{
            selectedRowKeys: selected,
            onChange: setSelected,
            getCheckboxProps: () => ({ disabled: batchBusy || busy !== null }),
          }}
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
                { value: 'user', label: '普通用户（user）' },
                { value: 'admin', label: '管理员（admin）' },
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
                { value: 'active', label: '正常（active）' },
                { value: 'disabled', label: '已禁用（disabled）' },
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

/** 空间管理卡片（v2.0 统一空间模型，v2.3 增「已解散」筛选与彻底删除）：
 * 名称/所有者检索列表 + 越权编辑（名称/描述/配额，不受单空间配额上限
 * 约束）+ 解散（软删，含多选批量）；「已解散」视图列出软删空间并可
 * **彻底删除**（物理删文件/成员/分享，输入空间名二次确认）；默认空间
 * 不可解散。 */
function SpacesPanel({ onError, onNotice }: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const { modal } = AntdApp.useApp()
  const [spaces, setSpaces] = useState<AdminSpaceItem[] | null>(null)
  const [q, setQ] = useState('')
  const [busy, setBusy] = useState(false)
  /** 视图切换：active = 正常空间（默认）；dissolved = 已解散（软删）。 */
  const [view, setView] = useState<'active' | 'dissolved'>('active')
  /** 批量解散勾选（默认空间行禁选；仅 active 视图）。 */
  const [selectedSpaces, setSelectedSpaces] = useState<Key[]>([])
  const [editing, setEditing] = useState<AdminSpaceItem | null>(null)
  const [editName, setEditName] = useState('')
  const [editDesc, setEditDesc] = useState('')
  // 配额为字节数（0=不限），经 QuotaInput（数值+单位，1024 进制）编辑。
  const [editQuota, setEditQuota] = useState(0)

  const load = async (query: string, dissolved = view === 'dissolved') => {
    setBusy(true)
    try {
      setSpaces(await adminListSpaces(query.trim(), 100, dissolved))
    } catch (err) {
      onError(err instanceof Error ? err.message : '空间列表加载失败')
      setSpaces([])
    } finally {
      setBusy(false)
    }
  }

  useEffect(() => {
    void load('', view === 'dissolved')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view])

  const submit = (e: FormEvent) => {
    e.preventDefault()
    void load(q)
  }

  const saveEdit = async (e: FormEvent) => {
    e.preventDefault()
    if (!editing || busy || !editName.trim()) return
    if (!Number.isInteger(editQuota) || editQuota < 0) {
      onError('配额须为 ≥0 的数值（0=不限）')
      return
    }
    setBusy(true)
    try {
      await adminUpdateSpace(editing.id, {
        name: editName.trim(),
        description: editDesc.trim(),
        quotaBytes: editQuota,
      })
      setEditing(null)
      onNotice('空间已更新')
      await load(q)
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        setEditing(null)
        await load(q)
      } else {
        onError(err instanceof Error ? err.message : '更新空间失败')
      }
    } finally {
      setBusy(false)
    }
  }

  const dissolve = async (s: AdminSpaceItem) => {
    const ok = await confirmDialog(modal, {
      title: '解散空间',
      content: `确定解散空间「${s.name}」？空间内全部文件将被删除，不可恢复。`,
      okText: '解散',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBusy(true)
    try {
      await adminDeleteSpace(s.id)
      onNotice(`已解散空间「${s.name}」`)
      setSelectedSpaces((prev) => prev.filter((k) => k !== s.id))
      await load(q)
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load(q)
      else onError(err instanceof Error ? err.message : '解散空间失败')
    } finally {
      setBusy(false)
    }
  }

  /** 批量解散（v2.2：勾选 + 确认；默认空间自动跳过）。 */
  const batchDissolve = async () => {
    const targets = (spaces ?? []).filter((s) => selectedSpaces.includes(s.id) && !s.is_default)
    if (targets.length === 0 || busy) return
    const ok = await confirmDialog(modal, {
      title: '批量解散空间',
      content: `确定解散选中的 ${targets.length} 个空间？空间内全部文件将被删除，不可恢复。`,
      okText: '解散',
      danger: true,
      cancelText: '取消',
    })
    if (!ok) return
    setBusy(true)
    onError('')
    try {
      const results = await Promise.allSettled(targets.map((s) => adminDeleteSpace(s.id)))
      const failed = results.filter((r) => r.status === 'rejected').length
      if (failed > 0) onError(`批量解散：成功 ${targets.length - failed} 个，失败 ${failed} 个`)
      else onNotice(`已解散 ${targets.length} 个空间`)
      setSelectedSpaces([])
      await load(q)
    } finally {
      setBusy(false)
    }
  }

  /** 彻底删除已解散空间（物理删文件/成员/分享）：输入空间名二次确认。 */
  const purge = async (s: AdminSpaceItem) => {
    const name = await promptViaModal(modal, {
      title: `彻底删除空间：${s.name}`,
      label: `此操作将物理删除空间内全部文件、成员与分享记录，不可恢复。请输入空间名「${s.name}」确认：`,
      placeholder: s.name,
      okText: '彻底删除',
      cancelText: '取消',
    })
    if (name === null || name !== s.name) {
      if (name !== null) onError('输入的空间名不匹配，已取消彻底删除')
      return
    }
    setBusy(true)
    try {
      await adminPurgeSpace(s.id)
      onNotice(`已彻底删除空间「${s.name}」`)
      await load(q)
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) await load(q)
      else onError(err instanceof Error ? err.message : '彻底删除空间失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="panel setting-group">
      <h3>空间</h3>
      <div className="setting-desc muted" style={{ marginBottom: 12 }}>
        全站空间清单：admin 可越权修改名称/描述/配额并解散空间；默认空间为
        注册自动创建，可改名不可解散。解散后进入「已解散」视图，可彻底删除（物理删除文件/成员/分享，不可恢复）。
      </div>
      <form className="team-create-row" style={{ marginBottom: 12 }} onSubmit={submit}>
        <label className="field" style={{ flex: 1 }}>
          <span>按空间名 / 所有者用户名检索</span>
          <Input allowClear value={q} onChange={(e) => setQ(e.target.value)} placeholder="如：研发 或 admin" />
        </label>
        <Button type="primary" htmlType="submit" disabled={busy} loading={busy}>搜索</Button>
        {view === 'active' && selectedSpaces.length > 0 && (
          <Button danger disabled={busy} loading={busy} onClick={() => void batchDissolve()}>
            批量解散（{selectedSpaces.filter((id) => spaces?.find((s) => s.id === id && !s.is_default)).length}）
          </Button>
        )}
        <div style={{ display: 'flex', alignItems: 'flex-end' }}>
          <Segmented
            value={view}
            onChange={(v) => { setSelectedSpaces([]); setView(v as 'active' | 'dissolved') }}
            options={[
              { label: '正常', value: 'active' },
              { label: '已解散', value: 'dissolved' },
            ]}
          />
        </div>
      </form>
      {spaces === null ? (
        <div className="hint">加载中…</div>
      ) : (
        <Table<AdminSpaceItem>
          rowKey="id"
          pagination={false}
          dataSource={spaces}
          locale={{ emptyText: view === 'dissolved' ? '没有已解散的空间' : '没有匹配的空间' }}
          scroll={{ x: 900 }}
          rowSelection={view === 'active' ? {
            selectedRowKeys: selectedSpaces,
            onChange: setSelectedSpaces,
            // 默认空间不可解散，禁选防误勾。
            getCheckboxProps: (s) => ({ disabled: s.is_default || busy }),
          } : undefined}
          columns={[
            {
              title: '名称',
              dataIndex: 'name',
              key: 'name',
              render: (_, s) => (
                <span>
                  {s.name}
                  {s.is_default && <span className="badge" style={{ marginLeft: 8 }}>默认</span>}
                  {view === 'dissolved' && <span className="badge failed" style={{ marginLeft: 8 }}>已解散</span>}
                </span>
              ),
            },
            { title: '所有者', key: 'owner', render: (_, s) => <span className="muted">{s.owner_username || s.owner_id.slice(0, 8)}</span> },
            { title: '成员数', dataIndex: 'member_count', key: 'member_count', width: 90, render: (v: number | undefined) => v ?? '—' },
            { title: '配额', key: 'quota', width: 110, render: (_, s) => <span className="muted">{formatQuota(s.quota_bytes)}</span> },
            { title: '已用', key: 'used', width: 110, render: (_, s) => <span className="muted">{formatQuota(s.storage_used ?? 0, false)}</span> },
            view === 'dissolved'
              ? { title: '解散时间', key: 'deleted_at', width: 170, render: (_, s) => <span className="muted">{s.deleted_at ? formatTime(s.deleted_at) : '—'}</span> }
              : { title: '创建时间', key: 'created_at', width: 170, render: (_, s) => <span className="muted">{formatTime(s.created_at)}</span> },
            {
              title: '操作',
              key: 'actions',
              width: 150,
              render: (_, s) => (
                <div className="table-actions">
                  {view === 'active' ? (
                    <>
                      <Button
                        size="small"
                        disabled={busy}
                        onClick={() => { setEditing(s); setEditName(s.name); setEditDesc(s.description); setEditQuota(s.quota_bytes ?? 0) }}
                      >
                        编辑
                      </Button>
                      {!s.is_default && <Button size="small" danger disabled={busy} onClick={() => void dissolve(s)}>解散</Button>}
                    </>
                  ) : (
                    <Button size="small" danger disabled={busy} onClick={() => void purge(s)}>彻底删除</Button>
                  )}
                </div>
              ),
            },
          ]}
        />
      )}
      {editing && (
        <Modal title={`编辑空间：${editing.name}`} onClose={() => setEditing(null)}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={saveEdit}>
            <label className="field">
              <span>名称</span>
              <Input maxLength={100} required value={editName} onChange={(e) => setEditName(e.target.value)} />
            </label>
            <label className="field">
              <span>描述（留空清除）</span>
              <Input value={editDesc} onChange={(e) => setEditDesc(e.target.value)} />
            </label>
            <div className="field">
              <span>配额（0 = 不限）</span>
              <QuotaInput value={editQuota} onChange={setEditQuota} />
            </div>
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

const adminSections = [
  ['overview', '概览'], ['people', '人员与组'], ['spaces', '空间'], ['audit', '审计日志'], ['threat', '威胁防护'], ['backup', '备份'],
  // 平台设置拆分（v2.x）：AI / 创作舱 / 邮件 / TLS / 系统设置 各自独立分区，
  // 不再堆一个大 tag（用户反馈）；v2.8 增第六个 tag「配置总览」
  //（ConfigOverviewPanel：启动级 env / 运行时设置索引 / AI 能力状态）；
  // v2.9 增第七个 tag「安全与访问」（SecurityPanel：防爆破 / 限流 / 扫描
  // 策略 / WebDAV 平台开关与接入指引，对应 security.* / webdav.* 键）。
  ['ai', 'AI 设置'], ['agent', 'AI 创作舱'], ['mail', '邮件'], ['tls', 'TLS'], ['system', '系统设置'], ['security', '安全与访问'], ['config', '配置总览'],
] as const

export default function AdminPage() {
  const { section = 'overview' } = useParams()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [stats, setStats] = useState<AdminStats | null>(null)
  const [secrets, setSecrets] = useState<Record<string, boolean>>({})
  /** audit.retention_days 当前值（null = 尚未加载；0 = 永久）。 */
  const [auditRetentionDays, setAuditRetentionDays] = useState<number | null>(null)
  const [loading, setLoading] = useState(true)
  const [forbidden, setForbidden] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      const [result, st] = await Promise.all([adminGetSettings(), adminGetStats()])
      setSecrets(result.secrets ?? {})
      const retention = (result.settings ?? []).find((item) => item.key === 'audit.retention_days')
      setAuditRetentionDays(retention ? Number(retention.value) : 0)
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

  /** 保存审计保留期后刷新设置（拿回服务端归一化值）。 */
  const saveAuditRetention = async (days: number) => {
    const normalized = await adminPutSetting('audit.retention_days', days)
    setNotice(`已保存审计日志保留期：${normalized === 0 ? '永久保留' : `${normalized} 天`}`)
    try {
      const refreshed = await adminGetSettings()
      const retention = (refreshed.settings ?? []).find((item) => item.key === 'audit.retention_days')
      setAuditRetentionDays(retention ? Number(retention.value) : 0)
    } catch {
      setAuditRetentionDays(Number(normalized))
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
        <PeopleAndGroupsPanel
          onError={(m) => { setError(m); setNotice('') }}
          onNotice={(m) => { setNotice(m); setError('') }}
        />
      )}

      {section === 'spaces' && !loading && !forbidden && (
        <>
          {/* v3.1 归并：space.*（新空间默认配额 / 配额上限 / 每用户空间数）
              自「系统设置」迁入本页，与空间清单同处单一编辑入口。 */}
          <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
            <SystemSettingKeysCard
              prefixes={['space']}
              title={{ zh: '空间策略（新空间默认值与上限）', en: 'Space policy (defaults & limits)' }}
              onError={(m) => { setError(m); setNotice('') }}
              onNotice={(m) => { setNotice(m); setError('') }}
            />
          </Suspense>
          <SpacesPanel
            onError={(m) => { setError(m); setNotice('') }}
            onNotice={(m) => { setNotice(m); setError('') }}
          />
        </>
      )}

      {section === 'audit' && !loading && !forbidden && (
        <AuditPanel
          onError={(m) => { setError(m); setNotice('') }}
          retentionDays={auditRetentionDays}
          onSaveRetention={saveAuditRetention}
        />
      )}
      {section === 'backup' && !loading && !forbidden && (
        <>
          {/* v3.1 归并：backup.*（任务开关 / 保留天数 / 加密要求等）自
             「系统设置」迁入本页，与备份状态同处单一编辑入口。 */}
          <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
            <SystemSettingKeysCard
              prefixes={['backup']}
              title={{ zh: '备份策略', en: 'Backup policy' }}
              onError={(m) => { setError(m); setNotice('') }}
              onNotice={(m) => { setNotice(m); setError('') }}
            />
          </Suspense>
          <BackupPanel onError={(m) => { setError(m); setNotice('') }} onNotice={(m) => { setNotice(m); setError('') }} />
        </>
      )}
      {section === 'threat' && !loading && !forbidden && <QuarantinePanel onError={(m) => { setError(m); setNotice('') }} onNotice={(m) => { setNotice(m); setError('') }} />}
      {section === 'threat' && !loading && !forbidden && <SecretsPanel secrets={secrets} />}
      {/* 平台设置拆分分区（v2.x）：各自独立 tag（面板自 SettingsPage 导出
          复用），仅管理员可达；旧地址 /admin/platform 重定向至 ai。v2.7 起
          五面板均为 React.lazy 懒加载（见文件头），Suspense 兜底加载态。 */}
      {section === 'ai' && !loading && !forbidden && (
        <div className="admin-platform-stack">
          <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
            <AISettingsPanel
              onError={(m) => { setError(m); setNotice('') }}
              onNotice={(m) => { setNotice(m); setError('') }}
            />
          </Suspense>
        </div>
      )}
      {section === 'agent' && !loading && !forbidden && (
        <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
          <AgentPanel
            onError={(m) => { setError(m); setNotice('') }}
            onNotice={(m) => { setNotice(m); setError('') }}
          />
        </Suspense>
      )}
      {section === 'mail' && !loading && !forbidden && (
        <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
          <MailPanel
            onNotice={(m) => { setNotice(m); setError('') }}
            onError={(m) => { setError(m); setNotice('') }}
          />
        </Suspense>
      )}
      {section === 'tls' && !loading && !forbidden && (
        <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
          <TlsPanel onNotice={(m) => { setNotice(m); setError('') }} />
        </Suspense>
      )}
      {section === 'system' && !loading && !forbidden && (
        <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
          <SystemSettingsPanel
            onError={(m) => { setError(m); setNotice('') }}
            onNotice={(m) => { setNotice(m); setError('') }}
          />
        </Suspense>
      )}
      {/* v2.9 第七个平台设置 tag：安全与访问（登录防爆破 / 认证限流 / 扫描
          策略 + WebDAV 平台开关与接入指引；对应 security.* / webdav.* 键，
          这些键已从「系统设置」排除以保持单一入口）。 */}
      {section === 'security' && !loading && !forbidden && (
        <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
          <SecurityPanel
            onError={(m) => { setError(m); setNotice('') }}
            onNotice={(m) => { setNotice(m); setError('') }}
          />
        </Suspense>
      )}
      {/* v2.8 第六个平台设置 tag：配置总览（启动级 env 只读 / 运行时设置
          分类索引 / AI 能力状态；env 端点未就绪时占位降级）。 */}
      {section === 'config' && !loading && !forbidden && (
        <Suspense fallback={<div className="hint">{msg('loading')}</div>}>
          <ConfigOverviewPanel onError={(m) => { setError(m); setNotice('') }} />
        </Suspense>
      )}
      </div>
    </div>
  )
}
