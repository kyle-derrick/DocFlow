// 文件卡片 NodeView：
// - 普通文件 = 内联芯片（图标+名称+大小）。点击行为——认证态经全局事件
//   docflow:open-file-card 上提给 RichTextEditor 层弹出查看弹窗
//   （FileViewerDispatch 懒加载；不直接在 NodeView 内渲染 Modal：ProseMirror
//   会搬移 NodeView 的 DOM，antd Modal 的 portal 卸载会与 React
//   reconciliation 冲突）；公开分享态解析 raw/share URL 后新窗口打开。
// - v2.7 内容嵌入：图片文件 = 直接显示图片（可调宽度 25/50/75/100%；公开
//   分享态经 raw/share URL 直读）；.xmind = 复用 XMindViewer 渲染思维导图
//   （可调宽度，解析失败占位；公开态 raw 白名单不含 xmind，保持芯片）；
//   其他文件保持芯片不变。内容嵌入为 inline-block 块视觉（不破坏段落
//   DOM 结构）。
// - 右键（仅编辑页）：上提 docflow:card-context 事件，编辑器层弹「编辑
//   （新窗口）」菜单（editorRouteFor 按扩展名分发编辑器路由）。
import { lazy, Suspense, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import type { NodeViewProps } from '@tiptap/react'
import { NodeViewWrapper } from '@tiptap/react'
import { Button, Tooltip } from 'antd'
import { FileSpreadsheet, FileText, Image as ImageIcon, Network, PenLine } from 'lucide-react'
import { fetchPreview, getFileMeta } from '../../api'
import { formatQuota } from '../FileBrowser'
import { useLocale } from '../../i18n'
import type { DocflowFileCardAttrs } from './DocflowFileCard'
import { useRichTextPublic } from './RichTextPublicContext'
import { resolvePublicUrl } from './publicResource'

// XMindViewer（fflate/markmap 产物懒加载，仅内容嵌入态拉取）。
const XMindViewer = lazy(() => import('../XMindViewer'))

/** 卡片点击 → 编辑器层查看弹窗（detail: { fileId, name }）。 */
export const OPEN_FILE_CARD_EVENT = 'docflow:open-file-card'

/** 卡片右键 → 编辑器层上下文菜单（detail: { fileId, name, x, y }）。 */
export const CARD_CONTEXT_EVENT = 'docflow:card-context'

const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'avif', 'svg'])
const OFFICE_EXTS = new Set(['doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx', 'odt', 'ods', 'odp'])

function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i >= 0 ? name.slice(i + 1).toLowerCase() : ''
}

function iconOf(name: string): ReactNode {
  const props = { size: 13, strokeWidth: 2, 'aria-hidden': true } as const
  const ext = extOf(name)
  if (ext === 'drawio') return <Network {...props} />
  if (ext === 'excalidraw') return <PenLine {...props} />
  if (IMAGE_EXTS.has(ext)) return <ImageIcon {...props} />
  if (OFFICE_EXTS.has(ext)) return <FileSpreadsheet {...props} />
  return <FileText {...props} />
}

/** 文件元数据模块级缓存（同 fileId 多卡片共享一次请求）。 */
const metaCache = new Map<string, { name: string; size: number } | null>()

const CARD_WIDTHS = [25, 50, 75, 100]

export default function FileCardView({ node, editor, selected, getPos }: NodeViewProps) {
  const attrs = node.attrs as DocflowFileCardAttrs
  const publicBase = useRichTextPublic()
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const editable = editor.isEditable
  const widthPct = Math.min(100, Math.max(25, Number(attrs.width) || 100))

  /** 更新本节点 width attr：直接构造事务并 view.dispatch 落盘（同
   * EmbedView.setAttrs —— NodeViewProps.updateAttributes 在 React NodeView
   * 内不稳定，见该处说明）。 */
  const setWidth = (width: number) => {
    const pos = typeof getPos === 'function' ? Number(getPos()) : NaN
    if (!Number.isFinite(pos) || pos < 0) return
    const view = editor.view
    const target = view.state.doc.nodeAt(pos)
    if (!target || target.type.name !== 'docflowFileCard') return
    view.dispatch(view.state.tr.setNodeMarkup(pos, undefined, { ...target.attrs, width }))
  }
  const [meta, setMeta] = useState<{ name: string; size: number } | null>(() =>
    attrs.fileId ? metaCache.get(attrs.fileId) ?? null : null,
  )
  const [metaGone, setMetaGone] = useState(false)
  const [rawUrl, setRawUrl] = useState<string | null>(null)
  // 图片内容嵌入 blob URL。
  const [imgSrc, setImgSrc] = useState('')
  const [imgError, setImgError] = useState(false)

  const name = attrs.title || meta?.name || (attrs.fileId ? `${attrs.fileId.slice(0, 8)}…` : '（无效引用）')
  const size = attrs.size || meta?.size || 0
  const ext = extOf(name)
  // 内容嵌入：图片（认证态用预览 blob；公开分享态用 raw/share URL，白名单
  // 允许图片直读）；Xmind 仅认证态（raw 白名单不含 xmind，公开态保持芯片）。
  const asImage = IMAGE_EXTS.has(ext) && ext !== 'svg' && !imgError && (!publicBase || Boolean(rawUrl))
  const asXmind = !publicBase && ext === 'xmind'

  // 认证态：大小缺省时懒拉元数据（404 → 已删除样式）。
  useEffect(() => {
    if (publicBase || !attrs.fileId || size > 0 || metaCache.has(attrs.fileId)) return
    let alive = true
    getFileMeta(attrs.fileId)
      .then((m) => {
        const entry = { name: m.name, size: m.current_version?.size ?? 0 }
        metaCache.set(attrs.fileId, entry)
        if (alive) setMeta(entry)
      })
      .catch(() => {
        metaCache.set(attrs.fileId, null)
        if (alive) setMetaGone(true)
      })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [attrs.fileId, size, publicBase])

  // 图片内容嵌入：认证预览取 blob URL。
  useEffect(() => {
    if (!asImage || !attrs.fileId || imgSrc) return
    let alive = true
    let objectURL = ''
    void fetchPreview(attrs.fileId)
      .then((preview) => {
        if (!alive) {
          if (preview.url?.startsWith('blob:')) URL.revokeObjectURL(preview.url)
          return
        }
        if (preview.kind === 'image' && preview.url) {
          if (preview.url.startsWith('blob:')) objectURL = preview.url
          setImgSrc(preview.url)
        } else {
          setImgError(true)
        }
      })
      .catch(() => { if (alive) setImgError(true) })
    return () => {
      alive = false
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [asImage, attrs.fileId, imgSrc])

  // 公开分享态：解析 raw/share URL 供点击新窗口打开。
  useEffect(() => {
    if (!publicBase || !attrs.title) return
    let alive = true
    void resolvePublicUrl(publicBase, attrs.title).then((url) => {
      if (alive) setRawUrl(url)
    })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [publicBase, attrs.title])

  const open = () => {
    if (publicBase) {
      if (rawUrl) window.open(rawUrl, '_blank', 'noopener')
      return
    }
    if (attrs.fileId) {
      window.dispatchEvent(new CustomEvent(OPEN_FILE_CARD_EVENT, { detail: { fileId: attrs.fileId, name } }))
    }
  }

  const onContextMenu = (e: React.MouseEvent) => {
    if (!editable || !attrs.fileId) return
    e.preventDefault()
    e.stopPropagation()
    window.dispatchEvent(new CustomEvent(CARD_CONTEXT_EVENT, { detail: { fileId: attrs.fileId, name, x: e.clientX, y: e.clientY } }))
  }

  const gone = !publicBase && metaGone

  // ---- 内容嵌入渲染（图片 / XMind）----
  if (asImage || asXmind) {
    return (
      <NodeViewWrapper
        as="span"
        className={`rich-text-card-embed${selected ? ' selected' : ''}`}
        data-file-id={attrs.fileId}
        title={zh ? `${name}（双击查看 / 右键编辑）` : `${name} (double-click to view)`}
      >
        <span
          className="rich-text-card-embed-inner"
          style={{ width: `${widthPct}%` }}
          onDoubleClick={open}
          onContextMenu={onContextMenu}
          contentEditable={false}
        >
          {asImage ? (
            imgSrc || rawUrl ? (
              <img className="rich-text-card-embed-image" src={imgSrc || rawUrl || undefined} alt={name} loading="lazy" />
            ) : (
              <span className="rich-text-card-embed-hint">{zh ? '图片加载中…' : 'Loading image…'}</span>
            )
          ) : (
            <Suspense fallback={<span className="rich-text-card-embed-hint">{zh ? '思维导图加载中…' : 'Loading mind map…'}</span>}>
              <div className="rich-text-card-embed-xmind">
                <XMindViewer fileId={attrs.fileId} title={name} convertible={false} />
              </div>
            </Suspense>
          )}
          {editable && selected && (
            <span className="rich-text-card-embed-tools" contentEditable={false}>
              {CARD_WIDTHS.map((w) => (
                <Tooltip key={w} title={`${zh ? '宽度' : 'Width'} ${w}%`}>
                  <Button
                    type="text"
                    size="small"
                    className={`rich-text-tbtn${widthPct === w ? ' active' : ''}`}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => setWidth(w)}
                  >
                    {w}%
                  </Button>
                </Tooltip>
              ))}
            </span>
          )}
        </span>
      </NodeViewWrapper>
    )
  }

  // ---- 普通文件：内联芯片 ----
  return (
    <NodeViewWrapper as="span" title={publicBase ? (rawUrl ? (zh ? '新窗口打开' : 'Open in new window') : (zh ? '该文件不在分享范围内' : 'Not included in this share')) : (zh ? '点击查看文件' : 'Click to view file')}>
      <span
        className={`rich-text-filecard${selected ? ' selected' : ''}${gone ? ' gone' : ''}`}
        data-file-id={attrs.fileId}
        role="button"
        tabIndex={0}
        onClick={open}
        onContextMenu={onContextMenu}
        onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open() } }}
        contentEditable={false}
      >
        <span className="rich-text-filecard-icon">{iconOf(name)}</span>
        <span className="rich-text-filecard-name">{name}</span>
        <span className="rich-text-filecard-size">
          {gone ? (zh ? '已删除' : 'deleted') : size > 0 ? formatQuota(size, false) : ''}
        </span>
      </span>
    </NodeViewWrapper>
  )
}
