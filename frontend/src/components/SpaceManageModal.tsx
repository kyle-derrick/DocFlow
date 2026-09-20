// 空间管理综合弹窗（统一空间模型 v2.1，由 SpaceMembersModal 升级）：
// 布局仿系统设置——antd Modal 宽（min(880px,92vw)，body 70vh）+ 左侧竖向
// 导航（antd Menu inline）+ 右侧内容区。Tab：
// - 成员：成员表全功能（搜索 / 角色下拉 / 批量改角色 / 批量移出 / 移出 /
//   owner 行转让所有权）；直接成员与经用户组加入的用户合并展示（组来源
//   标注「组」徽标；组员只读——改角色在「用户组」tab 调整）；转让 owner
//   候选=成员列表全部用户（含组内，后端会先按组角色落直接成员再转让）；
// - 用户组：添加用户组一行式（系统 admin 下拉全部组，其余手输组 UUID）+
//   角色 + 添加按钮；授权表（组名 / 角色 / 组内成员数 / 移除）；用户组与
//   直接成员并存时权限取最高；
// - 邀请（owner/admin）：添加已有用户一行式（远程搜索多选 + 角色 + 按钮）
//   + 邮箱邀请一行式（邮箱 + 角色 + 按钮）+ 邀请记录表（邮箱 / 角色 /
//   发送时间 / 状态 / 复制链接 / 撤销）；
// - 空间设置（owner/admin）：名称 / 描述 / 配额进度条与修改（配额受系统
//   上限约束，越权由后端校验）+ 危险区（转让所有权、解散空间——输入空间
//   名二次确认；默认空间后端 400）。
// 权限：设置类 tab（邀请 / 空间设置）仅 owner/admin 可见；其他成员打开弹窗
// 只见成员 / 用户组两个只读 tab。
// 边界与后端一致：admin 角色仅 owner 可授予；非 owner 的 admin 不可改派/
// 移除其他 admin；owner 成员不可改派/移除。
import { FormEvent, Key, useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { AlertTriangle, Search, Settings, UserPlus, Users, UsersRound } from 'lucide-react'
import { App as AntdApp, Avatar, Button, Input, Menu, Modal as AntdModal, Select, Table, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  ASSIGNABLE_SPACE_ROLES,
  AssignableSpaceRole,
  Group,
  Space,
  SpaceGroup,
  SpaceGroupUser,
  SpaceInvite,
  SpaceMember,
  SpaceRole,
  UserInfoResult,
  UserSearchResult,
  addSpaceGroup,
  addSpaceMember,
  adminListGroups,
  createSpaceInvite,
  deleteSpace,
  getUserInfo,
  listSpaceGroups,
  listSpaceInvites,
  listSpaceMembers,
  removeSpaceGroup,
  removeSpaceMember,
  revokeSpaceInvite,
  searchUsers,
  transferSpaceOwnership,
  updateSpace,
  updateSpaceGroupRole,
  updateSpaceMemberRole,
  updateSpaceQuota,
} from '../api'
import { formatQuota, formatTime } from './FileBrowser'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 空间管理弹窗的 tab 标识（左导航项 key）。v2.4：独立「邀请」tab 移除——
 * 添加用户/创建邀请两行并入成员 tab 顶部，邀请记录改由标题行按钮弹独立 Modal。 */
export type SpaceManageTab = 'members' | 'groups' | 'settings'

/** 角色 → 展示名 i18n key（成员表 / 成员栏 / 卡片徽章共用）。 */
export const ROLE_LABEL_KEYS: Record<SpaceRole, MessageKey> = {
  owner: 'roleOwnerLabel',
  admin: 'roleAdminLabel',
  member_share: 'roleMemberShareLabel',
  member: 'roleMemberLabel',
  guest: 'roleGuestLabel',
}

/** 角色 → 权限说明 i18n key（下拉与徽章 tooltip 共用）。 */
export const ROLE_TIP_KEYS: Record<SpaceRole, MessageKey> = {
  owner: 'roleOwnerTip',
  admin: 'roleAdminTip',
  member_share: 'roleMemberShareTip',
  member: 'roleMemberTip',
  guest: 'roleGuestTip',
}

/** 成员显示名：nickname 优先，回退 username，再回退 UUID 前 8 位。 */
export function memberDisplayName(m: SpaceMember): string {
  return m.nickname || m.username || `${m.user_id.slice(0, 8)}…`
}

/** 由 UUID 稳定生成的头像底色（HSL 拉开色相，饱和度/亮度受限保证可读）。 */
export function avatarColor(id: string): string {
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) | 0
  return `hsl(${Math.abs(hash) % 360}, 55%, 45%)`
}

/**
 * 用户只读信息弹窗（v2.4 整改项 4）：成员列表行点击查看——头像/姓名/邮箱/
 * 部门/职位，全部只读（GET /users/:id）。显示名回退顺序与成员列表一致。
 */
export function UserInfoModal({
  userId,
  fallbackName,
  onClose,
}: {
  userId: string
  /** 加载前/失败时的显示名回退（成员行已知的昵称/用户名）。 */
  fallbackName: string
  onClose: () => void
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const [info, setInfo] = useState<UserInfoResult | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(true)
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    let alive = true
    setBusy(true)
    setError('')
    void getUserInfo(userId)
      .then((u) => { if (alive) setInfo(u) })
      .catch((err) => { if (alive) setError(err instanceof Error ? err.message : zh ? '用户信息加载失败' : 'Failed to load user') })
      .finally(() => { if (alive) setBusy(false) })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [userId])

  const display = info?.nickname || info?.username || fallbackName || (zh ? '用户' : 'User')
  const copyEmail = async () => {
    if (!info?.email) return
    try {
      await navigator.clipboard.writeText(info.email)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      /* 剪贴板不可用：保持选中态供手动复制 */
    }
  }

  return (
    <AntdModal
      open
      centered
      footer={null}
      width="min(440px, 92vw)"
      title={zh ? '用户信息' : 'User info'}
      styles={{ body: { paddingTop: 16 } }}
      onCancel={onClose}
    >
      <div className="profile-info-modal">
        {busy && <p className="hint">{zh ? '加载中…' : 'Loading…'}</p>}
        {error && !busy && <div className="error-text">{error}</div>}
        {info && (
          <>
            <div className="profile-info-head">
              <span className="avatar" style={{ width: 48, height: 48, fontSize: 22, background: avatarColor(info.id) }}>{display.slice(0, 1).toUpperCase()}</span>
              <div>
                <div className="profile-info-name">{display}</div>
                <div className="muted profile-info-sub">{info.username}</div>
              </div>
            </div>
            <dl className="profile-info-list">
              <div><dt>{zh ? '邮箱' : 'Email'}</dt>
                <dd title={info.email} style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                  <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{info.email || '—'}</span>
                  {info.email && (
                    <Button size="small" type="text" onClick={() => void copyEmail()}>{copied ? '✓' : (zh ? '复制' : 'Copy')}</Button>
                  )}
                </dd>
              </div>
              <div><dt>{zh ? '部门' : 'Dept'}</dt><dd>{info.department || '—'}</dd></div>
              <div><dt>{zh ? '职位' : 'Position'}</dt><dd>{info.position || '—'}</dd></div>
              <div><dt>ID</dt><dd className="setting-value-mono" title={info.id}>{info.id}</dd></div>
            </dl>
          </>
        )}
        <div className="modal-actions">
          <Button onClick={onClose}>{zh ? '关闭' : 'Close'}</Button>
        </div>
      </div>
    </AntdModal>
  )
}

/**
 * 成员表合并行（v2.3）：直接成员 + 经用户组加入的用户（组来源标注）。
 * - isDirect=true：直接成员（角色可管理，member 保留原行供角色/移除操作）；
 * - isDirect=false：仅经用户组加入（只读组员，改角色须在「用户组」tab；
 *   owner 可转让空间所有权——后端先按组角色落直接成员再转让）。
 * viaGroups 为该用户命中的全部组授权（直接成员命中组时也标注）。
 */
interface MemberRow {
  user_id: string
  display: string
  email: string
  isDirect: boolean
  /** 直接成员角色；组员为 null（展示取组内最高角色）。 */
  role: SpaceRole | null
  /** 组内最高角色（展示用；无组授权为 null）。 */
  groupRole: AssignableSpaceRole | null
  viaGroups: Array<{ id: string; name: string; role: AssignableSpaceRole }>
  joinedAt?: string
  /** 直接成员原始行（角色下拉/移出/转让回调用）。 */
  member?: SpaceMember
}

export default function SpaceManageModal({
  space,
  myRole,
  isOwner,
  initialTab = 'members',
  onClose,
  onChanged,
}: {
  space: Space
  /** 我在空间的直接角色（owner/admin/member_share/member/guest；null=未知）。 */
  myRole: SpaceRole | null
  /** 当前用户是否空间 owner（admin 授予/转让边界）。 */
  isOwner: boolean
  /** 初始 tab；非 owner/admin 传入设置类 tab 时回退「成员」。 */
  initialTab?: SpaceManageTab
  onClose: () => void
  /** 成员/用户组/设置变化后回调（宿主刷新空间数据等）。 */
  onChanged?: () => void
}) {
  const locale = useLocale()
  const navigate = useNavigate()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const zh = locale === 'zh-CN'
  // 管理权（邀请/设置 tab 可见性 + 各 tab 操作可用性）。
  const canManage = isOwner || myRole === 'admin'
  const [tab, setTab] = useState<SpaceManageTab>(
    initialTab === 'members' || initialTab === 'groups' ? initialTab : canManage ? initialTab : 'members',
  )
  // 用户只读信息弹窗（成员行点击查看，v2.4）。
  const [viewUser, setViewUser] = useState<{ id: string; name: string } | null>(null)
  // 邀请记录独立弹窗（v2.4：标题行「邀请记录」按钮打开）。
  const [invitesOpen, setInvitesOpen] = useState(false)
  const [resendingInviteId, setResendingInviteId] = useState('')
  const [members, setMembers] = useState<SpaceMember[]>([])
  /** 经用户组加入的用户条目（合并展示/转让候选）。 */
  const [groupUsers, setGroupUsers] = useState<SpaceGroupUser[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [memberQuery, setMemberQuery] = useState('')
  // 批量操作勾选（可管理成员 user_id）。
  const [selected, setSelected] = useState<Key[]>([])
  const [batchBusy, setBatchBusy] = useState(false)
  const [batchRole, setBatchRole] = useState<AssignableSpaceRole>('member')

  // ---- 用户组（space_group_members：直接成员 ∪ 组取最高） ----
  const [groups, setGroups] = useState<SpaceGroup[]>([])
  const [allGroups, setAllGroups] = useState<Group[]>([])
  const [groupQuery, setGroupQuery] = useState('')
  const [groupRole, setGroupRole] = useState<AssignableSpaceRole>('member')
  const [groupBusy, setGroupBusy] = useState(false)

  // ---- 邀请（已有用户搜索多选 + 邮箱邀请） ----
  const [inviteUserIds, setInviteUserIds] = useState<string[]>([])
  const [inviteQuery, setInviteQuery] = useState('')
  const [inviteOptions, setInviteOptions] = useState<UserSearchResult[]>([])
  const [inviteSearching, setInviteSearching] = useState(false)
  const [inviteRole, setInviteRole] = useState<AssignableSpaceRole>('member')
  const [inviteBusy, setInviteBusy] = useState(false)
  // 已选用户显示名缓存（id → 名），多选框回显。
  const inviteNameRef = useMemo(() => new Map<string, string>(), [])
  // 邮箱邀请表单。
  const [inviteEmail, setInviteEmail] = useState('')
  const [emailBusy, setEmailBusy] = useState(false)
  // 邀请记录 + 本会话创建的 token（明文仅创建响应一次，缓存供复制链接）。
  const [invites, setInvites] = useState<SpaceInvite[]>([])
  const [inviteTokens, setInviteTokens] = useState<Record<string, string>>({})
  const [copiedInviteId, setCopiedInviteId] = useState('')

  // ---- 空间设置（owner/admin）：名称/描述/配额 + 危险区 ----
  const [settingsName, setSettingsName] = useState(space.name)
  const [settingsDesc, setSettingsDesc] = useState(space.description)
  const [settingsQuota, setSettingsQuota] = useState(String(space.quota_bytes ?? 0))
  const [settingsBusy, setSettingsBusy] = useState(false)
  const [settingsError, setSettingsError] = useState('')
  const [settingsNotice, setSettingsNotice] = useState('')
  // 危险区：转让所有权（选既有非 owner 成员）。
  const [transferTarget, setTransferTarget] = useState('')

  // 解散空间危险区（输入空间名二次确认）。
  const [dissolveOpen, setDissolveOpen] = useState(false)
  const [dissolveName, setDissolveName] = useState('')
  const [dissolveBusy, setDissolveBusy] = useState(false)

  const loadMembers = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const r = await listSpaceMembers(space.id)
      setMembers(r.members)
      setGroupUsers(r.group_users)
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('membersLoadFailed'))
    } finally {
      setLoading(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [space.id, locale])

  const loadInvites = useCallback(async () => {
    try {
      setInvites(await listSpaceInvites(space.id))
    } catch {
      /* 邀请列表加载失败不阻塞成员管理。 */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [space.id])

  const loadGroups = useCallback(async () => {
    try {
      setGroups(await listSpaceGroups(space.id))
    } catch {
      /* 用户组列表加载失败不阻塞成员管理。 */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [space.id])

  useEffect(() => {
    void loadMembers()
    if (canManage) void loadInvites()
    void loadGroups()
    // 系统 admin 可拉取全部用户组供下拉（403 时降级手输 UUID）。
    if (canManage) void adminListGroups().then(setAllGroups).catch(() => setAllGroups([]))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [space.id, canManage])

  // 设置表单随空间切换初始化（同空间内的外部变更以表单值为准，不回写）。
  useEffect(() => {
    setSettingsName(space.name)
    setSettingsDesc(space.description)
    setSettingsQuota(String(space.quota_bytes ?? 0))
    setTransferTarget('')
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [space.id])

  // Esc 兜底关闭：本弹窗常从 Dropdown 菜单打开，antd 会把焦点还给触发按钮，
  // rc-dialog 的 wrap 不持焦点时 Esc 无法关闭——document 级监听保证可用。
  // 有更上层弹窗（如解散确认 antd confirm）时让位，由其自行处理 Esc。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      const foreignOpen = Array.from(document.querySelectorAll<HTMLElement>('.ant-modal-wrap')).some(
        (w) => w.style.display !== 'none' && !w.querySelector('.space-manage-content'),
      )
      if (!foreignOpen) onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  // 用户远程搜索（防抖 300ms，≥2 字）。
  useEffect(() => {
    const q = inviteQuery.trim()
    if (q.length < 2) {
      setInviteOptions([])
      return
    }
    const timer = window.setTimeout(() => {
      setInviteSearching(true)
      void searchUsers(q)
        .then((users) => {
          setInviteOptions(users)
          for (const u of users) {
            inviteNameRef.set(u.id, u.nickname ?? u.profile?.nickname ?? u.username)
          }
        })
        .catch(() => setInviteOptions([]))
        .finally(() => setInviteSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [inviteQuery, inviteNameRef])

  /** 该成员行是否可被我改派/移除：owner 恒不可；非 owner 的 admin 不可动 admin。 */
  const canManageMember = (m: SpaceMember): boolean => {
    if (!canManage) return false
    if (m.role === 'owner') return false
    if (!isOwner && m.role === 'admin') return false
    return true
  }

  /** 角色下拉可选项：内置可授予角色（admin 仅 owner 可授予）。 */
  const roleOptions = ASSIGNABLE_SPACE_ROLES
    .filter((r) => isOwner || r !== 'admin')
    .map((r) => ({ value: r, label: `${msg(ROLE_LABEL_KEYS[r])}（${r}）`, title: msg(ROLE_TIP_KEYS[r]) }))

  /** 添加已有用户（多选逐个添加，部分成功语义）。 */
  const handleInviteUsers = async (e: FormEvent) => {
    e.preventDefault()
    if (inviteUserIds.length === 0) return
    setInviteBusy(true)
    setError('')
    setNotice('')
    try {
      const results = await Promise.allSettled(inviteUserIds.map((uid) => addSpaceMember(space.id, uid, inviteRole)))
      const failed = results.filter((r) => r.status === 'rejected').length
      if (failed > 0) {
        setError(failed === inviteUserIds.length
          ? msg('addMemberFailed')
          : formatMessage(msg('batchOpDone'), { ok: inviteUserIds.length - failed, fail: failed }))
      } else {
        setNotice(formatMessage(msg('batchOpDone'), { ok: inviteUserIds.length, fail: 0 }))
      }
      setInviteUserIds([])
      setInviteQuery('')
      setInviteOptions([])
      await loadMembers()
      onChanged?.()
    } finally {
      setInviteBusy(false)
    }
  }

  /** 创建邮箱邀请：成功后立即复制一次性链接（明文 token 仅本次可见）。 */
  const handleCreateInvite = async (e: FormEvent) => {
    e.preventDefault()
    const email = inviteEmail.trim()
    if (!email) return
    setEmailBusy(true)
    setError('')
    setNotice('')
    try {
      const created = await createSpaceInvite(space.id, email, inviteRole)
      if (created.join_url) {
        const token = created.join_url.replace(/^\/(spaces|teams)\/join\//, '')
        setInviteTokens((prev) => ({ ...prev, [created.id]: token }))
        const link = `${window.location.origin}/spaces/join/${token}`
        try {
          await navigator.clipboard.writeText(link)
          setCopiedInviteId(created.id)
          setNotice(`${msg('copyInviteLink')} ✓：${link}`)
        } catch {
          // 剪贴板不可用（非安全上下文等）：直接展示链接供手动复制。
          setNotice(`${msg('inviteOnceHint')}\n${link}`)
        }
      } else {
        // 幂等命中既有邀请：无新链接。
        setNotice(msg('inviteLinkMissing'))
      }
      setInviteEmail('')
      await loadInvites()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('inviteCreateFailed'))
    } finally {
      setEmailBusy(false)
    }
  }

  const copyInviteLink = async (inv: SpaceInvite) => {
    const token = inviteTokens[inv.id]
    if (!token) return
    try {
      await navigator.clipboard.writeText(`${window.location.origin}/spaces/join/${token}`)
      setCopiedInviteId(inv.id)
      setTimeout(() => setCopiedInviteId((cur) => (cur === inv.id ? '' : cur)), 2000)
    } catch {
      setError(msg('clipboardCopyFailed'))
    }
  }

  const handleRevokeInvite = (inv: SpaceInvite) => {
    antdModal.confirm({
      title: msg('delete'),
      content: zh ? `撤销发给「${inv.email}」的邀请？链接立即失效。` : `Revoke the invitation to ${inv.email}? The link becomes invalid immediately.`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await revokeSpaceInvite(space.id, inv.id)
          await loadInvites()
        } catch (err) {
          setError(err instanceof Error ? err.message : msg('inviteRevokeFailed'))
        }
      },
    })
  }

  /** 重发邀请（v2.4）：撤销旧邀请（pending 时）后按原邮箱/角色重建，
   *  生成新的一次性链接并自动复制（与创建邀请行为一致）。 */
  const handleResendInvite = async (inv: SpaceInvite) => {
    if (resendingInviteId) return
    setResendingInviteId(inv.id)
    setError('')
    setNotice('')
    try {
      if (inv.status === 'pending') {
        await revokeSpaceInvite(space.id, inv.id)
      }
      const created = await createSpaceInvite(space.id, inv.email, inv.role)
      if (created.join_url) {
        const token = created.join_url.replace(/^\/(spaces|teams)\/join\//, '')
        setInviteTokens((prev) => ({ ...prev, [created.id]: token }))
        const link = `${window.location.origin}/spaces/join/${token}`
        try {
          await navigator.clipboard.writeText(link)
          setCopiedInviteId(created.id)
          setNotice(`${zh ? '已重发，新链接已复制' : 'Resent; new link copied'} ✓：${link}`)
        } catch {
          setNotice(`${msg('inviteOnceHint')}\n${link}`)
        }
      } else {
        setNotice(msg('inviteLinkMissing'))
      }
      await loadInvites()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('inviteCreateFailed'))
    } finally {
      setResendingInviteId('')
    }
  }

  const handleChangeRole = async (m: SpaceMember, role: AssignableSpaceRole) => {
    setError('')
    try {
      await updateSpaceMemberRole(space.id, m.user_id, role)
      await loadMembers()
      onChanged?.()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('changeRoleFailed'))
      await loadMembers()
    }
  }

  const handleRemove = (m: SpaceMember) => {
    antdModal.confirm({
      title: msg('delete'),
      content: zh ? `确定移除成员「${memberDisplayName(m)}」？` : `Remove member “${memberDisplayName(m)}”?`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await removeSpaceMember(space.id, m.user_id)
          setSelected((prev) => prev.filter((k) => k !== m.user_id))
          await loadMembers()
          onChanged?.()
        } catch (err) {
          setError(err instanceof Error ? err.message : msg('removeMemberFailed'))
        }
      },
    })
  }

  /** 批量改角色 / 批量移出（逐项调用现有端点，部分成功语义）。 */
  const runBatch = async (kind: 'role' | 'remove') => {
    const ids = selected.map(String)
    if (ids.length === 0) return
    antdModal.confirm({
      title: kind === 'role' ? msg('batchRoleBtn') : msg('batchRemoveBtn'),
      content: kind === 'role'
        ? formatMessage(msg('batchRoleConfirm'), { n: ids.length, role: msg(ROLE_LABEL_KEYS[batchRole]) })
        : formatMessage(msg('batchRemoveConfirm'), { n: ids.length }),
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setBatchBusy(true)
        setError('')
        setNotice('')
        try {
          const calls = kind === 'role'
            ? ids.map((uid) => updateSpaceMemberRole(space.id, uid, batchRole))
            : ids.map((uid) => removeSpaceMember(space.id, uid))
          const results = await Promise.allSettled(calls)
          const failed = results.filter((r) => r.status === 'rejected').length
          if (failed > 0) setError(formatMessage(msg('batchOpDone'), { ok: ids.length - failed, fail: failed }))
          else setNotice(formatMessage(msg('batchOpDone'), { ok: ids.length, fail: 0 }))
          setSelected([])
          await loadMembers()
          onChanged?.()
        } finally {
          setBatchBusy(false)
        }
      },
    })
  }

  const handleTransfer = (target: { user_id: string; display: string }) => {
    antdModal.confirm({
      title: msg('transferOwner'),
      content: formatMessage(msg('transferOwnerConfirm'), { name: target.display })
        + (members.some((m) => m.user_id === target.user_id)
          ? ''
          : (zh ? '\n该成员经用户组加入，转让时将按其组角色先落为直接成员。' : '\nThis member joins via a group; they will be added as a direct member on transfer.')),
      okText: msg('transferOwner'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        setSettingsError('')
        try {
          await transferSpaceOwnership(space.id, target.user_id)
          setTransferTarget('')
          await loadMembers()
          onChanged?.()
        } catch (err) {
          setError(err instanceof Error ? err.message : msg('transferOwnerFailed'))
        }
      },
    })
  }

  /** 解散空间（owner）：输入空间名二次确认，成功后关闭弹窗并回空间列表。 */
  const handleDissolve = async () => {
    if (dissolveName.trim() !== space.name) return
    setDissolveBusy(true)
    setError('')
    try {
      await deleteSpace(space.id)
      setDissolveOpen(false)
      onClose()
      onChanged?.()
      navigate('/spaces')
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('teamDeleteFailed'))
    } finally {
      setDissolveBusy(false)
    }
  }

  // ---- 空间设置 ----

  /** 保存名称/描述/配额（配额变化才发 quota 请求；越系统上限由后端拒绝）。 */
  const handleSaveSettings = async (e: FormEvent) => {
    e.preventDefault()
    const n = settingsName.trim()
    if (!n) return
    const q = Number(settingsQuota.trim() || '0')
    if (!Number.isInteger(q) || q < 0) {
      setSettingsError(msg('quotaBytesLabel'))
      return
    }
    setSettingsBusy(true)
    setSettingsError('')
    setSettingsNotice('')
    try {
      await updateSpace(space.id, n, settingsDesc.trim())
      if (q !== (space.quota_bytes ?? 0)) await updateSpaceQuota(space.id, q)
      setSettingsNotice(msg('saved'))
      onChanged?.()
    } catch (err) {
      setSettingsError(err instanceof Error ? err.message : msg('teamUpdateFailed'))
    } finally {
      setSettingsBusy(false)
    }
  }

  // ---- 用户组操作 ----

  /** 添加用户组：admin 下拉全部组；其余手输组 UUID。 */
  const handleAddGroup = async (e: FormEvent) => {
    e.preventDefault()
    const gid = groupQuery.trim()
    if (!gid) return
    setGroupBusy(true)
    setError('')
    setNotice('')
    try {
      await addSpaceGroup(space.id, gid, groupRole)
      setGroupQuery('')
      await loadGroups()
      onChanged?.()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('addGroupFailed'))
    } finally {
      setGroupBusy(false)
    }
  }

  const handleChangeGroupRole = async (g: SpaceGroup, role: AssignableSpaceRole) => {
    setError('')
    try {
      await updateSpaceGroupRole(space.id, g.group_id, role)
      await loadGroups()
      onChanged?.()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('changeGroupRoleFailed'))
      await loadGroups()
    }
  }

  const handleRemoveGroup = (g: SpaceGroup) => {
    antdModal.confirm({
      title: msg('delete'),
      content: zh ? `移除用户组「${g.group_name ?? g.group_id.slice(0, 8)}」的授权？组内成员将失去对应权限（直接成员不受影响）。` : `Remove group “${g.group_name ?? g.group_id.slice(0, 8)}”? Its members lose the granted permissions (direct members unaffected).`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await removeSpaceGroup(space.id, g.group_id)
          await loadGroups()
          onChanged?.()
        } catch (err) {
          setError(err instanceof Error ? err.message : msg('removeGroupFailed'))
        }
      },
    })
  }

  // ---- 成员合并行：直接成员 ∪ 组内用户（组来源标注；组员只读） ----

  /** 角色等级比较（转让候选排序与组内最高角色取值用）。 */
  const ROLE_LEVELS: SpaceRole[] = ['guest', 'member', 'member_share', 'admin', 'owner']
  const highestGroupRole = (roles: AssignableSpaceRole[]): AssignableSpaceRole | null => {
    let best: AssignableSpaceRole | null = null
    for (const r of roles) {
      if (best === null || ROLE_LEVELS.indexOf(r) > ROLE_LEVELS.indexOf(best)) best = r
    }
    return best
  }

  const memberRows: MemberRow[] = useMemo(() => {
    // 组来源按用户聚合（同一用户可命中多个组）。
    const byUser = new Map<string, Array<{ id: string; name: string; role: AssignableSpaceRole }>>()
    for (const gu of groupUsers) {
      const list = byUser.get(gu.user_id) ?? []
      list.push({ id: gu.group_id, name: gu.group_name || gu.group_id.slice(0, 8), role: gu.group_role })
      byUser.set(gu.user_id, list)
    }
    const rows: MemberRow[] = members.map((m) => {
      const via = byUser.get(m.user_id) ?? []
      return {
        user_id: m.user_id,
        display: memberDisplayName(m),
        email: m.email ?? '',
        isDirect: true,
        role: m.role,
        groupRole: highestGroupRole(via.map((v) => v.role)),
        viaGroups: via,
        joinedAt: m.joined_at ?? m.created_at,
        member: m,
      }
    })
    // 仅经用户组加入的用户（非直接成员）追加在直接成员之后。
    const directIds = new Set(members.map((m) => m.user_id))
    for (const [uid, via] of byUser) {
      if (directIds.has(uid)) continue
      const sample = groupUsers.find((g) => g.user_id === uid)
      rows.push({
        user_id: uid,
        display: sample?.nickname || sample?.username || `${uid.slice(0, 8)}…`,
        email: sample?.email ?? '',
        isDirect: false,
        role: null,
        groupRole: highestGroupRole(via.map((v) => v.role)),
        viaGroups: via,
      })
    }
    return rows
  }, [members, groupUsers])

  // 成员搜索过滤（昵称/用户名/邮箱，前端过滤——列表本身为空间全量）。
  const needle = memberQuery.trim().toLowerCase()
  const visibleRows = needle
    ? memberRows.filter((r) =>
      r.display.toLowerCase().includes(needle)
      || r.email.toLowerCase().includes(needle)
      || r.user_id.toLowerCase().includes(needle)
      || r.viaGroups.some((g) => g.name.toLowerCase().includes(needle)))
    : memberRows

  /** 组来源徽标（多组时 tooltip 列全部：组名（角色））。 */
  const groupBadge = (row: MemberRow) =>
    row.viaGroups.length === 0 ? null : (
      <Tooltip
        title={row.viaGroups.map((g) => `${g.name}（${msg(ROLE_LABEL_KEYS[g.role])}）`).join('、')}
      >
        <span className="badge badge-group-src" title={undefined}>
          {zh ? '组' : 'Group'}
        </span>
      </Tooltip>
    )

  const memberColumns: ColumnsType<MemberRow> = [
    {
      title: msg('memberCol'),
      key: 'member',
      render: (_, row) => (
        <span className="member-cell">
          <Avatar size={26} className="member-avatar" style={{ backgroundColor: avatarColor(row.user_id) }}>
            {row.display.slice(0, 1).toUpperCase()}
          </Avatar>
          <span className="member-name" title={row.user_id}>{row.display}</span>
          {groupBadge(row)}
        </span>
      ),
    },
    {
      title: msg('emailCol'),
      key: 'email',
      render: (_, row) => <span className="muted member-email">{row.email || '—'}</span>,
    },
    {
      title: msg('roleLabel'),
      key: 'role',
      width: 170,
      render: (_, row) => {
        // 组员（非直接成员）只读：展示组内最高角色，改角色须在「用户组」tab。
        if (!row.isDirect || !row.member) {
          const shown = row.groupRole
          return (
            <Tooltip title={zh ? '经用户组加入，角色在「用户组」tab 按组调整' : 'Joined via group; adjust the role in the Groups tab'}>
              <span className={`badge role-${shown ?? 'guest'}`}>{shown ? msg(ROLE_LABEL_KEYS[shown]) : '—'}</span>
            </Tooltip>
          )
        }
        const m = row.member
        return canManageMember(m) ? (
          <Select
            className="member-role-select"
            size="small"
            value={m.role}
            onChange={(v) => void handleChangeRole(m, v as AssignableSpaceRole)}
            title={zh ? '修改成员角色' : 'Change role'}
            options={roleOptions}
          />
        ) : (
          <Tooltip title={msg(ROLE_TIP_KEYS[m.role])}>
            <span className={`badge role-${m.role}`}>{msg(ROLE_LABEL_KEYS[m.role])}</span>
          </Tooltip>
        )
      },
    },
    {
      title: msg('joinedAtCol'),
      key: 'joined',
      width: 165,
      render: (_, row) => (
        <span className="muted member-time" title={zh ? `加入于 ${formatTime(row.joinedAt ?? '')}` : `Joined ${formatTime(row.joinedAt ?? '')}`}>
          {row.joinedAt ? formatTime(row.joinedAt) : '—'}
        </span>
      ),
    },
    ...(canManage ? [{
      title: msg('actions'),
      key: 'actions',
      className: 'col-actions',
      width: 180,
      render: (_: unknown, row: MemberRow) => {
        // 局部常量承接 row.member：闭包内保留类型收窄（组来源行 member 为 undefined）。
        const direct = row.member
        return (
          <>
            {isOwner && row.role !== 'owner' && (
              <Button size="small" onClick={() => handleTransfer({ user_id: row.user_id, display: row.display })}>{msg('transferOwner')}</Button>
            )}{' '}
            {direct && canManageMember(direct) && (
              <Button size="small" danger onClick={() => handleRemove(direct)}>{zh ? '移出' : 'Remove'}</Button>
            )}
          </>
        )
      },
    }] : []),
  ]

  const inviteColumns: ColumnsType<SpaceInvite> = [
    { title: msg('emailCol'), key: 'email', render: (_, inv) => <span title={inv.email}>{inv.email}</span> },
    {
      title: msg('roleLabel'),
      key: 'role',
      width: 150,
      render: (_, inv) => <Tooltip title={msg(ROLE_TIP_KEYS[inv.role])}><span className={`badge role-${inv.role}`}>{msg(ROLE_LABEL_KEYS[inv.role])}</span></Tooltip>,
    },
    {
      title: msg('inviteSentCol'),
      key: 'sent',
      width: 165,
      render: (_, inv) => <span className="muted">{formatTime(inv.created_at)}</span>,
    },
    {
      title: msg('status'),
      key: 'status',
      width: 90,
      render: (_, inv) => inv.status === 'pending'
        ? <span className="badge">{zh ? '待接受' : 'Pending'}</span>
        : inv.status === 'accepted'
          ? <span className="badge ok-badge">{zh ? '已接受' : 'Accepted'}</span>
          : <span className="badge failed">{zh ? '已过期' : 'Expired'}</span>,
    },
    {
      title: msg('actions'),
      key: 'actions',
      className: 'col-actions',
      width: 230,
      render: (_, inv) => (
        <>
          {inviteTokens[inv.id] && inv.status === 'pending' && (
            <Button size="small" onClick={() => void copyInviteLink(inv)}>
              {copiedInviteId === inv.id ? msg('copied') : msg('copyInviteLink')}
            </Button>
          )}{' '}
          {inv.status === 'pending' && (
            <>
              <Button size="small" disabled={resendingInviteId !== ''} loading={resendingInviteId === inv.id} onClick={() => void handleResendInvite(inv)}>
                {zh ? '重发' : 'Resend'}
              </Button>{' '}
              <Button size="small" danger onClick={() => handleRevokeInvite(inv)}>{zh ? '撤销' : 'Revoke'}</Button>
            </>
          )}
        </>
      ),
    },
  ]

  const groupColumns: ColumnsType<SpaceGroup> = [
    {
      title: msg('groupCol'),
      key: 'group',
      render: (_, g) => (
        <span className="member-cell">
          <Avatar size={26} className="member-avatar" style={{ backgroundColor: avatarColor(g.group_id) }}>
            {(g.group_name ?? '?').slice(0, 1).toUpperCase()}
          </Avatar>
          <span className="member-name" title={g.group_id}>{g.group_name ?? `${g.group_id.slice(0, 8)}…`}</span>
        </span>
      ),
    },
    {
      title: msg('groupMemberCountCol'),
      key: 'count',
      width: 110,
      render: (_, g) => <span className="muted">{g.member_count ?? '—'}</span>,
    },
    {
      title: msg('roleLabel'),
      key: 'role',
      width: 170,
      render: (_, g) => canManage ? (
        <Select
          className="member-role-select"
          size="small"
          value={g.role}
          onChange={(v) => void handleChangeGroupRole(g, v as AssignableSpaceRole)}
          options={roleOptions}
        />
      ) : (
        <Tooltip title={msg(ROLE_TIP_KEYS[g.role])}>
          <span className={`badge role-${g.role}`}>{msg(ROLE_LABEL_KEYS[g.role])}</span>
        </Tooltip>
      ),
    },
    {
      title: msg('joinedAtCol'),
      key: 'joined',
      width: 165,
      render: (_, g) => <span className="muted member-time">{formatTime(g.created_at)}</span>,
    },
    ...(canManage ? [{
      title: msg('actions'),
      key: 'actions',
      className: 'col-actions',
      width: 100,
      render: (_: unknown, g: SpaceGroup) => (
        <Button size="small" danger onClick={() => handleRemoveGroup(g)}>{zh ? '移除' : 'Remove'}</Button>
      ),
    }] : []),
  ]

  // 左侧竖向导航：成员/用户组全员可见；空间设置（设置类）仅 owner/admin。
  // v2.4：独立「邀请」tab 移除（加人两行并入成员 tab；记录在标题行弹窗）。
  const navItems: MenuProps['items'] = [
    { key: 'members', icon: <Users size={14} strokeWidth={2} aria-hidden="true" />, label: formatMessage(msg('membersTitle'), { n: memberRows.length }) },
    { key: 'groups', icon: <UsersRound size={14} strokeWidth={2} aria-hidden="true" />, label: formatMessage(msg('groupsTitle'), { n: groups.length }) },
    ...(canManage ? [
      { key: 'settings', icon: <Settings size={14} strokeWidth={2} aria-hidden="true" />, label: msg('teamSettings') },
    ] : []),
  ]

  const quota = space.quota_bytes ?? 0
  const used = space.storage_used ?? 0
  const pct = quota > 0 ? Math.min(100, Math.round((used / quota) * 100)) : 0
  // 危险区转让候选：成员列表全部非 owner 用户（含组内；后端对组员先落直接成员）。
  const transferCandidates = memberRows.filter((r) => r.role !== 'owner')

  return (
    <AntdModal
      open
      centered
      footer={null}
      width="min(880px, 92vw)"
      title={
        <div className="space-manage-title-row">
          <span>{`${space.name} · ${msg('spaceManage')}`}</span>
          {canManage && (
            <Button
              size="small"
              className="space-manage-invites-btn"
              icon={<UserPlus size={13} strokeWidth={2} aria-hidden="true" />}
              onClick={() => { void loadInvites(); setInvitesOpen(true) }}
            >
              {msg('inviteListTitle')}
            </Button>
          )}
        </div>
      }
      styles={{ body: { height: '70vh', overflow: 'hidden', padding: 0 } }}
      classNames={{
        header: 'docflow-modal-header',
        title: 'docflow-modal-title',
        body: 'space-manage-modal-body',
        close: 'docflow-modal-close',
      }}
      onCancel={onClose}
    >
      <div className="space-manage-body">
        <nav className="space-manage-nav" aria-label={msg('spaceManage')}>
          <Menu
            mode="inline"
            selectedKeys={[tab]}
            onClick={({ key }) => setTab(key as SpaceManageTab)}
            items={navItems}
          />
          {!canManage && <p className="hint space-manage-ro">{msg('spaceManageReadOnly')}</p>}
        </nav>
        <div className="space-manage-content">
          {/* ---- 成员 ---- */}
          {tab === 'members' && (
            <>
              {/* v2.4：加人两行并入成员 tab 顶部（原独立「邀请」tab 移除）：
                  ① 直接添加已有用户（即时生效）；② 邮箱创建一次性邀请链接。 */}
              {canManage && (
                <>
                  <form className="member-add member-add-row" onSubmit={handleInviteUsers}>
                    <Select
                      className="member-add-main user-search-select"
                      mode="multiple"
                      showSearch
                      allowClear
                      filterOption={false}
                      value={inviteUserIds}
                      loading={inviteSearching}
                      placeholder={zh ? '添加已有用户：输入昵称、用户名或邮箱（至少 2 字）' : 'Add existing users: nickname, username or email (2+ chars)'}
                      notFoundContent={inviteSearching ? (zh ? '搜索中…' : 'Searching…') : null}
                      onSearch={setInviteQuery}
                      onChange={setInviteUserIds}
                      aria-label={msg('inviteBySearchLabel')}
                      options={[
                        ...inviteOptions.map((user) => {
                          const nickname = user.nickname ?? user.profile?.nickname
                          return {
                            value: user.id,
                            label: (
                              <span className="user-search-option">
                                <strong>{nickname || user.username}</strong>
                                <span className="muted">{user.username} · {user.email}</span>
                              </span>
                            ),
                          }
                        }),
                        ...inviteUserIds
                          .filter((id) => !inviteOptions.some((u) => u.id === id))
                          .map((id) => ({ value: id, label: inviteNameRef.get(id) ?? id })),
                      ]}
                    />
                    <Select
                      className="member-add-role"
                      value={inviteRole}
                      onChange={(v) => setInviteRole(v as AssignableSpaceRole)}
                      options={roleOptions}
                      aria-label={msg('roleLabel')}
                    />
                    <Button className="member-add-submit" type="primary" htmlType="submit" disabled={inviteBusy || inviteUserIds.length === 0}>
                      {inviteBusy ? msg('creating') : msg('addMember')}
                    </Button>
                  </form>
                  <form className="member-add member-add-row" onSubmit={handleCreateInvite}>
                    <Input
                      className="member-add-main"
                      type="email"
                      allowClear
                      value={inviteEmail}
                      onChange={(e) => setInviteEmail(e.target.value)}
                      placeholder={zh ? '邀请未注册用户：输入邮箱生成一次性链接（7 天有效）' : 'Invite by email: one-time link'}
                      aria-label={msg('inviteByEmailLabel')}
                    />
                    <Select
                      className="member-add-role"
                      value={inviteRole}
                      onChange={(v) => setInviteRole(v as AssignableSpaceRole)}
                      options={roleOptions}
                      aria-label={msg('roleLabel')}
                    />
                    <Button className="member-add-submit" htmlType="submit" disabled={emailBusy || !inviteEmail.trim()}>
                      {emailBusy ? msg('creating') : msg('createInviteBtn')}
                    </Button>
                  </form>
                  <p className="hint" style={{ margin: '0 0 10px' }}>{msg('inviteOnceHint')}</p>
                </>
              )}

              {/* 成员搜索 + 批量操作栏。 */}
              <div className="member-list-toolbar">
                <Input
                  size="small"
                  allowClear
                  className="member-search-input"
                  value={memberQuery}
                  onChange={(e) => setMemberQuery(e.target.value)}
                  placeholder={zh ? '搜索成员（昵称/用户名/邮箱）…' : 'Search members…'}
                  prefix={<Search size={14} strokeWidth={2} aria-hidden="true" />}
                />
                {canManage && selected.length > 0 && (
                  <span className="member-batch-bar">
                    <Select
                      size="small"
                      className="member-batch-role"
                      value={batchRole}
                      onChange={(v) => setBatchRole(v as AssignableSpaceRole)}
                      options={roleOptions}
                    />
                    <Button size="small" disabled={batchBusy} onClick={() => void runBatch('role')}>{msg('batchRoleBtn')}</Button>
                    <Button size="small" danger disabled={batchBusy} onClick={() => void runBatch('remove')}>{msg('batchRemoveBtn')}</Button>
                  </span>
                )}
              </div>

              {error && <div className="error-text">{error}</div>}
              {notice && <div className="banner ok member-notice">{notice}</div>}

              <Table<MemberRow>
                rowKey="user_id"
                size="small"
                className="member-antd-table"
                columns={memberColumns}
                dataSource={visibleRows}
                loading={loading}
                pagination={false}
                rowSelection={canManage ? {
                  selectedRowKeys: selected,
                  onChange: setSelected,
                  // 仅可管理的直接成员可勾选（组员/owner 行/admin 动 admin 不可）。
                  getCheckboxProps: (row) => ({ disabled: !row.member || !canManageMember(row.member) || batchBusy }),
                } : undefined}
                locale={{ emptyText: msg('noMatchingMembers') }}
                onRow={(row) => ({
                  // 行点击查看用户只读信息（v2.4）；行内控件（勾选/角色下拉/
                  // 按钮）不触发——命中可交互元素时忽略。
                  onClick: (e) => {
                    if ((e.target as HTMLElement).closest('button, .ant-select, .ant-checkbox, a, input, .ant-btn')) return
                    setViewUser({ id: row.user_id, name: row.display })
                  },
                })}
              />
            </>
          )}

          {/* ---- 用户组 ---- */}
          {tab === 'groups' && (
            <>
              <p className="hint">{msg('groupPermHint')}</p>
              {/* 添加用户组（一行式）：组选择/UUID + 角色 + 添加按钮。 */}
              {canManage && (
                <form className="member-add member-add-row" onSubmit={handleAddGroup}>
                  {allGroups.length > 0 ? (
                    <Select
                      className="member-add-main"
                      showSearch
                      allowClear
                      optionFilterProp="label"
                      value={groupQuery || undefined}
                      onChange={(v) => setGroupQuery(v ?? '')}
                      placeholder={zh ? '选择用户组…' : 'Select group…'}
                      aria-label={msg('addGroup')}
                      options={allGroups.map((g) => ({ value: g.id, label: `${g.name}（${g.member_count ?? 0}）` }))}
                    />
                  ) : (
                    <Input
                      className="member-add-main"
                      allowClear
                      value={groupQuery}
                      onChange={(e) => setGroupQuery(e.target.value)}
                      placeholder={zh ? '输入用户组 UUID（由管理员提供）' : 'Group UUID (from admin)'}
                      aria-label={msg('addGroup')}
                    />
                  )}
                  <Select
                    className="member-add-role"
                    value={groupRole}
                    onChange={(v) => setGroupRole(v as AssignableSpaceRole)}
                    options={roleOptions}
                    aria-label={msg('roleLabel')}
                  />
                  <Button className="member-add-submit" type="primary" htmlType="submit" disabled={groupBusy || !groupQuery.trim()}>
                    {groupBusy ? msg('creating') : msg('addGroup')}
                  </Button>
                </form>
              )}

              {error && <div className="error-text">{error}</div>}
              {notice && <div className="banner ok member-notice">{notice}</div>}

              <Table<SpaceGroup>
                rowKey="group_id"
                size="small"
                className="member-antd-table"
                columns={groupColumns}
                dataSource={groups}
                pagination={false}
                locale={{ emptyText: msg('noGroups') }}
              />
            </>
          )}

          {/* ---- 邀请 tab 已删除（v2.4）：加人两行并入成员 tab；
              邀请记录由标题行「邀请记录」按钮弹独立 Modal 展示。 ---- */}

          {/* ---- 空间设置（owner/admin） ---- */}
          {tab === 'settings' && canManage && (
            <>
              {/* 配额进度条（用量取宿主传入的空间数据，保存后 onChanged 刷新；
                  formatQuota 统一 B→KiB/MiB/GiB/TiB 自适应 + 1 位小数，0=不限）。 */}
              <div
                className="quota-bar"
                title={quota > 0 ? formatMessage(msg('quotaUsedOf'), { used: formatQuota(used, false), quota: formatQuota(quota) }) : `${formatQuota(used, false)} / ${msg('quotaUnlimited')}`}
              >
                <div className={`quota-fill${pct >= 90 ? ' quota-danger' : pct >= 75 ? ' quota-warn' : ''}`} style={{ width: quota > 0 ? `${pct}%` : '0%' }} />
              </div>
              <p className="hint">
                {msg('storageUsedLabel')} {formatQuota(used, false)}{quota > 0 ? ` / ${formatQuota(quota)}（${pct}%）` : ` / ${msg('quotaUnlimited')}`}
              </p>

              <form className="team-create-row space-settings-form" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={handleSaveSettings}>
                <label className="field">
                  <span>{msg('teamNameLabel')}</span>
                  <Input value={settingsName} onChange={(e) => setSettingsName(e.target.value)} maxLength={100} />
                </label>
                <label className="field">
                  <span>{msg('teamDescLabel')}</span>
                  <Input value={settingsDesc} onChange={(e) => setSettingsDesc(e.target.value)} />
                </label>
                <label className="field">
                  <span>{msg('quotaBytesLabel')}</span>
                  <Input value={settingsQuota} onChange={(e) => setSettingsQuota(e.target.value)} inputMode="numeric" />
                </label>
                {settingsError && <div className="error-text">{settingsError}</div>}
                {settingsNotice && <div className="banner ok member-notice">{settingsNotice}</div>}
                <div className="team-create-row" style={{ justifyContent: 'flex-end' }}>
                  <Button htmlType="submit" type="primary" disabled={settingsBusy || !settingsName.trim()}>{settingsBusy ? msg('loading') : msg('save')}</Button>
                </div>
              </form>

              {/* owner 危险区：转让所有权 + 解散空间（输入空间名二次确认）。 */}
              {isOwner && (
                <div className="member-danger-zone">
                  <div className="member-danger-title">
                    <AlertTriangle size={14} strokeWidth={2} aria-hidden="true" /> {msg('dissolveDangerZone')}
                  </div>

                  {/* 转让所有权：成员列表全部非 owner 用户（含组内，成员表行内亦有入口）。 */}
                  <div className="space-manage-transfer">
                    <Select
                      className="member-role-select"
                      value={transferTarget || undefined}
                      onChange={setTransferTarget}
                      placeholder={zh ? '选择新所有者（成员列表全部用户，含组内）' : 'Pick the new owner (any listed member, incl. via groups)'}
                      options={transferCandidates.map((r) => ({ value: r.user_id, label: `${r.display}${r.email ? ` · ${r.email}` : ''}${r.isDirect ? '' : (zh ? '（组）' : ' (group)')}` }))}
                      notFoundContent={zh ? '暂无可转让的成员' : 'No eligible members'}
                    />
                    <Button danger disabled={!transferTarget} onClick={() => {
                      const row = memberRows.find((x) => x.user_id === transferTarget)
                      if (row) handleTransfer({ user_id: row.user_id, display: row.display })
                    }}>
                      {msg('transferOwner')}
                    </Button>
                  </div>

                  <p className="hint">{formatMessage(msg('dissolveTeamConfirm'), { name: space.name })}</p>
                  {space.is_default ? (
                    <p className="hint">{msg('defaultSpaceNoDelete')}</p>
                  ) : (
                    <Button danger onClick={() => { setDissolveOpen(true); setDissolveName('') }}>
                      {msg('dissolveTeam')}
                    </Button>
                  )}
                </div>
              )}
            </>
          )}
        </div>
      </div>

      {/* 邀请记录独立弹窗（v2.4：记录表 + 撤销 + 重发）。 */}
      <AntdModal
        open={invitesOpen}
        centered
        footer={null}
        width="min(760px, 92vw)"
        title={msg('inviteListTitle')}
        styles={{ body: { maxHeight: '62vh', overflow: 'auto', paddingTop: 12 } }}
        onCancel={() => { if (!resendingInviteId) setInvitesOpen(false) }}
      >
        <p className="hint" style={{ marginTop: 0 }}>{msg('inviteOnceHint')}</p>
        <Table<SpaceInvite>
          rowKey="id"
          size="small"
          className="member-antd-table invite-antd-table"
          columns={inviteColumns}
          dataSource={invites}
          pagination={false}
          locale={{ emptyText: msg('noTeamInvites') }}
        />
      </AntdModal>

      {/* 成员行点击的用户只读信息弹窗（v2.4）。 */}
      {viewUser && (
        <UserInfoModal userId={viewUser.id} fallbackName={viewUser.name} onClose={() => setViewUser(null)} />
      )}

      {/* 解散确认弹窗（受控：输入空间名匹配才可确认）。 */}
      <AntdModal
        open={dissolveOpen}
        title={msg('dissolveTeam')}
        okText={msg('dissolveTeam')}
        okButtonProps={{ danger: true, disabled: dissolveName.trim() !== space.name }}
        cancelText={zh ? '取消' : 'Cancel'}
        confirmLoading={dissolveBusy}
        onOk={() => void handleDissolve()}
        onCancel={() => { if (!dissolveBusy) setDissolveOpen(false) }}
        width="min(460px, 92vw)"
      >
        <p className="hint">{formatMessage(msg('dissolveTeamConfirm'), { name: space.name })}</p>
        <p className="hint">{formatMessage(msg('dissolveTeamTypeName'), { name: space.name })}</p>
        <Input
          autoFocus
          allowClear
          status={dissolveName.trim() !== '' && dissolveName.trim() !== space.name ? 'error' : undefined}
          value={dissolveName}
          onChange={(e) => setDissolveName(e.target.value)}
          placeholder={space.name}
        />
        {dissolveName.trim() !== '' && dissolveName.trim() !== space.name && (
          <p className="error-text">{msg('dissolveTeamNameMismatch')}</p>
        )}
      </AntdModal>
    </AntdModal>
  )
}
