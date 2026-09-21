// 富文本文档页（.dfrt 主后缀 / .dfdoc 兼容别名；路由 /dfdoc/:fileId）：
// DocFlow 专属富文本格式，Tiptap JSON 存储（见 RichTextEditor）——编辑态
// 加载 JSON 进 Tiptap，保存整篇回写新版本；查看态 readonly 渲染同一编辑器
//（嵌入块内联渲染 drawio/白板/图片等）。by-path 路由经 prop 传入 file_id。
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'
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
  const navigate = useNavigate()
  const returnTo = safeReturnTo(searchParams.get('returnTo'))
  const { modal: antdModal } = AntdApp.useApp()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [name, setName] = useState('document.dfrt')
  const [doc, setDoc] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [dirty, setDirty] = useState(false)
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
      await uploadFileVersion(new File([doc], name, { type: 'application/json' }), fileId, () => {})
      dirtyRef.current = false
      setDirty(false)
      setNotice(msg('saved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saving, doc, name, fileId])

  // Ctrl/Cmd+S 保存（编辑态）。
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

  // 返回（退出）：与其他编辑页一致的未保存二次确认。
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
        <div className="text-editor-head-title">
          <div className="text-editor-back">
            <Button type="text" size="small" onClick={exitWithConfirm}>{msg('back')}</Button>
          </div>
          <div>
            <h1>{locale === 'zh-CN' ? '富文本文档' : 'Rich text document'}</h1>
            <div className="muted">
              {name}
              {dirty && <span className="badge uploading text-editor-dirty-badge">{locale === 'zh-CN' ? '未保存' : 'Unsaved'}</span>}
            </div>
          </div>
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
            setDirty(true)
          }}
        />
      </Suspense>
    </main>
  )
}
