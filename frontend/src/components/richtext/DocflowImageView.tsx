// 文档图片 NodeView：认证态 blob URL / 公开态 raw/share 直连；选中时
//（可编辑）浮出宽度快捷条（25/50/75/100%）+ 删除；失败态占位 + 移除按钮。
import { useEffect, useState } from 'react'
import type { NodeViewProps } from '@tiptap/react'
import { NodeViewWrapper } from '@tiptap/react'
import { Button, Tooltip } from 'antd'
import { Trash2 } from 'lucide-react'
import { fetchPreview } from '../../api'
import { useLocale } from '../../i18n'
import type { DocflowImageAttrs } from './DocflowImage'
import { IMAGE_WIDTHS } from './DocflowImage'
import { useRichTextPublic } from './RichTextPublicContext'
import { resolvePublicUrl } from './publicResource'

export default function DocflowImageView({ node, editor, selected, deleteNode, getPos }: NodeViewProps) {
  const attrs = node.attrs as DocflowImageAttrs
  const publicBase = useRichTextPublic()
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const editable = editor.isEditable

  /** 更新本节点 width attr：经 editor.chain().command 直接 setNodeMarkup
   *（NodeViewProps.updateAttributes 在当前 @tiptap/react 版本静默失效，见
   * EmbedView.setAttrs 同款说明）。 */
  const setWidth = (width: number) => {
    const pos = typeof getPos === 'function' ? Number(getPos()) : NaN
    if (!Number.isFinite(pos) || pos < 0) return
    const view = editor.view
    const target = view.state.doc.nodeAt(pos)
    if (!target || target.type.name !== 'docflowImage') return
    view.dispatch(view.state.tr.setNodeMarkup(pos, undefined, { ...target.attrs, width }))
  }

  const [src, setSrc] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!attrs.fileId) {
      setLoading(false)
      setError(zh ? '图片引用缺少文件 ID' : 'Image reference is missing file ID')
      return
    }
    let alive = true
    let objectURL = ''
    setLoading(true)
    setError('')
    setSrc('')
    const load = async () => {
      try {
        if (publicBase) {
          // 公开分享态：按文件名在分享树内解析（同级 assets/ 优先）。
          if (!attrs.title) throw new Error(zh ? '资源不在分享范围内' : 'Resource not included in this share')
          const url = await resolvePublicUrl(publicBase, attrs.title)
          if (!url) throw new Error(zh ? '资源不在分享范围内' : 'Resource not included in this share')
          if (alive) setSrc(url)
        } else {
          const preview = await fetchPreview(attrs.fileId)
          if (!alive) {
            if (preview.url?.startsWith('blob:')) URL.revokeObjectURL(preview.url)
            return
          }
          if (preview.kind !== 'image' || !preview.url) {
            throw new Error(zh ? '文件已删除或不可访问' : 'File deleted or inaccessible')
          }
          if (preview.url.startsWith('blob:')) objectURL = preview.url
          setSrc(preview.url)
        }
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : (zh ? '图片加载失败' : 'Failed to load image'))
      } finally {
        if (alive) setLoading(false)
      }
    }
    void load()
    return () => {
      alive = false
      if (objectURL) URL.revokeObjectURL(objectURL)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [attrs.fileId, attrs.title, publicBase, zh])

  return (
    <NodeViewWrapper className={`rich-text-dfimg${selected ? ' selected' : ''}`} data-file-id={attrs.fileId}>
      {editable && selected && (
        <div className="rich-text-dfimg-tools" contentEditable={false}>
          {IMAGE_WIDTHS.map((w) => (
            <Tooltip key={w} title={`${zh ? '宽度' : 'Width'} ${w}%`}>
              <Button
                type="text"
                size="small"
                className={`rich-text-tbtn${attrs.width === w ? ' active' : ''}`}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => setWidth(w)}
              >
                {w}%
              </Button>
            </Tooltip>
          ))}
          <Tooltip title={zh ? '删除图片' : 'Delete image'}>
            <Button
              type="text"
              size="small"
              className="rich-text-tbtn"
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => deleteNode()}
            >
              <Trash2 size={14} strokeWidth={2} aria-hidden="true" />
            </Button>
          </Tooltip>
        </div>
      )}
      {error ? (
        <div className="rich-text-dfimg-error" contentEditable={false}>
          <span className="rich-text-dfimg-error-title">{attrs.title || (zh ? '图片' : 'Image')}</span>
          <small>{error}</small>
          {editable && (
            <Button size="small" danger onMouseDown={(e) => e.preventDefault()} onClick={() => deleteNode()}>
              {zh ? '移除' : 'Remove'}
            </Button>
          )}
        </div>
      ) : (
        <img
          className="rich-text-dfimg-el"
          src={loading ? undefined : src}
          alt={attrs.title}
          style={{ width: `${attrs.width}%` }}
          loading="lazy"
        />
      )}
      {loading && !error && <div className="rich-text-dfimg-hint" contentEditable={false}>{zh ? '图片加载中…' : 'Loading image…'}</div>}
    </NodeViewWrapper>
  )
}
