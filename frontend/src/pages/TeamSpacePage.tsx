import { FormEvent, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { ArrowLeft, Users } from 'lucide-react'
import { Button, Input, Select } from 'antd'
import {
  ACLAction,
  FileItem,
  FileQueryOptions,
  FolderACLEntry,
  Team,
  UserSearchResult,
  copyFile,
  createTeamFolder,
  currentUserId,
  getFolderACL,
  listTeamFiles,
  listTeams,
  putFolderACL,
  searchUsers,
  uploadFile,
} from '../api'
import { FileBrowserWithTree } from '../components/FolderTreeNav'
import { DirListing, Modal } from '../components/FileBrowser'
import TeamMembersModal from '../components/TeamMembersModal'
import VersionHistoryModal from '../components/VersionHistoryModal'
import SpaceSwitcher from '../components/SpaceSwitcher'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 权限动作 → i18n key（ACL 勾选表单共用）。 */
const PERM_LABEL_KEYS: Record<string, MessageKey> = {
  read: 'permRead',
  write: 'permWrite',
  delete: 'permDelete',
  share: 'permShare',
  admin: 'permAdmin',
}

/** 路径级 ACL 权限动作清单（不含 admin，与后端 folder ACL 契约一致）。 */
const ACL_ACTIONS: readonly ACLAction[] = ['read', 'write', 'delete', 'share']

/** ACL 主体类型 → i18n key（条目表格与添加行共用；migration 037 起自定义
 * 角色主体已移除，仅 user/team）。 */
const ACL_SUBJECT_KEYS: Record<string, MessageKey> = {
  user: 'aclSubjectUser',
  team: 'aclSubjectTeam',
}

/**
 * 团队空间（v1.7 成员管理重构）：与个人空间完全同构的两栏布局
 *（FileBrowserWithTree：全宽顶栏 + 目录树 240px + 文件管理 flex:1，右侧
 * 280px 成员面板已移除）。权限差异（五级内置角色矩阵）只体现在工具栏/
 * 菜单可用性：guest 隐藏新建/上传/复制（不注入 createFolderFn/uploadFn/
 * copyFn），删除/重命名越权由后端 403 统一提示「无写权限」；「成员管理」
 * 弹窗（owner/admin）与目录权限 ACL（owner）从顶栏进入。
 */
export default function TeamSpacePage() {
  const { id = '' } = useParams()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)

  const [team, setTeam] = useState<Team | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // 全局视图（全部 / 收藏 / 最近）：SpaceSwitcher 切换、透传 FileBrowser。
  const [spaceView, setSpaceView] = useState<'all' | 'starred' | 'recent'>('all')
  // 「成员管理」弹窗开关（owner/admin 可开）。
  const [manageOpen, setManageOpen] = useState(false)

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
  const [aclNewEffect, setAclNewEffect] = useState<FolderACLEntry['effect']>('allow')
  const [aclNewPerms, setAclNewPerms] = useState<Record<string, boolean>>({ read: true })
  // ACL 主体选择（搜索化，替代手输 UUID）：user = 远程搜索用户；team = 我的团队列表。
  const [aclSubjectUser, setAclSubjectUser] = useState('')
  const [aclSubjectQuery, setAclSubjectQuery] = useState('')
  const [aclSubjectOptions, setAclSubjectOptions] = useState<UserSearchResult[]>([])
  const [aclSubjectSearching, setAclSubjectSearching] = useState(false)
  const [aclSubjectTeam, setAclSubjectTeam] = useState('')
  const [aclTeams, setAclTeams] = useState<Team[]>([])

  useEffect(() => {
    void listTeams()
      .then(setAclTeams)
      .catch(() => {})
  }, [])

  useEffect(() => {
    const q = aclSubjectQuery.trim()
    if (q.length < 2 || aclSubjectUser) {
      setAclSubjectOptions([])
      return
    }
    const timer = window.setTimeout(() => {
      setAclSubjectSearching(true)
      void searchUsers(q)
        .then(setAclSubjectOptions)
        .catch(() => setAclSubjectOptions([]))
        .finally(() => setAclSubjectSearching(false))
    }, 300)
    return () => window.clearTimeout(timer)
  }, [aclSubjectQuery, aclSubjectUser])

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
    setAclSubjectUser('')
    setAclSubjectQuery('')
    setAclSubjectOptions([])
    setAclSubjectTeam('')
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
    const subjectId = aclNewType === 'user' ? aclSubjectUser : aclSubjectTeam
    if (!subjectId) {
      setAclError(aclNewType === 'user' ? (locale === 'zh-CN' ? '请先搜索并选择用户' : 'Search and select a user first') : msg('aclNoPerms'))
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
    setAclSubjectUser('')
    setAclSubjectQuery('')
    setAclSubjectOptions([])
    setAclSubjectTeam('')
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
  const myRole = team?.my_role ?? null
  const isOwner = team !== null && myId !== null && team.owner_id === myId
  // 写权限（矩阵 owner/admin/member_share/member）：注入新建/上传/复制能力；
  // guest（只读）不注入——对应工具栏按钮自动隐藏。
  const canWrite = myRole === 'owner' || myRole === 'admin' || myRole === 'member_share' || myRole === 'member'
  // 成员管理（owner/admin）：顶栏「成员管理」弹窗入口。
  const canManage = myRole === 'owner' || myRole === 'admin'

  useEffect(() => {
    const init = async () => {
      setLoading(true)
      setError('')
      try {
        // 契约无单团队查询端点，从「我的团队」列表中定位（非成员不在列表中）。
        const teams = await listTeams()
        const found = teams.find((tm) => tm.id === id) ?? null
        setTeam(found)
        if (!found) setError(msg('teamNotFound'))
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

  const listItems = async (parentId: string | null, opts?: FileQueryOptions): Promise<DirListing> => {
    const res = await listTeamFiles(id, parentId, opts)
    return { items: res.files ?? [], folderId: res.parent_id }
  }

  return (
    <div className="page wide-page files-page">
      {error && <div className="banner error">{error}</div>}
      {aclNotice && <div className="banner ok">{aclNotice}</div>}
      {loading && <div className="hint">{msg('loading')}</div>}

      {!loading && team && (
        <>
          {/* 两栏布局（与个人空间一致，无右侧成员面板）；返回团队列表入口
              并入面包屑最前（crumbPrefix）。 */}
          <FileBrowserWithTree
            rootLabel={team.name}
            toolbarPrefix={
              <>
                <SpaceSwitcher activeView={spaceView} onViewChange={setSpaceView} />
                {canManage && (
                  <Button
                    size="small"
                    icon={<Users size={13} strokeWidth={2} aria-hidden="true" />}
                    onClick={() => setManageOpen(true)}
                  >
                    {msg('memberManage')}
                  </Button>
                )}
              </>
            }
            crumbPrefix={
              <Link to="/teams" className="crumb-back" title={msg('backToTeams')} aria-label={msg('backToTeams')}>
                <ArrowLeft size={14} strokeWidth={2} aria-hidden="true" />
              </Link>
            }
            listItems={listItems}
            createFolderFn={canWrite ? (name, parentId) => createTeamFolder(id, name, parentId) : undefined}
            uploadFn={canWrite ? uploadFile : undefined}
            copyFn={canWrite ? (fileId, parentId) => copyFile(fileId, parentId) : undefined}
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

          {/* 成员管理弹窗（owner/admin）：邀请（用户搜索 + 五级角色）/ 改派 /
              移除；owner 额外可转让所有权（TeamMembersModal 内）。 */}
          {manageOpen && team && (
            <TeamMembersModal
              teamId={team.id}
              teamName={team.name}
              isOwner={isOwner}
              onClose={() => setManageOpen(false)}
            />
          )}
        </>
      )}

      {historyTarget && (
        <VersionHistoryModal
          file={historyTarget}
          onClose={() => setHistoryTarget(null)}
          onChanged={() => setFileReloadKey((k) => k + 1)}
        />
      )}

      {/* 路径级 ACL 管理：条目列表（主体/效果/权限勾选可改、删除行）+ 添加行，
          保存时整体 PUT 覆盖全部条目（主体仅 user/team）。 */}
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
                  ]}
                />
                {aclNewType === 'user' ? (
                  <Select
                    className="acl-subject-select"
                    showSearch
                    allowClear
                    filterOption={false}
                    value={aclSubjectUser || undefined}
                    searchValue={aclSubjectQuery}
                    loading={aclSubjectSearching}
                    placeholder={locale === 'zh-CN' ? '搜索用户（昵称/用户名/邮箱，至少 2 字）' : 'Search users (2+ chars)'}
                    notFoundContent={aclSubjectSearching ? (locale === 'zh-CN' ? '搜索中…' : 'Searching…') : null}
                    onSearch={(v) => {
                      // antd 选中 option 后派发 onSearch('')：不清除已选中
                      // 主体；仅主动输入非空文本时重置。
                      setAclSubjectQuery(v)
                      if (v !== '') setAclSubjectUser('')
                    }}
                    onClear={() => {
                      setAclSubjectQuery('')
                      setAclSubjectUser('')
                    }}
                    onChange={(value, option) => {
                      // antd6 单选选中标准回调（onSelect 部分受控场景不触发）。
                      if (value === undefined) return
                      setAclSubjectUser(String(value))
                      setAclSubjectQuery(String((option as { searchText?: string })?.searchText ?? ''))
                    }}
                    options={aclSubjectOptions.map((u) => {
                      const nickname = u.nickname ?? u.profile?.nickname
                      return {
                        value: u.id,
                        searchText: nickname || u.username,
                        label: (
                          <span className="user-search-option">
                            <strong>{nickname || u.username}</strong>
                            <span className="muted">{u.username} · {u.email}</span>
                          </span>
                        ),
                      }
                    })}
                  />
                ) : (
                  <Select
                    className="acl-subject-select"
                    value={aclSubjectTeam || undefined}
                    placeholder={locale === 'zh-CN' ? '选择团队' : 'Select team'}
                    onChange={(v) => setAclSubjectTeam(v)}
                    options={aclTeams.map((tm) => ({ value: tm.id, label: tm.name }))}
                  />
                )}
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
                <Button size="small" htmlType="submit" disabled={aclBusy || (aclNewType === 'user' ? !aclSubjectUser : !aclSubjectTeam)}>
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
