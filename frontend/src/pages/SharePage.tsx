import { FormEvent, useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import {
  ApiError,
  PASSWORD_REQUIRED_CODE,
  PublicShareInfo,
  fetchPublicPreviewText,
  fetchPublicWebpkgPreview,
  getPublicShare,
  previewKind,
  verifyPublicShare,
} from '../api'

function formatSize(size: number): string {
  if (size < 1024) return `${size} B`
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`
  if (size < 1024 * 1024 * 1024) return `${(size / 1024 / 1024).toFixed(1)} MB`
  return `${(size / 1024 / 1024 / 1024).toFixed(2)} GB`
}

type LoadState =
  | { status: 'loading' }
  | { status: 'password' }
  | { status: 'ok'; info: PublicShareInfo }
  | { status: 'gone' }
  | { status: 'error'; message: string }

function ErrorCard({ title, detail }: { title: string; detail?: string }) {
  return (
    <div className="share-error">
      <div className="share-error-card">
        <h1 className="share-title">😕</h1>
        <h2>{title}</h2>
        <p className="hint">{detail ?? '请向分享者确认链接是否有效'}</p>
      </div>
    </div>
  )
}

/** 水印覆盖层：repeat 斜排文字（服务端渲染的模板结果 + 「DocFlow」），
 * pointer-events:none 不拦截任何交互。 */
function WatermarkOverlay({ text }: { text: string }) {
  const line = `${text} · DocFlow`
  const items = Array.from({ length: 24 }, (_, i) => i)
  return (
    <div className="watermark-overlay" aria-hidden="true">
      {items.map((i) => (
        <span key={i} className="watermark-item">{line}</span>
      ))}
    </div>
  )
}

/** 密码解锁卡片：公开分享受密码保护时的输入表单。 */
function PasswordCard({ busy, error, onSubmit }: {
  busy: boolean
  error: string
  onSubmit: (password: string) => void
}) {
  const [password, setPassword] = useState('')
  const submit = (e: FormEvent) => {
    e.preventDefault()
    if (password) onSubmit(password)
  }
  return (
    <div className="share-error">
      <form className="share-error-card password-card" onSubmit={submit}>
        <h1 className="share-title">🔒</h1>
        <h2>该分享受密码保护</h2>
        <p className="hint">请输入分享者提供的访问密码</p>
        <input
          className="password-input"
          type="password"
          autoFocus
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          placeholder="访问密码"
        />
        {error && <div className="error-text">{error}</div>}
        <div className="modal-actions">
          <button type="submit" className="btn primary" disabled={busy || !password}>
            {busy ? '校验中…' : '解锁'}
          </button>
        </div>
      </form>
    </div>
  )
}

/** 公开分享页（无需登录）：文件元信息 + 下载 + 按类型的内联预览（含网页包）；
 * 密码保护分享先解锁（HttpOnly 会话 cookie），水印开启时叠加全屏覆盖层。 */
export default function SharePage() {
  const { token = '' } = useParams()
  const [state, setState] = useState<LoadState>({ status: 'loading' })
  const [text, setText] = useState<string | null>(null)
  const [textError, setTextError] = useState('')
  const [webpkgUrl, setWebpkgUrl] = useState<string | null>(null)
  const [passwordBusy, setPasswordBusy] = useState(false)
  const [passwordError, setPasswordError] = useState('')

  const load = (cancelled: () => boolean) => {
    setState({ status: 'loading' })
    setText(null)
    setTextError('')
    getPublicShare(token)
      .then((info) => {
        if (!cancelled()) setState({ status: 'ok', info })
      })
      .catch((err) => {
        if (cancelled()) return
        if (err instanceof ApiError && err.code === PASSWORD_REQUIRED_CODE) {
          setState({ status: 'password' })
          return
        }
        if (err instanceof ApiError && (err.status === 404 || err.status === 410)) {
          setState({ status: 'gone' })
        } else {
          setState({ status: 'error', message: err instanceof Error ? err.message : '加载失败' })
        }
      })
  }

  useEffect(() => {
    let cancelled = false
    setPasswordError('')
    load(() => cancelled)
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token])

  const handleVerify = async (password: string) => {
    setPasswordBusy(true)
    setPasswordError('')
    let cancelled = false
    try {
      await verifyPublicShare(token, password)
      // 解锁成功：会话 cookie 已下发，重新拉取元信息。
      load(() => cancelled)
    } catch (err) {
      if (!cancelled) {
        setPasswordError(err instanceof ApiError && err.status === 401 ? '密码错误，请重试' : err instanceof Error ? err.message : '校验失败')
      }
      setPasswordBusy(false)
      return
    }
    setPasswordBusy(false)
  }

  const okState = state.status === 'ok' ? state : null

  useEffect(() => {
    if (!okState || previewKind(okState.info.mime_type) !== 'text') return
    let cancelled = false
    setText(null)
    setTextError('')
    fetchPublicPreviewText(token)
      .then((t) => {
        if (!cancelled) setText(t)
      })
      .catch(() => {
        if (!cancelled) setTextError('预览内容加载失败')
      })
    return () => {
      cancelled = true
    }
  }, [okState, token])

  // 网页包（zip）：/preview 存在 ready 解包结果时改返内容入口 URL，
  // 以 sandbox iframe 渲染；无包（415）时静默回退普通预览分支。
  const meta = okState?.info
  const isZipCandidate =
    !!meta &&
    meta.version_status === 'available' &&
    (meta.mime_type.split(';')[0].trim().toLowerCase() === 'application/zip' || /\.zip$/i.test(meta.name))

  useEffect(() => {
    if (!okState || !isZipCandidate) return
    let cancelled = false
    setWebpkgUrl(null)
    fetchPublicWebpkgPreview(token)
      .then((url) => {
        if (!cancelled) setWebpkgUrl(url)
      })
      .catch(() => {
        // 无 ready 包：维持类型分支的默认展示（不支持预览/下载提示）
      })
    return () => {
      cancelled = true
    }
  }, [okState, isZipCandidate, token])

  if (state.status === 'loading') {
    return (
      <div className="share-page">
        <header className="share-head">
          <span className="brand">DocFlow</span>
        </header>
        <main className="share-card">
          <p className="hint">加载中…</p>
        </main>
      </div>
    )
  }
  if (state.status === 'password') {
    return <PasswordCard busy={passwordBusy} error={passwordError} onSubmit={(pwd) => void handleVerify(pwd)} />
  }
  if (state.status === 'gone') {
    return (
      <ErrorCard title="链接不存在或已失效" detail="该分享可能已过期、被撤销或达到下载上限" />
    )
  }
  if (state.status === 'error') {
    return <ErrorCard title="无法打开分享" detail={state.message} />
  }

  const info = state.info
  const kind = previewKind(info.mime_type)

  const previewUrl = `/api/v1/public/shares/${encodeURIComponent(token)}/preview`
  const downloadUrl = `/api/v1/public/shares/${encodeURIComponent(token)}/download`
  const showWatermark = info.watermark_enabled && !!info.watermark_text

  return (
    <div className="share-page">
      {showWatermark && <WatermarkOverlay text={info.watermark_text as string} />}
      <header className="share-head">
        <span className="brand">DocFlow</span>
      </header>
      <main className="share-card">
        <h1 className="share-title">{info.name}</h1>
        <div className="share-meta">
          <span>大小：{formatSize(info.size)}</span>
          <span>类型：{info.mime_type || '未知'}</span>
          {info.permission === 'download' ? (
            <a className="btn primary" href={downloadUrl}>下载</a>
          ) : (
            <span className="badge">仅预览</span>
          )}
        </div>

        {info.version_status !== 'available' ? (
          <div className="empty">文件暂不可用，无法预览</div>
        ) : webpkgUrl ? (
          // 网页包：sandbox 不含 allow-same-origin（唯一化 origin 沙箱），
          // 内容端点带严格 CSP/nosniff，不携带主站认证信息。
          <div className="preview-box">
            <iframe className="preview-frame" sandbox="allow-scripts" src={webpkgUrl} title={info.name} />
          </div>
        ) : kind === 'image' ? (
          <div className="preview-box">
            <img className="preview-image" src={previewUrl} alt={info.name} />
          </div>
        ) : kind === 'pdf' ? (
          <div className="preview-box">
            <iframe className="preview-frame" src={previewUrl} title={info.name} />
          </div>
        ) : kind === 'text' ? (
          <div className="preview-box">
            {textError ? (
              <div className="error-text">{textError}</div>
            ) : text === null ? (
              <p className="hint">加载预览内容…</p>
            ) : (
              <pre className="preview-text">{text}</pre>
            )}
          </div>
        ) : (
          <div className="empty">
            该文件类型不支持在线预览{info.permission === 'download' ? '，可下载后查看' : ''}
          </div>
        )}
      </main>
    </div>
  )
}
