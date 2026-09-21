// 公开分享页（/s/:token，无需登录）：
// - 文件分享：元信息 + 下载 + 按类型内联预览（含网页包与 Office 文档的
//   OnlyOffice 只读查看会话）；
// - 目录分享（type=folder，v1.1）：tree 模式——默认探测根 index.html，存在
//   则整站 iframe（raw/share）+ 顶部「文件列表」切换；否则直接文件列表。
//   目录可逐级进入（tree API），文件按类型内联预览（raw）或下载，html 用
//   raw/share 新窗口打开；
// - v2.4：目录/打包分享点击文件改为弹窗查看（PublicEntryViewer，与文件页
//   查看弹窗同规格），补齐富文本(.dfrt/.dfdoc)/md/xmind/白板/mermaid/
//   drawio 的只读渲染路径（内容经 /raw/share 白名单扩展获取）；
// - 密码保护分享先解锁（HttpOnly 会话 cookie），水印开启时叠加全屏覆盖层。
import { FormEvent, Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import { FileQuestion, FileText, Folder, Lock } from 'lucide-react'
import { Button, Input, Modal as AntdModal, Segmented } from 'antd'
import {
  ApiError,
  PASSWORD_REQUIRED_CODE,
  PublicShareInfo,
  PublicShareOffice,
  PublicShareTree,
  ShareTreeEntry,
  fetchPublicPreviewText,
  fetchPublicShareOffice,
  fetchPublicWebpkgPreview,
  getPublicShare,
  isDfdocFile,
  isDrawioFile,
  isDrawioXmlContent,
  isExcalidrawFile,
  isMermaidFile,
  isOfficeFile,
  isXmindFile,
  previewKind,
  publicShareTree,
  verifyPublicShare,
} from '../api'
import { formatQuota } from '../components/FileBrowser'
import { loadDocEditorScript } from './EditorPage'
import { MarkdownViewer } from './TextEditorPage'
import { parseScene } from './ExcalidrawPage'
import DrawioViewer from '../components/DrawioViewer'
import ExcalidrawViewer from '../components/ExcalidrawViewer'
import MermaidDiagram from '../components/MermaidDiagram'
import XMindViewer from '../components/XMindViewer'
import { useColorMode } from '../theme'

// 富文本编辑器（Tiptap 产物 1MB+）懒加载独立 chunk（与 DfdocEditorPage 同法）。
const RichTextEditor = lazy(() => import('../components/richtext/RichTextEditor'))

/** 字节数可读格式（v2.6 统一 formatQuota 口径：B→KiB/MiB/GiB/TiB）。 */
function formatSize(size: number): string {
  return formatQuota(size, false)
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
        <h1 className="share-title"><FileQuestion size={44} strokeWidth={2} aria-hidden="true" /></h1>
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
        <h1 className="share-title"><Lock size={44} strokeWidth={2} aria-hidden="true" /></h1>
        <h2>该分享受密码保护</h2>
        <p className="hint">请输入分享者提供的访问密码</p>
        <Input.Password
          className="password-input"
          autoFocus
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          placeholder="访问密码"
        />
        {error && <div className="error-text">{error}</div>}
        <div className="modal-actions">
          <Button type="primary" htmlType="submit" disabled={busy || !password}>
            {busy ? '校验中…' : '解锁'}
          </Button>
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

/**
 * Office 文档公开查看（访客态）：GET /public/shares/:token/office 取 JWT
 * 签名的只读 DocEditor 配置，复用 EditorPage 的 api.js 动态加载初始化编辑
 * 器（恒 view 模式）；失败（集成未启用/网络）回落「不支持在线预览」文案。
 */
function PublicOfficeViewer({ token, name }: { token: string; name: string }) {
  const dark = useColorMode() === 'dark'
  const [phase, setPhase] = useState<'loading' | 'ready' | 'failed'>('loading')
  const shellRef = useRef<HTMLDivElement | null>(null)
  const editorRef = useRef<{ destroyEditor: () => void } | null>(null)

  useEffect(() => {
    let alive = true
    const shell = shellRef.current
    const init = async () => {
      setPhase('loading')
      try {
        const { server_url, config }: PublicShareOffice = await fetchPublicShareOffice(token, 'zh-CN')
        await loadDocEditorScript(server_url)
        if (!alive || !window.DocsAPI?.DocEditor || !shellRef.current) return
        const holder = document.createElement('div')
        const holderId = `share-onlyoffice-${Math.random().toString(36).slice(2)}`
        holder.id = holderId
        holder.className = 'editor-placeholder'
        shellRef.current.replaceChildren(holder)
        editorRef.current = window.DocsAPI.DocEditor(holderId, {
          ...config,
          width: '100%',
          height: '100%',
          customization: {
            ...((config.customization as Record<string, unknown> | undefined) ?? {}),
            uiTheme: dark ? 'theme-dark' : 'theme-classic-light',
          },
        })
        if (alive) setPhase('ready')
      } catch {
        if (alive) setPhase('failed')
      }
    }
    void init()
    return () => {
      alive = false
      editorRef.current?.destroyEditor()
      editorRef.current = null
      shell?.replaceChildren()
    }
    // dark 变化不重建会话（uiTheme 仅初始化时生效，主题切换少见场景忽略）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [token])

  if (phase === 'failed') {
    return <div className="empty">该文件类型不支持在线预览，请联系分享者开启 OnlyOffice 集成或下载后查看</div>
  }
  return (
    <div className="preview-box share-office-box">
      {phase === 'loading' && <p className="hint">正在加载文档查看器…</p>}
      <div className="editor-shell" ref={shellRef} title={name} />
    </div>
  )
}

/** 经 raw/share 拉取文本内容（raw 端点白名单内扩展名；失败抛 Error）。 */
async function fetchRawText(url: string): Promise<string> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`内容加载失败（${res.status}）`)
  return res.text()
}

/** 经 raw/share 拉取二进制内容（.xmind 等本地解析）。 */
async function fetchRawBuffer(url: string): Promise<ArrayBuffer> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`内容加载失败（${res.status}）`)
  return res.arrayBuffer()
}

/** 弹窗内文本加载态/错误态（与独立查看页的 text-editor-state 同视觉）。 */
function ViewerState({ text }: { text: string }) {
  return <div className="text-editor-page viewer-only"><div className="text-editor-state">{text}</div></div>
}

/**
 * 单文件分享的富文本文档查看：同一 RichTextEditor 只读渲染；内容与
 * raw_base 均经 tree API 获取（单文件分享的根即文件自身，raw 直取 .dfrt
 * 文本——/preview 端点对存储 mime=octet-stream 的文档会 415，raw 扩展名
 * 白名单更稳）。引用节点以 publicBase 走 raw/share 探测；单文件分享无
 * 兄弟资源授权，探测不到显示占位提示。
 */
function PublicSingleDfdocViewer({ token }: { token: string }) {
  const [state, setState] = useState<{ rawBase: string; docPath: string; text: string } | null>(null)
  const [error, setError] = useState('')
  useEffect(() => {
    let alive = true
    publicShareTree(token)
      .then(async (res) => {
        // 单文件分享根即文件自身：raw 空路径（尾斜杠）命中根文件；
        // 拼文件名会走子树解析（文件基座无子路径）而 404。
        const raw = await fetch(`${res.raw_base}/`)
        if (!raw.ok) throw new Error(`内容加载失败（${raw.status}）`)
        const text = await raw.text()
        if (alive) setState({ rawBase: res.raw_base, docPath: res.path, text })
      })
      .catch(() => {
        if (alive) setError('文档内容加载失败')
      })
    return () => { alive = false }
  }, [token])
  if (error) return <div className="error-text">{error}</div>
  if (!state) return <p className="hint">正在加载文档…</p>
  return (
    <RichTextEditor
      initialJSON={state.text}
      readonly
      publicBase={{ rawBase: state.rawBase, docPath: state.docPath }}
    />
  )
}

/**
 * 目录分享条目的弹窗查看内容（v2.4）：按扩展名分发只读渲染，与文件页
 * FileViewerDispatch 同口径——富文本(.dfrt/.dfdoc)/Markdown/mermaid/
 * xmind/白板(drawio/excalidraw)/图片/PDF/纯文本。内容经 raw/share 获取
 * （后端白名单已扩展相应扩展名）；html 仍新窗口打开（点击处已分流）。
 */
function PublicEntryViewer({ rawBase, entry }: { rawBase: string; entry: ShareTreeEntry }) {
  const dark = useColorMode() === 'dark'
  const lower = entry.name.toLowerCase()
  const url = rawUrlOf(rawBase, entry.path, false)
  const kind = previewKind(entry.mime_type ?? '')
  const [text, setText] = useState<string | null>(null)
  const [error, setError] = useState('')
  const endsXml = lower.endsWith('.xml')

  // 文本类内容统一拉取（md/mermaid/drawio/xml 嗅探/dfrt/excalidraw/txt 等）。
  const needsText =
    isDfdocFile(lower)
    || lower.endsWith('.md') || lower.endsWith('.markdown')
    || isMermaidFile(lower)
    || isExcalidrawFile(lower)
    || isDrawioFile(lower)
    || endsXml
    || kind === 'text'

  useEffect(() => {
    if (!needsText) return
    let alive = true
    setText(null)
    setError('')
    void fetchRawText(url)
      .then((t) => { if (alive) setText(t) })
      .catch(() => { if (alive) setError('内容加载失败') })
    return () => { alive = false }
  }, [url, needsText])

  // 富文本（Tiptap JSON）readonly 渲染：同一 RichTextEditor 组件（嵌入块/
  // 图片/文件卡片按 publicBase 走 raw/share 公开端点解析）。
  if (isDfdocFile(lower)) {
    if (error) return <ViewerState text={error} />
    if (text === null) return <ViewerState text="正在加载文档…" />
    return (
      <Suspense fallback={<ViewerState text="正在加载查看器…" />}>
        <RichTextEditor initialJSON={text} readonly publicBase={{ rawBase, docPath: entry.path }} />
      </Suspense>
    )
  }
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) {
    if (error) return <ViewerState text={error} />
    if (text === null) return <ViewerState text="正在加载文档…" />
    return <div className="text-editor-page viewer-only"><MarkdownViewer source={text} /></div>
  }
  if (isMermaidFile(lower)) {
    if (error) return <ViewerState text={error} />
    if (text === null) return <ViewerState text="正在加载图表…" />
    return <div className="text-editor-page viewer-only diagram-viewer-page"><MermaidDiagram source={text} dark={dark} /></div>
  }
  if (isExcalidrawFile(lower)) {
    if (error) return <ViewerState text={error} />
    if (text === null) return <ViewerState text="正在加载白板…" />
    const scene = parseScene(text)
    if (scene.elements.length === 0) return <ViewerState text="该白板为空或内容损坏" />
    return (
      <div className="text-editor-page viewer-only diagram-viewer-page">
        <ExcalidrawViewer elements={scene.elements} appState={scene.appState} files={scene.files} title={entry.name} />
      </div>
    )
  }
  if (isDrawioFile(lower)) {
    if (error) return <ViewerState text={error} />
    if (text === null) return <ViewerState text="正在加载图表…" />
    return (
      <div className="text-editor-page viewer-only diagram-viewer-page">
        <DrawioViewer baseURL="/drawio" xml={text || '<mxfile><diagram/></mxfile>'} title={entry.name} dark={dark} lang="zh" />
      </div>
    )
  }
  if (endsXml) {
    if (text === null) return <ViewerState text="正在加载…" />
    if (text !== null && isDrawioXmlContent(text)) {
      return (
        <div className="text-editor-page viewer-only diagram-viewer-page">
          <DrawioViewer baseURL="/drawio" xml={text} title={entry.name} dark={dark} lang="zh" />
        </div>
      )
    }
    return <div className="text-editor-page viewer-only"><pre className="preview-text">{text}</pre></div>
  }
  if (isXmindFile(lower)) {
    return (
      <div className="text-editor-page viewer-only diagram-viewer-page">
        <XMindViewer fileId="" title={entry.name} convertible={false} loadBuffer={() => fetchRawBuffer(url)} />
      </div>
    )
  }
  if (kind === 'image') {
    return <div className="preview-box"><img className="preview-image" src={url} alt={entry.name} /></div>
  }
  if (kind === 'pdf') {
    return <div className="preview-box"><iframe className="preview-frame" src={url} title={entry.name} /></div>
  }
  if (kind === 'text') {
    if (error) return <ViewerState text={error} />
    if (text === null) return <ViewerState text="正在加载内容…" />
    return <div className="text-editor-page viewer-only"><pre className="preview-text">{text}</pre></div>
  }
  return (
    <ViewerState text="该文件类型不支持在线预览，可下载后查看。" />
  )
}

/**
 * 目录分享条目查看弹窗（v2.4）：与文件页查看弹窗（FileBrowser Modal
 * modal-viewer 规格）同视觉——min(1180px,94vw) 宽 + 定高 body 内滚；
 * 内容渲染由 PublicEntryViewer 分发。下载按钮按分享权限展示。
 */
function ShareEntryModal({
  rawBase,
  entry,
  permission,
  onClose,
}: {
  rawBase: string
  entry: ShareTreeEntry
  permission: 'view' | 'download'
  onClose: () => void
}) {
  const url = rawUrlOf(rawBase, entry.path, false)
  return (
    <AntdModal
      open
      centered
      footer={null}
      width="min(1180px, 94vw)"
      title={<span className="share-entry-modal-title">查看「{entry.name}」</span>}
      styles={{ body: { overflow: 'auto', height: 'min(76vh, 760px)', maxHeight: 'min(76vh, 760px)' } }}
      classNames={{
        header: 'docflow-modal-header',
        title: 'docflow-modal-title',
        body: 'docflow-modal-body',
        close: 'docflow-modal-close',
      }}
      className="docflow-modal modal-viewer"
      onCancel={onClose}
    >
      <div className="preview-embed">
        <PublicEntryViewer rawBase={rawBase} entry={entry} />
      </div>
      {permission === 'download' && isRawServed(entry.name) && (
        <div className="preview-foot">
          <Button size="small" href={url} download={entry.name}>下载</Button>
        </div>
      )}
    </AntdModal>
  )
}

/**
 * 目录分享视图（tree 模式）：根 index.html 探测成功时整站 iframe；
 * 「网页视图 / 文件列表」切换与「下载整包」收进右上角小控件区（紧凑顶条），
 * 主体内容区最大化（flex 铺满剩余视口，内部滚动）。文件列表支持逐级进入
 * 子目录、按类型预览/下载/新窗口打开网页。每次 tree 拉取均刷新 raw_base
 *（10 分钟 grant，页面常驻时点击目录/文件前由最新 grant 支撑）。
 */
function FolderShareView({ token, info }: { token: string; info: PublicShareInfo }) {
  const [dir, setDir] = useState('')
  const [entries, setEntries] = useState<ShareTreeEntry[] | null>(null)
  const [rawBase, setRawBase] = useState('')
  const [error, setError] = useState('')
  // 根 index.html 探测：null = 探测中；true = 可整站渲染。
  const [siteReady, setSiteReady] = useState<boolean | null>(null)
  const [view, setView] = useState<'site' | 'files'>('site')
  // 弹窗查看目标（v2.4：点击文件弹出与文件页一致的查看弹窗）。
  const [viewing, setViewing] = useState<ShareTreeEntry | null>(null)

  const loadDir = useCallback(async (path: string) => {
    setError('')
    setViewing(null)
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
    // v2.4：其余类型弹窗查看（与文件页查看弹窗分发一致）。
    setViewing(entry)
  }

  const dirSegments = dir.split('/').filter(Boolean)

  return (
    <>
      {/* 紧凑顶条：左标题 + 权限徽章，右侧小控件区（视图切换 + 下载整包）。 */}
      <div className="share-folder-bar">
        <div className="share-folder-title">
          <Folder size={16} strokeWidth={2} aria-hidden="true" />
          <span className="share-folder-name">{info.name}</span>
          <span className="badge">{info.permission === 'download' ? '可下载' : '仅查看'}</span>
        </div>
        <div className="share-folder-actions">
          {siteReady === true && (
            <Segmented
              size="small"
              value={view}
              onChange={(v) => setView(v as 'site' | 'files')}
              options={[
                { label: '网页视图', value: 'site' },
                { label: '文件列表', value: 'files' },
              ]}
            />
          )}
          {/* 目录分享整包下载（公开端点直接 a[download]；view 权限分享后端 403，不展示）。 */}
          {info.permission === 'download' && (
            <Button
              size="small"
              href={`/api/v1/public/shares/${encodeURIComponent(token)}/download.zip`}
              download={`${info.name}.zip`}
              title="下载整个目录（ZIP）"
            >
              下载整包
            </Button>
          )}
        </div>
      </div>

      {/* 主体内容区：最大化铺满剩余视口，内部滚动。 */}
      <div className="share-folder-body">
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
          <div className="share-tree soft-fade" key={dir}>
            {siteReady === null && <p className="hint">正在检查网页入口…</p>}
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
                        <span className="icon">{entry.type === 'folder'
                          ? <Folder size={14} strokeWidth={2} aria-hidden="true" />
                          : <FileText size={14} strokeWidth={2} aria-hidden="true" />}</span>
                        <span>{entry.name}</span>
                      </button>
                      <span className="muted share-tree-size">
                        {entry.type === 'folder' ? '目录' : entry.size !== undefined ? formatSize(entry.size) : ''}
                      </span>
                      <span className="share-tree-actions">
                        {entry.type === 'file' && isHtml && (
                          <Button
                            size="small"
                            onClick={() => window.open(rawUrlOf(rawBase, entry.path, false), '_blank', 'noopener')}
                          >
                            打开网页
                          </Button>
                        )}
                        {entry.type === 'file' && !isHtml && isRawServed(entry.name) && (
                          <Button size="small" href={rawUrlOf(rawBase, entry.path, false)} download={entry.name}>
                            下载
                          </Button>
                        )}
                        {entry.type === 'file' && !previewable && <span className="muted">不支持在线预览</span>}
                      </span>
                    </li>
                  )
                })}
              </ul>
            )}
          </div>
        )}

        {/* 弹窗查看（v2.4：点击文件 = 与文件页一致的查看弹窗）。 */}
        {viewing && rawBase && (
          <ShareEntryModal rawBase={rawBase} entry={viewing} permission={info.permission} onClose={() => setViewing(null)} />
        )}
      </div>
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

  // 目录分享：tree 模式（根 index.html 整站 iframe / 文件列表），
  // 全幅布局（无顶栏：紧凑顶条 + 最大化内容区，见 FolderShareView）。
  if (info.type === 'folder') {
    return (
      <div className="share-page share-folder-page">
        {showWatermark && <WatermarkOverlay text={info.watermark_text as string} />}
        <FolderShareView token={token} info={info} />
      </div>
    )
  }

  const previewUrl = `/api/v1/public/shares/${encodeURIComponent(token)}/preview`

  return (
    <div className="share-page">
      {showWatermark && <WatermarkOverlay text={info.watermark_text as string} />}
      <main className="share-card soft-fade">
        <h1 className="share-title">{info.name}</h1>
        <div className="share-meta">
          <span>大小：{formatSize(info.size ?? 0)}</span>
          <span>类型：{info.mime_type || '未知'}</span>
          {info.permission === 'download' ? (
            <Button type="primary" href={downloadUrl}>下载</Button>
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
        ) : isOfficeFile(info.name) ? (
          // Office 文档：OnlyOffice 只读查看会话（公开端点，访客无需登录）。
          <PublicOfficeViewer token={token} name={info.name} />
        ) : isDfdocFile(info.name) ? (
          // 富文本文档（.dfrt/.dfdoc）：同一 RichTextEditor 只读渲染
          //（内容与引用资源均经 raw/share 公开端点，单文件分享无兄弟资源授权）。
          <div className="preview-box">
            <Suspense fallback={<p className="hint">正在加载查看器…</p>}>
              <PublicSingleDfdocViewer token={token} />
            </Suspense>
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
