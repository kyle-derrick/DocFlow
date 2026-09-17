// Excalidraw 白板编辑页（/excalidraw/:fileId）：
// - @excalidraw/excalidraw 经 React.lazy 动态加载（产物 1MB+，拆出独立
//   chunk 避免首包膨胀；包入口按 process.env 分发构建产物，见 vite.config
//   的 define 注释）；
// - 文件内容经 fetchFileText 读取（.excalidraw 即 Excalidraw JSON scene），
//   空/损坏内容回退空场景（剥离存档 theme，主题以 <html data-mode> 为准
//   并经 MutationObserver 实时跟随设置/系统明暗变化）；
// - onChange 只把最新 scene 写 ref（编辑器高频触发，避免逐笔重渲染），
//   「保存」用包导出的 serializeAsJSON 序列化后 uploadFileVersion 覆盖为
//   新版本，成功 banner 提示；「保存并返回」保存成功后统一 closeEditor()
//   （先 window.close()，未关则按已校验 returnTo / 历史 / '/' 回退，见
//   editorNavigation），失败留在页面显示错误；
// - 初始挂载 onChange 以首个序列化 JSON 为基线，后续对比相同不置脏。
import { Suspense, lazy, useEffect, useRef, useState } from 'react'
import type { ComponentProps } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { FileWithVersion, fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import ExcalidrawViewer from '../components/ExcalidrawViewer'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'
import { useColorMode } from '../theme'

// 懒加载编辑器组件：包入口为 CJS（Vite 构建期 interop），动态 import 使
// 其独立成 chunk，仅在进入本页时加载。
const Excalidraw = lazy(() =>
  import('@excalidraw/excalidraw').then((mod) => ({ default: mod.Excalidraw })),
)

// 从组件 props 推导 onChange 参数类型（包根入口未导出这些类型，避免
// 依赖包内部深层类型路径）。
type ExcalidrawChange = NonNullable<ComponentProps<typeof Excalidraw>['onChange']>
type SceneElements = Parameters<ExcalidrawChange>[0]
type SceneAppState = Parameters<ExcalidrawChange>[1]
type SceneFiles = Parameters<ExcalidrawChange>[2]

/** 编辑器内最新场景快照（onChange 更新，保存时序列化）。 */
interface SceneSnapshot {
  elements: SceneElements
  appState: Partial<SceneAppState>
  files: SceneFiles
}

/** 解析 .excalidraw JSON 场景；空文本/损坏 JSON/非数组 elements 回退空场景。 */
function parseScene(text: string): SceneSnapshot {
  const trimmed = text.trim()
  if (trimmed) {
    try {
      const parsed = JSON.parse(trimmed) as { elements?: unknown; appState?: unknown; files?: unknown }
      if (Array.isArray(parsed.elements)) {
        const appState: Partial<SceneAppState> =
          parsed.appState && typeof parsed.appState === 'object'
            ? { ...(parsed.appState as Partial<SceneAppState>) }
            : {}
        // 剥离存档中的主题：以页面 data-mode（theme prop）为准。
        delete appState.theme
        const files = parsed.files && typeof parsed.files === 'object' ? parsed.files as SceneFiles : {}
        return { elements: parsed.elements as SceneElements, appState, files }
      }
    } catch {
      // 损坏内容：回退空场景
    }
  }
  return { elements: [], appState: {}, files: {} }
}

export default function ExcalidrawPage({
  mode: routeMode,
  fileId: fileIdProp,
}: { mode?: 'edit' | 'view'; fileId?: string } = {}) {
  const { fileId: routeFileId = '' } = useParams()
  // by-path 路由经 prop 传入 resolve 得到的 file_id；缺省回退路由参数。
  const fileId = fileIdProp ?? routeFileId
  // 独立 /view 路由或 ?mode=view 均强制只读：viewModeEnabled + 隐藏保存入口。
  const [searchParams] = useSearchParams()
  const viewMode = routeMode === 'view' || searchParams.get('mode') === 'view'
  const returnTo = safeReturnTo(searchParams.get('returnTo'))
  const locale = useLocale()
  const navigate = useNavigate()
  const msg = (key: MessageKey) => t(locale, key)
  const mode = useColorMode()

  const closeEditor = () => closeEditorWithFallback(navigate, returnTo)

  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [initial, setInitial] = useState<SceneSnapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [saving, setSaving] = useState(false)

  const sceneRef = useRef<SceneSnapshot | null>(null)
  const dirtyRef = useRef(false)
  // 初始场景基线（编辑器挂载后首个 onChange 的序列化 JSON）：后续每次
  // onChange 与之对比，相同则不置脏——挂载期编辑器可能多次同步触发
  // onChange（字体加载等），仅凭「首次回调」标记会误判为已修改。
  const initialJSONRef = useRef<string | null>(null)

  // 拉取元数据与内容：内容读取失败/为空（404、无版本、空文件）回退空场景，
  // 不阻塞编辑；initialData 仅装载时生效，就绪后再挂编辑器。
  useEffect(() => {
    let alive = true
    const init = async () => {
      setLoading(true)
      setError('')
      setNotice('')
      setInitial(null)
      sceneRef.current = null
      dirtyRef.current = false
      initialJSONRef.current = null
      try {
        const [meta, text] = await Promise.all([
          getFileMeta(fileId).catch(() => null),
          fetchFileText(fileId).catch(() => ''),
        ])
        if (!alive) return
        setFile(meta)
        const scene = parseScene(text)
        sceneRef.current = scene
        setInitial(scene)
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : msg('loadFailed'))
      } finally {
        if (alive) setLoading(false)
      }
    }
    void init()
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])

  // onChange：仅写 ref（高频触发，不 setState 避免逐笔重渲染）；脏标记按
  // 序列化 JSON 与初始基线对比（serializeAsJSON 只落持久化字段，选中态、
  // 滚动位置等运行时 appState 不参与，纯选中/滚动不会误置脏）。
  const handleChange: ExcalidrawChange = (elements, appState, files) => {
    sceneRef.current = { elements, appState, files }
    void import('@excalidraw/excalidraw')
      .then(({ serializeAsJSON }) => serializeAsJSON(elements, appState, files, 'local'))
      .then((json) => {
        if (initialJSONRef.current === null) initialJSONRef.current = json
        else dirtyRef.current = json !== initialJSONRef.current
      })
      .catch(() => {
        // 序列化失败保守视为已修改（保存兜底提示优于漏提示）
        dirtyRef.current = true
      })
  }

  // 保存：serializeAsJSON 序列化（复用 React.lazy 已加载的同一 chunk）→
  // uploadFileVersion 覆盖为新版本；成功后刷新元数据展示新版本号。
  const save = async (thenBack: boolean) => {
    const scene = sceneRef.current
    if (!scene || saving) return
    setSaving(true)
    setError('')
    setNotice('')
    try {
      const { serializeAsJSON } = await import('@excalidraw/excalidraw')
      const json = serializeAsJSON(scene.elements, scene.appState, scene.files, 'local')
      const blob = new File([json], file?.name ?? 'whiteboard.excalidraw', { type: 'application/json' })
      await uploadFileVersion(blob, fileId, () => {})
      dirtyRef.current = false
      setNotice(msg('whiteboardSaved'))
      try {
        setFile(await getFileMeta(fileId))
      } catch {
        // 元数据刷新失败不影响保存结果展示
      }
      if (thenBack) closeEditor()
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
  }

  // 未保存兜底提示：有改动且尚未成功保存前拦截误关。
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [])

  const versionNo = file?.current_version?.version

  return (
    <div className={`editor-page${viewMode ? ' viewer-only' : ''}`}>
      {!viewMode && <div className="editor-head">
        <button type="button" className="btn ghost small" onClick={closeEditor}>{msg('back')}</button>
        <h2 className="editor-title">{file?.name ?? msg('loading')}</h2>
        {versionNo !== undefined && (
          <span className="badge current">{formatMessage(msg('currentVersion'), { n: versionNo })}</span>
        )}
        {saving && <span className="badge uploading">{msg('saving')}</span>}
        <span className="editor-head-actions">
            <button className="btn" disabled={saving || !initial} onClick={() => void save(false)}>
              {saving ? msg('saving') : msg('save')}
            </button>
            <button className="btn primary" disabled={saving || !initial} onClick={() => void save(true)}>
              {msg('saveAndBack')}
            </button>
          </span>
      </div>}

      {!viewMode && notice && <div className="banner ok editor-hint">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="hint">{viewMode ? '正在加载白板查看器…' : msg('whiteboardLoading')}</div>}

      {!error && !loading && initial && (viewMode ? (
        <ExcalidrawViewer
          elements={initial.elements}
          appState={{ ...initial.appState, theme: mode }}
          files={initial.files}
          title={file?.name ?? '白板'}
        />
      ) : (
        <div className="editor-shell excalidraw-shell">
          <Suspense fallback={<div className="excalidraw-loading">{msg('whiteboardLoading')}</div>}>
            <Excalidraw
              langCode={locale === 'zh-CN' ? 'zh-CN' : 'en'}
              theme={mode}
              initialData={{ elements: initial.elements, appState: initial.appState, files: initial.files, scrollToContent: true }}
              onChange={handleChange}
            />
          </Suspense>
        </div>
      ))}
    </div>
  )
}
