// 嵌入块 NodeView：按 kind 渲染——
// - drawio → DrawioExportImage（隐藏 iframe 经 drawio 导出 SVG/PNG 后以
//   <img> 呈现：无 viewer 脚本注入的 tooltip/状态条等任何残留 DOM，宽度
//   等比缩放高度贴合内容；导出失败回退 DrawioViewer 内联渲染）；
// - excalidraw → ExcalidrawViewer fit='width'（SVG 宽 100%、高按宽高比
//   自适应——容器高度随内容撑开，不出现内部滚动条）；
// - office/web/file → 卡片（图标 + 标题 + 新窗口打开）；file 为图片时附带
//   认证预览（fetchPreview blob URL）；
// 公开分享态（RichTextPublicContext 提供分享树 raw 基址）：资源按文件名在
// 分享树内探测（同级 assets/ 优先），drawio/白板取文本本地渲染，卡片新窗口
// 打开 raw URL；探测不到显示占位提示（不在分享范围内）。
// 统一右上角小工具条（可编辑态）：编辑（新窗口）、替换（经全局事件
// docflow:replace-embed 上提给编辑器层重开文件选择器——不直接在 NodeView
// 内渲染 Modal，避免 ProseMirror 搬移 DOM 与 React 冲突）、删除、刷新。
// 尺寸（v2.7）：width 25-100% + height（0=按内容自适应）；选中（可编辑）
// 浮出宽度快捷条、高度角柄拖拽与「自适应」复位。双击：编辑页=新窗口打开
// 对应编辑页；查看页=弹窗查看（经 OPEN_FILE_CARD_EVENT 上提）。从外部
// （编辑页新窗口）返回时 focus/visibilitychange 自动检测源文件版本变化并
// 刷新嵌入内容。
// readonly 态（editor 不可编辑）隐藏工具条；文件被删/不可访问的错误态在
// 可编辑态附「移除」按钮。
import { useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import type { NodeViewProps } from '@tiptap/react'
import { NodeViewWrapper } from '@tiptap/react'
import { Button, Tooltip } from 'antd'
import { FileSpreadsheet, FileText, Globe, Image, Maximize2, Network, PenLine } from 'lucide-react'
import {
  fetchFileText,
  fetchPreview,
  drawioStatus,
  getFileMeta,
  FileWithVersion,
} from '../../api'
import DrawioExportImage from '../DrawioExportImage'
import ExcalidrawViewer from '../ExcalidrawViewer'
import { DocflowEmbedAttrs } from './DocflowEmbed'
import { OPEN_FILE_CARD_EVENT } from './FileCardView'
import { useColorMode } from '../../theme'
import { useLocale } from '../../i18n'
import { useRichTextPublic } from './RichTextPublicContext'
import { resolvePublicUrl, fetchPublicText } from './publicResource'

/** 替换请求 → 编辑器层文件选择器（detail: { fileId, kind, pos }）。
 * pos 为发起替换的节点位置（getPos 实时值），编辑器层按位置精准替换，
 * 避免「按 fileId 扫描首个命中」在多个同源嵌入块时替换错节点（偶发被
 * 替换 bug 根因）。 */
export const REPLACE_EMBED_EVENT = 'docflow:replace-embed'

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

const EMBED_WIDTHS = [25, 50, 75, 100]

export default function EmbedView({ node, editor, selected, deleteNode, getPos }: NodeViewProps) {
  const attrs = node.attrs as DocflowEmbedAttrs
  const kind = attrs.kind
  const fileId = attrs.fileId
  const widthPct = Math.min(100, Math.max(25, Number(attrs.width) || 100))
  const heightPx = Math.max(0, Number(attrs.height) || 0)
  const publicBase = useRichTextPublic()
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
  const [publicURL, setPublicURL] = useState('')

  const displayName = attrs.title || meta?.name || (fileId ? `${fileId.slice(0, 8)}…` : (zh ? '（无效嵌入块）' : '(invalid embed)'))
  const editable = editor.isEditable

  // 拖拽高度角柄：拖拽中记录起始 Y 与起始高度，mousemove 全局监听。
  const resizingRef = useRef<{ startX: number; startY: number; startH: number; startW: number; parentW: number } | null>(null)
  const [resizing, setResizing] = useState(false)

  const reload = useCallback(() => {
    setImageURL('')
    setVersion((v) => v + 1)
  }, [])

  /** 更新本节点 attrs（width/height）：经 editor.chain().command 直接
   * setNodeMarkup（getPos 实时取位）。不使用 NodeViewProps.updateAttributes
   * ——实测该版本（@tiptap/react 2.27.3）在 React NodeView 内静默失效。 */
  const setAttrs = useCallback((patch: Partial<DocflowEmbedAttrs>) => {
    const pos = typeof getPos === 'function' ? Number(getPos()) : NaN
    if (!Number.isFinite(pos) || pos < 0) return
    const view = editor.view
    const target = view.state.doc.nodeAt(pos)
    if (!target || target.type.name !== 'docflowEmbed') return
    view.dispatch(view.state.tr.setNodeMarkup(pos, undefined, { ...target.attrs, ...patch }))
  }, [editor])

  // 拖拽高度角柄：mousemove 全局监听（拖拽中持续生效）。
  useEffect(() => {
    if (!resizing) return
    const onMove = (e: MouseEvent) => {
      const st = resizingRef.current
      if (!st) return
      const nextH = Math.max(120, Math.round(st.startH + (e.clientY - st.startY)))
      const nextW = Math.min(100, Math.max(25, Math.round(st.startW + ((e.clientX - st.startX) / Math.max(1, st.parentW)) * 100)))
      setAttrs({ height: nextH, width: nextW })
    }
    const onUp = () => {
      resizingRef.current = null
      setResizing(false)
    }
    window.addEventListener('mousemove', onMove)
    window.addEventListener('mouseup', onUp)
    return () => {
      window.removeEventListener('mousemove', onMove)
      window.removeEventListener('mouseup', onUp)
    }
  }, [resizing, setAttrs])

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
    setPublicURL('')
    const load = async () => {
      try {
        if (publicBase) {
          // 公开分享态：按文件名在分享树内探测 raw URL。
          if (!attrs.title) throw new Error(zh ? '资源不在分享范围内' : 'Resource not included in this share')
          const url = await resolvePublicUrl(publicBase, attrs.title)
          if (!alive) return
          if (!url) throw new Error(zh ? '资源不在分享范围内' : 'Resource not included in this share')
          setPublicURL(url)
          if (kind === 'drawio') {
            const xml = await fetchPublicText(url)
            if (!alive) return
            setDrawio({ baseURL: '/drawio', xml: xml || '<mxfile><diagram/></mxfile>' })
          } else if (kind === 'excalidraw') {
            const text = await fetchPublicText(url)
            if (!alive) return
            setScene(parseExcalidrawScene(text))
          }
          return
        }
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
        if (!m && (kind === 'drawio' || kind === 'excalidraw')) {
          throw new Error(zh ? '源文件已被删除或不可访问' : 'Source file deleted or inaccessible')
        }
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
  }, [kind, fileId, version, attrs.title, zh, publicBase])

  // 认证态（非公开分享）：源文件版本监测——从嵌入源文件的新窗口编辑页返回
  // （window focus / tab 可见）时拉取最新元数据，current_version 变化则自动
  // 重载嵌入内容（编辑保存 → 新版本 → 回到文档页即刷新，无需手动点「刷新」）。
  useEffect(() => {
    if (publicBase || !fileId || (kind !== 'drawio' && kind !== 'excalidraw' && kind !== 'office')) return
    let alive = true
    const knownVersion = meta?.current_version?.version
    const check = () => {
      void getFileMeta(fileId)
        .then((m) => {
          if (!alive) return
          const v = m.current_version?.version
          if (v !== undefined && knownVersion !== undefined && v !== knownVersion) reload()
          setMeta(m)
        })
        .catch(() => { /* 文件被删/无权限：保持现状 */ })
    }
    const onFocus = () => window.setTimeout(check, 150)
    const onVisible = () => { if (document.visibilityState === 'visible') window.setTimeout(check, 150) }
    window.addEventListener('focus', onFocus)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      alive = false
      window.removeEventListener('focus', onFocus)
      document.removeEventListener('visibilitychange', onVisible)
    }
    // meta 变化更新基线版本号（初次加载也经此登记）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [publicBase, fileId, kind, meta?.current_version?.version])

  // file 嵌入：attrs.title 为空时按文件真实扩展名补拉图片预览。
  useEffect(() => {
    if (publicBase || kind !== 'file' || !meta) return
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
  }, [kind, fileId, meta, loading, imageURL, publicBase])

  const openTarget = () => {
    if (publicBase) {
      if (publicURL) window.open(publicURL, '_blank', 'noopener')
      return
    }
    if (!fileId) return
    const url = new URL(targetPath(kind, fileId), window.location.origin)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  /** 双击：编辑页（可编辑）= 新窗口打开对应编辑页；查看页 = 弹窗查看
   *（复用文件卡片查看弹窗事件通道，编辑器层统一渲染 Modal）。 */
  const onDoubleClick = () => {
    if (!fileId) return
    if (publicBase) {
      openTarget()
      return
    }
    if (editable) {
      openTarget()
      return
    }
    window.dispatchEvent(new CustomEvent(OPEN_FILE_CARD_EVENT, { detail: { fileId, name: displayName } }))
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

  const requestReplace = () => {
    const pos = typeof getPos === 'function' ? Number(getPos()) : -1
    window.dispatchEvent(new CustomEvent(REPLACE_EMBED_EVENT, { detail: { fileId: attrs.fileId, kind, pos } }))
  }

  // 画布类嵌入（drawio/白板）内容容器：宽度按 attrs.width 百分比；高度
  // height=0 自适应（贴合内容，无内部滚动），>0 固定 px（超出裁剪/内部滚动）。
  const isCanvas = (k: string) => k === 'drawio' || k === 'excalidraw'
  const canvasStyle: React.CSSProperties = {
    width: `${widthPct}%`,
    ...(heightPx > 0 ? { height: heightPx } : {}),
  }

  return (
    <NodeViewWrapper
      className={`rich-text-embed${selected ? ' selected' : ''}`}
      data-kind={kind}
      data-file-id={fileId}
      data-title={attrs.title || undefined}
      data-width={String(widthPct)}
      data-height={heightPx > 0 ? String(heightPx) : undefined}
      style={{ width: `${widthPct}%` }}
    >
      {editable && fileId && (
        <div className="rich-text-embed-tools" contentEditable={false}>
          <Button type="text" size="small" title={zh ? '在新窗口编辑' : 'Edit in new window'} onMouseDown={(e) => e.preventDefault()} onClick={openTarget}>
            ✏ {editLabel}
          </Button>
          <Button
            type="text"
            size="small"
            title={zh ? '替换为其他文件' : 'Replace with another file'}
            onMouseDown={(e) => e.preventDefault()}
            onClick={requestReplace}
          >
            ⇄ {zh ? '替换' : 'Replace'}
          </Button>
          <Button
            type="text"
            size="small"
            title={zh ? '删除嵌入块' : 'Delete embed'}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => deleteNode()}
          >
            ✕
          </Button>
          <Button
            type="text"
            size="small"
            title={zh ? '刷新（重新拉取最新版本）' : 'Refresh (reload latest version)'}
            onMouseDown={(e) => e.preventDefault()}
            onClick={reload}
          >
            ↻
          </Button>
        </div>
      )}
      {/* 尺寸工具条：选中（可编辑）浮出。宽度 25/50/75/100 + 高度自适应复位
          （画布类拖拽角柄设固定高度后可一键回到贴合内容）。 */}
      {editable && selected && (
        <div className="rich-text-embed-size" contentEditable={false}>
          {EMBED_WIDTHS.map((w) => (
            <Tooltip key={w} title={`${zh ? '宽度' : 'Width'} ${w}%`}>
              <Button
                type="text"
                size="small"
                className={`rich-text-tbtn${widthPct === w ? ' active' : ''}`}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => setAttrs({ width: w })}
              >
                {w}%
              </Button>
            </Tooltip>
          ))}
          {isCanvas(kind) && heightPx > 0 && (
            <Tooltip title={zh ? '高度恢复自适应（贴合内容）' : 'Auto height (fit content)'}>
              <Button
                type="text"
                size="small"
                className="rich-text-tbtn"
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => setAttrs({ height: 0 })}
              >
                <Maximize2 size={14} strokeWidth={2} aria-hidden="true" />
              </Button>
            </Tooltip>
          )}
        </div>
      )}
      {error ? (
        <div className="rich-text-embed-error">
          <span className="icon">{embedIcon(kind, displayName)}</span>
          <span className="rich-text-embed-title">
            {displayName}
            <small>{error}</small>
          </span>
          {!publicBase && fileId && (
            <Button size="small" onClick={openTarget}>
              {zh ? '新窗口打开' : 'Open in new window'}
            </Button>
          )}
          {editable && (
            <Button size="small" danger onMouseDown={(e) => e.preventDefault()} onClick={() => deleteNode()}>
              {zh ? '移除' : 'Remove'}
            </Button>
          )}
        </div>
      ) : loading ? (
        <div className="rich-text-embed-hint">{zh ? '嵌入内容加载中…' : 'Loading embed…'}</div>
      ) : kind === 'drawio' && drawio ? (
        <div className="rich-text-embed-canvas" style={canvasStyle} key={version} onDoubleClick={onDoubleClick} title={zh ? '双击在新窗口编辑' : 'Double-click to edit'}>
          <DrawioExportImage baseURL={drawio.baseURL} xml={drawio.xml} title={displayName} dark={dark} lang={zh ? 'zh' : ''} />
        </div>
      ) : kind === 'excalidraw' && scene ? (
        <div className="rich-text-embed-canvas" style={canvasStyle} key={version} onDoubleClick={onDoubleClick} title={zh ? '双击在新窗口编辑' : 'Double-click to edit'}>
          <ExcalidrawViewer elements={scene.elements} appState={scene.appState} files={scene.files} title={displayName} fit="width" />
        </div>
      ) : (
        <div className="rich-text-embed-card" role="button" tabIndex={0} onClick={openTarget} onDoubleClick={onDoubleClick} onKeyDown={(e) => { if (e.key === 'Enter') openTarget() }}>
          {kind === 'file' && (imageURL || (publicURL && /\.(png|jpe?g|gif|webp|bmp|avif)$/i.test(publicURL))) ? (
            <img className="rich-text-embed-image" src={imageURL || publicURL} alt={displayName} />
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
      {/* 高度拖拽角柄：画布类嵌入选中（可编辑）时右下角浮出，纵向拖拽设定
          固定高度（px）；松手即写回 attrs.height。 */}
      {editable && selected && isCanvas(kind) && (
        <span
          className="rich-text-embed-resizer"
          contentEditable={false}
          role="separator"
          aria-orientation="vertical"
          title={zh ? '拖拽调整高度' : 'Drag to resize height'}
          onMouseDown={(e) => {
            e.preventDefault()
            e.stopPropagation()
            const parent = e.currentTarget.parentElement
            resizingRef.current = {
              startX: e.clientX,
              startY: e.clientY,
              startH: heightPx > 0 ? heightPx : Math.max(160, Math.round(parent?.getBoundingClientRect().height ?? 380)),
              startW: widthPct,
              parentW: parent?.parentElement?.getBoundingClientRect().width ?? 800,
            }
            setResizing(true)
          }}
        />
      )}
      {resizing && <div className="rich-text-embed-resizing-mask" contentEditable={false} />}
    </NodeViewWrapper>
  )
}
