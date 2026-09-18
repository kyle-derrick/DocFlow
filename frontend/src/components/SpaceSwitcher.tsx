import { FormEvent, useEffect, useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Users } from 'lucide-react'
import { Team, createTeam, currentUserId, deleteTeam, listTeams, updateTeam } from '../api'
import { Modal, formatTime } from './FileBrowser'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/**
 * 空间切换行（文件页 / 团队空间页共用）：
 * - 空间下拉（我的文件 + 各团队）；
 * - 全局视图（全部 / 收藏 / 最近）下拉选择——受控组件，状态由页面持有
 *   并透传给 FileBrowser 的 activeView（v1.3 由三段按钮改为下拉，省宽）；
 * - 团队管理弹窗（创建 / 改名 / 删除，不再跳转 /teams 页）；
 * - 回收站入口。
 */
export default function SpaceSwitcher({
  activeView,
  onViewChange,
}: {
  /** 当前视图（受控）：FilesPage / TeamSpacePage 持有并传给 FileBrowser。 */
  activeView?: 'all' | 'starred' | 'recent'
  /** 视图切换回调；提供时渲染 全部/收藏/最近 下拉选择。 */
  onViewChange?: (view: 'all' | 'starred' | 'recent') => void
}) {
  const navigate = useNavigate()
  const location = useLocation()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [teams, setTeams] = useState<Team[]>([])
  useEffect(() => { void listTeams().then(setTeams).catch(() => setTeams([])) }, [])
  const value = location.pathname.startsWith('/teams/') ? location.pathname : '/'

  // ---- 团队管理弹窗（创建 / 改名 / 删除；进入团队走空间下拉或列表行） ----
  const [manageOpen, setManageOpen] = useState(false)
  const [teamName, setTeamName] = useState('')
  const [teamDesc, setTeamDesc] = useState('')
  const [teamBusy, setTeamBusy] = useState(false)
  const [manageError, setManageError] = useState('')
  const myId = currentUserId()

  const reloadTeams = async () => {
    try {
      setTeams(await listTeams())
    } catch {
      /* 列表刷新失败保持现状 */
    }
  }

  const handleCreateTeam = async (e: FormEvent) => {
    e.preventDefault()
    const name = teamName.trim()
    if (!name) return
    setTeamBusy(true)
    setManageError('')
    try {
      await createTeam(name, teamDesc.trim())
      setTeamName('')
      setTeamDesc('')
      await reloadTeams()
    } catch (err) {
      setManageError(err instanceof Error ? err.message : msg('teamCreateFailed'))
    } finally {
      setTeamBusy(false)
    }
  }

  const renameTeam = async (team: Team) => {
    const next = window.prompt(msg('teamNameLabel'), team.name)?.trim()
    if (!next || next === team.name) return
    setManageError('')
    try {
      await updateTeam(team.id, next, team.description)
      await reloadTeams()
    } catch (err) {
      setManageError(err instanceof Error ? err.message : msg('teamUpdateFailed'))
    }
  }

  const removeTeam = async (team: Team) => {
    if (!window.confirm(formatMessage(msg('deleteTeamConfirm'), { name: team.name }))) return
    setManageError('')
    try {
      await deleteTeam(team.id)
      // 删除的是当前所在团队空间时回到个人空间。
      if (location.pathname === `/teams/${team.id}`) navigate('/')
      await reloadTeams()
    } catch (err) {
      setManageError(err instanceof Error ? err.message : msg('teamDeleteFailed'))
    }
  }

  return (
    <div className="space-switcher">
      <select id="space-select" className="form-select" aria-label="选择团队空间" value={value} onChange={(e) => navigate(e.target.value)}>
        <option value="/">我的文件</option>
        {teams.map((team) => <option key={team.id} value={`/teams/${team.id}`}>{team.name}</option>)}
      </select>
      {onViewChange && (
        <select
          className="form-select space-view-select"
          aria-label={locale === 'zh-CN' ? '视图' : 'View'}
          value={activeView}
          onChange={(e) => onViewChange(e.target.value as 'all' | 'starred' | 'recent')}
        >
          <option value="all">{msg('viewAll')}</option>
          <option value="starred">{msg('viewStarred')}</option>
          <option value="recent">{msg('viewRecent')}</option>
        </select>
      )}
      <button type="button" className="btn small" onClick={() => setManageOpen(true)}>团队管理</button>
      <button type="button" className="btn small ghost" onClick={() => navigate('/trash')}>回收站</button>

      {manageOpen && (
        <Modal wide title="团队管理" onClose={() => setManageOpen(false)}>
          <p className="hint">创建、改名或删除团队；点击团队名进入对应团队空间。</p>
          <form className="team-create-row space-manage-create" onSubmit={handleCreateTeam}>
            <label className="field">
              <span>{msg('teamNameLabel')}</span>
              <input autoFocus value={teamName} onChange={(e) => setTeamName(e.target.value)} placeholder="例如：平台组" maxLength={100} />
            </label>
            <label className="field">
              <span>{msg('teamDescLabel')}</span>
              <input value={teamDesc} onChange={(e) => setTeamDesc(e.target.value)} placeholder="团队用途说明" />
            </label>
            <button type="submit" className="btn primary" disabled={teamBusy || !teamName.trim()}>
              {teamBusy ? msg('creating') : msg('createTeamTitle')}
            </button>
          </form>
          {manageError && <div className="error-text">{manageError}</div>}
          <ul className="space-manage-list">
            {teams.length === 0 && <li className="hint">暂无团队</li>}
            {teams.map((team) => (
              <li key={team.id} className="space-manage-row">
                <button
                  type="button"
                  className="space-manage-name"
                  title={locale === 'zh-CN' ? '进入团队空间' : 'Open team space'}
                  onClick={() => {
                    setManageOpen(false)
                    navigate(`/teams/${team.id}`)
                  }}
                >
                  <span aria-hidden="true"><Users size={14} strokeWidth={2} aria-hidden="true" /></span>
                  <span className="space-manage-name-text">{team.name}</span>
                  <span className="muted space-manage-time">{formatTime(team.created_at)}</span>
                </button>
                {myId !== null && team.owner_id === myId && (
                  <span className="space-manage-actions">
                    <button className="btn small" onClick={() => void renameTeam(team)}>{msg('edit')}</button>
                    <button className="btn small danger" onClick={() => void removeTeam(team)}>{msg('delete')}</button>
                  </span>
                )}
              </li>
            ))}
          </ul>
        </Modal>
      )}
    </div>
  )
}
