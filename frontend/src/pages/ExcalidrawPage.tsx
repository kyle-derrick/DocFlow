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
import { Suspense, lazy, useEffect, useMemo, useRef, useState } from 'react'
import type { ComponentProps } from 'react'
import { App as AntdApp, Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { FileWithVersion, fetchFileText, getFileMeta, uploadFileVersion } from '../api'
import ExcalidrawViewer from '../components/ExcalidrawViewer'
import { EditorLoadError, EditorLoadErrorBoundary } from '../components/EditorLoadError'
import type { AIEditTarget } from '../components/AIEditChat'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIEditTool, AIQuickCommand } from '../components/AIEditChat'
import { EXCALIDRAW_JSON_GUIDE } from '../components/AIEditChat'
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
  autoFixSkeletons(out)
  return out
}

/** AI 骨架自动校正（convertToExcalidrawElements 前的兜底，吸收社区成熟
 * skill 的输出规整经验，修复模型常见的「图标重叠 / 尺寸缺失 / 坐标漂移」）：
 * 1) 形状（rectangle/ellipse/diamond）宽高缺失或非法 → 补 160×80 默认值；
 * 2) 坐标归一化：所有元素 x/y 钳制到 [-500, 20000]（模型偶发输出 NaN/
 *    巨大坐标导致元素飞出视口）；
 * 3) 连线（arrow/line）points 归一：excalidraw 的 points 为相对 x/y 的
 *    偏移，而模型常输出绝对画布坐标——首点距原点较远时判定为绝对坐标并
 *    平移为相对（修复「箭头全部堆积在画布左上角」）；带 start/end 绑定
 *    但缺 points 的连线补 [[0,0],[0,0]]（官方转换器按绑定重新计算端点）；
 * 4) 重叠错开：形状两两包围盒相交时，后一个元素按 40px 步进右下错开
 *    （最多 100 步），保证每个节点在画布上独立可见。
 * 全部为幂等纯改写，不剔除任何元素。 */
function autoFixSkeletons(items: Array<Record<string, unknown>>): void {
  const MIN = -500
  const MAX = 20000
  const clamp = (v: number) => Math.min(MAX, Math.max(MIN, v))
  for (const el of items) {
    const nx = typeof el.x === 'number' && Number.isFinite(el.x) ? el.x : 0
    const ny = typeof el.y === 'number' && Number.isFinite(el.y) ? el.y : 0
    el.x = clamp(nx)
    el.y = clamp(ny)
    if (el.type === 'rectangle' || el.type === 'ellipse' || el.type === 'diamond') {
      if (typeof el.width !== 'number' || !Number.isFinite(el.width) || el.width <= 0) el.width = 160
      if (typeof el.height !== 'number' || !Number.isFinite(el.height) || el.height <= 0) el.height = 80
    }
    if (el.type === 'arrow' || el.type === 'line') {
      if (isValidPoints(el.points)) {
        // 绝对坐标风格启发式：相对 points 的首点几乎总在 [0,0] 附近；
        // 模型给的绝对首点通常远离原点 → 平移为相对。
        const first = (el.points as number[][])[0]
        if (Math.abs(first[0]) > 60 || Math.abs(first[1]) > 60) {
          el.points = (el.points as number[][]).map(([px, py]) => [px - nx, py - ny])
        }
      } else if ((el.start != null && typeof el.start === 'object') || (el.end != null && typeof el.end === 'object')) {
        // 绑定连线缺 points：官方转换器按绑定端点重建，占位两点即可。
        el.points = [[0, 0], [0, 0]]
      }
    }
  }
  // 形状包围盒两两错开（保留首个位置，后续元素右下步进避让）。
  const shapes = items.filter((el) => el.type === 'rectangle' || el.type === 'ellipse' || el.type === 'diamond')
  const bbox = (el: Record<string, unknown>) => ({
    x1: el.x as number,
    y1: el.y as number,
    x2: (el.x as number) + (el.width as number),
    y2: (el.y as number) + (el.height as number),
  })
  for (let i = 1; i < shapes.length; i++) {
    let steps = 0
    while (steps < 100) {
      const me = bbox(shapes[i])
      const hit = shapes.slice(0, i).some((prev) => {
        const b = bbox(prev)
        return me.x1 < b.x2 && b.x1 < me.x2 && me.y1 < b.y2 && b.y1 < me.y2
      })
      if (!hit) break
      shapes[i].x = clamp((shapes[i].x as number) + 40)
      shapes[i].y = clamp((shapes[i].y as number) + 40)
      steps++
    }
  }
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

  /** 骨架 JSON → 校验 → 官方转换 → 平移至现有内容下方追加插入（insert_elements
   * 工具与单轮 aiApply 共用管线）。返回插入的正式元素（含最终 id）。 */
  const insertSkeletons = async (json: string): Promise<{ ok: true; elements: SceneElements } | { ok: false; error: string }> => {
    const api = excalidrawAPIRef.current
    if (!api) return { ok: false, error: '白板编辑器未就绪' }
    let skeletons: Array<Record<string, unknown>>
    try {
      skeletons = parseExcalidrawSkeletons(json, new Set(api.getSceneElements().map((el) => el.id)))
    } catch (err) {
      return { ok: false, error: `excalidraw JSON 校验失败：${err instanceof Error ? err.message : String(err)}` }
    }
    try {
      // 官方转换器（与编辑器同一懒加载 chunk）：骨架 → 正式元素。
      const { convertToExcalidrawElements } = await import('@excalidraw/excalidraw')
      const converted = convertToExcalidrawElements(
        skeletons as unknown as Parameters<typeof convertToExcalidrawElements>[0],
        { regenerateIds: false },
      )
      if (!converted.length) return { ok: false, error: '未产生可插入的白板元素' }
      const current = api.getSceneElements()
      // 追加插入：平移到现有内容正下方（留 60px 间距）避免重叠。
      const bottom = current.reduce((max, el) => Math.max(max, el.y + (el.height ?? 0)), 0)
      const offset = current.length ? bottom + 60 : 0
      const placed = (offset ? converted.map((el) => ({ ...el, y: el.y + offset })) : converted) as SceneElements
      api.updateScene({ elements: [...current, ...placed] })
      api.scrollToContent(placed)
      return { ok: true, elements: placed }
    } catch (err) {
      return { ok: false, error: `excalidraw 元素转换失败：${err instanceof Error ? err.message : String(err)}` }
    }
  }

  /** AI excalidraw JSON 自动应用（单轮生成通道，AIEditChat 提取围栏 JSON）：
   * 复用 insertSkeletons 管线；返回 string=失败原因（applyError，画布不动）。 */
  const aiApply = async (_mode: 'insert' | 'replace', json: string): Promise<string | void> => {
    const r = await insertSkeletons(json)
    if (!r.ok) {
      const head = json.replace(/\s+/g, ' ').slice(0, 100)
      return (locale === 'zh-CN'
        ? `${r.error}，画布未被修改；原文已保留在上面对话中（开头：${head}…）`
        : `${r.error}; the canvas was left unchanged. The original reply is kept above (starts with: ${head}…)`)
    }
  }

  /** v7 白板 Agent 工具集：元素级读/增/改/移/删——修改既有内容不再需要
   *  整图重画（id 定位、白名单属性更新；连线绑定与标签联动清理）。 */
  const aiAgentTools: AIEditTool[] = useMemo(() => {
    const sceneEls = (): SceneElements => {
      const api = excalidrawAPIRef.current
      return api ? api.getSceneElements() : (sceneRef.current?.elements ?? ([] as unknown as SceneElements))
    }
    const apiOr = () => excalidrawAPIRef.current
    return [
      {
        name: 'read_scene',
        desc: '{} → 读画布元素清单（id/type/坐标/尺寸/文字/描边色/填充色/容器绑定，最多 300 个）——修改前先读，拿元素 id。',
        label: () => '读取画布',
        exec: async () => {
          const els = sceneEls()
          const items = els.slice(0, 300).map((el) => {
            const e = el as unknown as Record<string, unknown>
            const label = e.label && typeof (e.label as { text?: unknown }).text === 'string'
              ? (e.label as { text: string }).text
              : ''
            return {
              id: el.id,
              type: el.type,
              x: Math.round(el.x),
              y: Math.round(el.y),
              w: Math.round(el.width ?? 0),
              h: Math.round(el.height ?? 0),
              text: (el.type === 'text' ? String(e.text ?? '') : label) || undefined,
              stroke: el.strokeColor,
              fill: el.backgroundColor === 'transparent' ? undefined : el.backgroundColor,
              containerId: typeof e.containerId === 'string' ? e.containerId : undefined,
            }
          })
          return { ok: true, count: els.length, elements: items }
        },
      },
      {
        name: 'insert_elements',
        desc: '{elements:[骨架对象数组]} → 插入新元素（一次可传整图全部元素；rectangle/ellipse/diamond/text/arrow/line，含 id 供连线引用）——生成新图形/整图用本工具。',
        label: (a: Record<string, unknown>) => `插入 ${Array.isArray(a.elements) ? a.elements.length : '?'} 元素`,
        exec: async (a: Record<string, unknown>) => {
          const els = a.elements
          if (!Array.isArray(els) || !els.length) return { ok: false, error: 'elements 必填：骨架对象数组（type/x/y 等，见格式规范）' }
          const r = await insertSkeletons(JSON.stringify(els))
          if (!r.ok) return r
          return { ok: true, inserted: r.elements.length, ids: r.elements.map((el) => el.id) }
        },
      },
      {
        name: 'update_elements',
        desc: '{updates:[{id, x?, y?, width?, height?, angle?, text?, fontSize?, strokeColor?, backgroundColor?, strokeWidth?, opacity?}]} → 按 id 修改既有元素（改颜色/文字/位置/尺寸；id 来自 read_scene）——局部修改用本工具，不要重画整图。',
        label: (a: Record<string, unknown>) => `更新 ${Array.isArray(a.updates) ? a.updates.length : '?'} 元素`,
        exec: async (a: Record<string, unknown>) => {
          const api = apiOr()
          if (!api) return { ok: false, error: '编辑器未就绪' }
          const updates = Array.isArray(a.updates) ? a.updates : []
          if (!updates.length) return { ok: false, error: 'updates 必填：[{id, …属性}]' }
          const byId = new Map<string, Record<string, unknown>>()
          for (const u of updates) {
            if (u && typeof (u as { id?: unknown }).id === 'string') byId.set((u as { id: string }).id, u as Record<string, unknown>)
          }
          if (!byId.size) return { ok: false, error: 'updates 缺少有效 id' }
          const NUMERIC = ['x', 'y', 'width', 'height', 'angle', 'fontSize', 'strokeWidth', 'opacity'] as const
          const COLOR = ['strokeColor', 'backgroundColor'] as const
          const current = api.getSceneElements()
          const found = new Set<string>()
          const next = current.map((el) => {
            const u = byId.get(el.id)
            if (!u) return el
            found.add(el.id)
            const e = { ...el } as unknown as Record<string, unknown>
            for (const k of NUMERIC) {
              const v = u[k]
              if (typeof v === 'number' && Number.isFinite(v)) e[k] = v
            }
            for (const k of COLOR) {
              const v = u[k]
              if (typeof v === 'string' && v) e[k] = k === 'backgroundColor' && v === 'none' ? 'transparent' : v
            }
            const text = u.text
            if (typeof text === 'string') {
              if (e.type === 'text' || typeof e.containerId === 'string') {
                e.text = text
                // 文本量宽粗估（编辑器渲染时再精调）：行宽 ≈ 字数×字号×0.6。
                const fs = typeof e.fontSize === 'number' ? e.fontSize : 20
                const lines = text.split('\n')
                e.width = Math.max(20, Math.max(...lines.map((s) => s.length)) * fs * 0.6)
                e.height = Math.max(20, lines.length * fs * 1.25)
                e.originalText = text
              }
            }
            return e as typeof el
          })
          if (!found.size) {
            return { ok: false, error: `未命中任何元素 id（可用 id 见 read_scene；收到 ${[...byId.keys()].slice(0, 5).join(', ')}…）` }
          }
          api.updateScene({ elements: next })
          const missing = [...byId.keys()].filter((id) => !found.has(id))
          return { ok: true, updated: found.size, missing: missing.length ? missing : undefined }
        },
      },
      {
        name: 'move_elements',
        desc: '{ids:[…], dx, dy} → 平移一组元素（像素；dx/dy 正值向右下）。',
        label: (a: Record<string, unknown>) => `平移 ${Array.isArray(a.ids) ? a.ids.length : '?'} 元素`,
        exec: async (a: Record<string, unknown>) => {
          const api = apiOr()
          if (!api) return { ok: false, error: '编辑器未就绪' }
          const ids = new Set(Array.isArray(a.ids) ? a.ids.filter((x): x is string => typeof x === 'string') : [])
          const dx = Number(a.dx), dy = Number(a.dy)
          if (!ids.size || !Number.isFinite(dx) || !Number.isFinite(dy)) return { ok: false, error: 'ids/dx/dy 必填' }
          const current = api.getSceneElements()
          let moved = 0
          const next = current.map((el) => {
            if (!ids.has(el.id)) return el
            moved++
            return { ...el, x: el.x + dx, y: el.y + dy }
          })
          if (!moved) return { ok: false, error: '未命中任何元素 id' }
          api.updateScene({ elements: next })
          return { ok: true, moved }
        },
      },
      {
        name: 'delete_elements',
        desc: '{ids:[…]} → 删除元素（连带其标签与绑定连线一并清理，避免悬空引用）。',
        label: (a: Record<string, unknown>) => `删除 ${Array.isArray(a.ids) ? a.ids.length : '?'} 元素`,
        exec: async (a: Record<string, unknown>) => {
          const api = apiOr()
          if (!api) return { ok: false, error: '编辑器未就绪' }
          const ids = new Set(Array.isArray(a.ids) ? a.ids.filter((x): x is string => typeof x === 'string') : [])
          if (!ids.size) return { ok: false, error: 'ids 必填' }
          const current = api.getSceneElements()
          // 扩张删除集：被删形状的标签（containerId 指向它）与绑定箭头一并删。
          const kill = new Set(ids)
          for (const el of current) {
            const e = el as unknown as Record<string, unknown>
            if (typeof e.containerId === 'string' && kill.has(e.containerId)) kill.add(el.id)
            const bound = e.boundElements
            if (Array.isArray(bound) && kill.has(el.id)) {
              for (const b of bound as Array<{ id?: unknown; type?: unknown }>) {
                if (typeof b.id === 'string' && b.type === 'arrow') kill.add(b.id)
              }
            }
          }
          let removed = 0
          const next = current
            .filter((el) => {
              if (kill.has(el.id)) { removed++; return false }
              return true
            })
            .map((el) => {
              const e = el as unknown as Record<string, unknown>
              const patch: Record<string, unknown> = {}
              // 清理幸存元素的悬空引用：boundElements / startBinding / endBinding。
              if (Array.isArray(e.boundElements)) {
                const keep = (e.boundElements as Array<{ id?: unknown }>).filter((b) => !kill.has(String(b.id ?? '')))
                if (keep.length !== (e.boundElements as unknown[]).length) patch.boundElements = keep.length ? keep : undefined
              }
              for (const key of ['startBinding', 'endBinding'] as const) {
                const b = e[key] as { elementId?: unknown } | undefined
                if (b && typeof b.elementId === 'string' && kill.has(b.elementId)) patch[key] = null
              }
              return Object.keys(patch).length ? ({ ...el, ...patch } as typeof el) : el
            })
          if (!removed) return { ok: false, error: '未命中任何元素 id' }
          api.updateScene({ elements: next })
          return { ok: true, deleted: removed }
        },
      },
    ]
    // insertSkeletons 为组件内闭包（ref 访问无状态依赖），工具仅经 ref 操作。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

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
          {/* AI 对话式创作面板（v7：agentTools 元素级工具循环——read_scene /
              insert_elements / update_elements / move_elements /
              delete_elements；单轮生成通道保留为回退）。 */}
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
            agentTools={aiAgentTools}
            agentSystemExtra={[
              '白板宿主约束：',
              '- 画新图形/整图：一次 insert_elements 传入全部元素（含标题 text 与连线 arrow）。',
              '- 改既有内容（颜色/文字/位置/尺寸）：先 read_scene 拿 id，再 update_elements 局部更新——不要重画整图。',
              '- 删除元素连带其标签与绑定箭头自动清理（delete_elements）。',
              '- insert_elements 的元素骨架字段规范（忽略其中关于围栏输出/整体布局区间的表述，坐标系以画布现状为准）：',
              EXCALIDRAW_JSON_GUIDE,
            ].join('\n')}
          />
        </div>
      ))}
    </div>
  )
}

