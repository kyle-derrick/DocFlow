import { FormEvent, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Team, createTeam, currentUserId, listTeams } from '../api'
import { formatTime } from '../components/FileBrowser'

/** 团队列表页：创建团队 + 我的团队卡片（点击进入团队空间）。 */
export default function TeamsPage() {
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
      setError(err instanceof Error ? err.message : '加载团队失败')
      setTeams([])
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
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
      setFormError(err instanceof Error ? err.message : '创建团队失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>团队</h2>
        <button className="btn ghost" onClick={() => void load()}>刷新</button>
      </div>

      <form className="panel team-create" onSubmit={handleCreate}>
        <h3>创建团队</h3>
        <div className="team-create-row">
          <label className="field">
            <span>团队名称</span>
            <input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：平台组" maxLength={100} />
          </label>
          <label className="field">
            <span>描述（可选）</span>
            <input value={desc} onChange={(e) => setDesc(e.target.value)} placeholder="团队用途说明" />
          </label>
          <button type="submit" className="btn primary" disabled={busy || !name.trim()}>
            {busy ? '创建中…' : '创建团队'}
          </button>
        </div>
        {formError && <div className="error-text">{formError}</div>}
      </form>

      {error && <div className="banner error">{error}</div>}
      {loading && <div className="hint">加载中…</div>}
      {!loading && !error && teams.length === 0 && (
        <div className="empty">还没有团队，创建一个开始协作吧</div>
      )}

      {teams.length > 0 && (
        <div className="team-grid">
          {teams.map((t) => (
            <button key={t.id} className="team-card" onClick={() => navigate(`/teams/${t.id}`)}>
              <div className="team-card-head">
                <span className="icon">👥</span>
                <span className="team-card-name">{t.name}</span>
                {myId !== null && t.owner_id === myId && <span className="badge role-owner">我管理</span>}
              </div>
              <p className="team-card-desc">{t.description || '暂无描述'}</p>
              <span className="muted team-card-time">创建于 {formatTime(t.created_at)}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
