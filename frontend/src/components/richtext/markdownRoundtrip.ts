// 富文本嵌入块（embed）的共享类型定义。
// - 嵌入块 = 富文本文档内引用其他文件节点的增强块（drawio 图表 / excalidraw
//   白板 / office 文档 / 网页目录 / 普通文件），NodeView 渲染见 EmbedView.tsx；
// - v1.7 起 .dfdoc 为 Tiptap JSON 存储（嵌入块原生存进文档 JSON），旧的
//   Markdown fenced-code-block 往返（解析/序列化/无损比较）已随 .md 回归
//   纯源码编辑一并移除。

/** 支持的嵌入块类型。 */
export type EmbedKind = 'drawio' | 'excalidraw' | 'xmind' | 'office' | 'web' | 'file'

export const EMBED_KINDS: readonly EmbedKind[] = ['drawio', 'excalidraw', 'xmind', 'office', 'web', 'file']

/** 嵌入块引用（对应 DocflowEmbed node 的 attrs）。 */
export interface EmbedRef {
  kind: EmbedKind
  fileId: string
  title: string
}
