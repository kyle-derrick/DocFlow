import { FormEvent, useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ApiError, OIDC_LOGIN_PATH, TOTP_REQUIRED_CODE, getOIDCStatus, login, loginTotp } from '../api'

/**
 * 输入分流：6 位纯数字按 TOTP 码提交，其余（含连字符/长度不符）按恢复码
 * 提交（服务端二选一校验；恢复码命中即消耗）。
 */
function splitTotpInput(raw: string): { code: string; recoveryCode: string } {
  const input = raw.trim()
  return /^\d{6}$/.test(input) ? { code: input, recoveryCode: '' } : { code: '', recoveryCode: input }
}

export default function LoginPage() {
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [totpInput, setTotpInput] = useState('')
  const [totpRequired, setTotpRequired] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  // SSO 可用性：登录页加载时探测一次（公开端点；未启用/请求失败不展示按钮）。
  const [ssoEnabled, setSsoEnabled] = useState(false)

  useEffect(() => {
    let alive = true
    void getOIDCStatus().then((status) => {
      if (alive) setSsoEnabled(status.enabled)
    })
    return () => {
      alive = false
    }
  }, [])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError('')
    try {
      if (!totpRequired) {
        await login(email, password)
      } else {
        const { code, recoveryCode } = splitTotpInput(totpInput)
        await loginTotp(email, password, code, recoveryCode)
      }
      navigate('/', { replace: true })
    } catch (err) {
      if (err instanceof ApiError && err.code === TOTP_REQUIRED_CODE) {
        // 密码已验证通过，展开两步验证输入（email/password 保留在表单态）。
        setTotpRequired(true)
        setError('')
        return
      }
      setError(err instanceof Error ? err.message : '登录失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1 className="login-title">DocFlow</h1>
        <p className="hint">登录以访问你的文件</p>
        <label className="field">
          <span>邮箱</span>
          <input
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="you@example.com"
            autoComplete="username"
          />
        </label>
        <label className="field">
          <span>密码</span>
          <input
            type="password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="••••••••"
            autoComplete="current-password"
          />
        </label>
        {totpRequired && (
          <>
            <div className="hint" style={{ margin: '4px 0 10px' }}>
              该账号已启用两步验证，请输入认证器中的 6 位验证码；丢失认证器时可输入一次性恢复码。
            </div>
            <label className="field">
              <span>两步验证码 / 恢复码</span>
              <input
                type="text"
                required
                value={totpInput}
                onChange={(e) => setTotpInput(e.target.value)}
                placeholder="123456 或 abcd-efgh"
                autoComplete="one-time-code"
                autoFocus
              />
            </label>
          </>
        )}
        {error && <div className="error-text">{error}</div>}
        <button className="btn primary block" type="submit" disabled={busy}>
          {busy ? '登录中…' : totpRequired ? '验证并登录' : '登录'}
        </button>
        {ssoEnabled && (
          <>
            <div className="login-divider">或</div>
            <button
              className="btn block"
              type="button"
              // 整页跳转到后端 /auth/oidc/login（302 → IdP；回调后落地 /sso）。
              onClick={() => {
                window.location.href = OIDC_LOGIN_PATH
              }}
            >
              使用 SSO 登录
            </button>
          </>
        )}
        <p className="hint">
          <Link to="/forgot">忘记密码？</Link>
        </p>
      </form>
    </div>
  )
}
