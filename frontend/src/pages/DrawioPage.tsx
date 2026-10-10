// draw.io 图表编辑页（/drawio/:fileId）：
// - GET /drawio/config 探测集成，取 url 以 iframe embed 模式加载编辑器
//   （URL 参数 embed=1&proto=json&spin=1&saveAndExit=1&noSaveBtn=0&libraries=1）；
// - postMessage JSON 协议：receive {event:"init"} → send {action:"load",
//   xml, autosave:0}；receive {event:"save", xml}（或 export）→ 经
//   uploadFileVersion 把导出 XML 作为新版本上传（覆盖链路，服务端忽略
//   name/parent_id）→ banner 提示；保存后不销毁编辑器（继续编辑）；
// - 文件内容经 fetchFileText 认证下载；读取失败/空内容回退初始模板
//   （EMPTY_DRAWIO_XML，404/无版本等容错）；
// - 页面卸载移除 message 监听；保存失败置未保存标记（beforeunload 提示
//   兜底，常规未保存保护依赖 drawio 的「保存并退出」按钮——协议本身不
//   通知父页脏态，autosave:0 亦不产生自动保存事件）。
// - 集成禁用或探测失败显示「图表服务不可用」。
import { useEffect, useMemo, useRef, useState } from 'react'
import { App as AntdApp, Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  EMPTY_DRAWIO_XML,
  FileWithVersion,
  drawioStatus,
  fetchFileText,
  getFileMeta,
  uploadFileVersion,
} from '../api'
import DrawioViewer from '../components/DrawioViewer'
import { EditorLoadError } from '../components/EditorLoadError'
import type { AIEditTarget } from '../components/AIEditChat'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIEditTool, AIQuickCommand } from '../components/AIEditChat'
import { DRAWIO_XML_GUIDE } from '../components/AIEditChat'
import { useAIEnabled } from '../aiFeature'
import { useLocale } from '../i18n'
import { useColorMode } from '../theme'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'

/**
 * draw.io embed 编辑器/查看器 iframe URL（proto=json postMessage 协议）。
 * lang 跟随界面语言（drawio 资源名 zh/en）；ui 跟随站点明暗（浅色 min /
 * 深色 dark，编辑器界面主题与站点一致，避免深色站点里白闪编辑器）；
 * view=true 为只读查看器（viewer=1，隐藏编辑工具与保存），否则编辑模式。
 * v2.7：不再传 saveAndExit=1——drawio 的「保存并退出」按钮与 Ctrl+S 均只
 * 触发 save 事件（保存不退出），退出统一走页面「← 返回」（带 dirty 确认）。
 */
function drawioEditorURL(base: string, lang: string, view: boolean, dark: boolean): string {
  const trimmed = base.replace(/\/+$/, '')
  const params = new URLSearchParams({
    embed: '1',
    proto: 'json',
    spin: '1',
    libraries: '1',
    lang,
    ui: dark ? 'dark' : 'min',
  })
  if (view) params.set('viewer', '1')
  else {
    params.set('noSaveBtn', '0')
  }
  return `${trimmed}/?${params.toString()}`
}

/** draw.io postMessage JSON 协议消息（仅用到的事件/字段；exit 标记保存并退出）。 */
interface DrawioMessage {
  event?: string
  action?: string
  xml?: string
  exit?: boolean
}

/** draw.io iframe 初始化等待上限：超时仍未收到 init 事件（编辑器静态资源
 * 不可达/服务端配置了浏览器不可达的地址）即展示页面级错误卡。 */
const DRAWIO_INIT_TIMEOUT_MS = 20000

/** AI 生成 drawio XML 自动校正（应用前守门，吸收社区成熟 drawio skill 的
 * 输出规整经验，修复模型常见问题——箭头引用悬空、节点缺几何、坐标漂移、
 * 节点重叠）：
 * 1) DOMParser 解析，失败抛错（调用方回落展示原文不应用）；
 * 2) 找到 mxGraphModel/root（mxfile 自动下钻）；缺失抛错；
 * 3) 确保基础单元格 id=0（parent=空）与 id=1（parent=0）存在；
 * 4) 顶点（vertex=1）：补缺失/非法 mxGeometry（默认 40,40,160,60），坐标
 *    钳制 [-500,20000]；
 * 5) 边（edge=1）：source/target 指向不存在单元格 → 删除该引用（浮端，
 *    避免 drawio 渲染异常）；补相对 mxGeometry；
 * 6) 顶点包围盒两两重叠 → 后者按 40px 步进右下错开（≤100 步）。
 * 返回规整后的 XML 字符串；幂等纯改写，不剔除合法单元格。 */
function fixDrawioXML(xml: string): string {
  const doc = new DOMParser().parseFromString(xml, 'application/xml')
  if (doc.querySelector('parsererror')) throw new Error('invalid XML')
  const root = doc.getElementsByTagName('mxGraphModel')[0]?.getElementsByTagName('root')[0]
    ?? doc.getElementsByTagName('root')[0]
  if (!root) throw new Error('missing mxGraphModel/root')
  const cells = Array.from(root.getElementsByTagName('mxCell'))
  const byId = new Map<string, Element>()
  for (const c of cells) {
    const id = c.getAttribute('id') ?? ''
    if (id) byId.set(id, c)
  }
  // 基础单元格兜底（AI 偶发省略）。
  if (!byId.has('0')) {
    const c = doc.createElement('mxCell')
    c.setAttribute('id', '0')
    root.appendChild(c)
    byId.set('0', c)
  }
  if (!byId.has('1')) {
    const c = doc.createElement('mxCell')
    c.setAttribute('id', '1')
    c.setAttribute('parent', '0')
    root.appendChild(c)
    byId.set('1', c)
  }
  const clamp = (v: number) => Math.min(20000, Math.max(-500, v))
  const geometry = (c: Element): Element => {
    let g = Array.from(c.children).find((ch) => ch.tagName === 'mxGeometry')
    if (!g) {
      g = doc.createElement('mxGeometry')
      g.setAttribute('as', 'geometry')
      c.appendChild(g)
    }
    return g
  }
  const vertices: Element[] = []
  for (const c of cells) {
    const id = c.getAttribute('id') ?? ''
    if (!id || id === '0' || id === '1') continue
    if (c.getAttribute('vertex') === '1') {
      const g = geometry(c)
      const num = (name: string, def: number): number => {
        const raw = g.getAttribute(name)
        const v = raw == null ? NaN : Number(raw)
        return Number.isFinite(v) && v >= 0 ? v : def
      }
      const x = clamp(num('x', 40))
      const y = clamp(num('y', 40))
      const w = num('width', 160)
      const h = num('height', 60)
      g.setAttribute('x', String(x))
      g.setAttribute('y', String(y))
      g.setAttribute('width', String(w))
      g.setAttribute('height', String(h))
      if (!g.getAttribute('as')) g.setAttribute('as', 'geometry')
      if (!c.getAttribute('parent')) c.setAttribute('parent', '1')
      vertices.push(g)
    } else if (c.getAttribute('edge') === '1') {
      for (const key of ['source', 'target'] as const) {
        const ref = c.getAttribute(key)
        if (ref && !byId.has(ref)) c.removeAttribute(key)
      }
      const g = geometry(c)
      if (!g.getAttribute('relative')) g.setAttribute('relative', '1')
      if (!c.getAttribute('parent')) c.setAttribute('parent', '1')
    }
  }
  // 顶点两两重叠错开（后一个右下步进避让）。
  const box = (g: Element) => ({
    x1: Number(g.getAttribute('x')),
    y1: Number(g.getAttribute('y')),
    x2: Number(g.getAttribute('x')) + Number(g.getAttribute('width')),
    y2: Number(g.getAttribute('y')) + Number(g.getAttribute('height')),
  })
  for (let i = 1; i < vertices.length; i++) {
    let steps = 0
    while (steps < 100) {
      const me = box(vertices[i])
      const hit = vertices.slice(0, i).some((prev) => {
        const b = box(prev)
        return me.x1 < b.x2 && b.x1 < me.x2 && me.y1 < b.y2 && b.y1 < me.y2
      })
      if (!hit) break
      vertices[i].setAttribute('x', String(clamp(Number(vertices[i].getAttribute('x')) + 40)))
      vertices[i].setAttribute('y', String(clamp(Number(vertices[i].getAttribute('y')) + 40)))
      steps++
    }
  }
  return new XMLSerializer().serializeToString(doc)
}

// ---- v7 Agent 工具的 XML DOM 操作（元素级读/增/改/删，均作用于当前
//      mxGraphModel；改完统一走 fixDrawioXML 守门 + load/export 应用链路）----

/** 解析 XML 文本 → {doc, root}；非法或无 root 抛错。 */
function parseDrawioDoc(xml: string): { doc: Document; root: Element } {
  const doc = new DOMParser().parseFromString(xml, 'application/xml')
  if (doc.querySelector('parsererror')) throw new Error('invalid XML')
  const root = doc.getElementsByTagName('mxGraphModel')[0]?.getElementsByTagName('root')[0]
    ?? doc.getElementsByTagName('root')[0]
  if (!root) throw new Error('missing mxGraphModel/root')
  return { doc, root }
}

/** root 下业务单元格（跳过基础单元格 0/1）。 */
function drawioCells(root: Element): Element[] {
  return Array.from(root.getElementsByTagName('mxCell')).filter((c) => {
    const id = c.getAttribute('id') ?? ''
    return !!id && id !== '0' && id !== '1'
  })
}

/** mxCell 概要（list_cells 返回 / AI 定位用）。 */
function drawioCellInfo(c: Element): Record<string, unknown> {
  const g = Array.from(c.children).find((ch) => ch.tagName === 'mxGeometry')
  const num = (n: string): number | undefined => {
    const v = Number(g?.getAttribute(n))
    return Number.isFinite(v) ? v : undefined
  }
  const kind = c.getAttribute('vertex') === '1' ? 'vertex' : c.getAttribute('edge') === '1' ? 'edge' : 'cell'
  return {
    id: c.getAttribute('id') ?? '',
    kind,
    value: (c.getAttribute('value') ?? '').slice(0, 40) || undefined,
    x: num('x'), y: num('y'), w: num('width'), h: num('height'),
    source: c.getAttribute('source') ?? undefined,
    target: c.getAttribute('target') ?? undefined,
    style: (c.getAttribute('style') ?? '').slice(0, 80) || undefined,
  }
}

/** 生成不与现有冲突的新 id（ai-N 递增，前缀标明 AI 来源）。 */
function freshDrawioIds(root: Element, count: number): string[] {
  const used = new Set(drawioCells(root).map((c) => c.getAttribute('id') ?? ''))
  const out: string[] = []
  let n = 1
  while (out.length < count) {
    const id = `ai-${n++}`
    if (!used.has(id)) { used.add(id); out.push(id) }
  }
  return out
}

/** style 串 ↔ 键值映射（update_cells 的 styleSet 合并用；保留末尾分号风格）。 */
function drawioStyleMap(style: string): Map<string, string> {
  const m = new Map<string, string>()
  for (const part of style.split(';')) {
    const seg = part.trim()
    if (!seg) continue
    const eq = seg.indexOf('=')
    if (eq > 0) m.set(seg.slice(0, eq), seg.slice(eq + 1))
    else if (!m.has(seg)) m.set(seg, '')
  }
  return m
}
function drawioStyleString(m: Map<string, string>): string {
  return [...m.entries()].map(([k, v]) => (v === '' ? k : `${k}=${v}`)).join(';') + ';'
}

export default function DrawioPage({ mode, fileId: fileIdProp }: { mode?: 'edit' | 'view'; fileId?: string } = {}) {
  const { fileId: routeFileId = '' } = useParams()
  // by-path 路由经 prop 传入 resolve 得到的 file_id；缺省回退路由参数。
  const fileId = fileIdProp ?? routeFileId
  // 独立 /view 路由或 ?mode=view 均强制只读；编辑器语言跟随界面语言。
  const [searchParams] = useSearchParams()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const returnTo = safeReturnTo(searchParams.get('returnTo'))
  const locale = useLocale()
  const aiOn = useAIEnabled()
  const navigate = useNavigate()
  // 只读渲染跟随站点明暗主题与界面语言（zh-CN → drawio 中文资源）。
  const colorMode = useColorMode()
  const viewerLang = locale === 'zh-CN' ? 'zh' : ''

  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [editorURL, setEditorURL] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [saving, setSaving] = useState(false)
  /** 页面级加载失败（EditorLoadError 错误卡）：iframe 加载失败或初始化
   * 超时（超时仍未收到 init postMessage），展示图表服务地址与排查提示。 */
  const [frameError, setFrameError] = useState<string | null>(null)

  const frameRef = useRef<HTMLIFrameElement | null>(null)
  // 初始图表 XML（init 事件时发给编辑器）、未保存标记（保存失败兜底提示）
  // 与保存中标记（message 监听闭包防并发保存用，避免 state 闭包陈旧）。
  const xmlRef = useRef('')
  const dirtyRef = useRef(false)
  const savingRef = useRef(false)
  const savePromiseRef = useRef<Promise<boolean> | null>(null)
  // 自动保存 debounce（autosave 事件静置 10s 落版本；v2.7 自动保存间隔）。
  const autosaveTimerRef = useRef(0)
  // 「保存并退出」之外的退出流程抑制 beforeunload（返回确认走 antd 弹窗，
  // 不再叠加浏览器原生弹窗）。
  const closingRef = useRef(false)
  // AI 对话面板（AIEditChat）展开态（收起不清空会话）与头部下拉快捷指令。
  const [aiChatOpen, setAiChatOpen] = useState(false)
  const [aiQuick, setAiQuick] = useState<AIQuickCommand | null>(null)
  const { modal: antdModal } = AntdApp.useApp()

  // 探测集成 → 拉取文件元数据与内容 → 挂 iframe。内容读取失败或为空
  // （含新建模板上传后立即打开）回退初始模板，不阻塞编辑。
  useEffect(() => {
    let alive = true
    const init = async () => {
      setLoading(true)
      setError('')
      setNotice('')
      setFrameError(null)
      try {
        const status = await drawioStatus()
        if (!alive) return
        if (!status.enabled || !status.url) {
          setError('图表服务不可用（draw.io 集成已关闭或无法访问）')
          return
        }
        const [meta, text] = await Promise.all([
          getFileMeta(fileId).catch(() => null),
          fetchFileText(fileId).catch(() => ''),
        ])
        if (!alive) return
        setFile(meta)
        xmlRef.current = text.trim() ? text : EMPTY_DRAWIO_XML
        // ui 主题取当前明暗（效应不依赖 colorMode，切换主题不重载编辑器 iframe，
        // 避免编辑中途丢失未保存内容；下次进入生效）。
        setEditorURL(viewMode ? status.url.replace(/\/+$/, '') : drawioEditorURL(status.url, locale === 'zh-CN' ? 'zh' : 'en', false, colorMode === 'dark'))
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '图表编辑器加载失败')
      } finally {
        if (alive) setLoading(false)
      }
    }
    void init()
    return () => {
      alive = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, viewMode, locale])

  // 退出编辑器：显式返回打开时记录的文件页，不依赖 noopener 新窗口中不可靠的
  // history。脚本可关闭由 window.open 创建的窗口；普通标签页则确定性导航。
  const closeEditor = () => closeEditorWithFallback(navigate, returnTo)

  // postMessage JSON 协议：init → load（autosave:1 启用 autosave 事件，供
  // 父页跟踪脏态与静置自动保存）；save/export → 覆盖为新版本（保存后不退
  // 出——v2.7：Ctrl=S/保存按钮仅保存 + banner 提示，退出走「← 返回」带
  // dirty 确认）；autosave → 置脏 + 10s 静置自动落版本。
  useEffect(() => {
    if (!editorURL || viewMode) return
    // 初始化超时兜底（v2.7 反馈 8）：drawio 静态资源不可达时 iframe 不会
    // 发出 init 事件（跨域 iframe 加载失败多数也不触发 onerror），超时
    // 即展示页面级错误卡（替代无限 loading/白屏）。
    const initTimer = window.setTimeout(() => {
      setFrameError(locale === 'zh-CN'
        ? `图表编辑器初始化超时（${DRAWIO_INIT_TIMEOUT_MS / 1000} 秒内未就绪），图表服务可能不可达`
        : `Diagram editor failed to initialize within ${DRAWIO_INIT_TIMEOUT_MS / 1000}s; the diagram service may be unreachable`)
    }, DRAWIO_INIT_TIMEOUT_MS)
    const onMessage = (e: MessageEvent) => {
      const frame = frameRef.current
      if (!frame || e.source !== frame.contentWindow) return
      let msg: DrawioMessage
      try {
        msg = JSON.parse(String(e.data)) as DrawioMessage
      } catch {
        return // 非 JSON 协议消息忽略
      }
      if (msg.event === 'init') {
        window.clearTimeout(initTimer)
        frame.contentWindow?.postMessage(JSON.stringify({ action: 'load', xml: xmlRef.current, autosave: 1 }), '*')
        return
      }
      if (msg.event === 'exit') {
        closeEditor()
        return
      }
      if (msg.event === 'autosave') {
        if (msg.xml) {
          // 跟踪最新 XML（AI 上下文与 AI 应用前保存基线用）。
          xmlRef.current = msg.xml
          dirtyRef.current = true
          window.clearTimeout(autosaveTimerRef.current)
          autosaveTimerRef.current = window.setTimeout(() => {
            // 静置自动保存：静默落版本（不弹 banner），仍脏且无保存进行时。
            if (dirtyRef.current && !savingRef.current) void saveDiagram(msg.xml ?? '', true)
          }, 10000)
        }
        return
      }
      if (msg.event === 'save' || msg.event === 'export') {
        if (msg.xml) {
          // 跟踪最新 XML（save/export 均为编辑器当前内容——AI 应用后的
          // export 回包在此同步，保证后续 AI 轮次的 XML 上下文不陈旧）。
          xmlRef.current = msg.xml
          window.clearTimeout(autosaveTimerRef.current)
          const pending = saveDiagram(msg.xml)
          savePromiseRef.current = pending
          void pending.finally(() => {
            if (savePromiseRef.current === pending) savePromiseRef.current = null
          })
        }
      }
    }
    window.addEventListener('message', onMessage)
    return () => {
      window.removeEventListener('message', onMessage)
      window.clearTimeout(initTimer)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editorURL, fileId, file?.name, viewMode, locale])

  // 保存：导出 XML 作为新版本上传（file_id 会话沿用目标文件名/父目录），
  // 成功后刷新元数据展示新版本号；失败置未保存标记。返回是否保存成功。
  // silent=true 为静置自动保存（不弹「已保存为新版本」banner）。
  const saveDiagram = async (xml: string, silent = false): Promise<boolean> => {
    if (!xml) return false
    if (savingRef.current) return savePromiseRef.current ?? false
    savingRef.current = true
    setSaving(true)
    setError('')
    try {
      const blob = new File([xml], file?.name ?? 'diagram.drawio', { type: 'text/xml' })
      await uploadFileVersion(blob, fileId, () => {})
      dirtyRef.current = false
      if (!silent) setNotice('已保存为新版本')
      try {
        setFile(await getFileMeta(fileId))
      } catch {
        // 元数据刷新失败不影响保存结果展示
      }
      return true
    } catch (err) {
      dirtyRef.current = true
      setError(err instanceof Error ? err.message : '保存失败')
      return false
    } finally {
      savingRef.current = false
      setSaving(false)
    }
  }

  // 返回（退出）：有未落盘修改（autosave 事件置脏且静置窗口内未保存）时
  // 二次确认；与 OnlyOffice/白板/文本编辑页一致。
  const exitWithConfirm = () => {
    if (!dirtyRef.current) {
      closeEditor()
      return
    }
    antdModal.confirm({
      title: '有未保存的修改',
      content: '图表存在尚未保存到服务器的修改，直接退出可能丢失。仍要退出吗？（编辑静置 10 秒后会自动保存）',
      okText: '仍然退出',
      okButtonProps: { danger: true },
      cancelText: '继续编辑',
      onOk: () => {
        closingRef.current = true
        closeEditor()
      },
    })
  }

  // ---- 编辑器 AI（applyKind=drawio-xml）：AI 基于当前 XML 生成/修改完整
  //      drawio XML，经既有 postMessage 通道整体替换画布并落新版本 ----

  /** AI 上下文目标：图表无选区概念，恒全文（=当前最新已知 XML）。 */
  const aiGetTarget = (): AIEditTarget => ({ hasSelection: false, text: xmlRef.current })

  /** 应用 XML 到画布（Agent 工具与单轮 aiApply 共用）：fixDrawioXML 守门
   * （悬空引用/缺几何/坐标漂移/重叠）→ iframe load 整体替换 → 主动 export
   * 走既有 saveDiagram 链路落版本。返回 string=失败原因。 */
  const applyDiagramXML = (xml: string): string | void => {
    const frame = frameRef.current?.contentWindow
    if (!frame) return '图表编辑器未就绪，未应用'
    let fixed = xml
    try {
      fixed = fixDrawioXML(xml)
    } catch (e) {
      return e instanceof Error ? `XML 校正失败：${e.message}` : 'XML 校正失败'
    }
    frame.postMessage(JSON.stringify({ action: 'load', xml: fixed, autosave: 1 }), '*')
    dirtyRef.current = true
    frame.postMessage(JSON.stringify({ action: 'export', format: 'xml' }), '*')
  }

  /** AI XML 自动应用（AIEditChat 已提取完整 drawio XML；单轮生成通道）。 */
  const aiApply = (_mode: 'insert' | 'replace', xml: string): string | void => applyDiagramXML(xml)

  /** v7 drawio Agent 工具集：单元格级读/列/增/改/删 + 整图替换——局部修改
   *  不再需要输出整份 XML（id 定位、样式键值合并、悬空边清理）。 */
  const aiAgentTools: AIEditTool[] = useMemo(() => {
    const currentXML = () => (xmlRef.current.trim() ? xmlRef.current : EMPTY_DRAWIO_XML)
    /** 改动应用：返回 string=失败原因，void=成功（已 load+export 落版本）。 */
    const apply = (doc: Document): string | void => applyDiagramXML(new XMLSerializer().serializeToString(doc))
    return [
      {
        name: 'read_diagram',
        desc: '{} → 读当前图表完整 XML（超 12000 字截断；小图优先用本工具掌握全貌）。',
        label: () => '读取图表 XML',
        exec: async () => {
          const xml = currentXML()
          return { ok: true, cells: drawioCells(parseDrawioDoc(xml).root).length, xml: xml.slice(0, 12000), truncated: xml.length > 12000 }
        },
      },
      {
        name: 'list_cells',
        desc: '{} → 单元格清单（id/类型/文本/坐标尺寸/连线端点/样式摘要，最多 150 个）——修改前先读，拿单元格 id。',
        label: () => '列出单元格',
        exec: async () => {
          const { root } = parseDrawioDoc(currentXML())
          return { ok: true, cells: drawioCells(root).slice(0, 150).map(drawioCellInfo) }
        },
      },
      {
        name: 'insert_cells',
        desc: '{xml} → 插入单元格片段（<mxCell …/> 列表或含 <mxGraphModel> 的整段；顶点须带 <mxGeometry x/y/width/height>，边 source/target 引用本批次或既有 id）——新增图形/局部补图用本工具。',
        label: () => '插入单元格',
        exec: async (a: Record<string, unknown>) => {
          const xml = String(a.xml ?? '')
          if (!xml.includes('mxCell')) return { ok: false, error: 'xml 必填：mxCell 片段（顶点先于引用它的边）' }
          let fragRoot: Element
          try {
            const wrapped = /<mxGraphModel[\s>]/.test(xml) ? xml : `<mxGraphModel><root>${xml}</root></mxGraphModel>`
            fragRoot = parseDrawioDoc(wrapped).root
          } catch (e) {
            return { ok: false, error: `片段解析失败：${e instanceof Error ? e.message : String(e)}` }
          }
          let doc: Document, root: Element
          try {
            ({ doc, root } = parseDrawioDoc(currentXML()))
          } catch (e) {
            return { ok: false, error: `当前图表解析失败：${e instanceof Error ? e.message : String(e)}` }
          }
          const incoming = drawioCells(fragRoot)
          if (!incoming.length) return { ok: false, error: '片段中没有 mxCell' }
          // id 缺失/与既有冲突 → 补新 id；批内引用同步改写。
          const existing = new Set(drawioCells(root).map((c) => c.getAttribute('id') ?? ''))
          const rename = new Map<string, string>()
          const fresh = freshDrawioIds(root, incoming.length * 2)
          let fi = 0
          const adopted = doc.importNode(fragRoot, false)
          for (const cell of incoming) {
            const id = cell.getAttribute('id') ?? ''
            let finalId = id
            if (!id || existing.has(id) || rename.has(id)) {
              finalId = fresh[fi++] ?? `ai-x${Date.now()}${fi}`
              rename.set(id || finalId, finalId)
            }
            existing.add(finalId)
            cell.setAttribute('id', finalId)
            if (!cell.getAttribute('parent')) cell.setAttribute('parent', '1')
            adopted.appendChild(doc.importNode(cell, true))
          }
          for (const cell of Array.from(adopted.children)) {
            const c = cell as Element
            if (c.tagName !== 'mxCell' || c.getAttribute('edge') !== '1') continue
            for (const key of ['source', 'target'] as const) {
              const ref = c.getAttribute(key)
              if (ref && rename.has(ref)) c.setAttribute(key, rename.get(ref) as string)
            }
          }
          root.appendChild(adopted)
          const err = apply(doc)
          if (err) return { ok: false, error: err }
          return { ok: true, inserted: incoming.length, ids: incoming.map((c) => c.getAttribute('id') ?? '') }
        },
      },
      {
        name: 'update_cells',
        desc: '{updates:[{id, value?, style?, styleSet?, x?, y?, w?, h?}]} → 按 id 修改单元格：value 改文本；style 整串替换或 styleSet 键值合并（如 {"fillColor":"#dae8fc","fontStyle":"1"}，null 删键）；x/y/w/h 改几何——局部修改用本工具，不要重画整图。',
        label: (a: Record<string, unknown>) => `更新 ${Array.isArray(a.updates) ? a.updates.length : '?'} 单元格`,
        exec: async (a: Record<string, unknown>) => {
          const updates = Array.isArray(a.updates) ? (a.updates as Array<Record<string, unknown>>) : []
          if (!updates.length) return { ok: false, error: 'updates 必填：[{id, …}]' }
          let doc: Document, root: Element
          try {
            ({ doc, root } = parseDrawioDoc(currentXML()))
          } catch (e) {
            return { ok: false, error: `当前图表解析失败：${e instanceof Error ? e.message : String(e)}` }
          }
          const byId = new Map<string, Element>()
          for (const c of drawioCells(root)) byId.set(c.getAttribute('id') ?? '', c)
          const missing: string[] = []
          let updated = 0
          for (const u of updates) {
            const id = String((u as { id?: unknown }).id ?? '')
            const cell = byId.get(id)
            if (!cell) { missing.push(id); continue }
            if ((u as { value?: unknown }).value !== undefined) {
              cell.setAttribute('value', String((u as { value?: unknown }).value))
            }
            const style = (u as { style?: unknown }).style
            if (typeof style === 'string' && style) cell.setAttribute('style', style)
            const styleSet = (u as { styleSet?: unknown }).styleSet
            if (styleSet && typeof styleSet === 'object' && !Array.isArray(styleSet)) {
              const m = drawioStyleMap(cell.getAttribute('style') ?? '')
              for (const [k, v] of Object.entries(styleSet as Record<string, unknown>)) {
                if (v === null) m.delete(k)
                else m.set(k, String(v))
              }
              cell.setAttribute('style', drawioStyleString(m))
            }
            const geo = { x: 'x', y: 'y', w: 'width', h: 'height' } as const
            let g = Array.from(cell.children).find((ch) => ch.tagName === 'mxGeometry')
            const needsGeo = (Object.keys(geo) as Array<keyof typeof geo>).some((k) => typeof (u as Record<string, unknown>)[k] === 'number')
            if (needsGeo && !g) {
              g = doc.createElement('mxGeometry')
              g.setAttribute('as', 'geometry')
              cell.appendChild(g)
            }
            if (g) {
              for (const [k, attr] of Object.entries(geo) as Array<[keyof typeof geo, string]>) {
                const v = (u as Record<string, unknown>)[k]
                if (typeof v === 'number' && Number.isFinite(v)) g.setAttribute(attr, String(v))
              }
            }
            updated++
          }
          if (!updated) return { ok: false, error: `未命中任何单元格 id（可用 id 见 list_cells；收到 ${missing.slice(0, 5).join(', ')}…）` }
          const err = apply(doc)
          if (err) return { ok: false, error: err }
          return { ok: true, updated, missing: missing.length ? missing : undefined }
        },
      },
      {
        name: 'delete_cells',
        desc: '{ids:[…]} → 删除单元格（引用它们的连线一并删除，避免悬空）。',
        label: (a: Record<string, unknown>) => `删除 ${Array.isArray(a.ids) ? a.ids.length : '?'} 单元格`,
        exec: async (a: Record<string, unknown>) => {
          const ids = new Set(Array.isArray(a.ids) ? a.ids.filter((x): x is string => typeof x === 'string') : [])
          if (!ids.size) return { ok: false, error: 'ids 必填' }
          let doc: Document, root: Element
          try {
            ({ doc, root } = parseDrawioDoc(currentXML()))
          } catch (e) {
            return { ok: false, error: `当前图表解析失败：${e instanceof Error ? e.message : String(e)}` }
          }
          const cells = drawioCells(root)
          // 扩张删除集：source/target 指向被删单元格的边一并删。
          const kill = new Set(ids)
          for (const c of cells) {
            if (c.getAttribute('edge') === '1') {
              const s = c.getAttribute('source'), t = c.getAttribute('target')
              if ((s && kill.has(s)) || (t && kill.has(t))) kill.add(c.getAttribute('id') ?? '')
            }
          }
          let removed = 0
          for (const c of cells) {
            if (kill.has(c.getAttribute('id') ?? '')) { c.parentNode?.removeChild(c); removed++ }
          }
          if (!removed) return { ok: false, error: '未命中任何单元格 id' }
          const err = apply(doc)
          if (err) return { ok: false, error: err }
          return { ok: true, deleted: removed }
        },
      },
      {
        name: 'replace_diagram',
        desc: '{xml} → 整图替换（仅当重排全图/从零新建时使用；改动少数元素优先 insert_cells/update_cells）。',
        label: () => '整图替换',
        exec: async (a: Record<string, unknown>) => {
          const xml = String(a.xml ?? '')
          if (!/<(mxGraphModel|mxfile)[\s>]/.test(xml)) return { ok: false, error: 'xml 必填：完整 drawio XML' }
          const err = applyDiagramXML(xml)
          return err ? { ok: false, error: err } : { ok: true }
        },
      },
    ]
    // applyDiagramXML 为组件内闭包（ref 访问无状态依赖），工具仅经 ref 操作。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  /** 版本保护前置：应用前先把当前图表（最新已知 XML）保存为基线版本。 */
  const aiEnsureSaved = async (): Promise<{ versionId: string; version: number } | null> => {
    try {
      if (dirtyRef.current) {
        const ok = await saveDiagram(xmlRef.current, true)
        if (!ok) return null
      }
      const meta = await getFileMeta(fileId)
      return meta.current_version
        ? { versionId: meta.current_version.id, version: meta.current_version.version }
        : null
    } catch {
      return null
    }
  }

  /** 撤销回退后刷新画布：重新拉取文件内容，经 load 消息重载编辑器。 */
  const aiReload = async (): Promise<void> => {
    const text = await fetchFileText(fileId)
    xmlRef.current = text.trim() ? text : EMPTY_DRAWIO_XML
    dirtyRef.current = false
    frameRef.current?.contentWindow?.postMessage(
      JSON.stringify({ action: 'load', xml: xmlRef.current, autosave: 1 }),
      '*',
    )
    try {
      setFile(await getFileMeta(fileId))
    } catch {
      // 版本号刷新失败不影响回退结果
    }
  }

  /** 头部下拉快捷指令：打开面板并透传给 AIEditChat 自动执行。 */
  const openAiChatWith = (cmd: AIQuickCommand) => {
    setAiChatOpen(true)
    setAiQuick(cmd)
  }

  // 未保存兜底提示：仅保存失败/静置窗口内退出时拦截（dirty 由 autosave
  // 事件驱动）；退出确认流程（closingRef）不叠加浏览器原生弹窗。
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
        <Button type="text" size="small" onClick={exitWithConfirm}>← 返回</Button>
        <h2 className="editor-title">{file?.name ?? '加载中…'}</h2>
        {versionNo !== undefined && <span className="badge current">当前版本 v{versionNo}</span>}
        {saving && <span className="badge uploading">保存中…</span>}
        {/* AI 统一入口：完整 AIEditChat 右侧面板（applyKind=drawio-xml，可
            修改模式下 AI 生成完整 drawio XML 自动替换画布并落新版本、版本
            保护可撤销）。原「AI 生成」mermaid 小弹窗（AIDrawio）已下线。
            与下方提示均并入顶栏同一行（不新增行挤压编辑区高度）。 */}
        <AIEditChatButton open={aiChatOpen} onToggle={() => setAiChatOpen((v) => !v)} onQuick={openAiChatWith} kind="drawio-xml" disabled={loading || !!frameError} />
        {!viewMode && <span className="muted drawio-save-hint">Ctrl+S 保存（不退出）</span>}
        {/* 官方 draw.io MCP 提示（顶栏行内，宽度不足省略收缩；title 悬浮看全文）。 */}
        {!frameError && aiOn && (
          <span
            className="muted drawio-mcp-hint"
            title={locale === 'zh-CN'
              ? '提示：平台 MCP 服务可添加官方 draw.io MCP（https://mcp.draw.io/mcp）在对话中生成/预览图表'
              : 'Tip: platform MCP services can add the official draw.io MCP (https://mcp.draw.io/mcp) to generate/preview diagrams in chat'}
          >
            {locale === 'zh-CN'
              ? '提示：平台 MCP 服务可添加官方 draw.io MCP 在对话中生成/预览图表'
              : 'Tip: add the official draw.io MCP to generate/preview diagrams in chat'}
          </span>
        )}
      </div>}

      {!viewMode && notice && !frameError && <div className="banner ok editor-hint">{notice}</div>}
      {error && !frameError && <div className="banner error">{error}</div>}
      {loading && !error && !frameError && <div className="hint">{viewMode ? '正在加载图表查看器…' : '正在加载图表编辑器…'}</div>}

      {/* 页面级加载失败错误卡（v2.7 反馈 8）：图表服务地址不可达（如
          服务端返回 127.0.0.1 回环地址）或静态资源拉取失败时不再白屏。 */}
      {frameError && (
        <EditorLoadError
          message={frameError}
          resourceLabel={locale === 'zh-CN' ? '图表服务地址' : 'Diagram service URL'}
          resourceUrl={editorURL}
          hints={locale === 'zh-CN' ? [
            '该地址来自平台 draw.io 集成配置，必须是当前浏览器可达的地址。',
            '跨机/容器访问时请勿使用 127.0.0.1 等回环地址：请将 DRAWIO_PUBLIC_URL 配置为外部可达地址，或经反向代理域名访问。',
            'HTTPS 页面无法加载 HTTP 资源（混合内容拦截），请保持协议一致；也可刷新页面重试。',
          ] : [
            'This URL comes from the platform draw.io integration config and must be reachable from your browser.',
            'For cross-machine/container access, avoid 127.0.0.1 loopback addresses: configure DRAWIO_PUBLIC_URL to an externally reachable URL, or access via a reverse-proxy domain.',
            'An HTTPS page cannot load HTTP resources (mixed content); keep the protocol consistent, or retry by reloading.',
          ]}
          onRetry={() => window.location.reload()}
          retryText={locale === 'zh-CN' ? '刷新重试' : 'Reload'}
        />
      )}

      {!error && !frameError && !loading && editorURL && (viewMode ? (
        <DrawioViewer
          baseURL={editorURL}
          xml={xmlRef.current}
          title={file?.name ?? '图表'}
          dark={colorMode === 'dark'}
          lang={viewerLang}
        />
      ) : (
        <div className="editor-with-ai">
          <div className="editor-shell">
            <iframe
              ref={frameRef}
              className="drawio-frame"
              src={editorURL}
              title={file?.name ?? '图表编辑器'}
              onError={() => {
                // iframe 加载失败（部分浏览器对无效 src 触发；多数不可达场景
                // 由上方 init 超时兜底覆盖）。
                setFrameError(locale === 'zh-CN'
                  ? '图表编辑器 iframe 加载失败，图表服务不可达'
                  : 'Failed to load the diagram editor iframe; the diagram service is unreachable')
              }}
            />
          </div>
          {/* AI 对话式创作面板（drawio XML 整体替换；应用前 aiEnsureSaved 保存
              基线版本，撤销回退后 aiReload 重载画布）。 */}
          {/* v7：agentTools 单元格级工具循环——read_diagram/list_cells/
              insert_cells/update_cells/delete_cells/replace_diagram；单轮
              整图 XML 通道保留为回退（agentTools 生效时由 AI 自主选择）。 */}
          <AIEditChat
            open={aiChatOpen}
            onClose={() => setAiChatOpen(false)}
            getTarget={aiGetTarget}
            getAllText={() => xmlRef.current}
            onApply={aiApply}
            fileId={fileId}
            ensureSaved={aiEnsureSaved}
            reload={aiReload}
            quickCommand={aiQuick}
            onQuickConsumed={() => setAiQuick(null)}
            applyKind="drawio-xml"
            agentTools={aiAgentTools}
            agentSystemExtra={[
              'drawio 宿主约束：',
              '- 画新图/补元素：insert_cells {xml}（mxCell 片段，顶点先于引用它的边，顶点须带 mxGeometry）；一次可传整图。',
              '- 改既有元素（文本/颜色/位置/尺寸）：先 list_cells 或 read_diagram 拿 id，再 update_cells 局部更新（styleSet 键值合并改样式）——不要重画整图。',
              '- 删除元素连带引用它的边自动清理（delete_cells）；整图重排才用 replace_diagram。',
              '- insert_cells/replace_diagram 的 XML 规范（忽略其中「输出单个代码块」的表述）：',
              DRAWIO_XML_GUIDE,
            ].join('\n')}
          />
        </div>
      ))}
    </div>
  )
}

