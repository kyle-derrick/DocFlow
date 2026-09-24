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
import { EditorLoadError, EditorLoadErrorBoundary } from '../components/EditorLoadError'
import type { AIEditTarget } from '../components/AIEdit'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIQuickCommand } from '../components/AIEditChat'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'
import { MessageKey, formatMessage, t, useLocale } from '../i18n'
import { useColorMode } from '../theme'
import { setAIContextFile } from '../components/AIAssistant'

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
// 同法推导命令式 API 实例类型（excalidrawAPI 回调首参：updateScene /
// scrollToContent / getSceneElements / addFiles）。
type ExcalidrawAPIInstance = Parameters<NonNullable<ComponentProps<typeof Excalidraw>['excalidrawAPI']>>[0]

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

// ---- AI excalidraw 元素骨架校验（applyKind=excalidraw-json 前置）----

/** 合法骨架 type 白名单（与官方 convertToExcalidrawElements 的生成类支持
 * 集对齐；image/embeddable/freedraw/frame/selection 等非本次生成路径的类型
 * 一律剔除）。 */
const AI_ELEMENT_TYPES = new Set(['rectangle', 'ellipse', 'diamond', 'text', 'arrow', 'line'])

/** 随机 id（AI 元素缺失/冲突时补；前缀 ai- 便于人工辨认来源）。 */
const randomAIElementId = () => `ai-${Math.random().toString(36).slice(2, 10)}`

/** points 字段深校验：[[x,y],...]（至少 2 点，坐标须为有限数）。 */
const isValidPoints = (v: unknown): boolean =>
  Array.isArray(v) && v.length >= 2 && v.every(
    (p) => Array.isArray(p) && p.length === 2
      && typeof p[0] === 'number' && Number.isFinite(p[0])
      && typeof p[1] === 'number' && Number.isFinite(p[1]),
  )

/** 解析并规整 AI 生成的 excalidraw 元素骨架数组（应用前唯一守门）：
 * 1) JSON.parse——失败抛错（调用方回落展示原文）；
 * 2) 逐项过滤非法元素：非对象 / type 缺失或不在白名单 / x、y 非有限数 /
 *    text 元素缺字符串 text / arrow·line 既无 start·end 绑定又无合法
 *    points / label 非 {text:string}（label 非法时删除字段保留元素）；
 * 3) id 规整：缺失/空串/与现有画布 id 冲突/数组内重复 → 补随机 id，并记
 *    录映射同步改写其它元素 start/end 引用；
 * 4) start/end 引用改写后仍指向未知 id → 删除该端绑定；两端皆失且无
 *    points 的连线剔除（避免悬空绑定）。
 * 全部被剔除时抛错；返回规整后的骨架数组（交给官方
 * convertToExcalidrawElements 补全派生字段）。 */
function parseExcalidrawSkeletons(raw: string, existingIds: Set<string>): Array<Record<string, unknown>> {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    throw new Error('invalid JSON')
  }
  if (!Array.isArray(parsed) || !parsed.length) {
    throw new Error('expected a non-empty JSON array of elements')
  }
  const items = parsed.filter((el): el is Record<string, unknown> => {
    if (!el || typeof el !== 'object' || Array.isArray(el)) return false
    if (typeof el.type !== 'string' || !AI_ELEMENT_TYPES.has(el.type)) return false
    if (typeof el.x !== 'number' || !Number.isFinite(el.x)) return false
    if (typeof el.y !== 'number' || !Number.isFinite(el.y)) return false
    if (el.type === 'text' && typeof el.text !== 'string') return false
    if (el.type === 'arrow' || el.type === 'line') {
      const hasBind = (el.start != null && typeof el.start === 'object') || (el.end != null && typeof el.end === 'object')
      if (!hasBind && !isValidPoints(el.points)) return false
    }
    // label 非法（缺 text 字符串）时删除字段保留元素（label 可选）。
    if (el.label !== undefined && (!el.label || typeof el.label !== 'object' || typeof (el.label as { text?: unknown }).text !== 'string')) {
      delete el.label
    }
    return true
  })
  if (!items.length) {
    throw new Error('no valid elements — every element needs a supported type and numeric x/y')
  }
  // id 规整：缺失/冲突补随机（旧 id→新 id 映射供引用改写）。
  const rename = new Map<string, string>()
  const seen = new Set(existingIds)
  for (const el of items) {
    const id = el.id
    if (typeof id === 'string' && id && !seen.has(id)) {
      seen.add(id)
      continue
    }
    const fresh = randomAIElementId()
    if (typeof id === 'string' && id) rename.set(id, fresh)
    el.id = fresh
    seen.add(fresh)
  }
  // start/end 引用改写与悬空剔除。
  const known = new Set(items.map((el) => String(el.id)))
  const out = items.filter((el) => {
    if (el.type !== 'arrow' && el.type !== 'line') return true
    let keep = true
    for (const key of ['start', 'end'] as const) {
      const bind = el[key]
      if (!bind || typeof bind !== 'object') continue
      const ref = typeof (bind as { id?: unknown }).id === 'string' ? (bind as { id: string }).id : ''
      const mapped = (ref && rename.get(ref)) || ref
      if (mapped && known.has(mapped)) {
        if (mapped !== ref) (bind as { id: string }).id = mapped
      } else {
        delete el[key]
        const other = key === 'start' ? el.end : el.start
        if (!other && !isValidPoints(el.points)) keep = false
      }
    }
    return keep
  })
  if (!out.length) {
    throw new Error('all connector elements reference unknown ids')
  }
  return out
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
  // 命令式 API 实例（excalidrawAPI 回调登记；AI 元素 JSON 插入用）。
  const excalidrawAPIRef = useRef<ExcalidrawAPIInstance | null>(null)
  // AI 对话面板（AIEditChat）展开态（收起不清空会话）与头部下拉快捷指令。
  const [aiChatOpen, setAiChatOpen] = useState(false)
  const [aiQuick, setAiQuick] = useState<AIQuickCommand | null>(null)
  // 编辑器重挂 key（AI 撤销回退 reload 时强制重载 initialData）。
  const [reloadKey, setReloadKey] = useState(0)
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

  useEffect(() => {
    if (file) setAIContextFile({ fileId: file.id, fileName: file.name })
    return () => setAIContextFile(null)
  }, [file])

  // ---- 编辑器 AI（applyKind=excalidraw-json）：AI 生成官方元素 JSON（骨架）
  //      → 前端校验规整 → 官方 convertToExcalidrawElements 补全为正式元素
  //      追加画布 ----

  /** 白板内容摘要（AI 上下文用）：白板无天然全文，取元素计数与各元素文本
   * 标签（无文本用元素类型）按行拼接，截前 4000 字符——仅供 AI 理解画布
   * 现状，不参与落盘。 */
  const sceneSummary = (): string => {
    const els = sceneRef.current?.elements ?? []
    if (!els.length) return ''
    const parts = [`${locale === 'zh-CN' ? '白板元素' : 'whiteboard elements'}: ${els.length}`]
    for (const el of els) {
      const raw = (el as { text?: unknown }).text
      const label = typeof raw === 'string' ? raw.trim() : ''
      parts.push(label || el.type)
    }
    return parts.join('\n').slice(0, 4000)
  }

  /** AI 上下文目标：白板无选区概念，恒全文（=摘要）。 */
  const aiGetTarget = (): AIEditTarget => ({ hasSelection: false, text: sceneSummary() })

  /** AI excalidraw JSON 自动应用（AIEditChat 已提取 ```excalidraw-json 围栏
   * 内的 JSON 数组文本）：JSON.parse 校验 → 过滤非法元素（缺 type/数值 x/y、
   * text 缺 text、连线缺绑定与 points 均剔除）→ id 缺失/与画布冲突自动补
   * 随机（start/end 引用同步改写，悬空引用的连线剔除）→ 官方
   * convertToExcalidrawElements 把骨架补全为正式元素（seed/versionNonce/
   * 文本量宽/绑定端点等派生字段全部自动生成）→ 平移到现有内容下方 →
   * updateScene 追加插入（不覆盖现有内容）→ scrollToContent 对焦 →
   * onChange 链路自动置脏并走 8s 静置自动保存。
   * 返回 string=失败原因（AIEditChat 显示 applyError，画布未被修改）。 */
  const aiApply = async (_mode: 'insert' | 'replace', json: string): Promise<string | void> => {
    const api = excalidrawAPIRef.current
    if (!api) return locale === 'zh-CN' ? '白板编辑器未就绪，未应用' : 'Whiteboard editor not ready; not applied'
    let skeletons: Array<Record<string, unknown>>
    try {
      skeletons = parseExcalidrawSkeletons(json, new Set(api.getSceneElements().map((el) => el.id)))
    } catch (err) {
      const why = err instanceof Error ? err.message : String(err)
      const head = json.replace(/\s+/g, ' ').slice(0, 100)
      return (locale === 'zh-CN'
        ? `excalidraw JSON 校验失败（${why}），画布未被修改；原文已保留在上面对话中（开头：${head}…）`
        : `Failed to validate the excalidraw JSON (${why}); the canvas was left unchanged. The original reply is kept above (starts with: ${head}…)`)
    }
    try {
      // 官方转换器（与编辑器同一懒加载 chunk）：骨架 → 正式元素。
      const { convertToExcalidrawElements } = await import('@excalidraw/excalidraw')
      const converted = convertToExcalidrawElements(
        skeletons as unknown as Parameters<typeof convertToExcalidrawElements>[0],
        { regenerateIds: false },
      )
      if (!converted.length) {
        return locale === 'zh-CN' ? '未产生可插入的白板元素，画布未被修改' : 'No insertable whiteboard elements were produced; the canvas was left unchanged'
      }
      const current = api.getSceneElements()
      // 追加插入：平移到现有内容正下方（留 60px 间距）避免重叠。
      const bottom = current.reduce((max, el) => Math.max(max, el.y + (el.height ?? 0)), 0)
      const offset = current.length ? bottom + 60 : 0
      const placed = offset ? converted.map((el) => ({ ...el, y: el.y + offset })) : converted
      api.updateScene({ elements: [...current, ...placed] })
      api.scrollToContent(placed)
    } catch (err) {
      // 骨架字段类型错误等：不写画布，错误文案回 AIEditChat 显示。
      const why = err instanceof Error ? err.message : String(err)
      return (locale === 'zh-CN' ? 'excalidraw 元素转换失败：' : 'Excalidraw element conversion failed: ') + why
    }
  }

  /** 版本保护前置：应用前先保存当前场景为基线版本（照文本编辑页模式）。 */
  const aiEnsureSaved = async (): Promise<{ versionId: string; version: number } | null> => {
    try {
      if (dirtyRef.current) {
        await save(false, true)
        if (dirtyRef.current) return null
      }
      const meta = await getFileMeta(fileId)
      return meta.current_version
        ? { versionId: meta.current_version.id, version: meta.current_version.version }
        : null
    } catch {
      return null
    }
  }

  /** 撤销回退后刷新画布：重新拉取文件内容，重挂编辑器（key 递增强制
   * initialData 重载），重置脏基线。 */
  const aiReload = async (): Promise<void> => {
    const text = await fetchFileText(fileId)
    const scene = parseScene(text)
    sceneRef.current = scene
    initialJSONRef.current = null
    dirtyRef.current = false
    setInitial(scene)
    setReloadKey((k) => k + 1)
    setNotice('')
  }

  /** 头部下拉快捷指令：打开面板并透传给 AIEditChat 自动执行。 */
  const openAiChatWith = (cmd: AIQuickCommand) => {
    setAiChatOpen(true)
    setAiQuick(cmd)
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
            {/* AI 统一入口：完整 AIEditChat 右侧面板（applyKind=
                excalidraw-json）：可修改模式下 AI 生成官方元素 JSON 自动转换
                为白板原生元素追加画布（版本保护可撤销）。 */}
            <AIEditChatButton open={aiChatOpen} onToggle={() => setAiChatOpen((v) => !v)} onQuick={openAiChatWith} kind="excalidraw-json" disabled={saving || !initial} />
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
        <div className="editor-with-ai">
          <div className="editor-shell excalidraw-shell">
          {/* 错误边界（v2.7 反馈 8）：excalidraw 编辑器为 1MB+ 懒加载
              chunk，静态资源拉取失败（网络中断/反向代理未放行 assets）时
              React.lazy 会向上抛异常——无边界即白屏。fail 时渲染页面级
              错误卡（资源地址 + 排查提示），替代白屏。 */}
          <EditorLoadErrorBoundary
            renderError={() => (
              <EditorLoadError
                message={locale === 'zh-CN' ? '白板编辑器（excalidraw）资源加载失败' : 'Failed to load the whiteboard editor (excalidraw) resources'}
                resourceLabel={locale === 'zh-CN' ? '静态资源站点' : 'Static assets origin'}
                resourceUrl={window.location.origin}
                hints={locale === 'zh-CN' ? [
                  '白板编辑器组件按需加载（独立 chunk），加载失败通常为网络中断或反向代理未正确放行 /assets 静态资源。',
                  '请检查反向代理配置（assets 目录转发与 gzip）与浏览器网络面板中的失败请求，然后刷新重试。',
                ] : [
                  'The whiteboard editor is loaded on demand as a separate chunk; failure usually means a network interruption or a reverse proxy not forwarding /assets correctly.',
                  'Check your reverse-proxy configuration (assets forwarding) and the failed request in the browser network panel, then reload.',
                ]}
                onRetry={() => window.location.reload()}
                retryText={locale === 'zh-CN' ? '刷新重试' : 'Reload'}
              />
            )}
          >
            <Suspense fallback={<div className="excalidraw-loading">{msg('whiteboardLoading')}</div>}>
              {/* key=reloadKey：AI 撤销回退后强制重挂重载 initialData；
                  excalidrawAPI：登记命令式实例（AI 元素 JSON 插入用）。 */}
              <Excalidraw
                key={reloadKey}
                langCode={locale === 'zh-CN' ? 'zh-CN' : 'en'}
                theme={mode}
                initialData={{ elements: initial.elements, appState: initial.appState, files: initial.files, scrollToContent: true }}
                onChange={handleChange}
                excalidrawAPI={(api) => {
                  excalidrawAPIRef.current = api
                }}
              />
            </Suspense>
          </EditorLoadErrorBoundary>
          </div>
          {/* AI 对话式创作面板（excalidraw 元素 JSON → 白板原生元素；应用前
              aiEnsureSaved 保存基线版本，撤销回退后 aiReload 重载画布）。 */}
          <AIEditChat
            open={aiChatOpen}
            onClose={() => setAiChatOpen(false)}
            getTarget={aiGetTarget}
            getAllText={sceneSummary}
            onApply={aiApply}
            fileId={fileId}
            ensureSaved={aiEnsureSaved}
            reload={aiReload}
            quickCommand={aiQuick}
            onQuickConsumed={() => setAiQuick(null)}
            applyKind="excalidraw-json"
          />
        </div>
      ))}
    </div>
  )
}
