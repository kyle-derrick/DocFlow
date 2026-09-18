import { FormEvent, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Users } from 'lucide-react'
import { Team, createTeam, currentUserId, deleteTeam, listTeams, updateTeam } from '../api'
import { formatTime } from '../components/FileBrowser'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

/** 团队列表页：创建团队 + 我的团队卡片（点击进入团队空间）。 */
export default function TeamsPage() {
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const navigate = useNavigate()
  const [teams, setTeams] = useState<Team[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [busy, setBusy] = useState(false)
  const [formError, setFormError] = useState('')

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

  const rename = async (t: Team) => {
    const next = window.prompt(msg('teamNameLabel'), t.name)?.trim()
    if (!next || next === t.name) return
    try { await updateTeam(t.id, next, t.description); await load() } catch (err) { setError(err instanceof Error ? err.message : msg('teamUpdateFailed')) }
  }
  const remove = async (t: Team) => {
    if (!window.confirm(formatMessage(msg('deleteTeamConfirm'), { name: t.name }))) return
    try { await deleteTeam(t.id); await load() } catch (err) { setError(err instanceof Error ? err.message : msg('teamDeleteFailed')) }
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>{msg('teamsTitle')}</h2>
        <button className="btn ghost" onClick={() => void load()}>{msg('refresh')}</button>
      </div>

      <form className="panel team-create" onSubmit={handleCreate}>
        <h3>{msg('createTeamTitle')}</h3>
        <div className="team-create-row">
          <label className="field">
            <span>{msg('teamNameLabel')}</span>
            <input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：平台组" maxLength={100} />
          </label>
          <label className="field">
            <span>{msg('teamDescLabel')}</span>
            <input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="团队用途说明" />
          </label>
          <button type="submit" className="btn primary" disabled={busy || !name.trim()}>
            {busy ? msg('creating') : msg('createTeamTitle')}
          </button>
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
          {teams.map((t) => (
            <button key={t.id} className="team-card" onClick={() => navigate(`/teams/${t.id}`)}>
              <div className="team-card-head">
                <span className="icon"><Users size={14} strokeWidth={2} aria-hidden="true" /></span>
                <span className="team-card-name">{t.name}</span>
                {myId !== null && t.owner_id === myId && <><button className="btn ghost" onClick={(e) => { e.stopPropagation(); void rename(t) }}>{msg('edit')}</button><button className="btn ghost" onClick={(e) => { e.stopPropagation(); void remove(t) }}>{msg('delete')}</button></>}
              </div>
              <p className="team-card-desc">{t.description || msg('noDesc')}</p>
              <span className="muted team-card-time">{msg('createdAt')} {formatTime(t.created_at)}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
