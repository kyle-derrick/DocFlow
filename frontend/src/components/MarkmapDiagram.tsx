// Markmap 思维导图只读渲染组件（markmap-lib + markmap-view，均动态
// import 独立 chunk）：
// - markmap-lib 的 Transformer 仅解析 Markdown 生成节点树；transform 返回
//   的 features/assets（katex、嵌入 ```js 等）一律不加载、不执行——纯静态
//   SVG 渲染，嵌入脚本不会运行；
// - markmap-view 的 Markmap.create 支持 d3 缩放/平移（仅查看交互，无编辑），
//   autoFit 自适应容器，卸载时 destroy 清理监听。
// 用于 Markdown ```markmap 代码块（内容为 Markdown 列表文本）。
import { useEffect, useRef, useState } from 'react'
import type { Markmap } from 'markmap-view'

interface MarkmapDiagramProps {
  /** Markdown 源（通常为列表结构）。 */
  source: string
}

export default function MarkmapDiagram({ source }: MarkmapDiagramProps) {
  const svgRef = useRef<SVGSVGElement | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    let markmap: Markmap | null = null
    const render = async () => {
      setError('')
      try {
        const [{ Transformer }, { Markmap }] = await Promise.all([
          import('markmap-lib'),
          import('markmap-view'),
        ])
        // 忽略 transform 返回的 features：不加载任何 JS/CSS 资源
        const { root } = new Transformer().transform(source)
        if (!alive || !svgRef.current) return
        markmap = Markmap.create(svgRef.current, { autoFit: true, zoom: true, pan: true }, root)
        void markmap.fit()
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '思维导图渲染失败')
      }
    }
    void render()
    return () => {
      alive = false
      markmap?.destroy()
      markmap = null
    }
  }, [source])

  if (error) {
    return (
      <div className="markmap-diagram markmap-diagram-error">
        <div className="banner error">Markmap 渲染失败：{error}</div>
        <pre className="preview-text">{source}</pre>
      </div>
    )
  }
  return <svg ref={svgRef} className="markmap-diagram" />
}
