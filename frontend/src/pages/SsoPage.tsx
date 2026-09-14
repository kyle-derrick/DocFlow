// SSO 落地页：后端 OIDC 回调成功后 302 到 /sso#access_token=<jwt>。
// URL fragment 不随请求发送（不进日志/Referer），本页读取后把 access_token
// 存入内存（api.ts 模块级变量，不写 localStorage），随即清除 fragment 并
// 跳转首页；refresh 会话由后端已下发的 HttpOnly cookie 维持。
import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { setAccessToken } from '../api'

export default function SsoPage() {
  const navigate = useNavigate()
  const [error, setError] = useState('')

  useEffect(() => {
    const match = window.location.hash.match(/access_token=([^&]+)/)
    if (match && match[1]) {
      setAccessToken(decodeURIComponent(match[1]))
      // 立即清除 fragment（令牌不留在地址栏/历史记录）。
      window.history.replaceState(null, '', window.location.pathname)
      navigate('/', { replace: true })
      return
    }
    setError('单点登录未完成：回调缺少访问令牌，请重新发起登录。')
  }, [navigate])

  return (
    <div className="login-wrap">
      <div className="login-card">
        <h1 className="login-title">DocFlow</h1>
        {error ? (
          <>
            <div className="error-text">{error}</div>
            <p className="hint">
              <Link to="/login">返回登录页</Link>
            </p>
          </>
        ) : (
          <p className="hint">正在完成单点登录…</p>
        )}
      </div>
    </div>
  )
}
