import { FormEvent, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Users } from 'lucide-react'
import { App as AntdApp, Button, Input, Tooltip } from 'antd'
import {
  Team,
  TeamRole,
  createTeam,
  currentUserId,
  deleteTeam,
  leaveTeam,
  listTeams,
  updateTeam,
} from '../api'
import TeamMembersModal from '../components/TeamMembersModal'
import { formatTime, promptViaModal } from '../components/FileBrowser'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 角色 → 展示名 i18n key（卡片徽章，与 TeamMembersModal 一致）。 */
const ROLE_LABEL_KEYS: Record<TeamRole, MessageKey> = {
  owner: 'roleOwnerLabel',
  admin: 'roleAdminLabel',
  member_share: 'roleMemberShareLabel',
  member: 'roleMemberLabel',
  guest: 'roleGuestLabel',
}

/** 角色 → 权限说明 i18n key（徽章 tooltip）。 */
const ROLE_TIP_KEYS: Record<TeamRole, MessageKey> = {
  owner: 'roleOwnerTip',
  admin: 'roleAdminTip',
  member_share: 'roleMemberShareTip',
  member: 'roleMemberTip',
  guest: 'roleGuestTip',
}

/** 存储用量展示（字节 → 可读；与仪表盘同口径）。 */
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 ** 3) return `${(n / 1024 ** 2).toFixed(1)} MB`
  return `${(n / 1024 ** 3).toFixed(2)} GB`
}

/**
 * 团队管理页（v1.7 重构，顶部导航「团队」入口）：创建团队 + 我的团队
 * 卡片（名称 / 我的角色（五级内置，tooltip 权限说明）/ 描述 / 成员数 /
 * 存储用量 / 创建时间），操作：进入（卡片点击）、成员管理（owner/admin，
 * 弹窗内邀请/改派/移除/转让[owner]）、编辑（owner）、解散（owner）、
 * 离开团队（非 owner 成员）。
 */
export default function TeamsPage() {
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const navigate = useNavigate()
  const [teams, setTeams] = useState<Team[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [busy, setBusy] = useState(false)
  const [formError, setFormError] = useState('')

  // 「成员管理」弹窗宿主团队（owner/admin）。
  const [manageTeam, setManageTeam] = useState<Team | null>(null)

  const myId = currentUserId()

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      setTeams(await listTeams())
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('teamLoadFailed'))
      setTeams([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const handleCreate = async (e: FormEvent) => {
    e.preventDefault()
    const teamName = name.trim()
    if (!teamName) return
    setBusy(true)
    setFormError('')
    try {
      await createTeam(teamName, desc.trim())
      setName('')
      setDesc('')
      await load()
    } catch (err) {
      setFormError(err instanceof Error ? err.message : msg('teamCreateFailed'))
    } finally {
      setBusy(false)
    }
  }

  const rename = async (tm: Team) => {
    const input = await promptViaModal(antdModal, {
      title: msg('edit'),
      label: msg('teamNameLabel'),
      initialValue: tm.name,
      okText: msg('save'),
      cancelText: msg('cancel'),
    })
    const next = input?.trim()
    if (!next || next === tm.name) return
    try { await updateTeam(tm.id, next, tm.description); await load() } catch (err) { setError(err instanceof Error ? err.message : msg('teamUpdateFailed')) }
  }
  const remove = (tm: Team) => {
    antdModal.confirm({
      title: msg('dissolveTeam'),
      content: formatMessage(msg('dissolveTeamConfirm'), { name: tm.name }),
      okText: msg('dissolveTeam'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        try { await deleteTeam(tm.id); await load() } catch (err) { setError(err instanceof Error ? err.message : msg('teamDeleteFailed')) }
      },
    })
  }
  const leave = (tm: Team) => {
    antdModal.confirm({
      title: msg('leaveTeam'),
      content: formatMessage(msg('leaveTeamConfirm'), { name: tm.name }),
      okText: msg('leaveTeam'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        try { await leaveTeam(tm.id); await load() } catch (err) { setError(err instanceof Error ? err.message : msg('leaveTeamFailed')) }
      },
    })
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('teamsTitle')}</h2>
        <Button type="text" onClick={() => void load()}>{msg('refresh')}</Button>
      </div>

      <form className="panel team-create" onSubmit={handleCreate}>
        <h3>{msg('createTeamTitle')}</h3>
        <div className="team-create-row">
          <label className="field">
            <span>{msg('teamNameLabel')}</span>
            <Input autoFocus allowClear value={name} onChange={(e) => setName(e.target.value)} placeholder={locale === 'zh-CN' ? '例如：平台组' : 'e.g. Platform'} maxLength={100} />
          </label>
          <label className="field">
            <span>{msg('teamDescLabel')}</span>
            <Input allowClear value={desc} onChange={(e) => setDesc(e.target.value)} placeholder={locale === 'zh-CN' ? '团队用途说明' : 'What is this team for?'} />
          </label>
          <Button type="primary" htmlType="submit" disabled={busy || !name.trim()}>
            {busy ? msg('creating') : msg('createTeamTitle')}
          </Button>
        </div>
        {formError && <div className="error-text">{formError}</div>}
      </form>

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && !error && teams.length === 0 && (
        <div className="empty">{msg('teamsEmpty')}</div>
      )}

      {!loading && !error && teams.length > 0 && (
        <div className="team-grid">
          {teams.map((tm) => {
            const isOwner = myId !== null && tm.owner_id === myId
            const role = tm.my_role ?? (isOwner ? 'owner' : 'member')
            const canManage = role === 'owner' || role === 'admin'
            return (
              <div key={tm.id} className="team-card team-card-manage">
                <button className="team-card-main" onClick={() => navigate(`/teams/${tm.id}`)}>
                  <div className="team-card-head">
                    <span className="icon"><Users size={14} strokeWidth={2} aria-hidden="true" /></span>
                    <span className="team-card-name">{tm.name}</span>
                    <Tooltip title={msg(ROLE_TIP_KEYS[role])}>
                      <span className={`badge role-${role}`}>{msg(ROLE_LABEL_KEYS[role])}</span>
                    </Tooltip>
                  </div>
                  <p className="team-card-desc">{tm.description || msg('noDesc')}</p>
                  <div className="team-card-stats">
                    <span className="muted">
                      {msg('memberCountLabel')} {tm.member_count ?? '—'}
                      {typeof tm.storage_used === 'number' && (
                        <> · {msg('storageUsedLabel')} {formatBytes(tm.storage_used)}</>
                      )}
                    </span>
                    <span className="muted team-card-time">{msg('createdAt')} {formatTime(tm.created_at)}</span>
                  </div>
                </button>
                <div className="team-card-actions">
                  <Button size="small" onClick={() => navigate(`/teams/${tm.id}`)}>{msg('enterTeam')}</Button>
                  {canManage && (
                    <Button size="small" onClick={() => setManageTeam(tm)}>{msg('memberManage')}</Button>
                  )}
                  {isOwner && (
                    <Button size="small" type="text" onClick={() => void rename(tm)}>{msg('edit')}</Button>
                  )}
                  {isOwner ? (
                    <Button size="small" type="text" danger onClick={() => remove(tm)}>{msg('dissolveTeam')}</Button>
                  ) : (
                    <Button size="small" type="text" danger onClick={() => leave(tm)}>{msg('leaveTeam')}</Button>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}

      {/* 成员管理弹窗（owner/admin）：邀请/改派/移除；owner 可转让所有权。 */}
      {manageTeam && (
        <TeamMembersModal
          teamId={manageTeam.id}
          teamName={manageTeam.name}
          isOwner={myId !== null && manageTeam.owner_id === myId}
          onClose={() => setManageTeam(null)}
          onChanged={() => void load()}
        />
      )}
    </div>
  )
}
