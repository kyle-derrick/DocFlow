import { FormEvent, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Button, Input } from 'antd'
import { ApiError, resetPassword } from '../api'

/** 密码重置页（无需登录）：凭邮件中的一次性链接 /reset/<token> 设置新密码，
 *  成功后全部会话失效，引导回登录页重新登录。 */
export default function ResetPage() {
  const { token = '' } = useParams()
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [done, setDone] = useState(false)
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    if (password !== confirm) {
      setError('两次输入的密码不一致')
      return
    }
    setBusy(true)
    setError('')
    try {
      await resetPassword(token, password)
      setDone(true)
    } catch (err) {
      if (err instanceof ApiError && err.status === 400) {
        setError('重置链接无效或已过期（30 分钟有效且仅可使用一次），请重新申请')
      } else {
        setError(err instanceof Error ? err.message : '重置失败')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1 className="login-title">DocFlow</h1>
        {done ? (
          <>
            <p className="hint">密码已重置，所有登录会话均已失效。</p>
            <Button type="primary" block>
              <Link to="/login" className="login-link-btn">去登录</Link>
            </Button>
          </>
        ) : (
          <>
            <p className="hint">设置新密码（重置后需重新登录）</p>
            <label className="field">
              <span>新密码</span>
              <Input.Password
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="至少 12 位，含大小写字母和数字"
                autoComplete="new-password"
              />
            </label>
            <label className="field">
              <span>确认新密码</span>
              <Input.Password
                required
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                placeholder="再次输入新密码"
                autoComplete="new-password"
              />
            </label>
            {error && <div className="error-text">{error}</div>}
            <Button className="login-submit" type="primary" htmlType="submit" block disabled={busy || !token}>
              {busy ? '提交中…' : '重置密码'}
            </Button>
          </>
        )}
      </form>
    </div>
  )
}
