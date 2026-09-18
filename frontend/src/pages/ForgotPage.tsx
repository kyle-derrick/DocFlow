import { FormEvent, useState } from 'react'
import { Link } from 'react-router-dom'
import { Button, Input } from 'antd'
import { forgotPassword } from '../api'

/** 忘记密码页（无需登录）：提交邮箱后提示已受理——无论邮箱是否存在
 *  后端一律 202（防枚举），邮件通道未配置时链接输出在后端日志。 */
export default function ForgotPage() {
  const [email, setEmail] = useState('')
  const [error, setError] = useState('')
  const [sent, setSent] = useState(false)
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await forgotPassword(email)
      setSent(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : '请求失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1 className="login-title">DocFlow</h1>
        {sent ? (
          <>
            <p className="hint">如果该邮箱存在账号，重置链接已发送（30 分钟内有效，仅可使用一次）。</p>
            <Button type="primary" block>
              <Link to="/login" className="login-link-btn">返回登录</Link>
            </Button>
          </>
        ) : (
          <>
            <p className="hint">输入账号邮箱，我们将发送密码重置链接</p>
            <label className="field">
              <span>邮箱</span>
              <Input
                type="email"
                required
                allowClear
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="you@example.com"
                autoComplete="email"
              />
            </label>
            {error && <div className="error-text">{error}</div>}
            <Button className="login-submit" type="primary" htmlType="submit" block disabled={busy}>
              {busy ? '发送中…' : '发送重置链接'}
            </Button>
            <p className="hint">
              想起密码了？<Link to="/login">返回登录</Link>
            </p>
          </>
        )}
      </form>
    </div>
  )
}
