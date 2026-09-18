// draw.io 纯只读渲染组件：加载 Caddy 静态镜像 /drawio 下的官方
// viewer-static.min.js（GraphViewer，约 3.6MB 自包含脚本），以 data-mxgraph
// 声明式配置渲染只读图表——不加载 embed 编辑器、无工具栏、不可编辑。
// - 资源路径（styles/shapes/stencils/img/mxgraph/resources）在脚本加载前
//   指向本地 /drawio 镜像（脚本默认指向 viewer.diagrams.net CDN，须覆盖）；
// - fit 页宽：容器内联宽高 100% + auto-fit（graph.fit 缩放到容器宽，上限
//   1 不放大），实测按页宽等比缩放；高度撑满查看区、超出滚动由外层处理；
// - 深浅主题：data-mxgraph 的 dark-mode 键跟随站点 data-mode，切换时重设
//   配置并 processElements() 重建（其内部会先清空容器）；
// - 悬停空 tooltip：内部 mxGraph TooltipHandler 默认启用，悬停图形会在
//   <body> 挂空内容 .mxTooltip 浮层（即「悬停出现无内容小框」），脚本为
//   自包含闭包且无配置开关可关——由 styles.css 全局隐藏 div.mxTooltip；
// - 中文：脚本加载前设 mxLanguage=zh，加载后补拉本地 resources/dia_zh
//   （viewer UI 文本极少，主要为提示/错误文案；同源请求无 CORS 问题）。
import { useEffect, useRef, useState } from 'react'

declare global {
  interface Window {
    GraphViewer?: { processElements: () => void }
    mxResources?: { add: (basename: string, lan?: string, callback?: () => void) => void }
    RESOURCES_PATH?: string
    STYLE_PATH?: string
    SHAPES_PATH?: string
    STENCIL_PATH?: string
    GRAPH_IMAGE_PATH?: string
    mxImageBasePath?: string
    mxBasePath?: string
    mxLanguage?: string
  }
}

/** viewer-static.min.js 会话级只加载一次（含资源路径初始化，语言取首次调用值）。 */
let viewerScriptPromise: Promise<void> | null = null
let viewerScriptLang = ''

function loadViewerScript(baseURL: string, lang: string): Promise<void> {
  if (window.GraphViewer) return Promise.resolve()
  if (viewerScriptPromise && viewerScriptLang === lang) return viewerScriptPromise
  if (viewerScriptPromise) return viewerScriptPromise // 已加载：语言以首次为准，静默沿用

  const base = baseURL.replace(/\/+$/, '')
  window.STYLE_PATH = `${base}/styles`
  window.SHAPES_PATH = `${base}/shapes`
  window.STENCIL_PATH = `${base}/stencils`
  window.GRAPH_IMAGE_PATH = `${base}/img`
  window.mxImageBasePath = `${base}/mxgraph/images`
  window.mxBasePath = `${base}/mxgraph`
  window.RESOURCES_PATH = `${base}/resources`
  if (lang) window.mxLanguage = lang

  viewerScriptLang = lang
  viewerScriptPromise = new Promise((resolve, reject) => {
    const script = document.createElement('script')
    script.src = `${base}/js/viewer-static.min.js`
    script.async = true
    script.dataset.docflowDrawioViewer = 'true'
    script.onload = () => resolve()
    script.onerror = () => {
      viewerScriptPromise = null
      viewerScriptLang = ''
      reject(new Error('draw.io 静态查看器脚本加载失败'))
    }
    document.head.appendChild(script)
  })
  return viewerScriptPromise
}

/** 补拉中文资源（dia_zh.txt 覆盖内联英文文案；失败静默回退英文）。 */
function loadViewerResources(baseURL: string, lang: string): void {
  if (!lang || !window.mxResources) return
  const base = baseURL.replace(/\/+$/, '')
  try {
    window.mxResources.add(`${base}/resources/dia`, lang)
  } catch {
    // 资源拉取失败不影响图表渲染
  }
}

interface DrawioViewerProps {
  /** draw.io 静态镜像基地址（/drawio/config 返回的浏览器可达地址）。 */
  baseURL: string
  /** mxfile/mxGraphModel XML 文本。 */
  xml: string
  title: string
  /** 深色主题（跟随站点 data-mode）。 */
  dark?: boolean
  /** 界面语言（'zh' 启用中文资源，其余沿用默认英文）。 */
  lang?: string
}

export default function DrawioViewer({ baseURL, xml, title, dark = false, lang = '' }: DrawioViewerProps) {
  const containerRef = useRef<HTMLDivElement | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    const render = async () => {
      setError('')
      try {
        await loadViewerScript(baseURL, lang)
        if (!alive || !containerRef.current || !window.GraphViewer) return
        loadViewerResources(baseURL, lang)
        const target = containerRef.current
        target.className = 'mxgraph drawio-viewer-content'
        target.setAttribute('data-mxgraph', JSON.stringify({
          xml,
          nav: false,
          resize: false,
          'auto-fit': true,
          lightbox: false,
          toolbar: '',
          // GraphViewer 以严格相等比较字符串（'dark'|'auto'），布尔值无效
          ...(dark ? { 'dark-mode': 'dark' } : {}),
        }))
        window.GraphViewer.processElements()
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : 'draw.io 图表渲染失败')
      }
    }
    void render()
    return () => { alive = false }
  }, [baseURL, xml, dark, lang])

  if (error) return <div className="banner error">{error}</div>
  return (
    <div className={`drawio-viewer${dark ? ' dark' : ''}`} role="img" aria-label={title}>
      <div ref={containerRef} style={{ width: '100%', height: '100%' }} />
    </div>
  )
}
