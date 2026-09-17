// DocFlow 富文本嵌入块节点（自定义 Tiptap node）：
// - atom 块级节点，attrs { kind, fileId, title }，NodeView 渲染实际内容
//   （见 EmbedView.tsx）；
// - Markdown 存储约定（tiptap-markdown 不认识自定义 node，经其
//   addStorage().markdown 扩展点成对补丁）：
//   序列化 → fenced code block：```drawio file:<uuid> title:<name>```
//   解析    → markdown-it fence 渲染规则补丁，命中约定的 fence 输出
//             <div data-docflow-embed=… data-file-id=… data-title=…>，
//             再由本节点 parseHTML 收集（与普通代码块互不干扰）。
import { Node, mergeAttributes, ReactNodeViewRenderer } from '@tiptap/react'
import EmbedView from './EmbedView'
import { EmbedKind, EmbedRef, escapeHtmlAttr, formatEmbedFence, parseEmbedFence } from './markdownRoundtrip'

/** tiptap-markdown 序列化 state 的最小结构类型（prosemirror-markdown 子集）。 */
interface MarkdownSerializerStateLike {
  write(text: string): void
  closeBlock(node: unknown): void
}

/** markdown-it 实例的最小结构类型（setup 回调入参）。 */
interface MarkdownItLike {
  renderer: {
    rules: Record<string, ((tokens: FenceTokenLike[], idx: number, options: unknown, env: unknown, self: unknown) => string) | undefined>
  }
  /** setup 在每次 parse 前都会被调用，用标记保证 fence 补丁幂等。 */
  __docflowEmbedPatched?: boolean
}

interface FenceTokenLike {
  type: string
  info?: string
  content?: string
}

/** Docs 命令类型扩展：editor.commands.insertDocflowEmbed(ref)。 */
declare module '@tiptap/core' {
  interface Commands<ReturnType> {
    docflowEmbed: {
      /** 在当前选区插入嵌入块（drawio/excalidraw/office/web/file 引用）。 */
      insertDocflowEmbed: (ref: EmbedRef) => ReturnType
    }
  }
}

export interface DocflowEmbedAttrs {
  kind: EmbedKind
  fileId: string
  title: string
}

const DEFAULT_ATTRS: DocflowEmbedAttrs = { kind: 'file', fileId: '', title: '' }

export const DocflowEmbed = Node.create({
  name: 'docflowEmbed',
  group: 'block',
  atom: true,
  selectable: true,

  addAttributes() {
    return {
      kind: {
        default: DEFAULT_ATTRS.kind,
        parseHTML: (element) => element.getAttribute('data-docflow-embed') || DEFAULT_ATTRS.kind,
        renderHTML: (attributes) => ({ 'data-docflow-embed': attributes.kind }),
      },
      fileId: {
        default: DEFAULT_ATTRS.fileId,
        parseHTML: (element) => element.getAttribute('data-file-id') || '',
        renderHTML: (attributes) => ({ 'data-file-id': attributes.fileId }),
      },
      title: {
        default: DEFAULT_ATTRS.title,
        parseHTML: (element) => element.getAttribute('data-title') || '',
        renderHTML: (attributes) => ({ 'data-title': attributes.title }),
      },
    }
  },

  parseHTML() {
    return [{ tag: 'div[data-docflow-embed]' }]
  },

  renderHTML({ HTMLAttributes }) {
    return ['div', mergeAttributes({ 'data-node-view-wrapper': '' }, HTMLAttributes)]
  },

  addNodeView() {
    return ReactNodeViewRenderer(EmbedView)
  },

  addCommands() {
    return {
      insertDocflowEmbed:
        (ref: EmbedRef) =>
        ({ commands }) =>
          commands.insertContent({ type: this.name, attrs: { kind: ref.kind, fileId: ref.fileId, title: ref.title } }),
    }
  },

  addStorage() {
    return {
      // tiptap-markdown 扩展点：getMarkdownSpec(extension) 读取
      // extension.storage.markdown，serialize/parse 成对定义保证无损往返。
      markdown: {
        serialize(state: MarkdownSerializerStateLike, node: { attrs: DocflowEmbedAttrs }) {
          state.write(formatEmbedFence({
            kind: node.attrs.kind,
            fileId: node.attrs.fileId,
            title: node.attrs.title,
          }))
          state.closeBlock(node)
        },
        parse: {
          setup(md: MarkdownItLike) {
            if (md.__docflowEmbedPatched) return
            md.__docflowEmbedPatched = true
            const original = md.renderer.rules.fence
            md.renderer.rules.fence = (tokens, idx, options, env, self) => {
              const token = tokens[idx]
              const ref = parseEmbedFence(token?.info ?? '', token?.content ?? '')
              if (ref) {
                return (
                  `<div data-docflow-embed="${escapeHtmlAttr(ref.kind)}"` +
                  ` data-file-id="${escapeHtmlAttr(ref.fileId)}"` +
                  ` data-title="${escapeHtmlAttr(ref.title)}"></div>`
                )
              }
              return original ? original(tokens, idx, options, env, self) : ''
            }
          },
        },
      },
    }
  },
})

export default DocflowEmbed
