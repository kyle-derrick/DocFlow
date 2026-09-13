import { useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import {
  ApiError,
  PublicShareInfo,
  fetchPublicPreviewText,
  fetchPublicWebpkgPreview,
  getPublicShare,
  previewKind,
} from '../api'

function formatSize(size: number): string {
  if (size < 1024) return `${size} B`
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`
  if (size < 1024 * 1024 * 1024) return `${(size / 1024 / 1024).toFixed(1)} MB`
  return `${(size / 1024 / 1024 / 1024).toFixed(2)} GB`
}

type LoadState =
  | { status: 'loading' }
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

/** 公开分享页（无需登录）：文件元信息 + 下载 + 按类型的内联预览（含网页包）。 */
export default function SharePage() {
  const { token = '' } = useParams()
  const [state, setState] = useState<LoadState>({ status: 'loading' })
  const [text, setText] = useState<string | null>(null)
  const [textError, setTextError] = useState('')
  const [webpkgUrl, setWebpkgUrl] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setState({ status: 'loading' })
    setText(null)
    setTextError('')
    getPublicShare(token)
      .then((info) => {
        if (!cancelled) setState({ status: 'ok', info })
      })
      .catch((err) => {
        if (cancelled) return
        if (err instanceof ApiError && (err.status === 404 || err.status === 410)) {
          setState({ status: 'gone' })
        } else {
          setState({ status: 'error', message: err instanceof Error ? err.message : '加载失败' })
        }
      })
    return () => {
      cancelled = true
    }
  }, [token])

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

  return (
    <div className="share-page">
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
