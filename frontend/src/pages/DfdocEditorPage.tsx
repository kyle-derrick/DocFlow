// 富文本文档页（.dfrt 主后缀 / .dfdoc 兼容别名；路由 /dfdoc/:fileId）：
// DocFlow 专属富文本格式，Tiptap JSON 存储（见 RichTextEditor）——编辑态
// 加载 JSON 进 Tiptap，保存整篇回写新版本；查看态 readonly 渲染同一编辑器
//（嵌入块内联渲染 drawio/白板/图片等）。by-path 路由经 prop 传入 file_id。
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { fetchFileText, getFileMeta, listDocumentComments, uploadFileVersion } from '../api'
import type { DocumentComment } from '../api'
import type { AIEditTarget } from '../components/AIEdit'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIQuickCommand } from '../components/AIEditChat'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'
import { MessageKey, t, useLocale } from '../i18n'
import type { Editor as TiptapEditor } from '@tiptap/react'
import type { JSONContent } from '@tiptap/core'
import type { CollabSnapshot } from '../components/richtext/CollabSession'

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
  const [versionId, setVersionId] = useState('')
  const [comments, setComments] = useState<DocumentComment[]>([])
  const [commentError, setCommentError] = useState('')
  const refreshComments = useCallback(async () => {
    try { setComments(await listDocumentComments(fileId)); setCommentError('') }
    catch (err) { setCommentError(err instanceof Error ? err.message : msg('loadFailed')) }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [dirty, setDirty] = useState(false)
  const dirtyRef = useRef(false)
  // 多人实时协作快照（collab WS 状态/成员/leader；null=未启用或已卸载）。
  const [collabState, setCollabState] = useState<CollabSnapshot | null>(null)
  const collabLeader = !!collabState
    && collabState.status === 'connected'
    && !!collabState.selfConnId
    && collabState.leaderConnId === collabState.selfConnId

  useEffect(() => {
    let alive = true
    setLoading(true)
    setError('')
    void Promise.all([getFileMeta(fileId), fetchFileText(fileId)])
      .then(([meta, content]) => {
        if (!alive) return
        setName(meta.name)
        setVersionId(meta.current_version?.id ?? '')
        setDoc(content)
        void refreshComments()
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
      const meta = await getFileMeta(fileId)
      setVersionId(meta.current_version?.id ?? '')
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

  // ---- 协作 leader 自动保存：本端为 leader（participants 含 leader 标记且为
  // 自己 connId）时每 3 秒检查一次，dirty 即调既有 save()；非 leader 保留
  // 手动保存按钮（协作中他人编辑经 onChange 全量回吐同样置 dirty）。----
  const saveRef = useRef(save)
  saveRef.current = save
  useEffect(() => {
    if (viewMode || !collabLeader) return
    const timer = window.setInterval(() => {
      if (dirtyRef.current) void saveRef.current()
    }, 3000)
    return () => window.clearInterval(timer)
  }, [viewMode, collabLeader])

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

  // ---- 编辑器 AI（原 AIEditMenu 快捷指令已并入「AI 对话」下拉按钮） ----
  const tiptapRef = useRef<TiptapEditor | null>(null)
  // AI 对话面板（AIEditChat）展开态（收起不清空会话）。
  const [aiChatOpen, setAiChatOpen] = useState(false)
  // 头部下拉快捷指令（打开面板后由 AIEditChat 消费一次）。
  const [aiQuick, setAiQuick] = useState<AIQuickCommand | null>(null)
  // 撤销回退后的编辑器重挂 key（Tiptap initialJSON 仅挂载时生效）。
  const [editorReloadKey, setEditorReloadKey] = useState(0)

  /** 选区读取：Tiptap state.selection（空选区回退全文 getText）。 */
  const aiGetTarget = (): AIEditTarget => {
    const ed = tiptapRef.current
    if (!ed) return { text: '', hasSelection: false }
    const { from, to } = ed.state.selection
    if (to > from) {
      return { text: ed.state.doc.textBetween(from, to, '\n'), hasSelection: true }
    }
    return { text: ed.getText(), hasSelection: false }
  }

  /** 结果落盘：insert = 选区末尾/光标处插入段落文本；replace = 替换选区
   *（无选区时追加到文末——富文本整文替换语义过强，避免误毁全文）。
   * output 为纯文本/markdown 字符串，或段落节点数组（AI 对话面板按换行
   * 拆段落传入，insertContentAt 接受 JSONContent[]）。 */
  const aiApply = (mode: 'insert' | 'replace', output: string | JSONContent[]) => {
    const ed = tiptapRef.current
    if (!ed) return
    const { from, to } = ed.state.selection
    const hasSelection = to > from
    if (mode === 'replace' && hasSelection) {
      ed.chain().focus().insertContentAt({ from, to }, output).run()
    } else if (hasSelection) {
      ed.chain().focus().insertContentAt(to, output).run()
    } else if (mode === 'insert') {
      ed.chain().focus().insertContentAt(from, output).run()
    } else {
      ed.chain().focus().insertContentAt(ed.state.doc.content.size, output).run()
    }
  }

  /** AI 对话面板落盘：面板输出为纯文本/markdown——按换行拆段落（空行→
   * 空段落，避免多行文本被 HTML 解析折叠成一行）后复用 aiApply 落盘。 */
  const aiChatApply = (mode: 'insert' | 'replace', output: string) => {
    const paragraphs: JSONContent[] = output
      .replace(/\r\n/g, '\n')
      .split('\n')
      .map((line) => (line ? { type: 'paragraph', content: [{ type: 'text', text: line }] } : { type: 'paragraph' }))
    aiApply(mode, paragraphs)
  }

  /** AI 自动应用·无选区：追加到文档末尾（aiApply replace 无选区即文末追加）。 */
  const aiChatAppendEnd = (output: string) => aiChatApply('replace', output)

  /** 版本保护前置：确保当前内容已保存（有未保存修改先 save），返回应用前
   * 版本信息（null=保存失败，AIEditChat 将放弃自动应用）。 */
  const aiEnsureSaved = useCallback(async (): Promise<{ versionId: string; version: number } | null> => {
    try {
      if (dirtyRef.current) {
        await save()
        // save() 内部捕获错误不抛出：dirtyRef 仍为 true 即保存失败。
        if (dirtyRef.current) return null
      }
      const meta = await getFileMeta(fileId)
      const cur = meta.current_version
      if (!cur) return null
      setVersionId(cur.id)
      return { versionId: cur.id, version: cur.version }
    } catch {
      return null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, save])

  /** 撤销回退后刷新编辑器：重新拉取最新内容并重挂 Tiptap（initialJSON 仅
   * 挂载时生效；协作连接随重挂重建）。 */
  const aiReload = useCallback(async () => {
    const [meta, content] = await Promise.all([getFileMeta(fileId), fetchFileText(fileId)])
    setName(meta.name)
    setVersionId(meta.current_version?.id ?? '')
    setDoc(content)
    dirtyRef.current = false
    setDirty(false)
    setNotice('')
    setEditorReloadKey((k) => k + 1)
    void refreshComments()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])

  /** 头部下拉快捷指令：打开面板并透传给 AIEditChat 自动执行。 */
  const openAiChatWith = (cmd: AIQuickCommand) => {
    setAiChatOpen(true)
    setAiQuick(cmd)
  }

  if (loading) return <main className="text-editor-page"><div className="text-editor-state">{msg('loading')}</div></main>
  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>

  if (viewMode) {
    return (
      <main className="text-editor-page viewer-only">
        <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
          <RichTextEditor key={fileId} initialJSON={doc} fileId={fileId} comments={comments} readonly />
        </Suspense>
      </main>
    )
  }

  return (
    <main className="text-editor-page">
      {/* 行布局：主列（头部/横幅/编辑器）+ 右侧 AI 对话面板（可收起，
          不破坏编辑区 flex 高度链；AI 未启用时面板不渲染）。 */}
      <div className="text-editor-body">
        <div className="text-editor-main">
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
                  {collabLeader && dirty && <span className="badge uploading text-editor-dirty-badge">{locale === 'zh-CN' ? '协作自动保存' : 'Autosaving'}</span>}
                </div>
              </div>
            </div>
            <div className="editor-head-actions">
              {/* AI 对话（主点击开面板；下拉=原 AIEditMenu 并入的快捷指令：
                  摘要/续写/润色/翻译成英文/自定义，打开面板自动发送）。 */}
              <AIEditChatButton open={aiChatOpen} onToggle={() => setAiChatOpen((v) => !v)} onQuick={openAiChatWith} disabled={saving} />
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
          {commentError && <div className="banner error">{commentError}</div>}
          {/* 协作提示：error 信封 / 重连超限回退单机（编辑器仍可用）/ 版本漂移建议刷新。 */}
          {collabState?.message && <div className="banner warn">{collabState.message}</div>}
          <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
            <RichTextEditor
              key={`${fileId}-${editorReloadKey}`}
              initialJSON={doc}
              fileId={fileId}
              versionId={versionId}
              comments={comments}
              collab
              onCollabState={setCollabState}
              onCommentsChange={() => void refreshComments()}
              onCommentError={setCommentError}
              onEditor={(ed) => { tiptapRef.current = ed }}
              onChange={(json) => {
                setDoc(json)
                setNotice('')
                dirtyRef.current = true
                setDirty(true)
              }}
            />
          </Suspense>
        </div>
        {/* AI 对话式创作/编辑侧栏面板（富文本输出按换行拆段落落盘；可修改
            模式自动应用前经 aiEnsureSaved 保存基线版本，撤销回退后 reload）。 */}
        <AIEditChat
          open={aiChatOpen}
          onClose={() => setAiChatOpen(false)}
          getTarget={aiGetTarget}
          getAllText={() => aiGetTarget().text}
          onApply={aiChatApply}
          onAppend={aiChatAppendEnd}
          fileId={fileId}
          ensureSaved={aiEnsureSaved}
          reload={aiReload}
          quickCommand={aiQuick}
          onQuickConsumed={() => setAiQuick(null)}
        />
      </div>
    </main>
  )
}
