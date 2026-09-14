import { FormEvent, useEffect, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  FileItem,
  FileQueryOptions,
  Team,
  TeamMember,
  UUID_RE,
  addTeamMember,
  createTeamFolder,
  currentUserId,
  listTeamFiles,
  listTeamMembers,
  listTeams,
  removeTeamMember,
  uploadFile,
} from '../api'
import FileBrowser, { DirListing, formatTime } from '../components/FileBrowser'
import VersionHistoryModal from '../components/VersionHistoryModal'

/**
 * 团队空间：左侧成员管理（owner 可添加/移除成员），右侧团队文件浏览
 * （列表 / 上传 / 下载 / 预览 / 新建文件夹 / 版本历史复用 FileBrowser；
 * 写操作 403 提示「无写权限」）。
 */
export default function TeamSpacePage() {
  const { id = '' } = useParams()

  const [team, setTeam] = useState<Team | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [members, setMembers] = useState<TeamMember[]>([])
  const [memberError, setMemberError] = useState('')
  const [memberUserId, setMemberUserId] = useState('')
  const [memberRole, setMemberRole] = useState<'editor' | 'viewer'>('viewer')
  const [memberBusy, setMemberBusy] = useState(false)

  const [fileReloadKey, setFileReloadKey] = useState(0)
  const [historyTarget, setHistoryTarget] = useState<FileItem | null>(null)

  const myId = currentUserId()
  const isOwner = team !== null && myId !== null && team.owner_id === myId

  const loadMembers = async () => {
    setMemberError('')
    try {
      setMembers(await listTeamMembers(id))
    } catch (err) {
      setMembers([])
      setMemberError(err instanceof Error ? err.message : '加载成员失败')
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
        if (found) await loadMembers()
        else setError('未找到该团队：可能已被删除或你不是团队成员')
      } catch (err) {
        setTeam(null)
        setError(err instanceof Error ? err.message : '加载团队失败')
      } finally {
        setLoading(false)
      }
    }
    void init()
  }, [id])

  const handleAddMember = async (e: FormEvent) => {
    e.preventDefault()
    const uid = memberUserId.trim()
    if (!UUID_RE.test(uid)) return
    setMemberBusy(true)
    setMemberError('')
    try {
      await addTeamMember(id, uid, memberRole)
      setMemberUserId('')
      await loadMembers()
    } catch (err) {
      setMemberError(err instanceof Error ? err.message : '添加成员失败')
    } finally {
      setMemberBusy(false)
    }
  }

  const handleRemoveMember = async (m: TeamMember) => {
    if (!window.confirm(`确定移除该成员（${m.user_id.slice(0, 8)}…）？`)) return
    setMemberError('')
    try {
      await removeTeamMember(id, m.user_id)
      await loadMembers()
    } catch (err) {
      setMemberError(err instanceof Error ? err.message : '移除成员失败')
    }
  }

  const listItems = async (parentId: string | null, opts?: FileQueryOptions): Promise<DirListing> => {
    const res = await listTeamFiles(id, parentId, opts)
    return { items: res.files ?? [], folderId: res.parent_id }
  }

  return (
    <div className="page">
      <div className="team-back">
        <Link className="btn ghost small" to="/teams">← 返回团队列表</Link>
      </div>

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">加载中…</div>}

      {!loading && team && (
        <div className="team-layout">
          <aside className="member-panel">
            <h3>成员（{members.length}）</h3>
            {isOwner ? (
              <form className="member-add" onSubmit={handleAddMember}>
                <label className="field">
                  <span>用户 UUID</span>
                  <input
                    value={memberUserId}
                    onChange={(e) => setMemberUserId(e.target.value)}
                    placeholder="例如 3f0c9c2e-…（完整 UUID）"
                  />
                </label>
                <label className="field">
                  <span>角色</span>
                  <select value={memberRole} onChange={(e) => setMemberRole(e.target.value as 'editor' | 'viewer')}>
                    <option value="viewer">viewer（只读）</option>
                    <option value="editor">editor（可上传/建目录）</option>
                  </select>
                </label>
                <button type="submit" className="btn primary block" disabled={memberBusy || !UUID_RE.test(memberUserId.trim())}>
                  {memberBusy ? '添加中…' : '添加成员'}
                </button>
              </form>
            ) : (
              <p className="hint member-hint">仅团队 owner 可管理成员。</p>
            )}
            {memberError && <div className="error-text">{memberError}</div>}
            <ul className="member-list">
              {members.map((m) => (
                <li key={m.user_id} className="member-row">
                  <div className="member-info">
                    <span className="member-id" title={m.user_id}>{m.user_id.slice(0, 8)}…</span>
                    <span className={`badge role-${m.role}`}>{m.role}</span>
                  </div>
                  <div className="member-side">
                    <span className="muted member-time" title={`加入于 ${formatTime(m.created_at)}`}>
                      {formatTime(m.created_at)}
                    </span>
                    {isOwner && m.role !== 'owner' && (
                      <button className="btn small danger" onClick={() => void handleRemoveMember(m)}>移除</button>
                    )}
                  </div>
                </li>
              ))}
            </ul>
          </aside>

          <div className="team-files">
            <FileBrowser
              title={team.name}
              rootLabel={team.name}
              listItems={listItems}
              createFolderFn={(name, parentId) => createTeamFolder(id, name, parentId)}
              uploadFn={uploadFile}
              reloadKey={fileReloadKey}
              rowActions={(item) =>
                item.type === 'file' ? (
                  <button className="btn small" onClick={() => setHistoryTarget(item)}>历史</button>
                ) : null
              }
              emptyHint="团队空间为空，上传文件或新建文件夹开始协作"
            />
          </div>
        </div>
      )}

      {historyTarget && (
        <VersionHistoryModal
          file={historyTarget}
          onClose={() => setHistoryTarget(null)}
          onChanged={() => setFileReloadKey((k) => k + 1)}
        />
      )}
    </div>
  )
}
