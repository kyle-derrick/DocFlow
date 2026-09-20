// 文件页右侧成员栏（统一空间模型 v2.2，默认展开）：
// - 顶部「空间管理」按钮（owner/admin，打开 SpaceManageModal 综合弹窗）；
// - 成员小节：头像 + 姓名 + 角色徽标的紧凑行列表（>20 人 simple 分页）；
// - 用户组小节（v2.5 可展开）：组行 caret 展开显示组内成员（头像+姓名，
//   数据 = GET /spaces/:id/members 的 group_users 条目按组聚合；行点击 =
//   用户只读信息弹窗，与成员行一致）；
// - 全员可见（guest 只读——本面板本身无管理操作）；
// - 成员行点击 = 用户只读信息弹窗（v2.4：头像/姓名/邮箱/部门/职位）；
// - 与中间文件区各自内滚（.member-scroll），宿主 FileBrowserWithTree 的
//   aside 槽（260px 列）；<1280px 由 FilesPage 自动收起（两栏布局）。
import { useEffect, useState } from 'react'
import { Avatar, Button, Pagination, Tooltip } from 'antd'
import { ChevronDown, ChevronRight } from 'lucide-react'
import { SpaceGroup, SpaceGroupUser, listSpaceGroups, listSpaceMembers, SpaceMember } from '../api'
import { ROLE_LABEL_KEYS, ROLE_TIP_KEYS, UserInfoModal, avatarColor, memberDisplayName } from './SpaceManageModal'
import { MessageKey, t, useLocale } from '../i18n'

/** 成员栏分页大小（>20 人分页 simple）。 */
const MEMBER_PAGE_SIZE = 20

export default function SpaceMemberPanel({
  spaceId,
  reloadKey,
  canManage,
  onManage,
}: {
  spaceId: string
  /** 宿主刷新信号（成员/用户组变更后联动重拉）。 */
  reloadKey?: number
  /** 是否显示顶部「空间管理」按钮（owner/admin）。 */
  canManage: boolean
  /** 打开空间管理弹窗。 */
  onManage: () => void
}) {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [members, setMembers] = useState<SpaceMember[]>([])
  const [groups, setGroups] = useState<SpaceGroup[]>([])
  // 经用户组加入的用户条目（GET /spaces/:id/members 的 group_users）：组行
  // 展开时按 group_id 聚合展示组内成员（v2.5）。
  const [groupUsers, setGroupUsers] = useState<SpaceGroupUser[]>([])
  const [page, setPage] = useState(1)
  // 展开的组（group_id 集合；组行 caret 点击切换）。
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set())
  // 成员/组内成员行点击的用户只读信息弹窗（v2.4）。
  const [viewUser, setViewUser] = useState<{ id: string; name: string } | null>(null)

  useEffect(() => {
    let alive = true
    setMembers([])
    setGroups([])
    setGroupUsers([])
    setPage(1)
    void listSpaceMembers(spaceId)
      .then((r) => {
        if (!alive) return
        setMembers(r.members)
        setGroupUsers(r.group_users)
      })
      .catch(() => { if (alive) { setMembers([]); setGroupUsers([]) } })
    void listSpaceGroups(spaceId)
      .then((gs) => { if (alive) setGroups(gs) })
      .catch(() => { if (alive) setGroups([]) })
    return () => { alive = false }
  }, [spaceId, reloadKey])

  /** 组内成员（group_users 按 group_id 过滤 + 同组同用户去重，展示序保持服务端排序）。 */
  const groupMembersOf = (groupId: string): SpaceGroupUser[] => {
    const seen = new Set<string>()
    const out: SpaceGroupUser[] = []
    for (const gu of groupUsers) {
      if (gu.group_id !== groupId || seen.has(gu.user_id)) continue
      seen.add(gu.user_id)
      out.push(gu)
    }
    return out
  }

  const toggleGroup = (groupId: string) => {
    setExpandedGroups((prev) => {
      const next = new Set(prev)
      if (next.has(groupId)) next.delete(groupId)
      else next.add(groupId)
      return next
    })
  }

  /** 组内用户展示名（昵称优先，回退用户名/ID 前缀）。 */
  const groupUserDisplayName = (gu: SpaceGroupUser): string =>
    gu.nickname || gu.username || `${gu.user_id.slice(0, 8)}…`

  const start = (page - 1) * MEMBER_PAGE_SIZE
  const pageMembers = members.slice(start, start + MEMBER_PAGE_SIZE)

  return (
    <div className="member-panel">
      {/* 空间管理入口（v2.2 由工具栏移到本栏顶部）。 */}
      {canManage && (
        <Button block size="small" type="primary" className="member-panel-manage" onClick={onManage}>
          {msg('spaceManage')}
        </Button>
      )}
      <h3>{msg('membersTab')} <span className="muted">{members.length}</span></h3>
      <div className="member-scroll">
        {members.length === 0 ? (
          <p className="hint">{msg('memberPanelHint')}</p>
        ) : (
          <ul className="member-list">
            {pageMembers.map((m) => (
              <li key={m.user_id} className="member-row member-row-clickable">
                <button type="button" className="member-info member-info-btn" onClick={() => setViewUser({ id: m.user_id, name: memberDisplayName(m) })} title={m.email || m.user_id}>
                  <Avatar size={22} className="member-avatar" style={{ backgroundColor: avatarColor(m.user_id) }}>
                    {memberDisplayName(m).slice(0, 1).toUpperCase()}
                  </Avatar>
                  <span className="member-name">{memberDisplayName(m)}</span>
                </button>
                <Tooltip title={msg(ROLE_TIP_KEYS[m.role])}>
                  <span className={`badge role-${m.role}`}>{msg(ROLE_LABEL_KEYS[m.role])}</span>
                </Tooltip>
              </li>
            ))}
          </ul>
        )}
        {/* 用户组小节（组行 = 组名 + 成员数 + 角色徽章；v2.5 可展开显示
            组内成员，全员可见）。 */}
        <h3 style={{ marginTop: 4 }}>{msg('groupsTab')} <span className="muted">{groups.length}</span></h3>
        {groups.length === 0 ? (
          <p className="hint">{msg('noGroups')}</p>
        ) : (
          <ul className="member-list">
            {groups.map((g) => {
              const expanded = expandedGroups.has(g.group_id)
              const groupMembers = expanded ? groupMembersOf(g.group_id) : []
              return (
                <li key={g.group_id} className="member-group-item">
                  <div className="member-row member-row-clickable member-group-row">
                    <button type="button" className="member-info member-info-btn" onClick={() => toggleGroup(g.group_id)} aria-expanded={expanded}>
                      <span className="member-group-caret" aria-hidden="true">
                        {expanded
                          ? <ChevronDown size={14} strokeWidth={2} aria-hidden="true" />
                          : <ChevronRight size={14} strokeWidth={2} aria-hidden="true" />}
                      </span>
                      <Avatar size={22} className="member-avatar" style={{ backgroundColor: avatarColor(g.group_id) }}>
                        {(g.group_name ?? '?').slice(0, 1).toUpperCase()}
                      </Avatar>
                      <span className="member-name" title={g.group_id}>{g.group_name ?? `${g.group_id.slice(0, 8)}…`}</span>
                      <span className="muted">{g.member_count ?? '—'} 人</span>
                    </button>
                    <Tooltip title={msg(ROLE_TIP_KEYS[g.role])}>
                      <span className={`badge role-${g.role}`}>{msg(ROLE_LABEL_KEYS[g.role])}</span>
                    </Tooltip>
                  </div>
                  {/* 组内成员（缩进列表：头像 + 姓名；点击 = 用户只读弹窗）。 */}
                  {expanded && (
                    <ul className="member-list member-group-members">
                      {groupMembers.length === 0 ? (
                        <li className="hint member-group-empty">（{locale === 'zh-CN' ? '暂无成员' : 'No members'}）</li>
                      ) : (
                        groupMembers.map((gu) => (
                          <li key={gu.user_id} className="member-row member-row-clickable">
                            <button type="button" className="member-info member-info-btn" onClick={() => setViewUser({ id: gu.user_id, name: groupUserDisplayName(gu) })} title={gu.email || gu.user_id}>
                              <Avatar size={20} className="member-avatar" style={{ backgroundColor: avatarColor(gu.user_id) }}>
                                {groupUserDisplayName(gu).slice(0, 1).toUpperCase()}
                              </Avatar>
                              <span className="member-name">{groupUserDisplayName(gu)}</span>
                            </button>
                          </li>
                        ))
                      )}
                    </ul>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </div>
      {members.length > MEMBER_PAGE_SIZE && (
        <div className="member-pager">
          <Pagination
            simple
            size="small"
            current={page}
            pageSize={MEMBER_PAGE_SIZE}
            total={members.length}
            onChange={setPage}
          />
        </div>
      )}

      {/* 成员/组内成员行点击 = 用户只读信息弹窗（v2.4）。 */}
      {viewUser && <UserInfoModal userId={viewUser.id} fallbackName={viewUser.name} onClose={() => setViewUser(null)} />}
    </div>
  )
}
