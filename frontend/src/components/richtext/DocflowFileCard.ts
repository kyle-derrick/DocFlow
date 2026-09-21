// DocFlow 富文本「文件卡片」内联节点（自定义 Tiptap node）：
// - inline + atom：嵌在段落文字流中的引用芯片（图标 + 文件名 + 大小），
//   attrs { fileId, title, size }，data-file-id 属性序列化进 HTML 导出；
// - 点击行为：认证态弹出文件查看弹窗（FileViewerDispatch 懒加载，与文件
//   页点击文件同规格），弹窗内提供「新窗口打开」；公开分享态新窗口打开
//   raw/share 解析出的资源 URL（解析不到则提示不在分享范围）；
// - 大小展示：attrs.size 缺省（0）时懒拉文件元数据补齐（模块级缓存，
//   同 fileId 多卡片只拉一次），拉不到（文件已删）显示「已删除」样式。
import { Node, mergeAttributes, ReactNodeViewRenderer } from '@tiptap/react'
import FileCardView from './FileCardView'

export interface DocflowFileCardAttrs {
  fileId: string
  title: string
  size: number
  /** 内容嵌入显示宽度（25-100%；图片/Xmind 卡片内容嵌入时生效，默认 100）。 */
  width: number
}

export const DocflowFileCard = Node.create({
  name: 'docflowFileCard',
  group: 'inline',
  inline: true,
  atom: true,
  selectable: true,

  addAttributes() {
    return {
      fileId: {
        default: '',
        parseHTML: (element) => element.getAttribute('data-file-id') || '',
        renderHTML: (attributes) => ({ 'data-file-id': attributes.fileId }),
      },
      title: {
        default: '',
        parseHTML: (element) => element.getAttribute('data-title') || '',
        renderHTML: (attributes) => ({ 'data-title': attributes.title }),
      },
      size: {
        default: 0,
        parseHTML: (element) => Number(element.getAttribute('data-size')) || 0,
        renderHTML: (attributes) => (attributes.size ? { 'data-size': String(attributes.size) } : {}),
      },
      width: {
        default: 100,
        parseHTML: (element) => Number(element.getAttribute('data-width')) || 100,
        renderHTML: (attributes) => ({ 'data-width': String(attributes.width) }),
      },
    }
  },

  parseHTML() {
    return [{ tag: 'span[data-docflow-file]' }]
  },

  renderHTML({ HTMLAttributes }) {
    return ['span', mergeAttributes({ 'data-docflow-file': '1' }, HTMLAttributes)]
  },

  addNodeView() {
    return ReactNodeViewRenderer(FileCardView)
  },
})

export default DocflowFileCard
