import { Fragment, ReactNode, useEffect, useRef, useState } from 'react'
import { useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import { MessageKey, t, useLocale } from '../i18n'

type EditorKind = 'text' | 'markdown'

function inlineMarkdown(text: string): ReactNode[] {
  const parts = text.split(/(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\(https?:\/\/[^\s)]+\))/g)
  return parts.map((part, index) => {
    if (part.startsWith('`') && part.endsWith('`')) return <code key={index}>{part.slice(1, -1)}</code>
    if (part.startsWith('**') && part.endsWith('**')) return <strong key={index}>{part.slice(2, -2)}</strong>
    const link = part.match(/^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/)
    if (link) return <a key={index} href={link[2]} target="_blank" rel="noreferrer">{link[1]}</a>
    return <Fragment key={index}>{part}</Fragment>
  })
}

function MarkdownPreview({ source }: { source: string }) {
  const lines = source.replace(/\r\n?/g, '\n').split('\n')
  const nodes: ReactNode[] = []
  let code: string[] | null = null
  let list: string[] = []
  const flushList = () => {
    if (list.length === 0) return
    nodes.push(<ul key={`list-${nodes.length}`}>{list.map((item, i) => <li key={i}>{inlineMarkdown(item)}</li>)}</ul>)
    list = []
  }
  lines.forEach((line) => {
    if (line.startsWith('```')) {
      flushList()
      if (code === null) code = []
      else {
        nodes.push(<pre key={`code-${nodes.length}`}><code>{code.join('\n')}</code></pre>)
        code = null
      }
      return
    }
    if (code !== null) {
      code.push(line)
      return
    }
    const item = line.match(/^[-*]\s+(.+)$/)
    if (item) {
      list.push(item[1])
      return
    }
    flushList()
    const heading = line.match(/^(#{1,6})\s+(.+)$/)
    if (heading) {
      const children = inlineMarkdown(heading[2])
      const level = heading[1].length
      if (level === 1) nodes.push(<h1 key={nodes.length}>{children}</h1>)
      else if (level === 2) nodes.push(<h2 key={nodes.length}>{children}</h2>)
      else if (level === 3) nodes.push(<h3 key={nodes.length}>{children}</h3>)
      else nodes.push(<h4 key={nodes.length}>{children}</h4>)
    } else if (line.startsWith('> ')) {
      nodes.push(<blockquote key={nodes.length}>{inlineMarkdown(line.slice(2))}</blockquote>)
    } else if (line.trim()) {
      nodes.push(<p key={nodes.length}>{inlineMarkdown(line)}</p>)
    } else {
      nodes.push(<br key={nodes.length} />)
    }
  })
  flushList()
  const trailingCode = code as string[] | null
  if (trailingCode !== null) nodes.push(<pre key={`code-${nodes.length}`}><code>{trailingCode.join('\n')}</code></pre>)
  return <div className="markdown-preview">{nodes}</div>
}

export default function TextEditorPage({ kind }: { kind: EditorKind }) {
  const { fileId = '' } = useParams()
  const [searchParams] = useSearchParams()
  const viewMode = searchParams.get('mode') === 'view'
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const markdown = kind === 'markdown'
  const [name, setName] = useState(markdown ? 'note.md' : 'text.txt')
  const [text, setText] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [preview, setPreview] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const dirtyRef = useRef(false)

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
      const file = new File([text], name, { type: markdown ? 'text/markdown' : 'text/plain' })
      await uploadFileVersion(file, fileId, () => {})
      dirtyRef.current = false
      setNotice(msg('saved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  if (viewMode) return (
    <main className="text-editor-page">
      {loading ? <div className="text-editor-state">{msg('loading')}</div> : error ? (
        <div className="banner error">{error}</div>
      ) : markdown ? <MarkdownPreview source={text} /> : <pre className="preview-text">{text}</pre>}
    </main>
  )

  return (
    <main className="text-editor-page">
      <header className="text-editor-head">
        <div>
          <h1>{markdown ? msg('markdownEditor') : msg('textEditor')}</h1>
          <div className="muted">{name}</div>
        </div>
        <div className="editor-head-actions">
          {markdown && (
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
      ) : preview && markdown ? (
        <MarkdownPreview source={text} />
      ) : (
        <textarea
          className="text-editor-input"
          aria-label={markdown ? msg('markdownEditor') : msg('textEditor')}
          value={text}
          spellCheck
          onChange={(event) => {
            setText(event.target.value)
            setNotice('')
            dirtyRef.current = true
          }}
        />
      )}
    </main>
  )
}
