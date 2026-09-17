// 公开分享页（/s/:token，无需登录）：
// - 文件分享：元信息 + 下载 + 按类型内联预览（含网页包）；
// - 目录分享（type=folder，v1.1）：tree 模式——默认探测根 index.html，存在
//   则整站 iframe（raw/share）+ 顶部「文件列表」切换；否则直接文件列表。
//   目录可逐级进入（tree API），文件按类型内联预览（raw）或下载，html 用
//   raw/share 新窗口打开；
// - 密码保护分享先解锁（HttpOnly 会话 cookie），水印开启时叠加全屏覆盖层。
import { FormEvent, useCallback, useEffect, useState } from 'react'
import { useParams } from 'react-router-dom'
import {
  ApiError,
  PASSWORD_REQUIRED_CODE,
  PublicShareInfo,
  PublicShareTree,
  ShareTreeEntry,
  fetchPublicPreviewText,
  fetchPublicWebpkgPreview,
  getPublicShare,
  previewKind,
  publicShareTree,
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

/** 与后端 /raw/* 扩展名白名单一致（不在白名单的内容 raw 端点 415）。 */
const RAW_SERVED_EXTS = new Set([
  'html', 'htm', 'css', 'js', 'json', 'txt', 'png', 'jpg', 'jpeg', 'gif', 'webp', 'avif',
  'svg', 'ico', 'woff', 'woff2', 'pdf', 'mp4', 'webm', 'mp3',
])

function isRawServed(name: string): boolean {
  const i = name.lastIndexOf('.')
  return i >= 0 && RAW_SERVED_EXTS.has(name.slice(i + 1).toLowerCase())
}

/** raw_base + 逐段编码路径（目录补尾斜杠，供 index.html 与相对资源解析）。 */
function rawUrlOf(rawBase: string, path: string, folder: boolean): string {
  const encoded = path.split('/').filter(Boolean).map(encodeURIComponent).join('/')
  return `${rawBase}/${encoded}${folder ? '/' : ''}`
}

function sortTreeEntries(entries: ShareTreeEntry[]): ShareTreeEntry[] {
  return [...entries].sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === 'folder' ? -1 : 1))
}

/** 选中文件的行内预览：文本（fetch 后 <pre>）/ 图片 / PDF 用 raw URL 直渲。 */
function ShareEntryPreview({ rawBase, entry }: { rawBase: string; entry: ShareTreeEntry }) {
  const url = rawUrlOf(rawBase, entry.path, false)
  const kind = previewKind(entry.mime_type ?? '')
  const [text, setText] = useState<string | null>(null)
  const [textError, setTextError] = useState('')

  useEffect(() => {
    if (kind !== 'text') return
    let cancelled = false
    setText(null)
    setTextError('')
    fetch(url)
      .then((res) => {
        if (!res.ok) throw new Error(`预览加载失败（${res.status}）`)
        return res.text()
      })
      .then((t) => {
        if (!cancelled) setText(t)
      })
      .catch(() => {
        if (!cancelled) setTextError('预览内容加载失败')
      })
    return () => {
      cancelled = true
    }
  }, [url, kind])

  return (
    <div className="share-tree-preview">
      <div className="share-tree-preview-head">
        <strong>{entry.name}</strong>
        {entry.size !== undefined && <span className="muted">{formatSize(entry.size)}</span>}
        <a className="btn small" href={url} download={entry.name}>下载</a>
      </div>
      {kind === 'image' ? (
        <div className="preview-box">
          <img className="preview-image" src={url} alt={entry.name} />
        </div>
      ) : kind === 'pdf' ? (
        <div className="preview-box">
          <iframe className="preview-frame" src={url} title={entry.name} />
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
      ) : isRawServed(entry.name) ? (
        <p className="hint">该类型不提供行内预览，可下载后查看。</p>
      ) : (
        <p className="hint">该文件类型不支持在线预览或下载。</p>
      )}
    </div>
  )
}

/**
 * 目录分享视图（tree 模式）：根 index.html 探测成功时整站 iframe（顶部
 * 「文件列表」切换）；文件列表支持逐级进入子目录、按类型预览/下载/新窗口
 * 打开网页。每次 tree 拉取均刷新 raw_base（10 分钟 grant，页面常驻时点击
 * 目录/文件前由最新 grant 支撑）。
 */
function FolderShareView({ token, info }: { token: string; info: PublicShareInfo }) {
  const [dir, setDir] = useState('')
  const [entries, setEntries] = useState<ShareTreeEntry[] | null>(null)
  const [rawBase, setRawBase] = useState('')
  const [error, setError] = useState('')
  // 根 index.html 探测：null = 探测中；true = 可整站渲染。
  const [siteReady, setSiteReady] = useState<boolean | null>(null)
  const [view, setView] = useState<'site' | 'files'>('site')
  const [selected, setSelected] = useState<ShareTreeEntry | null>(null)

  const loadDir = useCallback(async (path: string) => {
    setError('')
    setSelected(null)
    setEntries(null)
    try {
      const res = await publicShareTree(token, path)
      setEntries(sortTreeEntries(res.entries ?? []))
      setRawBase(res.raw_base)
      setDir(path)
    } catch (err) {
      setEntries([])
      setError(err instanceof Error ? err.message : '目录加载失败')
    }
  }, [token])

  useEffect(() => {
    void loadDir('')
  }, [loadDir])

  // 根 index.html 探测（一次性）：命中则默认整站 iframe，否则默认文件列表。
  useEffect(() => {
    let alive = true
    publicShareTree(token, 'index.html')
      .then((r: PublicShareTree) => {
        if (alive) setSiteReady(r.type === 'file')
      })
      .catch(() => {
        if (alive) setSiteReady(false)
      })
    return () => {
      alive = false
    }
  }, [token])

  useEffect(() => {
    if (siteReady === false) setView('files')
  }, [siteReady])

  const openEntry = (entry: ShareTreeEntry) => {
    if (entry.type === 'folder') {
      void loadDir(entry.path)
      return
    }
    // html：raw/share 新窗口打开（sandbox 由 raw 端点 CSP 下发）。
    if (entry.name.toLowerCase().endsWith('.html') || entry.name.toLowerCase().endsWith('.htm')) {
      window.open(rawUrlOf(rawBase, entry.path, false), '_blank', 'noopener')
      return
    }
    setSelected(entry)
  }

  const dirSegments = dir.split('/').filter(Boolean)

  return (
    <>
      <div className="share-tree-toolbar">
        {siteReady === null ? (
          <span className="muted">正在检查网页入口…</span>
        ) : (
          <div className="seg-group" role="tablist">
            <button
              type="button"
              role="tab"
              aria-selected={view === 'site'}
              className={`seg${view === 'site' ? ' active' : ''}`}
              onClick={() => setView('site')}
            >
              网页视图
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={view === 'files'}
              className={`seg${view === 'files' ? ' active' : ''}`}
              onClick={() => setView('files')}
            >
              文件列表
            </button>
          </div>
        )}
        {/* 目录分享整包下载（公开端点直接 a[download]；view 权限分享后端 403，不展示）。 */}
        {info.permission === 'download' && (
          <a
            className="btn small"
            href={`/api/v1/public/shares/${encodeURIComponent(token)}/download.zip`}
            download={`${info.name}.zip`}
            title="下载整个目录（ZIP）"
          >
            下载整包
          </a>
        )}
      </div>

      {view === 'site' && siteReady === true && rawBase && (
        // 整站渲染：目录尾斜杠 raw URL → 服务端解析 index.html，相对资源
        // 落在 raw/share 同一前缀下；sandbox 见 raw 端点 CSP（iframe 属性
        // 额外放宽 forms/popups/modals 以兼容交互式静态站）。
        <div className="share-site-box">
          <iframe
            className="share-site-frame"
            sandbox="allow-scripts allow-forms allow-popups allow-modals"
            src={rawUrlOf(rawBase, '', true)}
            title={info.name}
          />
        </div>
      )}

      {view === 'site' && siteReady === true && !rawBase && <p className="hint">加载网页…</p>}

      {(view === 'files' || siteReady !== true) && (
        <div className="share-tree">
          <nav className="share-tree-crumb">
            <button className={dirSegments.length === 0 ? 'current' : ''} onClick={() => void loadDir('')}>
              {info.name}
            </button>
            {dirSegments.map((seg, i) => (
              <span key={i} className="crumb">
                <span className="sep">/</span>
                <button onClick={() => void loadDir(dirSegments.slice(0, i + 1).join('/'))}>{seg}</button>
              </span>
            ))}
          </nav>
          {error && <div className="error-text">{error}</div>}
          {entries === null ? (
            <p className="hint">加载目录…</p>
          ) : entries.length === 0 && !error ? (
            <div className="empty">此目录为空</div>
          ) : (
            <ul className="share-tree-list">
              {entries.map((entry) => {
                const isHtml = entry.name.toLowerCase().endsWith('.html') || entry.name.toLowerCase().endsWith('.htm')
                const previewable = previewKind(entry.mime_type ?? '') !== 'unsupported' || isHtml || isRawServed(entry.name)
                return (
                  <li key={entry.path} className="share-tree-row">
                    <button className="share-tree-name" onClick={() => openEntry(entry)} title={entry.name}>
                      <span className="icon">{entry.type === 'folder' ? '📁' : '📄'}</span>
                      <span>{entry.name}</span>
                    </button>
                    <span className="muted share-tree-size">
                      {entry.type === 'folder' ? '目录' : entry.size !== undefined ? formatSize(entry.size) : ''}
                    </span>
                    <span className="share-tree-actions">
                      {entry.type === 'file' && isHtml && (
                        <button
                          className="btn small"
                          onClick={() => window.open(rawUrlOf(rawBase, entry.path, false), '_blank', 'noopener')}
                        >
                          打开网页
                        </button>
                      )}
                      {entry.type === 'file' && !isHtml && isRawServed(entry.name) && (
                        <a className="btn small" href={rawUrlOf(rawBase, entry.path, false)} download={entry.name}>
                          下载
                        </a>
                      )}
                      {entry.type === 'file' && !previewable && <span className="muted">不支持在线预览</span>}
                    </span>
                  </li>
                )
              })}
            </ul>
          )}
          {selected && <ShareEntryPreview rawBase={rawBase} entry={selected} />}
        </div>
      )}
    </>
  )
}

/** 公开分享页（无需登录）：文件元信息 + 下载 + 按类型的内联预览（含网页包）；
 * 目录分享进 tree 模式；密码保护分享先解锁（HttpOnly 会话 cookie），
 * 水印开启时叠加全屏覆盖层。 */
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
    setWebpkgUrl(null)
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
      // 解锁成功：会话 cookie 已下发，重新拉取元信息（目录分享随后走 tree）。
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
    if (!okState || okState.info.type === 'folder' || previewKind(okState.info.mime_type ?? '') !== 'text') return
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
    meta.type !== 'folder' &&
    meta.version_status === 'available' &&
    (meta.mime_type?.split(';')[0].trim().toLowerCase() === 'application/zip' || /\.zip$/i.test(meta.name))

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
  const kind = previewKind(info.mime_type ?? '')

  const downloadUrl = `/api/v1/public/shares/${encodeURIComponent(token)}/download`
  const showWatermark = info.watermark_enabled && !!info.watermark_text

  // 目录分享：tree 模式（根 index.html 整站 iframe / 文件列表）。
  if (info.type === 'folder') {
    return (
      <div className="share-page">
        {showWatermark && <WatermarkOverlay text={info.watermark_text as string} />}
        <header className="share-head">
          <span className="brand">DocFlow</span>
        </header>
        <main className="share-card share-card-wide">
          <h1 className="share-title">📁 {info.name}</h1>
          <div className="share-meta">
            <span>目录分享</span>
            <span className="badge">{info.permission === 'download' ? '可下载' : '仅查看'}</span>
          </div>
          <FolderShareView token={token} info={info} />
        </main>
      </div>
    )
  }

  const previewUrl = `/api/v1/public/shares/${encodeURIComponent(token)}/preview`

  return (
    <div className="share-page">
      {showWatermark && <WatermarkOverlay text={info.watermark_text as string} />}
      <header className="share-head">
        <span className="brand">DocFlow</span>
      </header>
      <main className="share-card">
        <h1 className="share-title">{info.name}</h1>
        <div className="share-meta">
          <span>大小：{formatSize(info.size ?? 0)}</span>
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
