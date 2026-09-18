import { FormEvent, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { App as AntdApp, Button, Input, Select } from 'antd'
import {
  ACLAction,
  FileItem,
  FileQueryOptions,
  FolderACLEntry,
  RolePermissions,
  Team,
  TeamMember,
  TeamRoleDef,
  UserSearchResult,
  UUID_RE,
  addTeamMember,
  createTeamFolder,
  createTeamRole,
  currentUserId,
  deleteTeamRole,
  getFolderACL,
  listTeamFiles,
  listTeamMembers,
  listTeamRoles,
  listTeams,
  putFolderACL,
  removeTeamMember,
  searchUsers,
  updateTeamMemberRole,
  updateTeamRole,
  uploadFile,
} from '../api'
import { FileBrowserWithTree } from '../components/FolderTreeNav'
import { DirListing, Modal, formatTime } from '../components/FileBrowser'
import VersionHistoryModal from '../components/VersionHistoryModal'
import SpaceSwitcher from '../components/SpaceSwitcher'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 权限动作清单（与后端 team.ValidActions 一致，设计 6.5.2）。 */
const PERM_ACTIONS = ['read', 'write', 'delete', 'share', 'admin'] as const

/** 权限动作 → i18n key（角色卡片与勾选表单共用）。 */
const PERM_LABEL_KEYS: Record<string, MessageKey> = {
  read: 'permRead',
  write: 'permWrite',
  delete: 'permDelete',
  share: 'permShare',
  admin: 'permAdmin',
}

/** 路径级 ACL 权限动作清单（不含 admin，与后端 folder ACL 契约一致）。 */
const ACL_ACTIONS: readonly ACLAction[] = ['read', 'write', 'delete', 'share']

/** ACL 主体类型 → i18n key（条目表格与添加行共用）。 */
const ACL_SUBJECT_KEYS: Record<string, MessageKey> = {
  user: 'aclSubjectUser',
  team: 'aclSubjectTeam',
  role: 'aclSubjectRole',
}

/** 成员表下拉的选项值：系统角色 'viewer'/'editor' 或自定义角色 'role:<id>'。 */
function memberRoleValue(m: TeamMember): string {
  if (m.role === 'custom' && m.role_id) return `role:${m.role_id}`
  return m.role === 'owner' ? 'owner' : m.role
}

/** 成员显示名：nickname 优先，回退 username，再回退 UUID 前 8 位（后端
 * 成员列表已 JOIN users 补齐 username/nickname）。 */
function memberDisplayName(m: TeamMember): string {
  return m.nickname || m.username || `${m.user_id.slice(0, 8)}…`
}

/** 权限摘要（角色卡片展示）：勾选动作 + deny 列表。 */
function permSummary(p: RolePermissions, label: (key: MessageKey) => string): string {
  const allowed = PERM_ACTIONS.filter((a) => p[a])
  const denied = p.deny ?? []
  const parts: string[] = []
  if (allowed.length > 0) parts.push(allowed.map((a) => label(PERM_LABEL_KEYS[a])).join('/'))
  if (denied.length > 0) parts.push(`${denied.map((a) => label(PERM_LABEL_KEYS[a])).join('/')} ×`)
  return parts.length > 0 ? parts.join('，') : '—'
}

/** 角色编辑表单状态（创建与更新共用）。 */
interface RoleFormState {
  name: string
  perms: Record<string, boolean>
  deny: Record<string, boolean>
}

const emptyRoleForm: RoleFormState = {
  name: '',
  perms: {},
  deny: {},
}

function roleToForm(r: TeamRoleDef): RoleFormState {
  const perms: Record<string, boolean> = {}
  const deny: Record<string, boolean> = {}
  for (const a of PERM_ACTIONS) {
    perms[a] = Boolean(r.permissions[a])
    deny[a] = Boolean(r.permissions.deny?.includes(a))
  }
  return { name: r.name, perms, deny }
}

function formToPermissions(f: RoleFormState): RolePermissions {
  const perms: RolePermissions = {}
  for (const a of PERM_ACTIONS) if (f.perms[a]) perms[a] = true
  const deny = PERM_ACTIONS.filter((a) => f.deny[a])
  if (deny.length > 0) perms.deny = deny
  return perms
}

/**
 * 团队空间：中间团队文件浏览（列表 / 上传 / 下载 / 预览 / 新建文件夹 /
 * 版本历史复用 FileBrowser；左侧目录树见 FolderTreeNav），右侧成员管理
 * 面板（280px 可折叠；owner 可添加/移除成员、改派角色）与自定义角色
 * 管理卡（CRUD + 权限勾选 + deny 显式拒绝，设计 6.5.2）；写操作 403
 * 提示「无写权限」。
 */
export default function TeamSpacePage() {
  const { id = '' } = useParams()
  const locale = useLocale()
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)

  const [team, setTeam] = useState<Team | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // 全局视图（全部 / 收藏 / 最近）：SpaceSwitcher 切换、透传 FileBrowser。
  const [spaceView, setSpaceView] = useState<'all' | 'starred' | 'recent'>('all')
  // 右侧成员面板折叠开关。
  const [memberCollapsed, setMemberCollapsed] = useState(false)

  const [members, setMembers] = useState<TeamMember[]>([])
  const [memberError, setMemberError] = useState('')
  const [memberUserId, setMemberUserId] = useState('')
  const [memberQuery, setMemberQuery] = useState('')
  const [memberOptions, setMemberOptions] = useState<UserSearchResult[]>([])
  const [memberSearching, setMemberSearching] = useState(false)
  const [memberRole, setMemberRole] = useState<string>('viewer')
  const [memberBusy, setMemberBusy] = useState(false)

  const [roles, setRoles] = useState<TeamRoleDef[]>([])
  const [roleError, setRoleError] = useState('')
  const [roleForm, setRoleForm] = useState<RoleFormState>(emptyRoleForm)
  const [editingRoleId, setEditingRoleId] = useState<string | null>(null)
  const [roleBusy, setRoleBusy] = useState(false)

  const [fileReloadKey, setFileReloadKey] = useState(0)
  const [historyTarget, setHistoryTarget] = useState<FileItem | null>(null)

  // ---- 路径级 ACL（文件夹行「权限」按钮，仅 owner 可见；保存整体 PUT 覆盖） ----
  const [aclTarget, setAclTarget] = useState<FileItem | null>(null)
  const [aclEntries, setAclEntries] = useState<FolderACLEntry[]>([])
  const [aclLoading, setAclLoading] = useState(false)
  const [aclBusy, setAclBusy] = useState(false)
  const [aclError, setAclError] = useState('')
  const [aclNotice, setAclNotice] = useState('')
  const [aclNewType, setAclNewType] = useState<FolderACLEntry['subject_type']>('user')
  const [aclNewId, setAclNewId] = useState('')
  const [aclNewEffect, setAclNewEffect] = useState<FolderACLEntry['effect']>('allow')
  const [aclNewPerms, setAclNewPerms] = useState<Record<string, boolean>>({ read: true })

  // 保存成功 toast：4 秒自动消失。
  useEffect(() => {
    if (!aclNotice) return
    const timer = window.setTimeout(() => setAclNotice(''), 4000)
    return () => window.clearTimeout(timer)
  }, [aclNotice])

  const openAcl = async (folder: FileItem) => {
    setAclTarget(folder)
    setAclEntries([])
    setAclError('')
    setAclLoading(true)
    setAclNewType('user')
    setAclNewId('')
    setAclNewEffect('allow')
    setAclNewPerms({ read: true })
    try {
      setAclEntries(await getFolderACL(folder.id))
    } catch (err) {
      setAclError(err instanceof Error ? err.message : msg('loadFailed'))
    } finally {
      setAclLoading(false)
    }
  }

  const closeAcl = () => {
    if (aclBusy) return
    setAclTarget(null)
  }

  const addAclEntry = (e: FormEvent) => {
    e.preventDefault()
    const subjectId = aclNewId.trim()
    if (!UUID_RE.test(subjectId)) {
      setAclError(msg('uuidInvalid'))
      return
    }
    const permissions = ACL_ACTIONS.filter((a) => aclNewPerms[a])
    if (permissions.length === 0) {
      setAclError(msg('aclNoPerms'))
      return
    }
    setAclEntries((prev) => [
      ...prev,
      { subject_type: aclNewType, subject_id: subjectId, effect: aclNewEffect, permissions: [...permissions] },
    ])
    setAclNewId('')
    setAclError('')
  }

  const removeAclEntry = (index: number) => {
    setAclEntries((prev) => prev.filter((_, i) => i !== index))
  }

  const setAclEntryEffect = (index: number, effect: FolderACLEntry['effect']) => {
    setAclEntries((prev) => prev.map((entry, i) => (i === index ? { ...entry, effect } : entry)))
  }

  const toggleAclEntryPerm = (index: number, action: ACLAction) => {
    setAclEntries((prev) =>
      prev.map((entry, i) =>
        i === index
          ? {
              ...entry,
              permissions: entry.permissions.includes(action)
                ? entry.permissions.filter((a) => a !== action)
                : [...entry.permissions, action],
            }
          : entry,
      ),
    )
  }

  const handleSaveAcl = async () => {
    if (!aclTarget) return
    setAclBusy(true)
    setAclError('')
    try {
      await putFolderACL(aclTarget.id, aclEntries)
      setAclTarget(null)
      setAclNotice(msg('aclSaved'))
    } catch (err) {
      setAclError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setAclBusy(false)
    }
  }

  const myId = currentUserId()
  const isOwner = team !== null && myId !== null && team.owner_id === myId

  const loadMembers = async () => {
    setMemberError('')
    try {
      setMembers(await listTeamMembers(id))
    } catch (err) {
      setMembers([])
      setMemberError(err instanceof Error ? err.message : msg('membersLoadFailed'))
    }
  }

  const loadRoles = async () => {
    if (!isOwner) return
    try {
      setRoles(await listTeamRoles(id))
    } catch {
      // 非.owner 或网络异常：保持空列表（角色卡仅 owner 可用）。
      setRoles([])
    }
  }

  useEffect(() => {
    const init = async () => {
      setLoading(true)
      setError('')
      try {
        // 契约无单团队查询端点，从「我的团队」列表中定位（非成员不在列表中）。
        const teams = await listTeams()
        const found = teams.find((t) => t.id === id) ?? null
        setTeam(found)
        if (found) {
          await loadMembers()
          // 角色列表仅 owner 可查（后端 403），初始化时按归属判定加载。
          if (myId !== null && found.owner_id === myId) {
            try {
              setRoles(await listTeamRoles(id))
            } catch {
              setRoles([])
            }
          }
        } else {
          setError(msg('teamNotFound'))
        }
      } catch (err) {
        setTeam(null)
        setError(err instanceof Error ? err.message : msg('teamLoadFailed'))
      } finally {
        setLoading(false)
      }
    }
    void init()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])

  useEffect(() => {
    const q = memberQuery.trim()
    if (q.length < 2 || memberUserId) {
      setMemberOptions([])
      return
    }
    const timer = window.setTimeout(() => {
      setMemberSearching(true)
      void searchUsers(q).then(setMemberOptions).catch(() => setMemberOptions([])).finally(() => setMemberSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [memberQuery, memberUserId])

  const handleAddMember = async (e: FormEvent) => {
    e.preventDefault()
    const uid = memberUserId.trim()
    if (!UUID_RE.test(uid)) return
    setMemberBusy(true)
    setMemberError('')
    try {
      if (memberRole.startsWith('role:')) {
        await addTeamMember(id, uid, 'viewer', memberRole.slice(5))
      } else {
        await addTeamMember(id, uid, memberRole as 'editor' | 'viewer')
      }
      setMemberUserId('')
      setMemberQuery('')
      setMemberOptions([])
      await loadMembers()
      await loadRoles()
    } catch (err) {
      setMemberError(err instanceof Error ? err.message : msg('addMemberFailed'))
    } finally {
      setMemberBusy(false)
    }
  }

  const handleRemoveMember = (m: TeamMember) => {
    antdModal.confirm({
      title: msg('delete'),
      content: `确定移除该成员（${m.user_id.slice(0, 8)}…）？`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setMemberError('')
        try {
          await removeTeamMember(id, m.user_id)
          await loadMembers()
          await loadRoles()
        } catch (err) {
          setMemberError(err instanceof Error ? err.message : msg('removeMemberFailed'))
        }
      },
    })
  }

  /** 改派成员角色：下拉含系统角色（owner 除外）与全部自定义角色。 */
  const handleChangeRole = async (m: TeamMember, value: string) => {
    setMemberError('')
    try {
      if (value.startsWith('role:')) {
        await updateTeamMemberRole(id, m.user_id, 'viewer', value.slice(5))
      } else {
        await updateTeamMemberRole(id, m.user_id, value as 'editor' | 'viewer')
      }
      await loadMembers()
      await loadRoles()
    } catch (err) {
      setMemberError(err instanceof Error ? err.message : msg('changeRoleFailed'))
      await loadMembers()
    }
  }

  const startEditRole = (r: TeamRoleDef) => {
    setEditingRoleId(r.id)
    setRoleForm(roleToForm(r))
    setRoleError('')
  }

  const resetRoleForm = () => {
    setEditingRoleId(null)
    setRoleForm(emptyRoleForm)
    setRoleError('')
  }

  const handleSaveRole = async (e: FormEvent) => {
    e.preventDefault()
    if (!roleForm.name.trim()) return
    setRoleBusy(true)
    setRoleError('')
    const permissions = formToPermissions(roleForm)
    try {
      if (editingRoleId) {
        await updateTeamRole(id, editingRoleId, roleForm.name.trim(), permissions)
      } else {
        await createTeamRole(id, roleForm.name.trim(), permissions)
      }
      resetRoleForm()
      await loadRoles()
    } catch (err) {
      setRoleError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setRoleBusy(false)
    }
  }

  const handleDeleteRole = (r: TeamRoleDef) => {
    antdModal.confirm({
      title: msg('delete'),
      content: `确定删除角色「${r.name}」？仍有成员引用时将被拒绝。`,
      okText: msg('delete'),
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '取消' : 'Cancel',
      onOk: async () => {
        setRoleError('')
        try {
          await deleteTeamRole(id, r.id)
          if (editingRoleId === r.id) resetRoleForm()
          await loadRoles()
        } catch (err) {
          setRoleError(err instanceof Error ? err.message : msg('operationFailed'))
        }
      },
    })
  }

  const listItems = async (parentId: string | null, opts?: FileQueryOptions): Promise<DirListing> => {
    const res = await listTeamFiles(id, parentId, opts)
    return { items: res.files ?? [], folderId: res.parent_id }
  }

  return (
    <div className="page wide-page">
      <SpaceSwitcher activeView={spaceView} onViewChange={setSpaceView} />
      <div className="team-back">
        <Button type="text" size="small"><Link to="/teams" className="plain-link">{msg('backToTeams')}</Link></Button>
      </div>

      {error && <div className="banner error">{error}</div>}
      {aclNotice && <div className="banner ok">{aclNotice}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}

      {!loading && team && (
        <div className="team-workspace">
          {/* 中区：团队文件浏览（左侧目录树 + 文件列表，复用 FileBrowser）。 */}
          <div className="team-workspace-main">
            <FileBrowserWithTree
              rootLabel={team.name}
              listItems={listItems}
              createFolderFn={(name, parentId) => createTeamFolder(id, name, parentId)}
              uploadFn={uploadFile}
              reloadKey={fileReloadKey}
              activeView={spaceView}
              ns={{ type: 'team', scope: id }}
              rowActions={(item) => (
                <div className="row-actions-group">
                  {item.type === 'folder' && isOwner && (
                    <Button size="small" onClick={() => void openAcl(item)}>{msg('acl')}</Button>
                  )}
                  {item.type === 'file' && (
                    <Button size="small" onClick={() => setHistoryTarget(item)}>历史</Button>
                  )}
                </div>
              )}
              emptyHint={msg('teamSpaceEmpty')}
            />
          </div>

          {/* 右侧：成员管理面板（280px 可折叠；添加成员 / 成员列表 / 角色管理）。 */}
          <aside className={`member-panel team-member-panel${memberCollapsed ? ' collapsed' : ''}`}>
            {memberCollapsed ? (
              <Button
                type="text"
                size="small"
                className="team-member-collapse-btn"
                title={locale === 'zh-CN' ? '展开成员面板' : 'Expand members panel'}
                aria-label={locale === 'zh-CN' ? '展开成员面板' : 'Expand members panel'}
                onClick={() => setMemberCollapsed(false)}
              >
                «
              </Button>
            ) : (
              <>
            <div className="team-member-head">
              <h3>{formatMessage(msg('membersTitle'), { n: members.length })}</h3>
              <Button
                type="text"
                size="small"
                className="team-member-collapse-btn"
                title={locale === 'zh-CN' ? '收起成员面板' : 'Collapse members panel'}
                aria-label={locale === 'zh-CN' ? '收起成员面板' : 'Collapse members panel'}
                onClick={() => setMemberCollapsed(true)}
              >
                »
              </Button>
            </div>
            {isOwner ? (
              <form className="member-add" onSubmit={handleAddMember}>
                <div className="field user-search-field">
                  <span>搜索用户</span>
                  {/* antd Select 远程搜索模式：输入即触发 memberQuery 搜索
                      （防抖见上方 effect），选中后以 memberUserId 提交。 */}
                  <Select
                    className="user-search-select"
                    showSearch
                    allowClear
                    filterOption={false}
                    value={memberUserId || undefined}
                    searchValue={memberQuery}
                    loading={memberSearching}
                    placeholder="输入昵称、用户名或邮箱（至少 2 字）"
                    notFoundContent={memberSearching ? '搜索中…' : null}
                    onSearch={(v) => {
                      setMemberQuery(v)
                      setMemberUserId('')
                    }}
                    onClear={() => {
                      setMemberQuery('')
                      setMemberUserId('')
                    }}
                    onSelect={(value, option) => {
                      setMemberUserId(String(value))
                      setMemberQuery(String(option.searchText ?? ''))
                    }}
                    options={memberOptions.map((user) => {
                      const nickname = user.nickname ?? user.profile?.nickname
                      return {
                        value: user.id,
                        searchText: nickname || user.username,
                        label: (
                          <span className="user-search-option">
                            <strong>{nickname || user.username}</strong>
                            <span className="muted">{user.username} · {user.email}</span>
                          </span>
                        ),
                      }
                    })}
                  />
                  {memberUserId && <span className="hint">已选择：{memberQuery}</span>}
                </div>
                <label className="field">
                  <span>{msg('roleLabel')}</span>
                  <Select
                    value={memberRole}
                    onChange={(v) => setMemberRole(v)}
                    options={[
                      { value: 'viewer', label: 'viewer（只读）' },
                      { value: 'editor', label: 'editor（可上传/建目录）' },
                      ...roles.map((r) => ({ value: `role:${r.id}`, label: `${r.name}（custom）` })),
                    ]}
                  />
                </label>
                <Button className="member-add-submit" type="primary" htmlType="submit" block disabled={memberBusy || !UUID_RE.test(memberUserId.trim())}>
                  {memberBusy ? msg('creating') : msg('addMember')}
                </Button>
              </form>
            ) : (
              <p className="hint member-hint">{msg('ownerOnlyHint')}</p>
            )}
            {memberError && <div className="error-text">{memberError}</div>}
            <ul className="member-list">
              {members.map((m) => (
                <li key={m.user_id} className="member-row">
                  <div className="member-info">
                    <span className="member-id" title={m.user_id}>{memberDisplayName(m)}</span>
                    {isOwner && m.role !== 'owner' ? (
                      <Select
                        className="member-role-select"
                        size="small"
                        value={memberRoleValue(m)}
                        onChange={(v) => void handleChangeRole(m, v)}
                        title="修改成员角色"
                        options={[
                          { value: 'viewer', label: 'viewer（只读）' },
                          { value: 'editor', label: 'editor（可上传/建目录）' },
                          ...roles.map((r) => ({ value: `role:${r.id}`, label: `${r.name}（custom）` })),
                        ]}
                      />
                    ) : (
                      <span className={`badge role-${m.role}`}>{m.role === 'custom' ? (m.role_name ?? 'custom') : m.role}</span>
                    )}
                  </div>
                  <div className="member-side">
                    <span className="muted member-time" title={`加入于 ${formatTime(m.created_at)}`}>
                      {formatTime(m.created_at)}
                    </span>
                    {isOwner && m.role !== 'owner' && (
                      <Button size="small" danger onClick={() => handleRemoveMember(m)}>{msg('delete')}</Button>
                    )}
                  </div>
                </li>
              ))}
            </ul>

            {isOwner && (
              <div className="role-panel">
                <h3>{formatMessage(msg('customRolesTitle'), { n: roles.length })}</h3>
                {roleError && <div className="error-text">{roleError}</div>}
                <ul className="role-list">
                  {roles.map((r) => (
                    <li key={r.id} className="role-row">
                      <div className="role-info">
                        <span className="role-name">{r.name}</span>
                        <span className="role-perms" title="权限摘要">{permSummary(r.permissions, msg)}</span>
                        <span className="muted role-count">{r.member_count}</span>
                      </div>
                      <div className="member-side">
                        <Button size="small" onClick={() => startEditRole(r)}>{msg('edit')}</Button>
                        <Button size="small" danger onClick={() => handleDeleteRole(r)}>{msg('delete')}</Button>
                      </div>
                    </li>
                  ))}
                  {roles.length === 0 && <li className="hint">{msg('noCustomRoles')}</li>}
                </ul>
                <form className="role-form" onSubmit={handleSaveRole}>
                  <label className="field">
                    <span>{editingRoleId ? '修改角色名称' : '新建角色名称'}</span>
                    <Input
                      allowClear
                      value={roleForm.name}
                      onChange={(e) => setRoleForm({ ...roleForm, name: e.target.value })}
                      placeholder="例如 审计员"
                    />
                  </label>
                  <div className="perm-grid">
                    {PERM_ACTIONS.map((a) => (
                      <label key={a} className="perm-check">
                        <input
                          type="checkbox"
                          checked={Boolean(roleForm.perms[a])}
                          onChange={(e) => setRoleForm({ ...roleForm, perms: { ...roleForm.perms, [a]: e.target.checked } })}
                        />
                        {msg(PERM_LABEL_KEYS[a])}
                      </label>
                    ))}
                  </div>
                  <div className="field">
                    <span>显式拒绝（deny，优先于勾选）</span>
                    <div className="perm-grid">
                      {PERM_ACTIONS.map((a) => (
                        <label key={a} className="perm-check deny">
                          <input
                            type="checkbox"
                            checked={Boolean(roleForm.deny[a])}
                            onChange={(e) => setRoleForm({ ...roleForm, deny: { ...roleForm.deny, [a]: e.target.checked } })}
                          />
                          {msg(PERM_LABEL_KEYS[a])}
                        </label>
                      ))}
                    </div>
                  </div>
                  <div className="member-side role-form-actions">
                    <Button type="primary" size="small" htmlType="submit" disabled={roleBusy || !roleForm.name.trim()}>
                      {roleBusy ? '保存中…' : editingRoleId ? '保存修改' : '创建角色'}
                    </Button>
                    {editingRoleId && (
                      <Button type="text" size="small" onClick={resetRoleForm}>取消</Button>
                    )}
                  </div>
                </form>
              </div>
            )}
              </>
            )}
          </aside>
        </div>
      )}

      {historyTarget && (
        <VersionHistoryModal
          file={historyTarget}
          onClose={() => setHistoryTarget(null)}
          onChanged={() => setFileReloadKey((k) => k + 1)}
        />
      )}

      {/* 路径级 ACL 管理：条目列表（主体/效果/权限勾选可改、删除行）+ 添加行，
          保存时整体 PUT 覆盖全部条目。 */}
      {aclTarget && (
        <Modal wide title={formatMessage(msg('aclTitle'), { name: aclTarget.name })} onClose={closeAcl}>
          <p className="hint">{msg('aclHint')}</p>
          {aclLoading ? (
            <p className="hint">{msg('loading')}</p>
          ) : (
            <>
              <div className="acl-list">
                <div className="acl-row acl-head">
                  <span>{msg('aclSubjectType')}</span>
                  <span>{msg('aclSubjectId')}</span>
                  <span>{msg('aclEffect')}</span>
                  <span>{msg('permission')}</span>
                  <span />
                </div>
                {aclEntries.length === 0 && <p className="hint">{msg('aclEmpty')}</p>}
                {aclEntries.map((entry, index) => (
                  <div key={index} className="acl-row">
                    <span className="acl-type">{msg(ACL_SUBJECT_KEYS[entry.subject_type] ?? 'aclSubjectUser')}</span>
                    <Input className="acl-uuid" readOnly value={entry.subject_id} title={entry.subject_id} />
                    <Select
                      className="acl-effect-select"
                      value={entry.effect}
                      disabled={aclBusy}
                      onChange={(v) => setAclEntryEffect(index, v as FolderACLEntry['effect'])}
                      options={[
                        { value: 'allow', label: msg('aclAllow') },
                        { value: 'deny', label: msg('aclDeny') },
                      ]}
                    />
                    <span className="acl-perms">
                      {ACL_ACTIONS.map((a) => (
                        <label key={a} className="check-item">
                          <input
                            type="checkbox"
                            checked={entry.permissions.includes(a)}
                            onChange={() => toggleAclEntryPerm(index, a)}
                          />
                          {msg(PERM_LABEL_KEYS[a])}
                        </label>
                      ))}
                    </span>
                    <Button size="small" danger disabled={aclBusy} onClick={() => removeAclEntry(index)}>
                      {msg('delete')}
                    </Button>
                  </div>
                ))}
              </div>
              <form className="acl-add" onSubmit={addAclEntry}>
                <Select
                  className="acl-type-select"
                  value={aclNewType}
                  onChange={(v) => setAclNewType(v as FolderACLEntry['subject_type'])}
                  options={[
                    { value: 'user', label: msg('aclSubjectUser') },
                    { value: 'team', label: msg('aclSubjectTeam') },
                    { value: 'role', label: msg('aclSubjectRole') },
                  ]}
                />
                <Input
                  allowClear
                  value={aclNewId}
                  onChange={(e) => setAclNewId(e.target.value)}
                  placeholder="3f0c9c2e-…"
                />
                <Select
                  className="acl-effect-select"
                  value={aclNewEffect}
                  onChange={(v) => setAclNewEffect(v as FolderACLEntry['effect'])}
                  options={[
                    { value: 'allow', label: msg('aclAllow') },
                    { value: 'deny', label: msg('aclDeny') },
                  ]}
                />
                <span className="acl-perms">
                  {ACL_ACTIONS.map((a) => (
                    <label key={a} className="check-item">
                      <input
                        type="checkbox"
                        checked={Boolean(aclNewPerms[a])}
                        onChange={(e) => setAclNewPerms({ ...aclNewPerms, [a]: e.target.checked })}
                      />
                      {msg(PERM_LABEL_KEYS[a])}
                    </label>
                  ))}
                </span>
                <Button size="small" htmlType="submit" disabled={aclBusy || !UUID_RE.test(aclNewId.trim())}>
                  {msg('aclAdd')}
                </Button>
              </form>
              {aclError && <div className="error-text">{aclError}</div>}
              <div className="modal-actions">
                <Button disabled={aclBusy} onClick={closeAcl}>{msg('close')}</Button>
                <Button type="primary" disabled={aclBusy} onClick={() => void handleSaveAcl()}>
                  {aclBusy ? msg('loading') : msg('save')}
                </Button>
              </div>
            </>
          )}
        </Modal>
      )}
    </div>
  )
}
