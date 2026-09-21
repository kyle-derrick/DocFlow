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
import { App as AntdApp, Button } from 'antd'
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
export interface SceneSnapshot {
  elements: SceneElements
  appState: Partial<SceneAppState>
  files: SceneFiles
}

/** 解析 .excalidraw JSON 场景；空文本/损坏 JSON/非数组 elements 回退空场景。
 * 导出供公开分享页静态查看复用（v2.4：分享页点击白板弹窗渲染同源）。 */
export function parseScene(text: string): SceneSnapshot {
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
  const { modal: antdModal } = AntdApp.useApp()
  const msg = (key: MessageKey) => t(locale, key)
  const mode = useColorMode()

  const closeEditor = () => closeEditorWithFallback(navigate, returnTo)

  /** 返回（退出）：与 OnlyOffice/文本编辑页一致的未保存二次确认（8s 静置
   *  自动保存之外仍有未落盘改动的窗口期）。 */
  const exitWithConfirm = () => {
    if (!dirtyRef.current) {
      closeEditor()
      return
    }
    antdModal.confirm({
      title: locale === 'zh-CN' ? '有未保存的修改' : 'Unsaved changes',
      content: locale === 'zh-CN'
        ? '白板存在尚未保存到服务器的修改，直接退出可能丢失。仍要退出吗？（编辑静置 8 秒后会自动保存）'
        : 'The whiteboard has changes not yet saved to the server. Exit anyway? (autosaves after 8s idle)',
      okText: locale === 'zh-CN' ? '仍然退出' : 'Exit anyway',
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '继续编辑' : 'Keep editing',
      onOk: () => {
        closingRef.current = true
        closeEditor()
      },
    })
  }

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
  // 「保存并退出」流程抑制 beforeunload（v2.7 根治）：保存成功后 closeEditor
  // 会 window.close()，若此刻仍挂着 dirty 的 beforeunload 监听，浏览器弹
  // 原生「离开页面？」——保存已成功却拦退出是误报。close 前置位本标记。
  const closingRef = useRef(false)
  // 自动保存 debounce 定时器（编辑静置 8s 自动落版本）。
  const autosaveTimerRef = useRef(0)
  // 保存进行中标记（闭包防并发：autosave 定时器与手动保存互斥）。
  const savingRef = useRef(false)

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
  // 滚动位置等运行时 appState 不参与，纯选中/滚动不会误置脏）。有改动时
  // 静置 8s 自动保存（v2.7 自动保存间隔）。
  const handleChange: ExcalidrawChange = (elements, appState, files) => {
    sceneRef.current = { elements, appState, files }
    void import('@excalidraw/excalidraw')
      .then(({ serializeAsJSON }) => serializeAsJSON(elements, appState, files, 'local'))
      .then((json) => {
        if (initialJSONRef.current === null) {
          initialJSONRef.current = json
          return
        }
        if (json === initialJSONRef.current) {
          dirtyRef.current = false
          return
        }
        dirtyRef.current = true
        window.clearTimeout(autosaveTimerRef.current)
        autosaveTimerRef.current = window.setTimeout(() => {
          // 静置自动保存：仅在确实有未保存改动且无手动保存进行时触发，
          // 静默落版本（不弹 banner 干扰，刷新元数据版本号）。
          if (dirtyRef.current && !savingRef.current) void save(false, true)
        }, 8000)
      })
      .catch(() => {
        // 序列化失败保守视为已修改（保存兜底提示优于漏提示）
        dirtyRef.current = true
      })
  }

  // 保存：serializeAsJSON 序列化（复用 React.lazy 已加载的同一 chunk）→
  // uploadFileVersion 覆盖为新版本；成功后刷新元数据展示新版本号。
  // silent=true 为自动保存（不弹成功 banner）。保存成功把本次落盘的 JSON
  // 登记为新基线（initialJSONRef）——handleChange 的对比是异步链，保存期间
  // 可能仍有在途的对比回调晚到，若不更新基线会把已保存状态重新置脏
  //（「保存并退出」触发浏览器原生 beforeunload 弹窗的根因）。
  const save = async (thenBack: boolean, silent = false) => {
    const scene = sceneRef.current
    if (!scene || savingRef.current) return
    savingRef.current = true
    setSaving(true)
    setError('')
    if (!silent) setNotice('')
    try {
      const { serializeAsJSON } = await import('@excalidraw/excalidraw')
      const json = serializeAsJSON(scene.elements, scene.appState, scene.files, 'local')
      const blob = new File([json], file?.name ?? 'whiteboard.excalidraw', { type: 'application/json' })
      await uploadFileVersion(blob, fileId, () => {})
      dirtyRef.current = false
      initialJSONRef.current = json
      window.clearTimeout(autosaveTimerRef.current)
      if (!silent) setNotice(msg('whiteboardSaved'))
      try {
        setFile(await getFileMeta(fileId))
      } catch {
        // 元数据刷新失败不影响保存结果展示
      }
      if (thenBack) {
        closingRef.current = true
        closeEditor()
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }

  // 未保存兜底提示：有改动且尚未成功保存前拦截误关；「保存并退出」流程
  //（closingRef）不拦——保存已完成，window.close() 不再触发原生弹窗。
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current || closingRef.current) return
      e.preventDefault()
      e.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => {
      window.removeEventListener('beforeunload', onBeforeUnload)
      window.clearTimeout(autosaveTimerRef.current)
    }
  }, [])

  const versionNo = file?.current_version?.version

  return (
    <div className={`editor-page${viewMode ? ' viewer-only' : ''}`}>
      {!viewMode && <div className="editor-head">
        <Button type="text" size="small" onClick={exitWithConfirm}>{msg('back')}</Button>
        <h2 className="editor-title">{file?.name ?? msg('loading')}</h2>
        {versionNo !== undefined && (
          <span className="badge current">{formatMessage(msg('currentVersion'), { n: versionNo })}</span>
        )}
        {saving && <span className="badge uploading">{msg('saving')}</span>}
        <span className="editor-head-actions">
            <Button size="small" disabled={saving || !initial} loading={saving} onClick={() => void save(false)}>
              {msg('save')}
            </Button>
            <Button type="primary" size="small" disabled={saving || !initial} onClick={() => void save(true)}>
              {msg('saveAndBack')}
            </Button>
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
