// DocFlow 富文本「文档图片」块级节点（自定义 Tiptap node）：
// - 存 fileId 引用（不存 URL：raw 授权 10 分钟过期，渲染时现取），attrs
//   { fileId, title, width(25|50|75|100) }，data-file-id 属性序列化；
// - 渲染：认证态 fetchPreview 换 blob URL（卸载 revoke）；公开分享态
//   raw/share 解析直连 URL；加载/失败态占位（失败可删，见 NodeView）；
// - 选中（可编辑态）浮出宽度快捷条 25/50/75/100%（NodeView updateAttributes）。
import { Node, mergeAttributes, ReactNodeViewRenderer } from '@tiptap/react'
import DocflowImageView from './DocflowImageView'

export interface DocflowImageAttrs {
  fileId: string
  title: string
  width: number
}

export const IMAGE_WIDTHS = [25, 50, 75, 100]

export const DocflowImage = Node.create({
  name: 'docflowImage',
  group: 'block',
  atom: true,
  selectable: true,
  draggable: true,

  addAttributes() {
    return {
      fileId: {
        default: '',
        parseHTML: (element) => element.getAttribute('data-file-id') || '',
        renderHTML: (attributes) => ({ 'data-file-id': attributes.fileId }),
      },
      title: {
        default: '',
        parseHTML: (element) => element.getAttribute('data-title') || element.getAttribute('alt') || '',
        renderHTML: (attributes) => ({ 'data-title': attributes.title, alt: attributes.title }),
      },
      width: {
        default: 100,
        parseHTML: (element) => Number(element.getAttribute('data-width')) || 100,
        renderHTML: (attributes) => ({ 'data-width': String(attributes.width) }),
      },
    }
  },

  parseHTML() {
    return [{ tag: 'div[data-docflow-image]' }]
  },

  renderHTML({ HTMLAttributes }) {
    return ['div', mergeAttributes({ 'data-docflow-image': '1' }, HTMLAttributes)]
  },

  addNodeView() {
    return ReactNodeViewRenderer(DocflowImageView)
  },
})

export default DocflowImage
