// 默认打开方式分发（个人 / 团队文件列表共用）：
// - 偏好（/me/open-with，按 ext）优先，未配置时按扩展名给出内置默认打开器；
// - opener → 前端路由（新窗口）：各打开器对应既有编辑/查看路由，集成
//   （ONLYOFFICE / draw.io）未启用时回退只读查看 /view/:id。
import { OpenWithMap, OpenWithOpener, isDrawioFile, isExcalidrawFile, isHtmlFile, isOfficeFile } from './api'

/** 打开器枚举全集（管理默认打开方式弹窗的下拉用）。 */
export const ALL_OPENERS: OpenWithOpener[] = [
  'office', 'drawio', 'excalidraw', 'text', 'markdown', 'code', 'web', 'default',
]

/** 打开器展示名（中/英）。 */
export function openerLabel(opener: OpenWithOpener, zh: boolean): string {
  const zhLabels: Record<OpenWithOpener, string> = {
    office: 'Office 文档',
    drawio: 'draw.io 图表',
    excalidraw: 'Excalidraw 白板',
    text: '文本编辑器',
    markdown: 'Markdown 编辑器',
    code: '代码编辑器',
    web: '网页查看',
    default: '默认（自动查看器）',
  }
  const enLabels: Record<OpenWithOpener, string> = {
    office: 'Office document',
    drawio: 'draw.io diagram',
    excalidraw: 'Excalidraw whiteboard',
    text: 'Text editor',
    markdown: 'Markdown editor',
    code: 'Code editor',
    web: 'Web view',
    default: 'Default (auto viewer)',
  }
  return zh ? zhLabels[opener] : enLabels[opener]
}

/** 取扩展名（小写、去点）；无扩展名返回空串。 */
export function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i >= 0 ? name.slice(i + 1).toLowerCase() : ''
}

/** Markdown 文件（.md / .markdown）。 */
function isMarkdownFile(name: string): boolean {
  const ext = extOf(name)
  return ext === 'md' || ext === 'markdown'
}

/** 常见源码/配置扩展名 → 代码编辑器。 */
const CODE_EXTS = new Set([
  'js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'json', 'css', 'scss', 'less', 'py', 'go', 'java', 'c', 'h',
  'cpp', 'hpp', 'cs', 'php', 'rb', 'rs', 'sh', 'bat', 'ps1', 'sql', 'yml', 'yaml', 'toml', 'ini',
  'conf', 'env', 'log',
])

/** 代码类文件（「打开方式」候选与内置默认判定共用）。 */
export function isCodeFile(name: string): boolean {
  return CODE_EXTS.has(extOf(name))
}

/** 内置默认打开器（无用户偏好时按扩展名自动选择；其余类型走 default 只读查看）。 */
function smartOpenerFor(name: string): OpenWithOpener {
  if (isMarkdownFile(name)) return 'markdown'
  if (extOf(name) === 'txt') return 'text'
  if (isCodeFile(name)) return 'code'
  if (isHtmlFile(name)) return 'web'
  if (isDrawioFile(name)) return 'drawio'
  if (isExcalidrawFile(name)) return 'excalidraw'
  if (isOfficeFile(name)) return 'office'
  return 'default'
}

/** 生效打开器：用户偏好（按 ext）优先，否则内置默认。 */
export function resolveOpener(name: string, prefs: OpenWithMap): OpenWithOpener {
  return prefs[extOf(name)] ?? smartOpenerFor(name)
}

/** 「打开方式」子菜单候选（按文件类型给出相关项，default 恒在末位）。 */
export function openWithOptions(name: string): OpenWithOpener[] {
  const opts: OpenWithOpener[] = []
  if (isOfficeFile(name)) opts.push('office')
  if (isDrawioFile(name)) opts.push('drawio')
  if (isExcalidrawFile(name)) opts.push('excalidraw')
  if (isMarkdownFile(name)) opts.push('markdown', 'text')
  else if (extOf(name) === 'txt') opts.push('text')
  if (isCodeFile(name)) opts.push('code')
  if (isHtmlFile(name)) opts.push('web')
  opts.push('default')
  return [...new Set(opts)]
}
