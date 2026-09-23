// 独立只读查看页（/view/:fileId）：按扩展名/内容分发到各查看器——
// Office → ONLYOFFICE view；.drawio 与 .xml(mxfile 嗅探) → draw.io 静态
// 只读渲染；.excalidraw → Excalidraw 静态 SVG；.xmind → 前端离线解析
//（fflate 解 zip → Markdown → Markmap）；
// .mmd/.mermaid → mermaid；.md → Markdown（含 mermaid/markmap 代码块）；
// .html/.htm → 网页查看（resolve 现取 raw_url 后 sandbox iframe 渲染）；
// 其余文本/图片/PDF/网页包走通用预览。编辑路由（/edit、/drawio、
// /excalidraw）不受影响，仅本页只读。
// FileViewerDispatch / RawHtmlViewer 同时供按路径查看页（/view/by-path）
// 复用：fileId 由 resolve 结果提供（各编辑器页支持 fileId prop）。
import { useCallback, useEffect, useState } from 'react'
import { Button } from 'antd'
import { useParams, useSearchParams } from 'react-router-dom'
import {
  FileWithVersion,
  PreviewContent,
  fetchFileText,
  fetchPreview,
  getFileMeta,
  isDfdocFile,
  isDrawioFile,
  isDrawioXmlContent,
  isExcalidrawFile,
  isHtmlFile,
  isMermaidFile,
  isOfficeFile,
  isXmindFile,
  resolveFileById,
} from '../api'
import DrawioPage from './DrawioPage'
import DfdocEditorPage from './DfdocEditorPage'
import EditorPage from './EditorPage'
import ExcalidrawPage from './ExcalidrawPage'
import TextEditorPage from './TextEditorPage'
import MermaidDiagram from '../components/MermaidDiagram'
import XMindViewer from '../components/XMindViewer'
import { AISummaryInline } from '../components/AISummary'
import { setAIContextFile } from '../components/AIAssistant'
import { useColorMode } from '../theme'

/**
 * 网页查看（内容决定行为）：每次进入页面经 resolveFn 现取 raw_url（10 分钟
 * grant 足够单次查看），sandbox iframe 渲染（allow-scripts/form/popups/modals，
 * 无 same-origin——文档进入唯一化 origin 沙箱，碰不到主站源）。失败兜底给
 * 重试按钮（重新 resolve 换新 grant）。
 */
export function RawHtmlViewer({ resolveFn, title }: { resolveFn: () => Promise<string | null>; title: string }) {
  const [src, setSrc] = useState<string | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  const load = useCallback(() => {
    setLoading(true)
    setError('')
    resolveFn()
      .then((url) => {
        if (url) setSrc(url)
        else setError('无法获取网页内容（路径解析失败）')
        setLoading(false)
      })
      .catch((err) => {
        setError(err instanceof Error ? err.message : '网页加载失败')
        setLoading(false)
      })
  }, [resolveFn])

  useEffect(() => {
    load()
  }, [load])

  if (error) {
    return (
      <main className="text-editor-page">
        <div className="banner error">{error}</div>
        <div className="preview-foot">
          <Button type="primary" onClick={load}>重试</Button>
        </div>
      </main>
    )
  }
  if (loading || !src) {
    return <main className="text-editor-page viewer-only"><div className="text-editor-state">正在加载网页…</div></main>
  }
  return (
    <iframe
      className="standalone-viewer-frame"
      sandbox="allow-scripts allow-forms allow-popups allow-modals"
      src={src}
      title={title}
    />
  )
}

function GenericViewer({ fileId, name }: { fileId: string; name: string }) {
  const [content, setContent] = useState<PreviewContent | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    let objectURL = ''
    void fetchPreview(fileId)
      .then((result) => {
        if (!alive) {
          if (result.url?.startsWith('blob:')) URL.revokeObjectURL(result.url)
          return
        }
        objectURL = result.url?.startsWith('blob:') ? result.url : ''
        setContent(result)
      })
      .catch((err) => {
        if (alive) setError(err instanceof Error ? err.message : '查看内容加载失败')
      })
    return () => {
      alive = false
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
  }, [fileId])

  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>
  if (!content) return <main className="text-editor-page"><div className="text-editor-state">正在加载…</div></main>
  if (content.kind === 'image') return <main className="text-editor-page viewer-only"><img className="preview-image" src={content.url} alt={name} /></main>
  if (content.kind === 'pdf') return <iframe className="standalone-viewer-frame" src={content.url} title={name} />
  if (content.kind === 'webpkg') return <iframe className="standalone-viewer-frame" sandbox="allow-scripts" src={content.url} title={name} />
  if (content.kind === 'text') return <main className="text-editor-page"><pre className="preview-text">{content.text}</pre></main>
  return <main className="text-editor-page"><div className="empty">该文件类型暂不支持在线查看</div></main>
}

/** .mmd/.mermaid 文件查看：认证读取源码后 mermaid 只读渲染。 */
function MermaidFileViewer({ fileId }: { fileId: string }) {
  const dark = useColorMode() === 'dark'
  const [source, setSource] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let alive = true
    setLoading(true)
    setError('')
    void fetchFileText(fileId)
      .then((text) => {
        if (!alive) return
        setSource(text)
        setLoading(false)
      })
      .catch((err) => {
        if (!alive) return
        setError(err instanceof Error ? err.message : '图表源码加载失败')
        setLoading(false)
      })
    return () => { alive = false }
  }, [fileId])

  return (
    <main className="text-editor-page viewer-only diagram-viewer-page">
      {error && <div className="banner error">{error}</div>}
      {loading && !error && <div className="text-editor-state">正在加载图表…</div>}
      {!loading && !error && <MermaidDiagram source={source} dark={dark} />}
    </main>
  )
}

/** .xmind 文件查看：认证下载 ArrayBuffer 后前端离线解析渲染（见 XMindViewer）。 */
function XMindFileViewer({ fileId, name }: { fileId: string; name: string }) {
  return (
    <main className="text-editor-page viewer-only diagram-viewer-page">
      <XMindViewer fileId={fileId} title={name} />
    </main>
  )
}

/**
 * 按文件名/扩展名分发的只读查看器（/view/:fileId 与 /view/by-path 共用，
 * FileBrowser 查看弹窗亦经此内嵌）：fileId 由调用方提供（路由参数或
 * resolve 结果）；resolveRawUrl 供 .html 网页查看现取 raw_url（重试时
 * 复用）。office 文档统一内嵌 OnlyOffice 只读视图（EditorPage mode=view，
 * 弹窗与独立页渲染一致；EditorPage 在 .preview-embed 内已去视口化自适应）。
 * force（?open= 查看方式强制）：非空时跳过按扩展名的自动分发，直接进入
 * 指定查看器（「打开方式」子菜单 / 用户偏好经 routeFor 传递）；'raw' 走
 * RawSourceViewer（认证读取源文本；二进制给出下载提示）。
 */
export function FileViewerDispatch({
  fileId,
  name,
  resolveRawUrl,
  force,
}: {
  fileId: string
  name: string
  resolveRawUrl: () => Promise<string | null>
  /** 强制查看方式（openers.ViewMethod）；缺省按扩展名自动分发。 */
  force?: string
}) {
  const lower = name.toLowerCase()
  const endsXml = lower.endsWith('.xml')
  // .xml 内容嗅探：mxfile/mxGraphModel 根元素 → draw.io 图表查看，
  // 其余 XML 走通用预览（null = 嗅探中）。
  const [xmlIsDrawio, setXmlIsDrawio] = useState<boolean | null>(null)

  useEffect(() => {
    if (!endsXml) return
    let alive = true
    setXmlIsDrawio(null)
    void fetchFileText(fileId)
      .then((text) => { if (alive) setXmlIsDrawio(isDrawioXmlContent(text)) })
      .catch(() => { if (alive) setXmlIsDrawio(false) })
    return () => { alive = false }
  }, [fileId, endsXml])

  // force 兜底（与打开方式菜单门槛同规则）：非法「扩展名 × 引擎」组合
  //（如 .dfrt 强制 office / .docx 强制 drawio）回落按扩展名自动分发，
  // 不再把文件塞进不认识的引擎（OnlyOffice fileType invalid 同类问题）。
  const forceOk =
    (force === 'office' && (isOfficeFile(lower) || /\.(pdf|txt|rtf)$/.test(lower))) ||
    (force === 'drawio' && isDrawioFile(lower)) ||
    (force === 'excalidraw' && isExcalidrawFile(lower)) ||
    (force === 'xmind' && isXmindFile(lower)) ||
    (force === 'richtext' && (isDfdocFile(lower) || /\.(md|markdown|mdx)$/.test(lower))) ||
    force === 'raw'
  const effForce = forceOk ? force : undefined
  if (effForce === 'office') return <EditorPage mode="view" fileId={fileId} />
  if (effForce === 'drawio') return <DrawioPage mode="view" fileId={fileId} />
  if (effForce === 'excalidraw') return <ExcalidrawPage mode="view" fileId={fileId} />
  if (effForce === 'xmind') return <XMindFileViewer fileId={fileId} name={name} />
  // richtext 查看方式：.dfdoc → Tiptap 只读渲染；.md 家族 → Markdown 渲染。
  if (effForce === 'richtext') {
    if (isDfdocFile(lower)) return <DfdocEditorPage mode="view" fileId={fileId} />
    return <TextEditorPage kind="markdown" mode="view" fileId={fileId} />
  }
  if (effForce === 'raw') return <RawSourceViewer fileId={fileId} />
  if (isOfficeFile(lower)) return <EditorPage mode="view" fileId={fileId} />
  if (isDrawioFile(lower)) return <DrawioPage mode="view" fileId={fileId} />
  if (isExcalidrawFile(lower)) return <ExcalidrawPage mode="view" fileId={fileId} />
  if (isDfdocFile(lower)) return <DfdocEditorPage mode="view" fileId={fileId} />
  if (isXmindFile(lower)) return <XMindFileViewer fileId={fileId} name={name} />
  if (isMermaidFile(lower)) return <MermaidFileViewer fileId={fileId} />
  if (endsXml) {
    if (xmlIsDrawio === null) return <main className="text-editor-page"><div className="text-editor-state">正在加载…</div></main>
    if (xmlIsDrawio) return <DrawioPage mode="view" fileId={fileId} />
    return <GenericViewer fileId={fileId} name={name} />
  }
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return <TextEditorPage kind="markdown" mode="view" fileId={fileId} />
  if (isHtmlFile(lower)) return <RawHtmlViewer title={name} resolveFn={resolveRawUrl} />
  if (lower.endsWith('.css')) return <TextEditorPage kind="css" mode="view" fileId={fileId} />
  if (lower.endsWith('.js') || lower.endsWith('.mjs') || lower.endsWith('.json')) return <TextEditorPage kind="javascript" mode="view" fileId={fileId} />
  if (lower.endsWith('.txt')) return <TextEditorPage kind="text" mode="view" fileId={fileId} />
  return <GenericViewer fileId={fileId} name={name} />
}

/** 强制「原始内容」查看（?open=raw）：认证读取源文本按 <pre> 渲染；
 * 读取失败（二进制/无权限）给出下载提示（raw 查看对二进制的兜底语义）。 */
function RawSourceViewer({ fileId }: { fileId: string }) {
  const [text, setText] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)
  useEffect(() => {
    let alive = true
    setFailed(false)
    setText(null)
    void fetchFileText(fileId)
      .then((t) => { if (alive) setText(t) })
      .catch(() => { if (alive) setFailed(true) })
    return () => { alive = false }
  }, [fileId])
  if (failed) {
    return (
      <main className="text-editor-page">
        <div className="empty">该文件为二进制内容，暂不支持原始查看，请下载后查看</div>
      </main>
    )
  }
  if (text === null) return <main className="text-editor-page"><div className="text-editor-state">正在加载…</div></main>
  return <main className="text-editor-page viewer-only"><pre className="preview-text">{text}</pre></main>
}

/**
 * 按文件名/扩展名分发的编辑器组件（v2.7 弹窗内编辑复用）：与
 * FileViewerDispatch 同口径的编辑分发——office → EditorPage（弹窗内嵌
 * OnlyOffice，二次初始化已修：全新 placeholder + api.js 常驻）；drawio →
 * DrawioPage 编辑；excalidraw → ExcalidrawPage 编辑；.dfrt/.dfdoc →
 * DfdocEditorPage；md 家族 → TextEditorPage(markdown)；css/js → code；
 * 其余文本 → TextEditorPage(text)。不支持编辑的类型回退查看。
 */
export function FileEditorDispatch({ fileId, name, force }: { fileId: string; name: string; force?: string }) {
  const lower = name.toLowerCase()
  // force（?open=）兜底：非法「扩展名 × 引擎」组合回落按扩展名自动分发
  //（与 ViewerPage/ByPathPage 同规则，防 OnlyOffice fileType invalid 同类）。
  const forceOk =
    (force === 'office' && (isOfficeFile(lower) || /\.(pdf|txt|rtf)$/.test(lower))) ||
    (force === 'drawio' && isDrawioFile(lower)) ||
    (force === 'excalidraw' && isExcalidrawFile(lower)) ||
    force === 'richtext' ||
    force === 'text'
  const effForce = forceOk ? force : undefined
  if (effForce === 'office') return <EditorPage mode="edit" fileId={fileId} />
  if (effForce === 'drawio') return <DrawioPage mode="edit" fileId={fileId} />
  if (effForce === 'excalidraw') return <ExcalidrawPage mode="edit" fileId={fileId} />
  if (effForce === 'richtext') {
    if (isDfdocFile(lower)) return <DfdocEditorPage mode="edit" fileId={fileId} />
    return <TextEditorPage kind="markdown" mode="edit" fileId={fileId} />
  }
  if (effForce === 'text') {
    const kind = lower.endsWith('.css') ? 'css' : lower.endsWith('.js') || lower.endsWith('.mjs') ? 'javascript' : lower.endsWith('.json') ? 'javascript' : 'text'
    return <TextEditorPage kind={kind} mode="edit" fileId={fileId} />
  }
  if (isOfficeFile(lower)) return <EditorPage mode="edit" fileId={fileId} />
  if (isDrawioFile(lower)) return <DrawioPage mode="edit" fileId={fileId} />
  if (isExcalidrawFile(lower)) return <ExcalidrawPage mode="edit" fileId={fileId} />
  if (isDfdocFile(lower)) return <DfdocEditorPage mode="edit" fileId={fileId} />
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return <TextEditorPage kind="markdown" mode="edit" fileId={fileId} />
  if (lower.endsWith('.css') || lower.endsWith('.js') || lower.endsWith('.mjs') || lower.endsWith('.json')) {
    return <TextEditorPage kind={lower.endsWith('.css') ? 'css' : 'javascript'} mode="edit" fileId={fileId} />
  }
  return <TextEditorPage kind="text" mode="edit" fileId={fileId} />
}

/**
 * UUID 直链编辑分发页（/edit/:fileId）：与 /view/:fileId 的 ViewerPage 同构
 * ——先取文件元数据，再按扩展名（及 ?open= 偏好，非法组合兜底回落）分发
 * 对应编辑器。此前该路由固定渲染 OnlyOffice（EditorPage），文件页在无命名
 * 空间/搜索模式下 routeFor 回退 UUID 直链时，txt/md/drawio/.dfrt 全被塞进
 * OnlyOffice（"txt 变 word"、OnlyOffice fileType invalid 的根因）。
 */
export function EditDispatchPage() {
  const { fileId = '' } = useParams()
  const [searchParams] = useSearchParams()
  const forceOpen = searchParams.get('open') ?? undefined
  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    void getFileMeta(fileId)
      .then((meta) => { if (alive) setFile(meta) })
      .catch((err) => { if (alive) setError(err instanceof Error ? err.message : '文件信息加载失败') })
    return () => { alive = false }
  }, [fileId])

  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>
  if (!file) return <main className="text-editor-page"><div className="text-editor-state">正在加载…</div></main>
  return <FileEditorDispatch fileId={file.id} name={file.name} force={forceOpen} />
}

export default function ViewerPage() {
  const { fileId = '' } = useParams()
  const [searchParams] = useSearchParams()
  // ?origin_content=1：resolve 携带 origin_content，raw_url 按
  // CONTENT_PUBLIC_BASE_URL 绝对化（跨 origin 内容域场景；未配置回退相对）。
  const originContent = searchParams.get('origin_content') === '1'
  // ?open=<viewMethod>：强制查看方式（「打开方式」子菜单 / 用户偏好传递）。
  const forceOpen = searchParams.get('open') ?? undefined
  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    void getFileMeta(fileId)
      .then((meta) => { if (alive) setFile(meta) })
      .catch((err) => { if (alive) setError(err instanceof Error ? err.message : '文件信息加载失败') })
    return () => { alive = false }
  }, [fileId])

  // AI 助手上下文：查看页打开即登记当前文件（全局 AI 侧边栏快捷摘要用）。
  useEffect(() => {
    if (file) setAIContextFile({ fileId: file.id, fileName: file.name })
    return () => setAIContextFile(null)
  }, [file])

  // .html 网页查看：由 fileId 重建命名空间与路径（沿 parent 链上溯）后
  // resolve 现取 raw_url；grant 10 分钟，失败可重试。
  const resolveRawUrl = useCallback(async () => {
    try {
      const r = await resolveFileById(fileId, { mode: 'view', originContent })
      return r.raw_url
    } catch {
      return null
    }
  }, [fileId, originContent])

  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>
  if (!file) return <main className="text-editor-page"><div className="text-editor-state">正在加载…</div></main>

  return (
    <>
      <div className="viewer-ai-actions"><AISummaryInline fileId={file.id} /></div>
      <FileViewerDispatch fileId={file.id} name={file.name} resolveRawUrl={resolveRawUrl} force={forceOpen} />
    </>
  )
}
