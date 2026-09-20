import { Suspense, lazy, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { isValidElement } from 'react'
import { Button, Segmented } from 'antd'
import ReactMarkdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import MarkmapDiagram from '../components/MarkmapDiagram'
import MermaidDiagram from '../components/MermaidDiagram'
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
  const dirtyRef = useRef(false)
  // markdown 视图模式（编辑/分栏/预览，默认分栏，localStorage 记住）。
  const [mdView, setMdView] = useState<MdViewMode>(loadMdViewMode)
  // 分栏同步滚动：Monaco 实例与预览容器百分比互推（lock 防回环）。
  const monacoRef = useRef<{ getScrollTop(): number; setScrollTop(v: number): void; getScrollHeight(): number; getHeight(): number } | null>(null)
  const previewScrollRef = useRef<HTMLDivElement | null>(null)
  const syncLockRef = useRef(false)

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
    const edMax = Math.max(1, ed.getScrollHeight() - ed.getHeight())
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
    ed.setScrollTop(ratio * Math.max(1, ed.getScrollHeight() - ed.getHeight()))
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

  useEffect(() => {
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [])

  const save = async () => {
    if (saving) return
    setSaving(true)
    setError('')
    setNotice('')
    try {
      await uploadFileVersion(new File([text], name, { type: mimeTypes[kind] }), fileId, () => {})
      dirtyRef.current = false
      setNotice(msg('saved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
  }

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
        <div>
          <h1>{isMarkdown ? msg('markdownEditor') : `${name.split('.').pop()?.toUpperCase() ?? '文本'} 编辑器`}</h1>
          <div className="muted">{name}</div>
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
          <div className="md-split">
            {/* 分栏：左源码（Monaco）右预览，百分比同步滚动。 */}
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
                    setNotice('')
                    dirtyRef.current = true
                  }}
                />
              </Suspense>
            </div>
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
                  setNotice('')
                  dirtyRef.current = true
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
                setNotice('')
                dirtyRef.current = true
              }}
            />
          </Suspense>
        </div>
      )}
    </main>
  )
}
