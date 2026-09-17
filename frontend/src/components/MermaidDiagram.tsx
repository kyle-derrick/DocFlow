// Mermaid 图表只读渲染组件（mermaid v12，动态 import 独立 chunk）：
// - securityLevel 固定 'strict'（亦为默认）：禁用 html 标签与点击回调，
//   图源中的脚本/html 注入不生效；
// - 主题跟随站点明暗（dark → mermaid 'dark' 主题），初始化按主题变化重设；
// - render 结果为 svg 字符串，容器限定 max-width 100% 自适应缩放。
// 用于 Markdown ```mermaid 代码块与 .mmd/.mermaid 文件查看。
import { useEffect, useRef, useState } from 'react'

/** mermaid 全局初始化状态与当前主题（会话级，多实例共用）。 */
let initialized = false
let initializedTheme = ''
let renderSeq = 0

interface MermaidDiagramProps {
  /** Mermaid 图表源码。 */
  source: string
  /** 深色主题（跟随站点 data-mode）。 */
  dark?: boolean
}

export default function MermaidDiagram({ source, dark = false }: MermaidDiagramProps) {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    const render = async () => {
      setError('')
      try {
        const mermaid = (await import('mermaid')).default
        const theme = dark ? 'dark' : 'default'
        if (!initialized || initializedTheme !== theme) {
          mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme })
          initialized = true
          initializedTheme = theme
        }
        renderSeq += 1
        const { svg } = await mermaid.render(`docflow-mermaid-${renderSeq}`, source)
        if (!alive || !hostRef.current) return
        hostRef.current.innerHTML = svg
        const svgEl = hostRef.current.querySelector('svg')
        if (svgEl) {
          svgEl.removeAttribute('width')
          svgEl.removeAttribute('height')
          svgEl.setAttribute('style', 'max-width:100%;height:auto;display:block;margin:0 auto')
        }
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : 'Mermaid 图表渲染失败')
      }
    }
    void render()
    return () => {
      alive = false
    }
  }, [source, dark])

  if (error) {
    return (
      <div className="mermaid-diagram mermaid-diagram-error">
        <div className="banner error">Mermaid 渲染失败：{error}</div>
        <pre className="preview-text">{source}</pre>
      </div>
    )
  }
  return <div ref={hostRef} className="mermaid-diagram" />
}
