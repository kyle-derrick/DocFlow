// Markdown 往返（round-trip）保障的纯函数集。
// - 嵌入块（drawio/excalidraw/office/web/file）在 md 中的存储约定：fenced
//   code block，info 串为 `` `<kind> file:<uuid> title:<name>` ``，正文为空；
// - normalizeMarkdownForCompare 把两侧文本归一到同一书写风格后再比较，
//   用于判定「解析 → 序列化」是否无损（失败则降级源码模式）；
// - 全部为无依赖纯函数，独立导出便于后续 node 侧冒烟测试。

import { UUID_RE } from '../../api'

/** 支持的嵌入块类型（与 fence info 首段一致）。 */
export type EmbedKind = 'drawio' | 'excalidraw' | 'office' | 'web' | 'file'

export const EMBED_KINDS: readonly EmbedKind[] = ['drawio', 'excalidraw', 'office', 'web', 'file']

/** 嵌入块引用（对应 DocflowEmbed node 的 attrs）。 */
export interface EmbedRef {
  kind: EmbedKind
  fileId: string
  title: string
}

/**
 * 解析嵌入块 fence（info + content）。
 * info 形如 `drawio file:3f0c… title:架构图`；title 可省略、可含空格；
 * 正文必须为空（嵌入块不携带内容）。不匹配约定时返回 null（回落普通代码块）。
 */
export function parseEmbedFence(info: string, content: string): EmbedRef | null {
  if (content.trim() !== '') return null
  const match = /^(\S+)[ \t]+file:([0-9a-fA-F-]{36})(?:[ \t]+title:([\s\S]*))?$/.exec(info.trim())
  if (!match) return null
  const kind = match[1].toLowerCase()
  if (!(EMBED_KINDS as readonly string[]).includes(kind)) return null
  if (!UUID_RE.test(match[2])) return null
  return { kind: kind as EmbedKind, fileId: match[2].toLowerCase(), title: (match[3] ?? '').trim() }
}

/** 嵌入块序列化为 fence 文本（不含结尾空行，结尾空行由 serializer state 补齐）。 */
export function formatEmbedFence(ref: EmbedRef): string {
  const title = ref.title.replace(/[`\r\n]+/g, ' ').trim()
  return `\`\`\`${ref.kind} file:${ref.fileId}${title ? ` title:${title}` : ''}\n\`\`\``
}

/** HTML 属性值转义（嵌入块 md→HTML 中转时的 title 注入防护）。 */
export function escapeHtmlAttr(value: string): string {
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}

/**
 * Markdown 书写风格归一（两侧同规则应用，仅用于等价性比较，不落盘）：
 * - CRLF → LF、行尾空白去除、≥2 连续空行压为 1 个空行、首尾空白去除；
 * - 列表风格：`* ` / `+ ` 项目符 → `- `；有序列表 `1)` → `1.`；
 * - 分割线 `***` / `___` / `* * *` 等 → `---`；
 * - setext 标题（文字下一行 ===/---）→ ATX `#/##`；
 * - 强调 `__x__` → `**x**`、`_x_` → `*x*`（词边界保守匹配）。
 */
export function normalizeMarkdownForCompare(md: string): string {
  let out = md.replace(/\r\n?/g, '\n')
  // setext 标题（仅匹配非空行为文字、下一行为纯 =/- 下划线；此前不能有
  // 空行——CommonMark setext 规则），先于分割线归一处理。
  out = out.replace(/^([^\n]+)\n=+[ \t]*$/gm, '# $1')
  out = out.replace(/^([^\n]+)\n-{2,}[ \t]*$/gm, '## $1')
  // 分割线归一为 ---。
  out = out.replace(/^[ \t]{0,3}([-*_])[ \t]*(?:\1[ \t]*){2,}$/gm, '---')
  // 列表风格归一。
  out = out.replace(/^([ \t]*)[*+][ \t]+/gm, '$1- ')
  out = out.replace(/^([ \t]*)(\d{1,9})\)[ \t]+/gm, '$1$2. ')
  // 强调定界符归一（保守：定界符两侧不邻接字母数字/下划线，避免破坏
  // snake_case 与行内代码中的下划线）。
  out = out.replace(/(^|[^\w*])__([^_\n]+)__(?![\w*])/g, '$1**$2**')
  out = out.replace(/(^|[^\w_])_([^_\n]+)_(?![\w_])/g, '$1*$2*')
  // 行尾空白、连续空行压缩、首尾修剪。
  out = out.replace(/[ \t]+$/gm, '')
  out = out.replace(/\n{3,}/g, '\n\n')
  return out.trim()
}

/**
 * 判定「源 md → 编辑器 → 序列化 md」是否无损：
 * 两侧风格归一后逐字相等视为无损（内容语义由书写风格归一兜底）。
 */
export function markdownRoundTripMatches(source: string, serialized: string): boolean {
  return normalizeMarkdownForCompare(source) === normalizeMarkdownForCompare(serialized)
}
