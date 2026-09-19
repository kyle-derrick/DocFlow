// 接受团队邀请落地页（/teams/join/:token，v1.7.1 成员管理完善）：
// 登录用户访问一次性邀请链接 → 调 POST /api/v1/team-invites/join/:token
// 接受（邮箱须匹配）→ 提示并跳转团队列表/团队空间。未登录时由
// RequireAuth 先引导登录（登录后回跳本页继续接受）。
import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Button } from 'antd'
import { acceptTeamInvite } from '../api'
import type { AcceptedTeamInvite } from '../api'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

export default function JoinTeamPage() {
  const { token = '' } = useParams()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const navigate = useNavigate()
  const [state, setState] = useState<'loading' | 'done' | 'error'>('loading')
  const [result, setResult] = useState<AcceptedTeamInvite | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!token) {
      setState('error')
      setError(msg('joinTeamFailed'))
      return
    }
    void acceptTeamInvite(token)
      .then((res) => {
        setResult(res)
        setState('done')
      })
      .catch((err) => {
        setError(err instanceof Error ? err.message : msg('joinTeamFailed'))
        setState('error')
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token])

  return (
    <div className="page join-team-page">
      <div className="page-head">
        <h2>{msg('joinTeamTitle')}</h2>
      </div>
      {state === 'loading' && <div className="hint">{msg('loading')}</div>}
      {state === 'done' && result && (
        <div className="banner ok">
          {result.already_member
            ? msg('joinTeamAlready')
            : formatMessage(msg('joinTeamSuccess'), { name: result.team.name })}
          <div className="join-team-actions">
            <Button type="primary" size="small" onClick={() => navigate(`/teams/${result.team.id}`)}>
              {msg('joinTeamOpen')}
            </Button>{' '}
            <Button size="small" onClick={() => navigate('/teams')}>{msg('joinTeamList')}</Button>
          </div>
        </div>
      )}
      {state === 'error' && (
        <div className="banner error">
          {error}
          <div className="join-team-actions">
            <Link to="/teams">{msg('joinTeamList')}</Link>
          </div>
        </div>
      )}
    </div>
  )
}
