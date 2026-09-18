// 嵌入块 NodeView：按 kind 渲染——
// - drawio → 只读 DrawioViewer（外部静态镜像脚本，按需加载）；
// - excalidraw → ExcalidrawViewer（导出 SVG，编辑器包内部懒加载）；
// - office/web/file → 卡片（图标 + 标题 + 新窗口打开）；file 为图片时附带
//   认证预览（fetchPreview blob URL）；
// 统一右上角小工具条：编辑（按 kind 跳 /drawio /excalidraw /edit /view，
// returnTo=当前编辑路径）与刷新（重新拉当前版本并重挂载查看器）；
// readonly 态（editor 不可编辑）隐藏工具条。
import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import type { NodeViewProps } from '@tiptap/react'
import { Button } from 'antd'
import { FileSpreadsheet, FileText, Globe, Image, Network, PenLine } from 'lucide-react'
import {
  fetchFileText,
  fetchPreview,
  drawioStatus,
  getFileMeta,
  FileWithVersion,
} from '../../api'
import DrawioViewer from '../DrawioViewer'
import ExcalidrawViewer from '../ExcalidrawViewer'
import { DocflowEmbedAttrs } from './DocflowEmbed'
import { useColorMode } from '../../theme'
import { useLocale } from '../../i18n'

type ExportToSvg = typeof import('@excalidraw/excalidraw')['exportToSvg']
type ExportOptions = Parameters<ExportToSvg>[0]

/** Excalidraw 场景（与 ExcalidrawPage.parseScene 同规则的最小类型）。 */
interface ExcalidrawScene {
  elements: ExportOptions['elements']
  appState: Partial<ExportOptions['appState']>
  files: ExportOptions['files']
}

/** 解析 .excalidraw JSON；空/损坏回退空场景。 */
function parseExcalidrawScene(text: string): ExcalidrawScene {
  const trimmed = text.trim()
  if (trimmed) {
    try {
      const parsed = JSON.parse(trimmed) as { elements?: unknown; appState?: unknown; files?: unknown }
      if (Array.isArray(parsed.elements)) {
        return {
          elements: parsed.elements as ExcalidrawScene['elements'],
          appState: parsed.appState && typeof parsed.appState === 'object' ? (parsed.appState as ExcalidrawScene['appState']) : {},
          files: parsed.files && typeof parsed.files === 'object' ? (parsed.files as ExcalidrawScene['files']) : {},
        }
      }
    } catch {
      /* 损坏内容回退空场景 */
    }
  }
  return { elements: [], appState: {}, files: {} }
}

const IMAGE_EXTS = ['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'avif']

function isImageName(name: string): boolean {
  const i = name.lastIndexOf('.')
  return i >= 0 && IMAGE_EXTS.includes(name.slice(i + 1).toLowerCase())
}

/** 按 kind 决定编辑/打开目标路由。 */
function targetPath(kind: string, fileId: string): string {
  if (kind === 'drawio') return `/drawio/${fileId}`
  if (kind === 'excalidraw') return `/excalidraw/${fileId}`
  if (kind === 'office') return `/edit/${fileId}`
  return `/view/${fileId}`
}

/** 嵌入块类型图标（lucide，14px 线性）。 */
function embedIcon(kind: string, name: string): ReactNode {
  const props = { size: 14, strokeWidth: 2, 'aria-hidden': true } as const
  if (kind === 'drawio') return <Network {...props} />
  if (kind === 'excalidraw') return <PenLine {...props} />
  if (kind === 'web') return <Globe {...props} />
  if (kind === 'office') return <FileSpreadsheet {...props} />
  if (kind === 'file') return isImageName(name) ? <Image {...props} /> : <FileText {...props} />
  return <FileText {...props} />
}

export default function EmbedView({ node, editor, selected }: NodeViewProps) {
  const attrs = node.attrs as DocflowEmbedAttrs
  const kind = attrs.kind
  const fileId = attrs.fileId
  const dark = useColorMode() === 'dark'
  const locale = useLocale()
  const zh = locale === 'zh-CN'

  const [version, setVersion] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [meta, setMeta] = useState<FileWithVersion | null>(null)
  const [drawio, setDrawio] = useState<{ baseURL: string; xml: string } | null>(null)
  const [scene, setScene] = useState<ExcalidrawScene | null>(null)
  const [imageURL, setImageURL] = useState('')

  const displayName = attrs.title || meta?.name || (fileId ? `${fileId.slice(0, 8)}…` : (zh ? '（无效嵌入块）' : '(invalid embed)'))
  const editable = editor.isEditable

  useEffect(() => {
    if (!fileId) {
      setLoading(false)
      setError(zh ? '嵌入块缺少文件 ID' : 'Embed block is missing file ID')
      return
    }
    let alive = true
    let objectURL = ''
    setLoading(true)
    setError('')
    setDrawio(null)
    setScene(null)
    const load = async () => {
      try {
        const metaPromise = getFileMeta(fileId).catch(() => null)
        if (kind === 'drawio') {
          const [status, xml] = await Promise.all([drawioStatus(), fetchFileText(fileId)])
          if (!alive) return
          if (!status.enabled || !status.url) throw new Error(zh ? 'draw.io 集成未启用，无法内嵌预览' : 'draw.io integration is disabled')
          setDrawio({ baseURL: status.url, xml })
        } else if (kind === 'excalidraw') {
          const text = await fetchFileText(fileId)
          if (!alive) return
          setScene(parseExcalidrawScene(text))
        } else if (kind === 'file' && isImageName(attrs.title || '')) {
          // attrs.title 为空时无法预判图片，待 meta 返回后由第二轮 effect 处理。
          const preview = await fetchPreview(fileId)
          if (!alive) return
          if (preview.kind === 'image' && preview.url?.startsWith('blob:')) {
            objectURL = preview.url
            setImageURL(preview.url)
          }
        }
        const m = await metaPromise
        if (!alive) return
        setMeta(m)
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : (zh ? '嵌入内容加载失败' : 'Failed to load embed'))
      } finally {
        if (alive) setLoading(false)
      }
    }
    void load()
    return () => {
      alive = false
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
    // attrs.title 参与 file-图片 分支判定；版本号驱动「刷新」。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, fileId, version, attrs.title, zh])

  // file 嵌入：attrs.title 为空时按文件真实扩展名补拉图片预览。
  useEffect(() => {
    if (kind !== 'file' || !meta) return
    if (!isImageName(meta.name)) return
    if (imageURL || loading) return
    let alive = true
    let objectURL = ''
    void fetchPreview(fileId)
      .then((preview) => {
        if (!alive) {
          if (preview.url?.startsWith('blob:')) URL.revokeObjectURL(preview.url)
          return
        }
        if (preview.kind === 'image' && preview.url?.startsWith('blob:')) {
          objectURL = preview.url
          setImageURL(preview.url)
        }
      })
      .catch(() => { /* 预览失败回退卡片 */ })
    return () => {
      alive = false
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, fileId, meta, loading, imageURL])

  const openTarget = () => {
    if (!fileId) return
    const url = new URL(targetPath(kind, fileId), window.location.origin)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  const kindLabel: Record<string, string> = {
    drawio: 'draw.io 图表',
    excalidraw: 'Excalidraw 白板',
    office: 'Office 文档',
    web: '网页',
    file: '文件',
  }
  const editLabel = kind === 'drawio' || kind === 'excalidraw' || kind === 'office'
    ? (zh ? '编辑' : 'Edit')
    : (zh ? '打开' : 'Open')

  return (
    <div className={`rich-text-embed${selected ? ' selected' : ''}`} data-kind={kind}>
      {editable && fileId && (
        <div className="rich-text-embed-tools" contentEditable={false}>
          <Button type="text" size="small" title={zh ? '在新窗口编辑' : 'Edit in new window'} onMouseDown={(e) => e.preventDefault()} onClick={openTarget}>
            ✏ {editLabel}
          </Button>
          <Button
            type="text"
            size="small"
            title={zh ? '刷新（重新拉取最新版本）' : 'Refresh (reload latest version)'}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => {
              setImageURL('')
              setVersion((v) => v + 1)
            }}
          >
            ↻
          </Button>
        </div>
      )}
      {error ? (
        <div className="rich-text-embed-error">
          <span className="icon">{embedIcon(kind, displayName)}</span>
          <span className="rich-text-embed-title">
            {displayName}
            <small>{error}</small>
          </span>
          {fileId && (
            <Button size="small" onClick={openTarget}>
              {zh ? '新窗口打开' : 'Open in new window'}
            </Button>
          )}
        </div>
      ) : loading ? (
        <div className="rich-text-embed-hint">{zh ? '嵌入内容加载中…' : 'Loading embed…'}</div>
      ) : kind === 'drawio' && drawio ? (
        <div className="rich-text-embed-canvas" key={version}>
          <DrawioViewer baseURL={drawio.baseURL} xml={drawio.xml} title={displayName} dark={dark} lang={zh ? 'zh' : ''} />
        </div>
      ) : kind === 'excalidraw' && scene ? (
        <div className="rich-text-embed-canvas" key={version}>
          <ExcalidrawViewer elements={scene.elements} appState={scene.appState} files={scene.files} title={displayName} />
        </div>
      ) : (
        <div className="rich-text-embed-card" role="button" tabIndex={0} onClick={openTarget} onKeyDown={(e) => { if (e.key === 'Enter') openTarget() }}>
          {kind === 'file' && imageURL ? (
            <img className="rich-text-embed-image" src={imageURL} alt={displayName} />
          ) : (
            <span className="rich-text-embed-icon">{embedIcon(kind, displayName)}</span>
          )}
          <span className="rich-text-embed-title">
            {displayName}
            <small>{kindLabel[kind] ?? kind}</small>
          </span>
          <span className="rich-text-embed-open muted">{zh ? '新窗口打开 ↗' : 'Open ↗'}</span>
        </div>
      )}
    </div>
  )
}
