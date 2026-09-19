// 富文本文档页（.dfrt 主后缀 / .dfdoc 兼容别名；路由 /dfdoc/:fileId）：
// DocFlow 专属富文本格式，Tiptap JSON 存储（见 RichTextEditor）——编辑态
// 加载 JSON 进 Tiptap，保存整篇回写新版本；查看态 readonly 渲染同一编辑器
//（嵌入块内联渲染 drawio/白板/图片等）。by-path 路由经 prop 传入 file_id。
import { Suspense, lazy, useEffect, useRef, useState } from 'react'
import { Button } from 'antd'
import { useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import { MessageKey, t, useLocale } from '../i18n'

// 富文本编辑器（Tiptap + lowlight 产物 1MB+）懒加载独立 chunk。
const RichTextEditor = lazy(() => import('../components/richtext/RichTextEditor'))

export default function DfdocEditorPage({
  mode,
  fileId: fileIdProp,
}: { mode?: 'edit' | 'view'; fileId?: string }) {
  const { fileId: routeFileId = '' } = useParams()
  const fileId = fileIdProp ?? routeFileId
  const [searchParams] = useSearchParams()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [name, setName] = useState('document.dfrt')
  const [doc, setDoc] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
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
        setDoc(content)
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
      await uploadFileVersion(new File([doc], name, { type: 'application/json' }), fileId, () => {})
      dirtyRef.current = false
      setNotice(msg('saved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  if (loading) return <main className="text-editor-page"><div className="text-editor-state">{msg('loading')}</div></main>
  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>

  if (viewMode) {
    return (
      <main className="text-editor-page viewer-only">
        <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
          <RichTextEditor key={fileId} initialJSON={doc} fileId={fileId} readonly />
        </Suspense>
      </main>
    )
  }

  return (
    <main className="text-editor-page">
      <header className="text-editor-head">
        <div>
          <h1>{locale === 'zh-CN' ? '富文本文档' : 'Rich text document'}</h1>
          <div className="muted">{name}</div>
        </div>
        <div className="editor-head-actions">
          <Button
            type="primary"
            size="small"
            disabled={saving}
            loading={saving}
            onClick={() => void save()}
          >
            {msg('save')}
          </Button>
        </div>
      </header>
      {notice && <div className="banner ok">{notice}</div>}
      <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
        <RichTextEditor
          key={fileId}
          initialJSON={doc}
          fileId={fileId}
          onChange={(json) => {
            setDoc(json)
            setNotice('')
            dirtyRef.current = true
          }}
        />
      </Suspense>
    </main>
  )
}
