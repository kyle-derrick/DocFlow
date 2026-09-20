// 接受空间邀请落地页（/spaces/join/:token）：
// 登录用户访问一次性邀请链接 → 调 POST /api/v1/space-invites/join/:token
// 接受（邮箱须匹配）→ 提示并跳转空间列表/空间文件页。未登录时由
// RequireAuth 先引导登录（登录后回跳本页继续接受）。
import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Button } from 'antd'
import { acceptSpaceInvite } from '../api'
import type { AcceptedSpaceInvite } from '../api'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

export default function JoinSpacePage() {
  const { token = '' } = useParams()
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const navigate = useNavigate()
  const [state, setState] = useState<'loading' | 'done' | 'error'>('loading')
  const [result, setResult] = useState<AcceptedSpaceInvite | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!token) {
      setState('error')
      setError(msg('joinTeamFailed'))
      return
    }
    void acceptSpaceInvite(token)
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
            : formatMessage(msg('joinTeamSuccess'), { name: result.space.name })}
          <div className="join-team-actions">
            <Button type="primary" size="small" onClick={() => navigate(`/files?space=${result.space.id}`)}>
              {msg('joinTeamOpen')}
            </Button>{' '}
            <Button size="small" onClick={() => navigate('/spaces')}>{msg('joinTeamList')}</Button>
          </div>
        </div>
      )}
      {state === 'error' && (
        <div className="banner error">
          {error}
          <div className="join-team-actions">
            <Link to="/spaces">{msg('joinTeamList')}</Link>
          </div>
        </div>
      )}
    </div>
  )
}
