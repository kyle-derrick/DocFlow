import { FormEvent, useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { App as AntdApp, Button, Dropdown, Input, Space as AntdSpace, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import { MoreHorizontal, Search } from 'lucide-react'
import {
  Space as SpaceType,
  SpaceRole,
  createSpace,
  currentUserId,
  leaveSpace,
  listSpaces,
  updateSpaceQuota,
} from '../api'
import { Modal, formatQuota, formatTime } from '../components/FileBrowser'
import QuotaInput from '../components/QuotaInput'
import SpaceManageModal from '../components/SpaceManageModal'
import type { SpaceManageTab } from '../components/SpaceManageModal'
import { SpaceAvatar } from '../components/SpaceSwitcher'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 角色 → 展示名 i18n key（卡片徽章，与 SpaceManageModal 一致）。 */
const ROLE_LABEL_KEYS: Record<SpaceRole, MessageKey> = {
  owner: 'roleOwnerLabel',
  admin: 'roleAdminLabel',
  member_share: 'roleMemberShareLabel',
  member: 'roleMemberLabel',
  guest: 'roleGuestLabel',
}

/** 角色 → 权限说明 i18n key（徽章 tooltip）。 */
const ROLE_TIP_KEYS: Record<SpaceRole, MessageKey> = {
  owner: 'roleOwnerTip',
  admin: 'roleAdminTip',
  member_share: 'roleMemberShareTip',
  member: 'roleMemberTip',
  guest: 'roleGuestTip',
}

/** 空间搜索防抖（毫秒）。 */
const SEARCH_DEBOUNCE_MS = 250

/**
 * 空间管理页（统一空间模型，顶部导航「空间」入口）：「新建空间」弹窗
 * （v2.2：名/描述/配额，平铺表单弹窗化）+ 我的空间卡片（徽标 / 名称 /
 * 我的角色 / 描述 / 成员数 / 配额用量进度条 / 创建时间）。卡片主体点击 =
 * 直接进入 /files?space=；操作收进右上角「⋯」Dropdown（v2.3：空间管理 /
 * 解散——解散不再独立确认，直接打开空间管理弹窗「空间设置」tab 的危险区
 *（输入空间名二次确认），全站仅此一处解散入口；非 owner 为「离开」）。
 * 卡片不再有底部按钮行，高度由内容自然决定。
 */
export default function SpacesPage() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const navigate = useNavigate()
  const { modal: antdModal } = AntdApp.useApp()
  const [spaces, setSpaces] = useState<SpaceType[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // 「新建空间」弹窗（v2.2：名/描述/配额）。
  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  // 配额为字节数（0=不限），经 QuotaInput（数值+单位，1024 进制）输入。
  const [quotaBytes, setQuotaBytes] = useState(0)
  const [busy, setBusy] = useState(false)
  const [formError, setFormError] = useState('')

  // 搜索过滤（v2.6：按名称/描述，250ms 防抖）。
  const [search, setSearch] = useState('')
  const [searchApplied, setSearchApplied] = useState('')
  useEffect(() => {
    const timer = window.setTimeout(() => setSearchApplied(search.trim().toLowerCase()), SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(timer)
  }, [search])

  // 「空间管理」综合弹窗宿主空间（owner/admin）+ 初始 tab（解散入口直开设置 tab）。
  const [manageSpace, setManageSpace] = useState<SpaceType | null>(null)
  const [manageTab, setManageTab] = useState<SpaceManageTab>('members')

  const myId = currentUserId()

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      setSpaces(await listSpaces())
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('teamLoadFailed'))
      setSpaces([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 过滤后的空间列表（名称/描述包含命中，大小写不敏感）。 */
  const visibleSpaces = useMemo(() => {
    if (!searchApplied) return spaces
    return spaces.filter((sp) =>
      sp.name.toLowerCase().includes(searchApplied) || (sp.description ?? '').toLowerCase().includes(searchApplied))
  }, [spaces, searchApplied])

  const handleCreate = async (e: FormEvent) => {
    e.preventDefault()
    const spaceName = name.trim()
    if (!spaceName || busy) return
    setBusy(true)
    setFormError('')
    try {
      const created = await createSpace(spaceName, desc.trim())
      // 配额可选：>0 时创建后单独设置（0 = 不限，跳过）。
      if (quotaBytes > 0) await updateSpaceQuota(created.id, quotaBytes)
      setName('')
      setDesc('')
      setQuotaBytes(0)
      setCreateOpen(false)
      await load()
    } catch (err) {
      setFormError(err instanceof Error ? err.message : msg('teamCreateFailed'))
    } finally {
      setBusy(false)
    }
  }

  /** 打开空间管理弹窗（tab 缺省「成员」；解散入口直开「空间设置」危险区）。 */
  const openManage = (sp: SpaceType, tab: SpaceManageTab = 'members') => {
    setManageSpace(sp)
    setManageTab(tab)
  }

  const leave = (sp: SpaceType) => {
    antdModal.confirm({
      title: msg('leaveTeam'),
      content: formatMessage(msg('leaveTeamConfirm'), { name: sp.name }),
      okText: msg('leaveTeam'),
      okButtonProps: { danger: true },
      cancelText: msg('cancel'),
      onOk: async () => {
        try { await leaveSpace(sp.id); await load() } catch (err) { setError(err instanceof Error ? err.message : msg('leaveTeamFailed')) }
      },
    })
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('teamsTitle')}</h2>
        <div className="page-head-actions">
          {/* 搜索过滤（v2.6：按名称/描述，250ms 防抖）。 */}
          <Input
            allowClear
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            prefix={<Search size={14} strokeWidth={2} aria-hidden="true" />}
            placeholder={locale === 'zh-CN' ? '搜索空间（名称 / 描述）' : 'Search spaces (name / description)'}
            aria-label={locale === 'zh-CN' ? '搜索空间' : 'Search spaces'}
            style={{ width: 240 }}
          />
          <Button type="text" onClick={() => void load()}>{msg('refresh')}</Button>
          <Button type="primary" onClick={() => { setCreateOpen(true); setName(''); setDesc(''); setQuotaBytes(0) }}>
            {msg('createTeamTitle')}
          </Button>
        </div>
      </div>

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}
      {!loading && !error && spaces.length === 0 && (
        <div className="empty">{msg('teamsEmpty')}</div>
      )}
      {!loading && !error && spaces.length > 0 && visibleSpaces.length === 0 && (
        <div className="empty">{locale === 'zh-CN' ? '没有匹配的空间' : 'No matching spaces'}</div>
      )}

      {!loading && !error && visibleSpaces.length > 0 && (
        <div className="team-grid">
          {visibleSpaces.map((sp) => {
            const isOwner = myId !== null && sp.owner_id === myId
            const role = sp.my_role ?? (isOwner ? 'owner' : 'member')
            const canManage = role === 'owner' || role === 'admin'
            const quota = sp.quota_bytes ?? 0
            const used = sp.storage_used ?? 0
            const pct = quota > 0 ? Math.min(100, Math.round((used / quota) * 100)) : 0
            // 右上角「管理」SplitButton 的下拉项（v2.4；v2.5 文案精简）：
            // 主按钮=管理弹窗（「管理」），下拉只放危险项——解散（owner 非默认，
            // 直开管理弹窗设置 tab 危险区）/ 离开（非 owner）。
            const menuItems: MenuProps['items'] = [
              ...(isOwner && !sp.is_default ? [{ key: 'dissolve', label: msg('dissolveShort'), danger: true }] : []),
              ...(!isOwner ? [{ key: 'leave', label: msg('leaveTeam'), danger: true }] : []),
            ]
            const onMenu: MenuProps['onClick'] = ({ key }) => {
              if (key === 'dissolve') openManage(sp, 'settings')
              else if (key === 'leave') leave(sp)
            }
            return (
              <div key={sp.id} className="team-card team-card-manage">
                {/* 卡片主体点击 = 直接进入该空间文件页；右上角「管理」
                    SplitButton（v2.4：主按钮开管理弹窗 + 下拉危险项「解散」，
                    样式对齐文件页上传按钮组；非 owner 为「离开」）。 */}
                <button
                  className="team-card-main"
                  title={locale === 'zh-CN' ? `点击进入「${sp.name}」的文件页` : `Open files of “${sp.name}”`}
                  onClick={() => navigate(`/files?space=${sp.id}`)}
                >
                  <div className="team-card-head">
                    <SpaceAvatar name={sp.name} size={22} />
                    <span className="team-card-name">{sp.name}</span>
                    {sp.is_default && <span className="badge badge-default">{msg('defaultSpaceBadge')}</span>}
                    <Tooltip title={msg(ROLE_TIP_KEYS[role])}>
                      <span className={`badge role-${role}`}>{msg(ROLE_LABEL_KEYS[role])}</span>
                    </Tooltip>
                  </div>
                  <p className="team-card-desc">{sp.description || msg('noDesc')}</p>
                  <div className="quota-bar" title={quota > 0 ? formatMessage(msg('quotaUsedOf'), { used: formatQuota(used, false), quota: formatQuota(quota) }) : `${formatQuota(used, false)} / ${msg('quotaUnlimited')}`}>
                    <div className={`quota-fill${pct >= 90 ? ' quota-danger' : pct >= 75 ? ' quota-warn' : ''}`} style={{ width: quota > 0 ? `${pct}%` : '0%' }} />
                  </div>
                  <div className="team-card-stats">
                    <span className="muted">
                      {msg('memberCountLabel')} {sp.member_count ?? '—'}
                      {typeof sp.storage_used === 'number' && (
                        <> · {msg('storageUsedLabel')} {formatQuota(used, false)}{quota > 0 ? ` / ${formatQuota(quota)}` : ` / ${msg('quotaUnlimited')}`}</>
                      )}
                    </span>
                    <span className="muted team-card-time">{msg('createdAt')} {formatTime(sp.created_at)}</span>
                  </div>
                </button>
                <div className="team-card-actions-split" onClick={(e) => e.stopPropagation()}>
                  {canManage ? (
                    <AntdSpace.Compact size="small">
                      <Button size="small" type="primary" title={msg('spaceManage')} onClick={() => openManage(sp)}>{msg('spaceManageShort')}</Button>
                      {menuItems.length > 0 && (
                        <Dropdown menu={{ items: menuItems, onClick: onMenu }} trigger={['click']} placement="bottomRight">
                          <Button size="small" type="primary" aria-label={locale === 'zh-CN' ? '更多操作' : 'More actions'}>
                            <MoreHorizontal size={13} strokeWidth={2} aria-hidden="true" />
                          </Button>
                        </Dropdown>
                      )}
                    </AntdSpace.Compact>
                  ) : (
                    menuItems.length > 0 && (
                      <Button size="small" danger onClick={() => leave(sp)}>{msg('leaveTeam')}</Button>
                    )
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}

      {/* 新建空间弹窗（v2.2：平铺表单弹窗化；名/描述/配额）。 */}
      {createOpen && (
        <Modal title={msg('createTeamTitle')} onClose={() => { if (!busy) { setCreateOpen(false); setFormError('') } }}>
          <form className="team-create-row" style={{ flexDirection: 'column', alignItems: 'stretch' }} onSubmit={handleCreate}>
            <label className="field">
              <span>{msg('teamNameLabel')}（≤100 字符）</span>
              <Input
                autoFocus
                allowClear
                maxLength={100}
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={locale === 'zh-CN' ? '例如：平台组' : 'e.g. Platform'}
              />
            </label>
            <label className="field">
              <span>{msg('teamDescLabel')}</span>
              <Input
                allowClear
                maxLength={200}
                value={desc}
                onChange={(e) => setDesc(e.target.value)}
                placeholder={locale === 'zh-CN' ? '空间用途说明' : 'What is this space for?'}
              />
            </label>
            <div className="field">
              <span>{msg('quotaLabel')}（0 = 不限，创建后可在空间设置中修改）</span>
              <QuotaInput value={quotaBytes} onChange={setQuotaBytes} />
            </div>
            {formError && <div className="error-text">{formError}</div>}
            <div className="setting-control" style={{ marginTop: 8, justifyContent: 'flex-end' }}>
              <Button type="primary" htmlType="submit" disabled={busy || !name.trim()}>
                {busy ? msg('creating') : msg('createTeamTitle')}
              </Button>
              <Button disabled={busy} onClick={() => { setCreateOpen(false); setFormError('') }}>
                {msg('cancel')}
              </Button>
            </div>
          </form>
        </Modal>
      )}

      {/* 空间管理综合弹窗（owner/admin）：成员/用户组/邀请/空间设置
          （改名/描述/配额/转让所有权/解散危险区）；initialTab 支持 ⋯ 菜单
          「解散」直开设置 tab。 */}
      {manageSpace && (
        <SpaceManageModal
          space={manageSpace}
          myRole={manageSpace.my_role ?? (myId !== null && manageSpace.owner_id === myId ? 'owner' : 'member')}
          isOwner={myId !== null && manageSpace.owner_id === myId}
          initialTab={manageTab}
          onClose={() => setManageSpace(null)}
          onChanged={() => void load()}
        />
      )}
    </div>
  )
}
