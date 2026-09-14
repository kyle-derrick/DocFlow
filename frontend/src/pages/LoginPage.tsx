import { FormEvent, useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ApiError, OIDC_LOGIN_PATH, TOTP_REQUIRED_CODE, getOIDCStatus, login, loginTotp } from '../api'
import { messages, saveLocale, t, useLocale } from '../i18n'

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
  const locale = useLocale()
  const msg = (key: keyof typeof messages['zh-CN']) => t(locale, key)
  const switchLocale = () => {
    const next = locale === 'zh-CN' ? 'en-US' : 'zh-CN'
    saveLocale(next)
    window.dispatchEvent(new Event('docflow:locale'))
  }
  const [identifier, setIdentifier] = useState('')
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
        await login(identifier, password)
      } else {
        const { code, recoveryCode } = splitTotpInput(totpInput)
        await loginTotp(identifier, password, code, recoveryCode)
      }
      navigate('/', { replace: true })
    } catch (err) {
      if (err instanceof ApiError && err.code === TOTP_REQUIRED_CODE) {
        // 密码已验证通过，展开两步验证输入（identifier/password 保留在表单态）。
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
        <button type="button" className="btn ghost" onClick={switchLocale}>{msg('switchLanguage')}</button>
        <p className="hint">{locale === 'zh-CN' ? '登录以访问你的文件' : 'Log in to access your files'}</p>
        <label className="field">
          <span>邮箱或用户名</span>
          <input
            type="text"
            required
            autoCapitalize="none"
            spellCheck={false}
            value={identifier}
            onChange={(e) => setIdentifier(e.target.value)}
            placeholder="you@example.com 或 username"
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
          {busy ? `${msg('login')}…` : totpRequired ? (locale === 'zh-CN' ? '验证并登录' : 'Verify and log in') : msg('login')}
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
