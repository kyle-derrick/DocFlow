import { Suspense, lazy, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { isValidElement } from 'react'
import ReactMarkdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import MarkmapDiagram from '../components/MarkmapDiagram'
import MermaidDiagram from '../components/MermaidDiagram'
import { MessageKey, t, useLocale } from '../i18n'
import { useColorMode } from '../theme'

// 富文本编辑器（Tiptap + lowlight 产物 1MB+）懒加载独立 chunk，仅 markdown
// 编辑态进入时按需拉取。
const RichTextEditor = lazy(() => import('../components/richtext/RichTextEditor'))

// Monaco（VSCode）编辑器：本地打包 + 按语言 worker（产物 3MB+ 独立 chunk，
// 仅编辑态进入时拉取；查看态一律 <pre> 纯渲染，不加载 Monaco）。
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

function MarkdownViewer({ source }: { source: string }) {
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
  // markdown 富文本/源码模式（默认富文本；往返失败自动降级源码并提示）。
  const [richMode, setRichMode] = useState(true)
  const [richKey, setRichKey] = useState(0)

  useEffect(() => {
    let alive = true
    setLoading(true)
    setError('')
    // 换文件时 markdown 回到默认富文本模式。
    setRichMode(true)
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

  const canPreview = kind === 'markdown' || kind === 'html'
  const isRich = editorKind === 'markdown' && richMode
  return (
    <main className="text-editor-page">
      <header className="text-editor-head">
        <div>
          <h1>{editorKind === 'markdown' ? msg('markdownEditor') : `${name.split('.').pop()?.toUpperCase() ?? '文本'} 编辑器`}</h1>
          <div className="muted">{name}</div>
        </div>
        <div className="editor-head-actions">
          {editorKind === 'markdown' && (
            <button
              type="button"
              className="btn"
              onClick={() => {
                setRichMode((value) => !value)
                setRichKey((key) => key + 1)
                setNotice('')
              }}
            >
              {richMode
                ? (locale === 'zh-CN' ? '源码' : 'Source')
                : (locale === 'zh-CN' ? '富文本' : 'Rich text')}
            </button>
          )}
          {canPreview && (
            <button type="button" className="btn" onClick={() => setPreview((value) => !value)}>
              {preview ? msg('editMode') : msg('previewMode')}
            </button>
          )}
          <button type="button" className="btn primary" disabled={loading || saving} onClick={() => void save()}>
            {saving ? msg('saving') : msg('save')}
          </button>
        </div>
      </header>
      {error && <div className="banner error">{error}</div>}
      {notice && <div className="banner ok">{notice}</div>}
      {loading ? (
        <div className="text-editor-state">{msg('loading')}</div>
      ) : preview && canPreview ? (
        <SourceViewer kind={editorKind} source={text} name={name} />
      ) : isRich ? (
        <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
          <RichTextEditor
            key={`${fileId}-${richKey}`}
            initialMarkdown={text}
            fileId={fileId}
            onChange={(md) => {
              setText(md)
              setNotice('')
              dirtyRef.current = true
            }}
            onRoundtripFail={() => {
              setRichMode(false)
              setNotice(locale === 'zh-CN'
                ? '该文档包含富文本无法无损承载的内容，已切换为源码模式'
                : 'This document cannot be losslessly represented in rich text, switched to source mode')
            }}
          />
        </Suspense>
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
