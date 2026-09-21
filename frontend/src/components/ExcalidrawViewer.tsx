import { useEffect, useRef, useState } from 'react'

type ExportToSvg = typeof import('@excalidraw/excalidraw')['exportToSvg']
type ExportOptions = Parameters<ExportToSvg>[0]

interface ExcalidrawViewerProps {
  elements: ExportOptions['elements']
  appState: ExportOptions['appState']
  files: ExportOptions['files']
  title: string
  /**
   * fit='fill'（默认，独立查看页/弹窗）：SVG 100%×100% 铺满容器（容器定高，
   * 超高内部滚动）；fit='width'（富文本嵌入块）：SVG 宽 100%、高按内容
   * 宽高比自适应——容器高度随内容撑开，不出现内部滚动条。
   */
  fit?: 'fill' | 'width'
}

export default function ExcalidrawViewer({ elements, appState, files, title, fit = 'fill' }: ExcalidrawViewerProps) {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let alive = true
    const render = async () => {
      setError('')
      try {
        const { exportToSvg } = await import('@excalidraw/excalidraw')
        const svg = await exportToSvg({
          elements,
          appState: { ...appState, exportBackground: true },
          files,
          exportPadding: 20,
        })
        if (!alive || !hostRef.current) return
        svg.setAttribute('role', 'img')
        svg.setAttribute('aria-label', title)
        svg.removeAttribute('width')
        svg.removeAttribute('height')
        svg.style.width = '100%'
        // width 模式：高随 viewBox 宽高比自适应（内容多高容器多高，无滚动条）。
        svg.style.height = fit === 'width' ? 'auto' : '100%'
        svg.style.display = 'block'
        hostRef.current.replaceChildren(svg)
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '白板渲染失败')
      }
    }
    void render()
    return () => { alive = false }
  }, [appState, elements, files, title, fit])

  if (error) return <div className="banner error">{error}</div>
  return <div ref={hostRef} className={`excalidraw-viewer${fit === 'width' ? ' fit-width' : ''}`} />
}
