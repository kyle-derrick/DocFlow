import { useEffect, useRef, useState } from 'react'

type ExportToSvg = typeof import('@excalidraw/excalidraw')['exportToSvg']
type ExportOptions = Parameters<ExportToSvg>[0]

interface ExcalidrawViewerProps {
  elements: ExportOptions['elements']
  appState: ExportOptions['appState']
  files: ExportOptions['files']
  title: string
}

export default function ExcalidrawViewer({ elements, appState, files, title }: ExcalidrawViewerProps) {
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
        svg.style.height = '100%'
        svg.style.display = 'block'
        hostRef.current.replaceChildren(svg)
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '白板渲染失败')
      }
    }
    void render()
    return () => { alive = false }
  }, [appState, elements, files, title])

  if (error) return <div className="banner error">{error}</div>
  return <div ref={hostRef} className="excalidraw-viewer" />
}
