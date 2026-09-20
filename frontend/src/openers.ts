// 默认打开方式分发（文件列表页共用）：
// - 按扩展名给出内置默认 {view, edit} 双方式（查看/编辑各自独立）；
// - 用户偏好（/me/open-with，按 ext，复合编码 v:<view>[+e:<edit>]）仅覆盖
//   显式设置的字段；v2.2 起设置页下拉放开为全枚举（用户自选，任何扩展名
//   可选任何方式），生效合并按枚举白名单校验（历史脏数据回落内置）；
// - 方式 → 前端路由（新窗口）：/view|edit/by-path 按扩展名自动分发，
//   用户偏好与「打开方式」子菜单选择经 ?open=<method> 显式覆盖（见
//   ViewerPage / ByPathPage 的 open 参数分发）。
import type { OpenWithPrefs } from './api'
import { isDrawioFile, isExcalidrawFile } from './api'

/** 查看方式枚举：raw=原始内容 / office=OnlyOffice / drawio=图表(转HTML) / excalidraw=白板(转图片) / xmind=思维导图 / richtext=富文本渲染（md 渲染或 .dfrt/.dfdoc）。 */
export type ViewMethod = 'raw' | 'office' | 'drawio' | 'excalidraw' | 'xmind' | 'richtext'

/** 编辑方式枚举：text=Monaco / office=OnlyOffice / drawio / excalidraw / richtext=Tiptap（.dfrt/.dfdoc 专属）/ none=不支持编辑。 */
export type EditMethod = 'text' | 'office' | 'drawio' | 'excalidraw' | 'richtext' | 'none'

/** 一个扩展名的完整打开方式（查看 + 编辑）。 */
export interface OpenWithPair {
  view: ViewMethod
  edit: EditMethod
}

/** 查看方式展示名（中/英）；ext 提供时按语境取词：md 家族的 richtext
 * 查看方式显示「md 渲染」（.dfrt/.dfdoc 的富文本渲染另名）。 */
export function viewMethodLabel(method: ViewMethod, zh: boolean, ext?: string): string {
  if (method === 'richtext' && (ext === 'md' || ext === 'markdown' || ext === 'mdx')) {
    return zh ? 'md 渲染' : 'Markdown'
  }
  const zhLabels: Record<ViewMethod, string> = {
    raw: '原始',
    office: 'Office',
    drawio: 'drawIO',
    excalidraw: '白板',
    xmind: 'XMind',
    richtext: '富文本',
  }
  const enLabels: Record<ViewMethod, string> = {
    raw: 'Raw',
    office: 'Office',
    drawio: 'draw.io',
    excalidraw: 'Whiteboard',
    xmind: 'XMind',
    richtext: 'Rich text',
  }
  return zh ? zhLabels[method] : enLabels[method]
}

/** 编辑方式展示名（中/英）。 */
export function editMethodLabel(method: EditMethod, zh: boolean): string {
  const zhLabels: Record<EditMethod, string> = {
    text: '文本编辑',
    office: 'Office',
    drawio: 'drawIO',
    excalidraw: '白板',
    richtext: '富文本',
    none: '不支持',
  }
  const enLabels: Record<EditMethod, string> = {
    text: 'Text editor',
    office: 'Office',
    drawio: 'draw.io',
    excalidraw: 'Whiteboard',
    richtext: 'Rich text',
    none: 'None',
  }
  return zh ? zhLabels[method] : enLabels[method]
}

/** 取扩展名（小写、去点）；无扩展名返回空串。 */
export function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i >= 0 ? name.slice(i + 1).toLowerCase() : ''
}

/** 常见源码/配置扩展名（内置默认与「文本文件」新建判定共用）。 */
const CODE_EXTS = new Set([
  'js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'json', 'css', 'scss', 'less', 'py', 'go', 'java', 'c', 'h',
  'cpp', 'hpp', 'cs', 'php', 'rb', 'rs', 'sh', 'bat', 'ps1', 'sql', 'yml', 'yaml', 'toml', 'ini',
  'conf', 'env', 'log',
])

/**
 * 「文本编辑」适用的全部文本类扩展名（内置默认 edit=text 判定、右键
 * 「编辑文本」、by-path 编辑分发共用）：md/markdown/txt 与常见标记/配置/
 * 源码/数据文件。office/drawio/excalidraw 等专项编辑器类型不在此列。
 */
const TEXT_EDITABLE_EXTS = new Set([
  // 纯文本与数据
  'txt', 'text', 'log', 'csv', 'tsv', 'md', 'markdown', 'mdx',
  // Web 标记与样式
  'html', 'htm', 'xhtml', 'css', 'scss', 'less', 'vue', 'svelte', 'astro',
  // 脚本与源码（含 CODE_EXTS 全集）
  ...CODE_EXTS,
  'svg', 'xml', 'graphql', 'gql', 'proto', 'lua', 'r',
])

/** Office 文档扩展名（内置 view/edit=office；OnlyOffice 查看与编辑）。 */
const OFFICE_DOC_EXTS = new Set(['doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx', 'odt', 'ods', 'odp'])

/** 富文本文档扩展名（DocFlow 专属 .dfrt 主后缀 + 旧 .dfdoc 别名：Tiptap JSON 存储，查看/编辑均富文本）。 */
const RICH_DOC_EXTS = new Set(['dfrt', 'dfdoc'])

/** 文本类文件（可进文本/代码编辑器安全编辑，非二进制）。 */
export function isTextEditableFile(name: string): boolean {
  return TEXT_EDITABLE_EXTS.has(extOf(name))
}

/** 常见源码/配置扩展名（「文本文件」新建路由推断用）。 */
export function isCodeFile(name: string): boolean {
  return CODE_EXTS.has(extOf(name))
}

/** 文本编辑器 kind（与 TextEditorPage 的 EditorKind 对齐）；非文本类返回 null。 */
export type TextEditorKind = 'markdown' | 'html' | 'css' | 'javascript' | 'text'

/** 按扩展名选择文本编辑器 kind（Monaco 语言/HTML 渲染按 kind 分发）。 */
export function textEditorKindFor(name: string): TextEditorKind | null {
  if (!isTextEditableFile(name)) return null
  const ext = extOf(name)
  if (ext === 'md' || ext === 'markdown' || ext === 'mdx') return 'markdown'
  if (ext === 'html' || ext === 'htm' || ext === 'xhtml') return 'html'
  if (ext === 'css' || ext === 'scss' || ext === 'less') return 'css'
  if (ext === 'js' || ext === 'mjs' || ext === 'cjs' || ext === 'ts' || ext === 'tsx'
    || ext === 'jsx' || ext === 'json') return 'javascript'
  return 'text'
}

// ---- 内置默认表（后缀 → {view, edit}） ----

/**
 * 内置默认打开方式（v1.7 md/富文本分离后）：
 * - md,markdown：richtext / text —— .md 回归纯 Markdown：查看 = md 渲染，
 *   编辑 = Monaco 源码（不再用富文本编辑器编辑 .md）；
 * - dfrt,dfdoc：richtext / richtext —— DocFlow 富文本文档（Tiptap JSON
 *   存储；.dfrt 为主后缀，.dfdoc 为兼容别名）；
 * - doc,docx,xls,xlsx,ppt,pptx（含 odt/ods/odp）：office / office；
 * - drawio：drawio / drawio；excalidraw：excalidraw / excalidraw；
 * - xmind：xmind / none（仅查看）；
 * - pdf：raw / office（OnlyOffice 可编辑 PDF）；
 * - 其余所有：raw /（文本类扩展名 text，否则 none——二进制不给 Monaco 乱码编辑）。
 */
export function builtinOpenWith(ext: string): OpenWithPair {
  if (RICH_DOC_EXTS.has(ext)) return { view: 'richtext', edit: 'richtext' }
  if (ext === 'md' || ext === 'markdown') return { view: 'richtext', edit: 'text' }
  if (OFFICE_DOC_EXTS.has(ext)) return { view: 'office', edit: 'office' }
  if (ext === 'drawio' || isDrawioFile(`x.${ext}`)) return { view: 'drawio', edit: 'drawio' }
  if (ext === 'excalidraw' || isExcalidrawFile(`x.${ext}`)) return { view: 'excalidraw', edit: 'excalidraw' }
  if (ext === 'xmind') return { view: 'xmind', edit: 'none' }
  if (ext === 'pdf') return { view: 'raw', edit: 'office' }
  if (TEXT_EDITABLE_EXTS.has(ext)) return { view: 'raw', edit: 'text' }
  return { view: 'raw', edit: 'none' }
}

// ---- 合法性（「打开方式」子菜单与设置页下拉过滤共用） ----

/** 该扩展名合法的查看方式（首个为内置默认；raw 为最终兜底，恒在列表中）。 */
export function viewOptionsFor(ext: string): ViewMethod[] {
  const builtin = builtinOpenWith(ext)
  const opts: ViewMethod[] = [builtin.view]
  if (OFFICE_DOC_EXTS.has(ext) || ext === 'pdf') {
    if (!opts.includes('office')) opts.push('office')
  }
  if (builtin.edit === 'richtext' || TEXT_EDITABLE_EXTS.has(ext)) {
    if (!opts.includes('richtext')) opts.push('richtext')
  }
  if (!opts.includes('raw')) opts.push('raw')
  return opts
}

/** 该扩展名合法的编辑方式（不含 none；空数组 = 不支持编辑）。 */
export function editOptionsFor(ext: string): EditMethod[] {
  const builtin = builtinOpenWith(ext)
  if (builtin.edit === 'none') return []
  const opts: EditMethod[] = [builtin.edit]
  // 富文本文档（.dfrt/.dfdoc）编辑专属（Tiptap JSON 不进 Monaco）；其余
  // 富文本可编辑类型（当前无——md 已回归 Monaco）可回落源码编辑。
  if (builtin.edit === 'richtext' && TEXT_EDITABLE_EXTS.has(ext) && !opts.includes('text')) {
    opts.push('text')
  }
  return opts
}

// ---- 全量方式列表（「打开方式」二级菜单 / 设置页内置表共用） ----

/** 全部查看方式（固定展示顺序）。 */
export const ALL_VIEW_METHODS: ViewMethod[] = ['raw', 'office', 'drawio', 'excalidraw', 'xmind', 'richtext']

/** 全部编辑方式（不含 none）。 */
export const ALL_EDIT_METHODS: EditMethod[] = ['text', 'office', 'drawio', 'excalidraw', 'richtext']

/** 集成可用性：office 须 OnlyOffice、drawio 须 draw.io embed（查看/编辑同门槛）。 */
export interface MethodAvailability {
  office: boolean
  drawio: boolean
}

/** 「打开方式」菜单的单项：方式 + 是否可用 + 不可用原因（tooltip）。 */
export interface OpenMethodEntry {
  method: ViewMethod | EditMethod
  /** false = 菜单灰显（Disabled）。 */
  enabled: boolean
  /** 不可用原因（合法性不符 / 集成未启用）。 */
  reason?: string
}

/** 全部查看方式（含非法/集成未启用的灰显项）：合法 + 可用 = enabled。 */
export function allViewEntries(ext: string, av: MethodAvailability, zh: boolean): OpenMethodEntry[] {
  const legal = viewOptionsFor(ext)
  return ALL_VIEW_METHODS.map((m) => {
    if (legal.includes(m) && methodAvOk(m, av)) return { method: m, enabled: true }
    return { method: m, enabled: false, reason: disabledReason(m, legal.includes(m), av, zh, 'view') }
  })
}

/** 全部编辑方式（同上口径；none 不在列表中）。 */
export function allEditEntries(ext: string, av: MethodAvailability, zh: boolean): OpenMethodEntry[] {
  const legal = editOptionsFor(ext)
  return ALL_EDIT_METHODS.map((m) => {
    if (legal.includes(m) && methodAvOk(m, av)) return { method: m, enabled: true }
    return { method: m, enabled: false, reason: disabledReason(m, legal.includes(m), av, zh, 'edit') }
  })
}

function methodAvOk(m: string, av: MethodAvailability): boolean {
  return !(m === 'office' && !av.office) && !(m === 'drawio' && !av.drawio)
}

function disabledReason(m: string, legal: boolean, av: MethodAvailability | undefined, zh: boolean, kind: 'view' | 'edit'): string {
  if (!legal) {
    // drawio/excalidraw/richtext 同时存在于查看与编辑方式集，须按调用方
    // 语境取名词，不能按 method 归属推断。
    const noun = kind === 'view' ? '查看' : '编辑'
    return zh ? `该文件类型不支持此${noun}方式` : `Not available for this file type`
  }
  if (m === 'office' && !av?.office) return zh ? 'OnlyOffice 集成未启用' : 'OnlyOffice integration disabled'
  if (m === 'drawio' && !av?.drawio) return zh ? 'draw.io 集成未启用' : 'draw.io integration disabled'
  return ''
}

/** 设置页「打开方式」内置默认表预填的扩展名清单（按类别排序展示）。 */
export const BUILTIN_OPENWITH_EXTS = [
  'txt', 'md', 'markdown', 'dfrt', 'dfdoc',
  'html', 'htm', 'css', 'js', 'json', 'csv', 'tsv', 'xml', 'svg', 'yaml',
  'doc', 'docx', 'xls', 'xlsx', 'ppt', 'pptx',
  'drawio', 'excalidraw', 'xmind', 'pdf',
] as const

/**
 * 生效打开方式：内置默认之上合并用户偏好（只覆盖显式设置的字段）。
 * v2.2：设置页「打开方式」下拉已放开为全枚举（用户自选，与后端白名单一致
 * ——任何扩展名可选任何方式），此处同步放开按枚举白名单校验（不再按扩展
 * 名合法性过滤），仅历史脏数据（枚举外值）回落内置；?open= 显式覆盖与
 * 右键「打开方式」菜单仍可随时临时切换。
 */
export function effectiveOpenWith(ext: string, prefs: OpenWithPrefs): OpenWithPair {
  const builtin = builtinOpenWith(ext)
  const pref = prefs[ext]
  if (!pref) return builtin
  const view = pref.view && ALL_VIEW_METHODS.includes(pref.view) ? pref.view : builtin.view
  const edit = pref.edit && (ALL_EDIT_METHODS.includes(pref.edit) || pref.edit === 'none') ? pref.edit : builtin.edit
  return { view, edit }
}

/** 文件名 → 生效打开方式（扩展名提取包装）。 */
export function effectiveOpenWithFor(name: string, prefs: OpenWithPrefs): OpenWithPair {
  return effectiveOpenWith(extOf(name), prefs)
}
