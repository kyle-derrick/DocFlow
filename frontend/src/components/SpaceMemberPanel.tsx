// 文件页右侧成员栏（统一空间模型 v2.2，默认展开）：
// - 顶部「空间管理」按钮（owner/admin，打开 SpaceManageModal 综合弹窗）；
// - 成员小节：头像 + 姓名 + 角色徽标的紧凑行列表（>20 人 simple 分页）；
// - 用户组小节：组名 + 组内成员数 + 角色徽章（授权角色，与空间管理弹窗同口径）；
// - 全员可见（guest 只读——本面板本身无管理操作）；
// - 成员行点击 = 用户只读信息弹窗（v2.4：头像/姓名/邮箱/部门/职位）；
// - 与中间文件区各自内滚（.member-scroll），宿主 FileBrowserWithTree 的
//   aside 槽（260px 列）；<1280px 由 FilesPage 自动收起（两栏布局）。
import { useEffect, useState } from 'react'
import { Avatar, Button, Pagination, Tooltip } from 'antd'
import { SpaceGroup, listSpaceGroups, listSpaceMembers, SpaceMember } from '../api'
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
  const [page, setPage] = useState(1)
  // 成员行点击的用户只读信息弹窗（v2.4）。
  const [viewUser, setViewUser] = useState<{ id: string; name: string } | null>(null)

  useEffect(() => {
    let alive = true
    setMembers([])
    setGroups([])
    setPage(1)
    void listSpaceMembers(spaceId)
      .then((r) => { if (alive) setMembers(r.members) })
      .catch(() => { if (alive) setMembers([]) })
    void listSpaceGroups(spaceId)
      .then((gs) => { if (alive) setGroups(gs) })
      .catch(() => { if (alive) setGroups([]) })
    return () => { alive = false }
  }, [spaceId, reloadKey])

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
        {/* 用户组小节（组行 = 组名 + 成员数 + 角色徽章；全员可见）。 */}
        <h3 style={{ marginTop: 4 }}>{msg('groupsTab')} <span className="muted">{groups.length}</span></h3>
        {groups.length === 0 ? (
          <p className="hint">{msg('noGroups')}</p>
        ) : (
          <ul className="member-list">
            {groups.map((g) => (
              <li key={g.group_id} className="member-row">
                <span className="member-info">
                  <Avatar size={22} className="member-avatar" style={{ backgroundColor: avatarColor(g.group_id) }}>
                    {(g.group_name ?? '?').slice(0, 1).toUpperCase()}
                  </Avatar>
                  <span className="member-name" title={g.group_id}>{g.group_name ?? `${g.group_id.slice(0, 8)}…`}</span>
                  <span className="muted">{g.member_count ?? '—'} 人</span>
                </span>
                <Tooltip title={msg(ROLE_TIP_KEYS[g.role])}>
                  <span className={`badge role-${g.role}`}>{msg(ROLE_LABEL_KEYS[g.role])}</span>
                </Tooltip>
              </li>
            ))}
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

      {/* 成员行点击 = 用户只读信息弹窗（v2.4）。 */}
      {viewUser && <UserInfoModal userId={viewUser.id} fallbackName={viewUser.name} onClose={() => setViewUser(null)} />}
    </div>
  )
}
