// 团队成员管理弹窗（v1.7.1 完善，参考 GitLab/GitHub 成员管理）：
// - 成员表：头像（首字母）/姓名/邮箱/角色下拉（五级内置 + 权限 tooltip）/
//   加入时间/操作（移出；owner 行另有转让所有权）；
// - 顶部搜索过滤 + 全选 + 批量改角色 / 批量移出（部分成功语义）；
// - 邀请区：① 添加已有用户（远程搜索多选 + 角色，逐个 addTeamMember）；
//   ② 邮箱邀请（createTeamInvite，一次性 token 链接仅创建时可见，自动复制）；
//   下方邀请记录表（邮箱/角色/发送时间/状态/复制链接[仅本会话创建]/撤销）；
// - owner 专属：转让所有权（二次确认）、解散团队危险区（红色，输入团队名
//   二次确认）。
// 边界与后端一致：admin 角色仅 owner 可授予；非 owner 的 admin 不可改派/
// 移除其他 admin；owner 成员不可改派/移除。
import { FormEvent, Key, useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { AlertTriangle, Search } from 'lucide-react'
import { App as AntdApp, Avatar, Button, Input, Modal as AntdModal, Select, Table, Tooltip } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import {
  ASSIGNABLE_TEAM_ROLES,
  AssignableTeamRole,
  TeamInvite,
  TeamMember,
  TeamRole,
  UserSearchResult,
  addTeamMember,
  createTeamInvite,
  deleteTeam,
  listTeamInvites,
  listTeamMembers,
  removeTeamMember,
  revokeTeamInvite,
  searchUsers,
  transferTeamOwnership,
  updateTeamMemberRole,
} from '../api'
import { Modal, formatTime } from './FileBrowser'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 角色 → 展示名 i18n key。 */
const ROLE_LABEL_KEYS: Record<TeamRole, MessageKey> = {
  owner: 'roleOwnerLabel',
  admin: 'roleAdminLabel',
  member_share: 'roleMemberShareLabel',
  member: 'roleMemberLabel',
  guest: 'roleGuestLabel',
}

/** 角色 → 权限说明 i18n key（下拉与徽章 tooltip 共用）。 */
const ROLE_TIP_KEYS: Record<TeamRole, MessageKey> = {
  owner: 'roleOwnerTip',
  admin: 'roleAdminTip',
  member_share: 'roleMemberShareTip',
  member: 'roleMemberTip',
  guest: 'roleGuestTip',
}

/** 成员显示名：nickname 优先，回退 username，再回退 UUID 前 8 位。 */
function memberDisplayName(m: TeamMember): string {
  return m.nickname || m.username || `${m.user_id.slice(0, 8)}…`
}

export default function TeamMembersModal({
  teamId,
  teamName,
  isOwner,
  onClose,
  onChanged,
}: {
  teamId: string
  teamName: string
  /** 当前用户是否团队 owner（admin 授予/转让边界）。 */
  isOwner: boolean
  onClose: () => void
  /** 成员列表变化（增/删/改派/转让）后回调（宿主刷新团队卡片数据等）。 */
  onChanged?: () => void
}) {
  const locale = useLocale()
  const navigate = useNavigate()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const zh = locale === 'zh-CN'
  const [members, setMembers] = useState<TeamMember[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [memberQuery, setMemberQuery] = useState('')
  // 批量操作勾选（可管理成员 user_id）。
  const [selected, setSelected] = useState<Key[]>([])
  const [batchBusy, setBatchBusy] = useState(false)
  const [batchRole, setBatchRole] = useState<AssignableTeamRole>('member')

  // ---- 邀请（已有用户搜索多选 + 邮箱邀请） ----
  const [inviteUserIds, setInviteUserIds] = useState<string[]>([])
  const [inviteQuery, setInviteQuery] = useState('')
  const [inviteOptions, setInviteOptions] = useState<UserSearchResult[]>([])
  const [inviteSearching, setInviteSearching] = useState(false)
  const [inviteRole, setInviteRole] = useState<AssignableTeamRole>('member')
  const [inviteBusy, setInviteBusy] = useState(false)
  // 已选用户显示名缓存（id → 名），多选框回显。
  const inviteNameRef = useMemo(() => new Map<string, string>(), [])
  // 邮箱邀请表单。
  const [inviteEmail, setInviteEmail] = useState('')
  const [emailBusy, setEmailBusy] = useState(false)
  // 邀请记录 + 本会话创建的 token（明文仅创建响应一次，缓存供复制链接）。
  const [invites, setInvites] = useState<TeamInvite[]>([])
  const [inviteTokens, setInviteTokens] = useState<Record<string, string>>({})
  const [copiedInviteId, setCopiedInviteId] = useState('')

  // 解散团队危险区（输入团队名二次确认）。
  const [dissolveOpen, setDissolveOpen] = useState(false)
  const [dissolveName, setDissolveName] = useState('')
  const [dissolveBusy, setDissolveBusy] = useState(false)

  const loadMembers = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      setMembers(await listTeamMembers(teamId))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('membersLoadFailed'))
    } finally {
      setLoading(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamId, locale])

  const loadInvites = useCallback(async () => {
    try {
      setInvites(await listTeamInvites(teamId))
    } catch {
      /* 邀请列表加载失败不阻塞成员管理（旧后端无此端点时静默）。 */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamId])

  useEffect(() => {
    void loadMembers()
    void loadInvites()
  }, [loadMembers, loadInvites])

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
  const canManageMember = (m: TeamMember): boolean => {
    if (m.role === 'owner') return false
    if (!isOwner && m.role === 'admin') return false
    return true
  }

  /** 角色下拉可选项：内置可授予角色（admin 仅 owner 可授予）。 */
  const roleOptions = ASSIGNABLE_TEAM_ROLES
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
      const results = await Promise.allSettled(inviteUserIds.map((uid) => addTeamMember(teamId, uid, inviteRole)))
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
      const created = await createTeamInvite(teamId, email, inviteRole)
      if (created.join_url) {
        const token = created.join_url.replace(/^\/teams\/join\//, '')
        setInviteTokens((prev) => ({ ...prev, [created.id]: token }))
        const link = `${window.location.origin}/teams/join/${token}`
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

  const copyInviteLink = async (inv: TeamInvite) => {
    const token = inviteTokens[inv.id]
    if (!token) return
    try {
      await navigator.clipboard.writeText(`${window.location.origin}/teams/join/${token}`)
      setCopiedInviteId(inv.id)
      setTimeout(() => setCopiedInviteId((cur) => (cur === inv.id ? '' : cur)), 2000)
    } catch {
      setError(msg('clipboardCopyFailed'))
    }
  }

  const handleRevokeInvite = (inv: TeamInvite) => {
    antdModal.confirm({
      title: msg('delete'),
      content: zh ? `撤销发给「${inv.email}」的邀请？链接立即失效。` : `Revoke the invitation to ${inv.email}? The link becomes invalid immediately.`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await revokeTeamInvite(teamId, inv.id)
          await loadInvites()
        } catch (err) {
          setError(err instanceof Error ? err.message : msg('inviteRevokeFailed'))
        }
      },
    })
  }

  const handleChangeRole = async (m: TeamMember, role: AssignableTeamRole) => {
    setError('')
    try {
      await updateTeamMemberRole(teamId, m.user_id, role)
      await loadMembers()
      onChanged?.()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('changeRoleFailed'))
      await loadMembers()
    }
  }

  const handleRemove = (m: TeamMember) => {
    antdModal.confirm({
      title: msg('delete'),
      content: zh ? `确定移除成员「${memberDisplayName(m)}」？` : `Remove member “${memberDisplayName(m)}”?`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await removeTeamMember(teamId, m.user_id)
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
            ? ids.map((uid) => updateTeamMemberRole(teamId, uid, batchRole))
            : ids.map((uid) => removeTeamMember(teamId, uid))
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

  const handleTransfer = (m: TeamMember) => {
    antdModal.confirm({
      title: msg('transferOwner'),
      content: formatMessage(msg('transferOwnerConfirm'), { name: memberDisplayName(m) }),
      okText: msg('transferOwner'),
      okButtonProps: { danger: true },
      cancelText: zh ? '取消' : 'Cancel',
      onOk: async () => {
        setError('')
        try {
          await transferTeamOwnership(teamId, m.user_id)
          await loadMembers()
          onChanged?.()
        } catch (err) {
          setError(err instanceof Error ? err.message : msg('transferOwnerFailed'))
        }
      },
    })
  }

  /** 解散团队（owner）：输入团队名二次确认，成功后关闭弹窗并回团队列表。 */
  const handleDissolve = async () => {
    if (dissolveName.trim() !== teamName) return
    setDissolveBusy(true)
    setError('')
    try {
      await deleteTeam(teamId)
      setDissolveOpen(false)
      onClose()
      onChanged?.()
      navigate('/teams')
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('teamDeleteFailed'))
    } finally {
      setDissolveBusy(false)
    }
  }

  // 成员搜索过滤（昵称/用户名/邮箱，前端过滤——列表本身为团队全量）。
  const needle = memberQuery.trim().toLowerCase()
  const visibleMembers = needle
    ? members.filter((m) =>
      memberDisplayName(m).toLowerCase().includes(needle)
      || (m.email ?? '').toLowerCase().includes(needle)
      || m.user_id.toLowerCase().includes(needle))
    : members

  const memberColumns: ColumnsType<TeamMember> = [
    {
      title: msg('memberCol'),
      key: 'member',
      render: (_, m) => (
        <span className="member-cell">
          <Avatar size={26} className="member-avatar" style={{ backgroundColor: avatarColor(m.user_id) }}>
            {memberDisplayName(m).slice(0, 1).toUpperCase()}
          </Avatar>
          <span className="member-name" title={m.user_id}>{memberDisplayName(m)}</span>
        </span>
      ),
    },
    {
      title: msg('emailCol'),
      key: 'email',
      render: (_, m) => <span className="muted member-email">{m.email || '—'}</span>,
    },
    {
      title: msg('roleLabel'),
      key: 'role',
      width: 170,
      render: (_, m) => canManageMember(m) ? (
        <Select
          className="member-role-select"
          size="small"
          value={m.role}
          onChange={(v) => void handleChangeRole(m, v as AssignableTeamRole)}
          title={zh ? '修改成员角色' : 'Change role'}
          options={roleOptions}
        />
      ) : (
        <Tooltip title={msg(ROLE_TIP_KEYS[m.role])}>
          <span className={`badge role-${m.role}`}>{msg(ROLE_LABEL_KEYS[m.role])}</span>
        </Tooltip>
      ),
    },
    {
      title: msg('joinedAtCol'),
      key: 'joined',
      width: 165,
      render: (_, m) => (
        <span className="muted member-time" title={zh ? `加入于 ${formatTime(m.joined_at ?? m.created_at)}` : `Joined ${formatTime(m.joined_at ?? m.created_at)}`}>
          {formatTime(m.joined_at ?? m.created_at)}
        </span>
      ),
    },
    {
      title: msg('actions'),
      key: 'actions',
      className: 'col-actions',
      width: 180,
      render: (_, m) => (
        <>
          {isOwner && m.role !== 'owner' && (
            <Button size="small" onClick={() => handleTransfer(m)}>{msg('transferOwner')}</Button>
          )}{' '}
          {canManageMember(m) && (
            <Button size="small" danger onClick={() => handleRemove(m)}>{zh ? '移出' : 'Remove'}</Button>
          )}
        </>
      ),
    },
  ]

  const inviteColumns: ColumnsType<TeamInvite> = [
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
      width: 170,
      render: (_, inv) => (
        <>
          {inviteTokens[inv.id] && inv.status === 'pending' && (
            <Button size="small" onClick={() => void copyInviteLink(inv)}>
              {copiedInviteId === inv.id ? msg('copied') : msg('copyInviteLink')}
            </Button>
          )}{' '}
          {inv.status === 'pending' && (
            <Button size="small" danger onClick={() => handleRevokeInvite(inv)}>{zh ? '撤销' : 'Revoke'}</Button>
          )}
        </>
      ),
    },
  ]

  return (
    <Modal wide title={`${teamName} · ${formatMessage(msg('membersTitle'), { n: members.length })}`} onClose={onClose}>
      {/* 邀请区（owner/admin）。
          ① 添加已有用户：远程搜索多选 + 角色（admin 仅 owner 可授予）。 */}
      <form className="member-add" onSubmit={handleInviteUsers}>
        <div className="field user-search-field">
          <span>{msg('inviteBySearchLabel')}</span>
          <Select
            className="user-search-select"
            mode="multiple"
            showSearch
            allowClear
            filterOption={false}
            value={inviteUserIds}
            loading={inviteSearching}
            placeholder={zh ? '输入昵称、用户名或邮箱（至少 2 字）' : 'Search nickname, username or email (2+ chars)'}
            notFoundContent={inviteSearching ? (zh ? '搜索中…' : 'Searching…') : null}
            onSearch={setInviteQuery}
            onChange={setInviteUserIds}
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
        </div>
        <label className="field">
          <span>{msg('roleLabel')}</span>
          <Select value={inviteRole} onChange={(v) => setInviteRole(v as AssignableTeamRole)} options={roleOptions} />
        </label>
        <Button className="member-add-submit" type="primary" htmlType="submit" block disabled={inviteBusy || inviteUserIds.length === 0}>
          {inviteBusy ? msg('creating') : msg('addMember')}
        </Button>
      </form>

      {/* ② 邮箱邀请：创建一次性邀请链接（仅创建时可见，自动复制）。 */}
      <form className="member-add" onSubmit={handleCreateInvite}>
        <div className="field user-search-field">
          <span>{msg('inviteByEmailLabel')}</span>
          <Input
            type="email"
            allowClear
            value={inviteEmail}
            onChange={(e) => setInviteEmail(e.target.value)}
            placeholder={msg('inviteEmailPlaceholder')}
          />
        </div>
        <label className="field">
          <span>{msg('roleLabel')}</span>
          <Select value={inviteRole} onChange={(v) => setInviteRole(v as AssignableTeamRole)} options={roleOptions} />
        </label>
        <Button className="member-add-submit" htmlType="submit" block disabled={emailBusy || !inviteEmail.trim()}>
          {emailBusy ? msg('creating') : msg('createInviteBtn')}
        </Button>
      </form>
      <p className="hint">{msg('inviteOnceHint')}</p>

      {error && <div className="error-text">{error}</div>}
      {notice && <div className="banner ok member-notice">{notice}</div>}

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
        {selected.length > 0 && (
          <span className="member-batch-bar">
            <Select
              size="small"
              className="member-batch-role"
              value={batchRole}
              onChange={(v) => setBatchRole(v as AssignableTeamRole)}
              options={roleOptions}
            />
            <Button size="small" disabled={batchBusy} onClick={() => void runBatch('role')}>{msg('batchRoleBtn')}</Button>
            <Button size="small" danger disabled={batchBusy} onClick={() => void runBatch('remove')}>{msg('batchRemoveBtn')}</Button>
          </span>
        )}
      </div>

      <Table<TeamMember>
        rowKey="user_id"
        size="small"
        className="member-antd-table"
        columns={memberColumns}
        dataSource={visibleMembers}
        loading={loading}
        pagination={false}
        rowSelection={{
          selectedRowKeys: selected,
          onChange: setSelected,
          // owner 行与不可管理行（admin 动 admin）不可勾选。
          getCheckboxProps: (m) => ({ disabled: !canManageMember(m) || batchBusy }),
        }}
        locale={{ emptyText: msg('noMatchingMembers') }}
      />

      {/* 邀请记录（邮箱/角色/发送时间/状态/复制链接/撤销）。 */}
      <div className="member-invites-head">{msg('inviteListTitle')}</div>
      <Table<TeamInvite>
        rowKey="id"
        size="small"
        className="member-antd-table invite-antd-table"
        columns={inviteColumns}
        dataSource={invites}
        pagination={false}
        locale={{ emptyText: msg('noTeamInvites') }}
      />

      {/* owner 专属危险区：解散团队（输入团队名二次确认）。 */}
      {isOwner && (
        <div className="member-danger-zone">
          <div className="member-danger-title">
            <AlertTriangle size={14} strokeWidth={2} aria-hidden="true" /> {msg('dissolveDangerZone')}
          </div>
          <p className="hint">{formatMessage(msg('dissolveTeamConfirm'), { name: teamName })}</p>
          <Button danger onClick={() => { setDissolveOpen(true); setDissolveName('') }}>
            {msg('dissolveTeam')}
          </Button>
        </div>
      )}

      {/* 解散确认弹窗（受控：输入团队名匹配才可确认）。 */}
      <AntdModal
        open={dissolveOpen}
        title={msg('dissolveTeam')}
        okText={msg('dissolveTeam')}
        okButtonProps={{ danger: true, disabled: dissolveName.trim() !== teamName }}
        cancelText={zh ? '取消' : 'Cancel'}
        confirmLoading={dissolveBusy}
        onOk={() => void handleDissolve()}
        onCancel={() => { if (!dissolveBusy) setDissolveOpen(false) }}
        width="min(460px, 92vw)"
      >
        <p className="hint">{formatMessage(msg('dissolveTeamConfirm'), { name: teamName })}</p>
        <p className="hint">{formatMessage(msg('dissolveTeamTypeName'), { name: teamName })}</p>
        <Input
          autoFocus
          allowClear
          status={dissolveName.trim() !== '' && dissolveName.trim() !== teamName ? 'error' : undefined}
          value={dissolveName}
          onChange={(e) => setDissolveName(e.target.value)}
          placeholder={teamName}
        />
        {dissolveName.trim() !== '' && dissolveName.trim() !== teamName && (
          <p className="error-text">{msg('dissolveTeamNameMismatch')}</p>
        )}
      </AntdModal>
    </Modal>
  )
}

/** 由 user_id 稳定生成的头像底色（HSL 拉开色相，饱和度/亮度受限保证可读）。 */
function avatarColor(id: string): string {
  let hash = 0
  for (let i = 0; i < id.length; i++) hash = (hash * 31 + id.charCodeAt(i)) | 0
  return `hsl(${Math.abs(hash) % 360}, 55%, 45%)`
}
