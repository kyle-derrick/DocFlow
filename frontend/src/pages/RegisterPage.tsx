import { FormEvent, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { Button, Input } from 'antd'
import { ApiError, register } from '../api'

/** 邀请注册页（无需登录）：凭邮件中的一次性链接 /register/<token> 完成注册，
 *  注册成功即登录进入文件页。 */
export default function RegisterPage() {
  const { token = '' } = useParams()
  const navigate = useNavigate()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
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
      await register(token, username, password)
      navigate('/', { replace: true })
    } catch (err) {
      if (err instanceof ApiError) {
        if (err.status === 404 || err.status === 410) setError('邀请链接不存在或已失效，请联系管理员重新发送')
        else if (err.status === 409) setError('用户名或邮箱已被占用，请换一个用户名')
        else setError(err.message)
      } else {
        setError('注册失败')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1 className="login-title">DocFlow</h1>
        <p className="hint">接受邀请，创建你的账号</p>
        <label className="field">
          <span>用户名</span>
          <Input
            required
            minLength={3}
            maxLength={32}
            pattern="[A-Za-z0-9_-]+"
            title="3-32 个字符，仅限字母、数字、下划线与连字符"
            allowClear
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="alice"
            autoComplete="username"
          />
        </label>
        <label className="field">
          <span>密码</span>
          <Input.Password
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="至少 12 位，含大小写字母和数字"
            autoComplete="new-password"
          />
        </label>
        <label className="field">
          <span>确认密码</span>
          <Input.Password
            required
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            placeholder="再次输入密码"
            autoComplete="new-password"
          />
        </label>
        {error && <div className="error-text">{error}</div>}
        <Button className="login-submit" type="primary" htmlType="submit" block disabled={busy || !token}>
          {busy ? '注册中…' : '完成注册'}
        </Button>
        <p className="hint">
          已有账号？<Link to="/login">返回登录</Link>
        </p>
      </form>
    </div>
  )
}
