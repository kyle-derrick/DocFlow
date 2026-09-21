// draw.io 嵌入块的「静态图」渲染（任务：hover 残留浮层根除）：
// 后端无 drawio→PNG/SVG 转换端点（convert 仅 xmind→md，webpkg 为网页包），
// 故在文档流外挂一个隐藏 embed iframe，经 postMessage JSON 协议让 drawio
// 自身把图表导出为 SVG/PNG，拿到结果后立即销毁 iframe，文档内只保留
// <img>——查看器脚本注入的 tooltip/浮层 div 不再出现在页面 DOM。
// 导出失败（超时/协议不支持）回退 DrawioViewer 内联渲染（行为不劣化）。
import { useEffect, useRef, useState } from 'react'
import DrawioViewer from './DrawioViewer'

interface DrawioExportImageProps {
  /** draw.io 静态镜像基地址（/drawio/config 返回的浏览器可达地址）。 */
  baseURL: string
  /** mxfile/mxGraphModel XML 文本。 */
  xml: string
  title: string
  dark?: boolean
  lang?: string
  /** 导出格式：xmlsvg（矢量，默认）/ xmlpng。 */
  format?: 'xmlsvg' | 'xmlpng'
}

/** 隐藏 iframe 导出超时（含编辑器加载；超时回退内联渲染）。 */
const EXPORT_TIMEOUT_MS = 15000

interface DrawioExportMessage {
  event?: string
  action?: string
  message?: unknown
  data?: unknown
  format?: string
}

/** embed 查看器 URL（proto=json；viewer=1 只读 + 导出能力）。 */
function embedViewerURL(base: string, lang: string, dark: boolean): string {
  const trimmed = base.replace(/\/+$/, '')
  const params = new URLSearchParams({
    embed: '1',
    proto: 'json',
    viewer: '1',
    spin: '1',
    lang,
    ui: dark ? 'dark' : 'min',
  })
  return `${trimmed}/?${params.toString()}`
}

/** 导出结果 → 可直接给 <img src> 的 URL（dataURL 原样；SVG 文本转 blob）。 */
function toImageURL(message: unknown): string | null {
  // 新版 drawio（27.x）export 事件载荷：{message:{action,format}（请求回显）,
  // xml:<mxfile 源>, data:<data:image/svg+xml;base64,... 导出图>}——图片在
  // data 字段；旧版协议为纯字符串载荷（message 本身即图片），两者兼容。
  const pick = (v: unknown): string | null => {
    if (typeof v !== 'string' || v === '') return null
    if (v.startsWith('data:')) return v
    if (v.startsWith('<')) return URL.createObjectURL(new Blob([v], { type: 'image/svg+xml' }))
    // 纯 base64（无 data: 前缀）按 png 处理。
    if (/^[A-Za-z0-9+/=]+$/.test(v.slice(0, 64))) return `data:image/png;base64,${v}`
    return null
  }
  if (typeof message === 'string') return pick(message)
  if (message && typeof message === 'object') {
    const o = message as { data?: unknown; message?: unknown; xml?: unknown }
    return pick(o.data) ?? pick(o.message) ?? null
  }
  return null
}

export default function DrawioExportImage({
  baseURL,
  xml,
  title,
  dark = false,
  lang = '',
  format = 'xmlsvg',
}: DrawioExportImageProps) {
  const [url, setUrl] = useState('')
  const [failed, setFailed] = useState(false)
  // blob URL 卸载回收。
  const blobRef = useRef('')

  useEffect(() => {
    let alive = true
    let objectURL = ''
    setUrl('')
    setFailed(false)
    const frameHost = document.createElement('div')
    frameHost.className = 'drawio-export-frame-host'
    const frame = document.createElement('iframe')
    frame.title = 'drawio-export'
    frame.src = embedViewerURL(baseURL, lang, dark)
    frameHost.appendChild(frame)
    document.body.appendChild(frameHost)

    const cleanup = () => {
      window.removeEventListener('message', onMessage)
      window.clearTimeout(timer)
      frameHost.remove()
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
    const fail = () => {
      if (!alive) return
      setFailed(true)
      cleanup()
    }
    const timer = window.setTimeout(fail, EXPORT_TIMEOUT_MS)

    const onMessage = (e: MessageEvent) => {
      if (e.source !== frame.contentWindow) return
      let msg: DrawioExportMessage
      try {
        msg = JSON.parse(String(e.data)) as DrawioExportMessage
      } catch {
        return
      }
      if (msg.event === 'init') {
        frame.contentWindow?.postMessage(JSON.stringify({ action: 'load', xml, autosave: 0 }), '*')
        frame.contentWindow?.postMessage(JSON.stringify({ action: 'export', format }), '*')
        return
      }
      if (msg.event === 'export') {
        const image = toImageURL(msg)
        if (!alive) {
          if (image && image.startsWith('blob:')) URL.revokeObjectURL(image)
          return
        }
        if (!image) {
          fail()
          return
        }
        if (image.startsWith('blob:')) {
          objectURL = image
          blobRef.current = image
        }
        setUrl(image)
        cleanup()
      }
    }
    window.addEventListener('message', onMessage)
    return () => {
      alive = false
      window.removeEventListener('message', onMessage)
      window.clearTimeout(timer)
      frameHost.remove()
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
    // xml 变化（嵌入刷新/替换）重新导出。
  }, [baseURL, xml, dark, lang, format])

  useEffect(() => () => {
    if (blobRef.current) URL.revokeObjectURL(blobRef.current)
  }, [])

  if (failed || !url) {
    // 导出未就绪/失败：内联查看器兜底（保持可见性，不留空白）。
    return <DrawioViewer baseURL={baseURL} xml={xml} title={title} dark={dark} lang={lang} />
  }
  return (
    <img
      className="drawio-export-image"
      src={url}
      alt={title}
      draggable={false}
    />
  )
}
