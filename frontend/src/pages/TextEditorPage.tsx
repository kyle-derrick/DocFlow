import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { isValidElement } from 'react'
import { App as AntdApp, Button, Segmented } from 'antd'
import ReactMarkdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import MarkmapDiagram from '../components/MarkmapDiagram'
import MermaidDiagram from '../components/MermaidDiagram'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'
import { MessageKey, t, useLocale } from '../i18n'
import { useColorMode } from '../theme'

// Monaco（VSCode）编辑器：本地打包 + 按语言 worker（产物 3MB+ 独立 chunk，
// 仅编辑态进入时拉取；查看态一律 <pre> 纯渲染，不加载 Monaco）。
// v1.7 起 .md 回归纯 Markdown（源码+渲染），富文本编辑仅 .dfdoc
//（见 DfdocEditorPage / RichTextEditor）。
const MonacoEditor = lazy(() => import('../components/MonacoEditor'))

export type EditorKind = 'text' | 'markdown' | 'html' | 'css' | 'javascript'

const defaultNames: Record<EditorKind, string> = {
  text: 'text.txt',
  markdown: 'note.md',
  html: 'index.html',
  css: 'style.css',
  javascript: 'script.js',
}

const mimeTypes: Record<EditorKind, string> = {
  text: 'text/plain',
  markdown: 'text/markdown',
  html: 'text/html',
  css: 'text/css',
  javascript: 'text/javascript',
}

/** editorKind → Monaco language id（text 无语法，plaintext）。 */
const monacoLanguages: Record<EditorKind, string> = {
  text: 'plaintext',
  markdown: 'markdown',
  html: 'html',
  css: 'css',
  javascript: 'javascript',
}

/** Monaco 明暗主题跟随站点 data-mode。 */
function monacoTheme(dark: boolean): string {
  return dark ? 'vs-dark' : 'vs'
}

/** Monaco 通用 options（编辑态 readOnly=false，查看态 true）。 */
function monacoOptions(readOnly: boolean) {
  return {
    minimap: { enabled: false },
    automaticLayout: true,
    fontSize: 14,
    wordWrap: 'on' as const,
    readOnly,
    scrollBeyondLastLine: false,
    contextmenu: !readOnly,
  }
}

/** 从 <pre> 的子节点中提取 mermaid/markmap 围栏代码块（{lang, code}）。 */
function diagramFromPre(children: ReactNode): { lang: 'mermaid' | 'markmap'; code: string } | null {
  const child = Array.isArray(children) ? children[0] : children
  if (!isValidElement<{ className?: unknown; children?: unknown }>(child)) return null
  const className = typeof child.props.className === 'string' ? child.props.className : ''
  const match = /^language-(mermaid|markmap)$/.exec(className)
  if (!match) return null
  if (typeof child.props.children !== 'string') return null
  return { lang: match[1] as 'mermaid' | 'markmap', code: child.props.children.replace(/\n$/, '') }
}

/** Markdown 只读渲染（mermaid/markmap 围栏代码块转只读图表）；
 * 导出供公开分享页查看器复用（v2.4：分享页点击 md 弹窗与文件页一致）。 */
export function MarkdownViewer({ source }: { source: string }) {
  const dark = useColorMode() === 'dark'
  // ```mermaid / ```markmap 围栏代码块渲染为只读图表，其余代码块原样展示。
  const components: Components = {
    pre: ({ children, ...rest }) => {
      const diagram = diagramFromPre(children)
      if (diagram?.lang === 'mermaid') {
        return <div className="diagram-block"><MermaidDiagram source={diagram.code} dark={dark} /></div>
      }
      if (diagram?.lang === 'markmap') {
        return <div className="diagram-block markmap-block"><MarkmapDiagram source={diagram.code} /></div>
      }
      return <pre {...rest}>{children}</pre>
    },
  }
  return <div className="markdown-preview"><ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>{source}</ReactMarkdown></div>
}

function HtmlViewer({ source, name }: { source: string; name: string }) {
  const url = useMemo(() => URL.createObjectURL(new Blob([source], { type: 'text/html' })), [source])
  useEffect(() => () => URL.revokeObjectURL(url), [url])
  return <iframe className="standalone-viewer-frame" sandbox="allow-scripts" src={url} title={name} />
}

// ---- Markdown 编辑器视图模式（v1.6）：编辑 / 分栏（左源码右预览同步滚动）/
//      预览；默认分栏，偏好持久化 localStorage。 ----

type MdViewMode = 'edit' | 'split' | 'preview'

const MD_VIEW_KEY = 'docflow.mdViewMode'

/** md 分栏比例（百分比，编辑侧宽度；localStorage 记住，默认 50）。 */
const MD_SPLIT_KEY = 'docflow.mdSplitRatio'

function loadMdViewMode(): MdViewMode {
  const v = window.localStorage.getItem(MD_VIEW_KEY)
  return v === 'edit' || v === 'split' || v === 'preview' ? v : 'split'
}

function saveMdViewMode(mode: MdViewMode): void {
  try {
    window.localStorage.setItem(MD_VIEW_KEY, mode)
  } catch {
    /* ignore */
  }
}

function loadSplitRatio(): number {
  const v = Number(window.localStorage.getItem(MD_SPLIT_KEY))
  return Number.isFinite(v) && v >= 20 && v <= 80 ? v : 50
}

/** 源码只读查看（txt/css/js 等纯文本类）：等宽 <pre> 直接渲染（自动换行、
 * 铺满滚动）——查看路径不加载 Monaco（chunk 3MB+ 且在弹窗内高度不稳），
 * Monaco 仅编辑态使用。 */
function TextViewer({ source }: { source: string }) {
  return <pre className="preview-text">{source}</pre>
}

function SourceViewer({ kind, source, name }: { kind: EditorKind; source: string; name: string }) {
  if (kind === 'markdown') return <MarkdownViewer source={source} />
  if (kind === 'html') return <HtmlViewer source={source} name={name} />
  return <TextViewer source={source} />
}

export default function TextEditorPage({
  kind,
  mode,
  fileId: fileIdProp,
}: { kind: EditorKind; mode?: 'edit' | 'view'; fileId?: string }) {
  const { fileId: routeFileId = '' } = useParams()
  // by-path 路由经 prop 传入 resolve 得到的 file_id；缺省回退路由参数。
  const fileId = fileIdProp ?? routeFileId
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const returnTo = safeReturnTo(searchParams.get('returnTo'))
  const { modal: antdModal } = AntdApp.useApp()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const requestedKind = searchParams.get('kind')
  const editorKind: EditorKind = requestedKind === 'html' || requestedKind === 'css' || requestedKind === 'javascript'
    ? requestedKind
    : kind
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const dark = useColorMode() === 'dark'
  const [name, setName] = useState(defaultNames[editorKind])
  const [text, setText] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [preview, setPreview] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [dirty, setDirty] = useState(false)
  const dirtyRef = useRef(false)
  // markdown 视图模式（编辑/分栏/预览，默认分栏，localStorage 记住）。
  const [mdView, setMdView] = useState<MdViewMode>(loadMdViewMode)
  // md 分栏比例（编辑侧宽度百分比，拖拽分隔条调整，localStorage 记住）。
  const [splitRatio, setSplitRatio] = useState(loadSplitRatio)
  // 分栏同步滚动：Monaco 实例与预览容器百分比互推（lock 防回环）。
  // 注意：IStandaloneCodeEditor 无 getHeight()（曾按此调用在分栏滚动时抛
  // TypeError: k.getHeight is not a function），视口高度经 getLayoutInfo().height。
  const monacoRef = useRef<{ getScrollTop(): number; setScrollTop(v: number): void; getScrollHeight(): number; getLayoutInfo(): { height: number } } | null>(null)
  const previewScrollRef = useRef<HTMLDivElement | null>(null)
  const syncLockRef = useRef(false)
  // 分栏拖拽：containerRef 记录分栏容器（比例按容器宽度换算）。
  const splitHostRef = useRef<HTMLDivElement | null>(null)

  const markDirty = () => {
    dirtyRef.current = true
    setDirty(true)
    setNotice('')
  }

  // 分栏拖拽：mousedown 分隔条 → mousemove 按容器宽度换算比例（20-80%）→
  // mouseup 持久化 localStorage（监听器命令式挂载，拖拽期间持续有效）。
  const startSplitDrag = (e: React.MouseEvent) => {
    if (mdView !== 'split') return
    e.preventDefault()
    const host = splitHostRef.current
    if (!host) return
    let latest = splitRatio
    const onMove = (ev: MouseEvent) => {
      const rect = host.getBoundingClientRect()
      const ratio = Math.round(((ev.clientX - rect.left) / Math.max(1, rect.width)) * 100)
      latest = Math.min(80, Math.max(20, ratio))
      setSplitRatio(latest)
    }
    const onUp = () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
      try {
        window.localStorage.setItem(MD_SPLIT_KEY, String(latest))
      } catch {
        /* ignore */
      }
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
  }

  const changeMdView = (mode: MdViewMode) => {
    setMdView(mode)
    saveMdViewMode(mode)
    syncLockRef.current = false
    monacoRef.current = null
  }

  /** 分栏同步滚动：源侧 → 预览侧（按滚动百分比互推；syncLock 抑制回环）。 */
  const syncScrollToPreview = () => {
    const ed = monacoRef.current
    const pv = previewScrollRef.current
    if (!ed || !pv || syncLockRef.current) return
    const edMax = Math.max(1, ed.getScrollHeight() - ed.getLayoutInfo().height)
    const ratio = Math.min(1, ed.getScrollTop() / edMax)
    syncLockRef.current = true
    pv.scrollTop = ratio * Math.max(1, pv.scrollHeight - pv.clientHeight)
    window.requestAnimationFrame(() => {
      syncLockRef.current = false
    })
  }

  const syncScrollToEditor = () => {
    const ed = monacoRef.current
    const pv = previewScrollRef.current
    if (!ed || !pv || syncLockRef.current) return
    const pvMax = Math.max(1, pv.scrollHeight - pv.clientHeight)
    const ratio = Math.min(1, pv.scrollTop / pvMax)
    syncLockRef.current = true
    ed.setScrollTop(ratio * Math.max(1, ed.getScrollHeight() - ed.getLayoutInfo().height))
    window.requestAnimationFrame(() => {
      syncLockRef.current = false
    })
  }

  useEffect(() => {
    let alive = true
    setLoading(true)
    setError('')
    void Promise.all([getFileMeta(fileId), fetchFileText(fileId)])
      .then(([meta, content]) => {
        if (!alive) return
        setName(meta.name)
        setText(content)
        dirtyRef.current = false
        setDirty(false)
      })
      .catch((err) => {
        if (alive) setError(err instanceof Error ? err.message : msg('loadFailed'))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])

  const save = useCallback(async () => {
    if (saving) return
    setSaving(true)
    setError('')
    setNotice('')
    try {
      await uploadFileVersion(new File([text], name, { type: mimeTypes[kind] }), fileId, () => {})
      dirtyRef.current = false
      setDirty(false)
      setNotice(msg('saved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saving, text, name, fileId, kind])

  // Ctrl/Cmd+S 保存（编辑态）：拦截浏览器默认「保存网页」。
  useEffect(() => {
    if (viewMode) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        void save()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [save, viewMode])

  // 返回（退出）：与其他编辑页一致的未保存二次确认 + beforeunload 兜底。
  const exitWithConfirm = () => {
    if (!dirtyRef.current) {
      closeEditorWithFallback(navigate, returnTo)
      return
    }
    antdModal.confirm({
      title: locale === 'zh-CN' ? '有未保存的修改' : 'Unsaved changes',
      content: locale === 'zh-CN'
        ? '文档存在尚未保存的修改，直接退出可能丢失。仍要退出吗？'
        : 'The document has unsaved changes. Exit anyway?',
      okText: locale === 'zh-CN' ? '仍然退出' : 'Exit anyway',
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '继续编辑' : 'Keep editing',
      onOk: () => {
        dirtyRef.current = false
        closeEditorWithFallback(navigate, returnTo)
      },
    })
  }

  useEffect(() => {
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [])

  if (viewMode) return (
    <main className="text-editor-page viewer-only">
      {loading ? <div className="text-editor-state">{msg('loading')}</div> : error ? (
        <div className="banner error">{error}</div>
      ) : <SourceViewer kind={editorKind} source={text} name={name} />}
    </main>
  )

  const isMarkdown = editorKind === 'markdown'
  const canPreview = isMarkdown || editorKind === 'html'
  return (
    <main className="text-editor-page">
      <header className="text-editor-head">
        <div className="text-editor-head-title">
          <div className="text-editor-back">
            <Button type="text" size="small" onClick={exitWithConfirm}>{msg('back')}</Button>
          </div>
          <div>
            <h1>{isMarkdown ? msg('markdownEditor') : `${name.split('.').pop()?.toUpperCase() ?? '文本'} 编辑器`}</h1>
            <div className="muted">
              {name}
              {dirty && <span className="badge uploading text-editor-dirty-badge">{locale === 'zh-CN' ? '未保存' : 'Unsaved'}</span>}
            </div>
          </div>
        </div>
        <div className="editor-head-actions">
          {/* markdown：视图切换（编辑/分栏/预览，默认分栏，localStorage 记住）。 */}
          {isMarkdown && (
            <Segmented
              value={mdView}
              onChange={(v) => changeMdView(v as MdViewMode)}
              options={[
                { label: locale === 'zh-CN' ? '编辑' : 'Edit', value: 'edit' },
                { label: locale === 'zh-CN' ? '分栏' : 'Split', value: 'split' },
                { label: locale === 'zh-CN' ? '预览' : 'Preview', value: 'preview' },
              ]}
            />
          )}
          {canPreview && !isMarkdown && (
            <Button size="small" onClick={() => setPreview((value) => !value)}>
              {preview ? msg('editMode') : msg('previewMode')}
            </Button>
          )}
          <Button type="primary" size="small" disabled={loading || saving} loading={saving} onClick={() => void save()}>
            {msg('save')}
          </Button>
        </div>
      </header>
      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading ? (
        <div className="text-editor-state">{msg('loading')}</div>
      ) : isMarkdown ? (
        mdView === 'preview' ? (
          <div className="md-preview-pane" ref={previewScrollRef}>
            <MarkdownViewer source={text} />
          </div>
        ) : mdView === 'split' ? (
          <div className="md-split md-split-draggable" ref={splitHostRef} style={{ gridTemplateColumns: `minmax(0, ${splitRatio}fr) 6px minmax(0, ${100 - splitRatio}fr)` }}>
            {/* 分栏：左源码（Monaco）右预览，百分比同步滚动；中间分隔条可拖拽
                调整比例（20-80%，localStorage 记住）。 */}
            <div className="md-split-editor">
              <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
                <MonacoEditor
                  language={monacoLanguages.markdown}
                  theme={monacoTheme(dark)}
                  value={text}
                  options={monacoOptions(false)}
                  onMount={(editor) => {
                    monacoRef.current = editor as unknown as typeof monacoRef.current
                    editor.onDidScrollChange(() => syncScrollToPreview())
                  }}
                  onChange={(value) => {
                    setText(value ?? '')
                    markDirty()
                  }}
                />
              </Suspense>
            </div>
            <div
              className="md-split-divider"
              role="separator"
              aria-orientation="vertical"
              title={locale === 'zh-CN' ? '拖拽调整分栏比例' : 'Drag to resize'}
              onMouseDown={startSplitDrag}
            />
            <div className="md-split-preview" ref={previewScrollRef} onScroll={syncScrollToEditor}>
              <MarkdownViewer source={text} />
            </div>
          </div>
        ) : (
          <div className="code-editor">
            <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
              <MonacoEditor
                language={monacoLanguages.markdown}
                theme={monacoTheme(dark)}
                value={text}
                options={monacoOptions(false)}
                onChange={(value) => {
                  setText(value ?? '')
                  markDirty()
                }}
              />
            </Suspense>
          </div>
        )
      ) : preview && canPreview ? (
        <SourceViewer kind={editorKind} source={text} name={name} />
      ) : (
        <div className="code-editor">
          <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
            <MonacoEditor
              language={monacoLanguages[editorKind]}
              theme={monacoTheme(dark)}
              value={text}
              options={monacoOptions(false)}
              onChange={(value) => {
                setText(value ?? '')
                markDirty()
              }}
            />
          </Suspense>
        </div>
      )}
    </main>
  )
}
