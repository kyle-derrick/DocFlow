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
import { useParams } from 'react-router-dom'
import {
  FileWithVersion,
  PreviewContent,
  fetchFileText,
  fetchPreview,
  getFileMeta,
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
import EditorPage from './EditorPage'
import ExcalidrawPage from './ExcalidrawPage'
import TextEditorPage from './TextEditorPage'
import MermaidDiagram from '../components/MermaidDiagram'
import XMindViewer from '../components/XMindViewer'
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
          <button type="button" className="btn primary" onClick={load}>重试</button>
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
 * 按文件名/扩展名分发的只读查看器（/view/:fileId 与 /view/by-path 共用）：
 * fileId 由调用方提供（路由参数或 resolve 结果）；resolveRawUrl 供 .html
 * 网页查看现取 raw_url（重试时复用）。
 */
export function FileViewerDispatch({
  fileId,
  name,
  resolveRawUrl,
}: {
  fileId: string
  name: string
  resolveRawUrl: () => Promise<string | null>
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

  if (isOfficeFile(lower)) return <EditorPage mode="view" fileId={fileId} />
  if (isDrawioFile(lower)) return <DrawioPage mode="view" fileId={fileId} />
  if (isExcalidrawFile(lower)) return <ExcalidrawPage mode="view" fileId={fileId} />
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

export default function ViewerPage() {
  const { fileId = '' } = useParams()
  const [file, setFile] = useState<FileWithVersion | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    void getFileMeta(fileId)
      .then((meta) => { if (alive) setFile(meta) })
      .catch((err) => { if (alive) setError(err instanceof Error ? err.message : '文件信息加载失败') })
    return () => { alive = false }
  }, [fileId])

  // .html 网页查看：由 fileId 重建命名空间与路径（沿 parent 链上溯）后
  // resolve 现取 raw_url；grant 10 分钟，失败可重试。
  const resolveRawUrl = useCallback(async () => {
    try {
      const r = await resolveFileById(fileId, { mode: 'view' })
      return r.raw_url
    } catch {
      return null
    }
  }, [fileId])

  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>
  if (!file) return <main className="text-editor-page"><div className="text-editor-state">正在加载…</div></main>

  return <FileViewerDispatch fileId={file.id} name={file.name} resolveRawUrl={resolveRawUrl} />
}
