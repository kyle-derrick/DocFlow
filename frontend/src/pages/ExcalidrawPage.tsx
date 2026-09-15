// Excalidraw 白板编辑页（/excalidraw/:fileId）：
// - @excalidraw/excalidraw 经 React.lazy 动态加载（产物 1MB+，拆出独立
//   chunk 避免首包膨胀；包入口按 process.env 分发构建产物，见 vite.config
//   的 define 注释）；
// - 文件内容经 fetchFileText 读取（.excalidraw 即 Excalidraw JSON scene），
//   空/损坏内容回退空场景（剥离存档 theme，主题以 <html data-mode> 为准
//   并经 MutationObserver 实时跟随设置/系统明暗变化）；
// - onChange 只把最新 scene 写 ref（编辑器高频触发，避免逐笔重渲染），
//   「保存」用包导出的 serializeAsJSON 序列化后 uploadFileVersion 覆盖为
//   新版本，成功 banner 提示；「保存并返回」保存成功后返回上一页；
// - 保存失败置未保存标记（beforeunload 兜底提示，同 DrawioPage）。
import { Suspense, lazy, useEffect, useRef, useState } from 'react'
import type { ComponentProps } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { FileWithVersion, fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'

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
      const parsed = JSON.parse(trimmed) as { elements?: unknown; appState?: unknown }
      if (Array.isArray(parsed.elements)) {
        const appState: Partial<SceneAppState> =
          parsed.appState && typeof parsed.appState === 'object'
            ? { ...(parsed.appState as Partial<SceneAppState>) }
            : {}
        // 剥离存档中的主题：以页面 data-mode（theme prop）为准。
        delete appState.theme
        return { elements: parsed.elements as SceneElements, appState, files: {} }
      }
    } catch {
      // 损坏内容：回退空场景
    }
  }
  return { elements: [], appState: {}, files: {} }
}

/** 读取并实时跟随 <html data-mode>（设置切换 / system 模式跟随系统明暗）。 */
function useColorMode(): 'dark' | 'light' {
  const read = () => (document.documentElement.dataset.mode === 'light' ? 'light' : 'dark')
  const [mode, setMode] = useState<'dark' | 'light'>(read)
  useEffect(() => {
    const observer = new MutationObserver(() => setMode(read()))
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-mode'] })
    return () => observer.disconnect()
  }, [])
  return mode
}

export default function ExcalidrawPage() {
  const { fileId = '' } = useParams()
  const locale = useLocale()
  const navigate = useNavigate()
  const msg = (key: MessageKey) => t(locale, key)
  const mode = useColorMode()

  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [initial, setInitial] = useState<SceneSnapshot | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [saving, setSaving] = useState(false)

  const sceneRef = useRef<SceneSnapshot | null>(null)
  const dirtyRef = useRef(false)

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

  // onChange：仅写 ref（高频触发，不 setState 避免逐笔重渲染）。
  const handleChange: ExcalidrawChange = (elements, appState, files) => {
    sceneRef.current = { elements, appState, files }
    dirtyRef.current = true
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
      if (thenBack) navigate(-1)
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
    <div className="editor-page">
      <div className="editor-head">
        <Link className="btn ghost small" to="/">{msg('back')}</Link>
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
      </div>

      {notice && <div className="banner ok editor-hint">{notice}</div>}
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="hint">{msg('whiteboardLoading')}</div>}

      {/* 编辑器：场景就绪后渲染（chunk 加载中显示占位提示）。 */}
      {!error && !loading && initial && (
        <div className="editor-shell excalidraw-shell">
          <Suspense fallback={<div className="excalidraw-loading">{msg('whiteboardLoading')}</div>}>
            <Excalidraw
              langCode={locale === 'zh-CN' ? 'zh-CN' : 'en'}
              theme={mode}
              initialData={{ elements: initial.elements, appState: initial.appState, scrollToContent: true }}
              onChange={handleChange}
            />
          </Suspense>
        </div>
      )}
    </div>
  )
}
