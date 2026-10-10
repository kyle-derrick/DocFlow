// 编辑器页「AI 对话式创作/编辑」侧栏面板：
// - AIEditChatButton：编辑器页头部「AI 对话」Dropdown.Button 入口（Sparkles；
//   AI 未启用不渲染，useAIEnabled 门控）——主点击展开/收起右侧面板，下拉
//   菜单为原 AIEditMenu 并入的快捷指令（摘要/续写/润色/翻译成英文/自定义），
//   点击经 onQuick 通知宿主页打开面板并自动发送（见 AIQuickCommand）；
// - AIEditChat（默认导出）：页面内嵌右侧面板，Monaco（md/text/code）与
//   Tiptap（.dfrt/.dfdoc）编辑页共用——多轮对话（SSE 流式 + markdown +
//   打字机光标，气泡/头像布局与全局 AIAssistant 统一）、上下文范围选择
//   （选区默认，无选区禁用并提示 / 全文）、快捷指令 chips、停止生成
//   （AbortController → aiChat signal，卸载自动 abort）。
// - 模式开关（Segmented）：「可修改」= 回复流式完成后自动应用（有选区替换
//   选区、无选区经 onAppend 追加文末），应用前先经 ensureSaved 保存新版本
//   （版本保护），消息下记录修改点并支持一键撤销（restoreVersion 回退到
//   应用前版本 + reload 刷新编辑器）；「仅对话」= 纯输出不改动文档。
//   手动「插入到光标 / 替换选区」按钮已移除，仅保留「复制」。
// - 应用通道 applyKind（默认 text 行为不变）：excalidraw-json（白板页：
//   AI 只回 excalidraw 元素 JSON 数组，自动应用时提取 JSON 交宿主经官方
//   convertToExcalidrawElements 转为白板原生元素追加插入）、drawio-xml
//   （图表页：AI 回完整 drawio XML，自动应用时提取 XML 整体替换画布）；
//   v3.9：OnlyOffice 编辑页不再挂本面板（Office 的 AI 入口 = 编辑器内
//   DocFlow AI 插件，见 frontend/public/oo-plugins/docflow-ai）；本面板
//   仅服务有落盘通道的宿主页（Monaco/Tiptap/白板/图表）。
import { useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button, Input, Popover, Segmented, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import SplitButton from './SplitButton'
import type { TextAreaRef } from 'antd/es/input/TextArea'
import { Check, Copy, FileText, HelpCircle, Paperclip, Sparkles, Trash2, X } from 'lucide-react'
import { aiChat, getFileMeta, listFiles, restoreVersion, searchFiles } from '../api'
import type { AIMessage } from '../api'
import { AIChatToggleBar, AISkillButton, AIToolCalls, AIWebSources, AI_MODEL_STORAGE_KEY, defaultAIModelKey, getAIModels, normalizeWebSources, renderSkillPrompt, useAIChatToggles } from './AIAssistant'
import { AIChatThinking, applyToolResult, toolEntryFrom } from './aichat'
import { CHAT_SEND_ICON, CHAT_STOP_ICON } from './aichat/icons'
import type { AIAttachFile } from './aichat'
import type { AIToolCallEntry } from './aichat'
import type { AIModelOption, AIWebSource } from './AIAssistant'
import AIModelSelect from './AIModelSelect'
import AIMarkdown from './AIMarkdown'
import { useAIEnabled } from '../aiFeature'
import { t, useLocale } from '../i18n'
/** 选区描述（无选区 = 全文模式；原独立 AIEdit.tsx 移除后迁此）。 */
export interface AIEditTarget {
  text: string
  hasSelection: boolean
}

/** 宿主注册的编辑工具（v5 内置 Agent 模式）：name/描述进 system，
 * exec 在宿主编辑器上执行并返回结构化结果（ok=false 时错误回喂 AI）。 */
export interface AIEditTool {
  name: string
  /** 工具说明（签名与语义，写入 system 提示）。 */
  desc: string
  /** 参数摘要展示（工具链 UI 的 label）。 */
  label: (args: Record<string, unknown>) => string
  exec: (args: Record<string, unknown>) => Promise<{ ok: boolean; data?: unknown; error?: string }>
}

/** 上下文（选区/全文）送入模型的最大长度（超限截取头部并在消息中注明）。 */
const MAX_CONTEXT_CHARS = 6000

/** 随每轮携带的问答历史轮数（每轮独立重建上下文）。 */
const HISTORY_ROUNDS = 2

/** 自动应用记录的文本摘要长度（前 N 字）。 */
const APPLY_SUMMARY_CHARS = 60

type ChatScope = 'selection' | 'full'

/** 面板模式：edit=可修改（自动应用+版本保护）；chat=仅对话（纯输出）。 */
type ChatMode = 'edit' | 'chat'

/** 应用通道类型（默认 text 行为完全不变）：
 * - text：写作助手（回复=最终文本，选区替换/文末追加）；
 * - excalidraw-json：白板页——AI 只回 excalidraw 元素 JSON 数组（骨架），
 *   宿主经官方 convertToExcalidrawElements 补全为正式元素插入画布；
 * - drawio-xml：drawio 页——AI 基于当前 XML 回完整 drawio XML，宿主整体
 *   替换画布内容。两者均无选区概念，统一走 onApply('replace')；
 * - richtext-patch：富文本页——指令式编辑（Cursor/Notion AI 形态）：AI 回
 *   ```docflow-edit 围栏内的 JSON 编辑指令数组（replace/insert/delete/
 *   replaceAll），宿主执行器（DfdocEditorPage.applyRichTextPatch）在
 *   ProseMirror 文档中按定位原文精确匹配逐条执行；围栏缺失时宿主回落
 *   追加。回复原文经 onApply('replace') 透传，解析在宿主侧。 */
export type AIApplyKind = 'text' | 'excalidraw-json' | 'drawio-xml' | 'richtext-patch'

/** 从 AI 回复中提取 excalidraw 元素 JSON 源码：优先 ```excalidraw-json 围栏；
 * 其次任意围栏代码块内容以 [ 开头；最后裸数组（trim 后 [ 起 ] 止）。
 * 只负责提取文本——JSON.parse 与元素合法性校验在宿主页（非法时回
 * applyError，画布不动）。 */
function extractExcalidrawJSON(reply: string): string {
  const fenced = /```(?:excalidraw-json|excalidraw)[^\n]*\n([\s\S]*?)```/.exec(reply)
  if (fenced && fenced[1].trim().startsWith('[')) return fenced[1].trim()
  const re = /```[^\n]*\n([\s\S]*?)```/g
  for (let m = re.exec(reply); m; m = re.exec(reply)) {
    const body = m[1].trim()
    if (body.startsWith('[')) return body
  }
  const trimmed = reply.trim()
  if (trimmed.startsWith('[') && trimmed.endsWith(']')) return trimmed
  return ''
}

/** 从 AI 回复中提取完整 drawio XML：优先 ```xml 围栏；裸文则取首个
 * <mxfile|<mxGraphModel 起至对应闭合标签。 */
function extractDrawioXML(reply: string): string {
  const fenced = /```(?:xml|drawio)[^\n]*\n([\s\S]*?)```/.exec(reply)
  if (fenced && /^<(?:mxfile|mxGraphModel)\b/.test(fenced[1].trim())) return fenced[1].trim()
  const start = reply.search(/<(?:mxfile|mxGraphModel)\b/)
  if (start >= 0) {
    const endFile = reply.lastIndexOf('</mxfile>')
    const endModel = reply.lastIndexOf('</mxGraphModel>')
    const end = Math.max(endFile, endModel)
    if (end > start) {
      const closeLen = end === endFile ? '</mxfile>'.length : '</mxGraphModel>'.length
      return reply.slice(start, end + closeLen).trim()
    }
  }
  return ''
}

/** drawio-xml 通道 system 指令附加的 XML 生成参考（吸收 jgraph/drawio-mcp
 * 官方指南精编，≤45 行）。技术规范统一英文——模型对英文 XML 约定遵循更稳，
 * 双语 intro 仍按现有 locale 机制（见 send()）。仅增强提示词，不影响
 * extractDrawioXML/应用逻辑。导出供宿主页（DrawioPage Agent 工具循环）
 * 复用作 insert_cells/replace_diagram 载荷格式规范。 */
export const DRAWIO_XML_GUIDE = `drawio XML quick reference:
- Structure: prefer the simplified form <mxGraphModel><root><mxCell id="0"/><mxCell id="1" parent="0"/>...cells...</root></mxGraphModel> (drawio auto-wraps mxfile/diagram; a full <mxfile>...</mxfile> is also accepted).
- Node (shape): <mxCell id="2" value="Label" style="..." vertex="1" parent="1"><mxGeometry x="40" y="40" width="160" height="60" as="geometry"/></mxCell>.
- Edge (connector): <mxCell id="5" value="Yes" style="..." edge="1" parent="1" source="2" target="3"><mxGeometry relative="1" as="geometry"/></mxCell>; define endpoint nodes first, then reference their ids via source/target.
- Common styles: rounded=1;whiteSpace=wrap;html=1; (rounded box) / ellipse;whiteSpace=wrap;html=1; (oval) / rhombus;whiteSpace=wrap;html=1; (decision diamond) / text;html=1; (plain label); edges default to edgeStyle=orthogonalEdgeStyle;rounded=0;. Tune with fillColor / strokeColor / fontSize / fontStyle=1 (bold).
- Layout: pick left-to-right or top-down flow; keep node gaps >=40; main-flow step 160-200, branch step 100 on the cross axis; put branch labels ("Yes"/"No") in the edge value.
- ids: incrementing numeric strings "2","3",...; keep id="0" (root) and id="1" (default parent) fixed; every cell gets parent="1" unless grouping.
- Edge integrity (mandatory): source/target MUST reference ids of cells defined in the SAME model, preferably defined BEFORE the edge; never invent or forward-reference missing ids; an edge with no valid endpoints must be omitted. Every vertex MUST carry an explicit <mxGeometry> with concrete x/y/width/height — never rely on defaults.
- Overlap (mandatory): never let two node boxes intersect; align on a grid (x/y multiples of 20), keep >=40px spacing in both axes.
- Editing an existing diagram: keep the ids of unchanged elements and only modify their style/geometry/value, or add/remove cells; never renumber the whole model.
- The XML must be well-formed; escape < > & " ' inside value attributes.`

/** richtext-patch 通道 system 指令附加的「富文本编辑指令规范」（同
 * DRAWIO_XML_GUIDE 模式：英文技术规范，双语 intro 按现有 locale 机制）。
 * AI 输出 ```docflow-edit 围栏内的 JSON 指令数组；宿主执行器按 find/after
 * 定位原文（纯文本精确匹配）逐条执行增删改插。 */
const RICHTEXT_PATCH_GUIDE = `Rich text edit instruction protocol (mandatory output format):
- Reply with exactly one fenced block starting with \`\`\`docflow-edit and ending with \`\`\`, containing ONLY a JSON array of edit operations. No explanations outside the fence.
- Operations, executed in array order:
  {"op":"replace","find":"<exact original snippet>","text":"<replacement, Markdown>"}
  {"op":"insert","after":"<exact original snippet>","text":"<new content, Markdown, inserted right after that snippet>"}
  {"op":"delete","find":"<exact original snippet to remove>"}
  {"op":"replaceAll","text":"<entire new document, Markdown>"} — only when the edits affect most of the document.
- PROTECTED CONTENT: lines like [DocFlow-Embed kind="drawio" fileId="..." title="..."], [DocFlow-Image fileId="..." title="..."] and [DocFlow-File fileId="..." title="..."] in the document are embedded blocks (diagrams/whiteboards/images/file cards). They MUST be preserved VERBATIM — copy them character-for-character (all attributes unchanged) into the output "text" whenever the surrounding content is replaced or rewritten, as standalone lines at a sensible position. NEVER edit, reword, merge, split or drop them. When emitting a replaceAll you are expected to keep every placeholder line.
- Locating rules: "find"/"after" must be copied VERBATIM from the document text given in the user message (plain text only — bold/links/headings are invisible to the matcher). Pick a snippet of 10-80 characters that is unique in the document and taken from within a single paragraph/heading; never rewrite, shorten or fabricate the snippet. Do not use placeholder lines (e.g. [DocFlow-Embed ...]) inside "find"/"after" — they are not plain text and cannot be located.
- Locate every operation against the ORIGINAL document text; a snippet that earlier operations already removed is skipped, so do not chain operations onto text you are replacing.
- "text" values are Markdown (headings, lists, tables, code fences, bold, links, ...). The fence body must be valid JSON: escape " as \\" and newlines inside values as \\n.
- Prefer several small precise operations over one huge replace; never output the whole document unless replaceAll is truly needed.`

/** excalidraw-json 通道 system 指令附加的元素 JSON 生成规范（以本地
 * @excalidraw/excalidraw 0.17.6 的 types/element/types.d.ts 字段定义与
 * types/data/transform.d.ts 的骨架（ExcalidrawElementSkeleton）契约为准
 * 精编：AI 只需输出骨架字段，前端经官方 convertToExcalidrawElements
 * 自动补全 seed/versionNonce/文本量宽/绑定端点等派生字段）。技术规范
 * 统一英文（同 DRAWIO_XML_GUIDE 约定）。导出供宿主页（ExcalidrawPage
 * Agent 工具循环）复用作 insert_elements 载荷格式规范。 */
export const EXCALIDRAW_JSON_GUIDE = `Excalidraw elements JSON quick reference (element skeletons):
- Output ONE \`\`\`excalidraw-json fenced block containing a JSON ARRAY of element objects; no text outside the fence.
- Every element object MUST have "type" and numeric "x"/"y". Give each element a short unique string "id" (e.g. "n1","n2") so arrows can reference nodes; other elements may reference an id via start/end.
- Shape types (all accept optional strokeColor/backgroundColor/fillStyle/roughness/strokeWidth/strokeStyle/opacity/roundness):
  - {"type":"rectangle","id","x","y","width","height","label":{"text":"Centered label","fontSize":20}} — rounded box; label auto-centers inside.
  - {"type":"diamond",...same fields} — decision diamond; use bigger width/height for long labels.
  - {"type":"ellipse",...same fields} — oval/stadium node.
  - {"type":"text","id","x","y","text":"Standalone note","fontSize":20,"strokeColor":"#868e96"} — plain label; do NOT set width/height (auto-measured).
- Connector types:
  - {"type":"arrow","id","x","y","start":{"id":"n1"},"end":{"id":"n2"},"strokeColor":"#1e1e1e","strokeWidth":2,"label":{"text":"Yes"}} — bind endpoints to node ids; x/y and points are computed automatically from bindings, do not hand-calculate them.
  - {"type":"line","id","x","y","points":[[0,0],[120,0]]} — plain polyline with points RELATIVE to x/y, for underlines/separators only.
- Field conventions: colors as hex strings (default strokeColor "#1e1e1e", backgroundColor "transparent"); fillStyle "solid"|"hachure"; roughness 0(architect)/1(default)/2; strokeWidth 2 default; roundness {"type":3} for rounded rectangles; font sizes 16-28.
- Do NOT output seed/version/versionNonce/isDeleted/updated/groupIds/boundElements — they are generated automatically.
- Layout: place the whole diagram inside a 1200x800 region starting at (0,0); pick one flow direction (top-down or left-right) and keep >=40px gaps between elements; process node 160-200 wide and 60-80 tall; align nodes on a grid; add a standalone {"type":"text"} title at the top-left of the group.
- Binding integrity (mandatory): arrow start/end MUST reference ids of elements present in the SAME array, declared BEFORE the arrow; never invent ids; an arrow with no valid bound endpoint must be omitted. Shapes MUST carry explicit width/height — never rely on defaults.
- Overlap (mandatory): no two shape bounding boxes may intersect; align on a 20px grid; if unsure, spread elements generously — the renderer nudges overlaps apart, so precise non-overlap layout is expected from you.
- Accent sparingly: strokeColor+backgroundColor pairs like #1971c2/#a5d8ff (blue), #2f9e44/#d3f9d8 (green), #e8590c/#ffe8cc (orange), #c2255c/#ffdeeb (pink) to group related nodes; keep connectors neutral #1e1e1e or #868e96.
- The output must be strictly valid JSON (double quotes, no trailing commas, no comments).`

/** 已应用 payload 回复的折叠视图：自动应用成功后，回复正文（白板 JSON/
 * drawio XML/编辑指令等大段 payload）不再整屏裸输出——摘要卡片 + 「查看
 * 原始回复」按需展开（展开后长代码块仍受 AIMarkdown 折叠约束）。 */
function AppliedReplyView({ content, zh, label }: { content: string; zh: boolean; label: string }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="ai-applied-payload">
      <div className="ai-applied-payload-head">
        <Check size={13} strokeWidth={2} className="ok-text" aria-hidden="true" />
        <span className="ai-applied-payload-label">{label}</span>
        <Button size="small" type="link" className="ai-applied-payload-toggle" onClick={() => setOpen((v) => !v)}>
          {open ? (zh ? '收起原始回复' : 'Collapse raw reply') : (zh ? '查看原始回复' : 'View raw reply')}
        </Button>
      </div>
      {open && <AIMarkdown text={content} zh={zh} />}
    </div>
  )
}

/** 头部下拉快捷指令：send=打开面板自动发送（editMode 指定以何种模式处理）；
 * focus=打开面板并聚焦输入框（自定义指令）。 */
export type AIQuickCommand =
  | { kind: 'send'; instruction: string; editMode: boolean }
  | { kind: 'focus' }

/** 单条已应用回复的「修改点」记录（撤销回退锚点）。 */
interface AppliedRecord {
  /** 应用前版本（撤销时恢复到该版本）。 */
  versionId: string
  version: number
  /** 应用文本摘要（前 60 字，空白折叠）。 */
  summary: string
  time: string
  revoked?: boolean
}

interface ChatTurn {
  id: number
  role: 'user' | 'assistant'
  content: string
  streaming?: boolean
  error?: string
  /** 附注：用户消息的上下文范围/长度/截断说明；助手消息的停止说明。 */
  note?: string
  /** 用户回合引用的其它文件（chips 展示，全文经 context.fileIds 注入）。 */
  files?: AIAttachFile[]
  /** 推理思考聚合文本（SSE thinking 增量；折叠区展示，不计入正文）。 */
  thinking?: string
  /** 首个思考增量时间戳（ms；计算用时）。 */
  thinkingStartedAt?: number
  /** 思考耗时（ms；流结束后写入）。 */
  thinkingMS?: number
  /** 联网搜索来源（SSE meta.sources 宽松归一化；底部折叠列表展示）。 */
  webSources?: AIWebSource[]
  /** 外部工具调用（SSE tool/tool_result 生命周期合并；ThoughtChain 展示）。 */
  toolCalls?: AIToolCallEntry[]
  /** 自动应用进行中（保存+落盘）。 */
  applying?: boolean
  /** 自动应用成功的修改点记录（可撤销）。 */
  applied?: AppliedRecord
  /** 自动应用失败原因（文档未被修改）。 */
  applyError?: string
}

/** 编辑器页头部「AI 对话」按钮（主点击开面板；下拉=并入的 AIEditMenu 快捷
 * 指令，AI 未启用不渲染）。kind 定制下拉指令（默认 text 保持原文案）：
 * excalidraw-json / drawio-xml / chat（仅对话页）分别给出契合场景的
 * 快捷指令。 */
export function AIEditChatButton({
  open,
  onToggle,
  onQuick,
  disabled,
  kind = 'text',
}: {
  open: boolean
  onToggle: () => void
  /** 下拉快捷指令回调（宿主页负责打开面板并透传给 AIEditChat）。 */
  onQuick: (cmd: AIQuickCommand) => void
  disabled?: boolean
  /** 下拉快捷指令形态（默认 text，与既有文本编辑页一致）。 */
  kind?: AIApplyKind | 'chat'
}) {
  const locale = useLocale()
  const aiOn = useAIEnabled()
  if (!aiOn) return null
  const zh = locale === 'zh-CN'
  type QuickItem = { key: string; label: string; instruction?: string; editMode?: boolean } | { key: string; label: string; focus: true }
  const itemsByKind: Record<AIApplyKind | 'chat', Array<QuickItem | 'divider'>> = {
    text: [
      { key: 'summary', label: t(locale, 'aiEditSummary'), instruction: zh ? '请总结当前文档' : 'Summarize the current document', editMode: false },
      { key: 'continue', label: t(locale, 'aiEditContinue'), instruction: zh ? '请顺着当前内容自然续写' : 'Continue writing naturally from the current content', editMode: true },
      { key: 'polish', label: t(locale, 'aiEditPolish'), instruction: zh ? '请润色当前文本，保持原意' : 'Polish the current text while keeping its meaning', editMode: true },
      { key: 'translate-en', label: t(locale, 'aiEditTranslateEn'), instruction: zh ? '把选区翻译成英文' : 'Translate the selection into English', editMode: false },
      'divider',
      { key: 'custom', label: t(locale, 'aiEditCustom'), focus: true },
    ],
    'excalidraw-json': [
      { key: 'flowchart', label: zh ? '生成流程图' : 'Flowchart', instruction: zh ? '画一个流程图（若白板上下文未给出主题，请生成一个通用的审批流程）' : 'Draw a flowchart (fall back to a generic approval flow if the whiteboard context has no topic)', editMode: true },
      { key: 'architecture', label: zh ? '生成架构图' : 'Architecture diagram', instruction: zh ? '画一个系统架构图（若上下文未给出主题，请生成一个前后端+数据库的三层架构示例）' : 'Draw an architecture diagram (fall back to a 3-tier web/database example if no topic in context)', editMode: true },
      { key: 'mindmap', label: zh ? '生成概念图' : 'Concept map', instruction: zh ? '画一个概念/思维导图（若上下文未给出主题，请围绕「产品设计」发散）' : 'Draw a concept/mind map (fall back to a “product design” topic if no topic)', editMode: true },
      'divider',
      { key: 'custom', label: t(locale, 'aiEditCustom'), focus: true },
    ],
    'drawio-xml': [
      { key: 'flowchart', label: zh ? '生成流程图' : 'Flowchart', instruction: zh ? '生成一个流程图（若上下文未给出主题，请生成一个通用的审批流程）' : 'Generate a flowchart (fall back to a generic approval flow if no topic)', editMode: true },
      { key: 'architecture', label: zh ? '生成架构图' : 'Architecture diagram', instruction: zh ? '生成一个系统架构图（若上下文未给出主题，请生成一个前后端+数据库的三层架构示例）' : 'Generate an architecture diagram (fall back to a 3-tier web/database example if no topic)', editMode: true },
      'divider',
      { key: 'custom', label: t(locale, 'aiEditCustom'), focus: true },
    ],
    // 富文本指令式编辑：快捷指令提示词显式引导走 docflow-edit 指令模式。
    'richtext-patch': [
      { key: 'polish', label: t(locale, 'aiEditPolish'), instruction: zh ? '请润色当前文档：找出需要改写的句子，逐条输出 replace 编辑指令，保持原意；[DocFlow-…] 占位行为受保护的嵌入内容，必须原样保留' : 'Polish the document: emit one replace instruction per sentence that needs rewriting, keeping the meaning; [DocFlow-...] placeholder lines are protected embeds and must be kept verbatim', editMode: true },
      { key: 'rewrite', label: zh ? '重构全文' : 'Rewrite all', instruction: zh ? '请重构全文结构与措辞：若改动覆盖大半文档，输出 replaceAll 整篇替换，否则分条 replace。文档中的 [DocFlow-Embed / DocFlow-Image / DocFlow-File] 占位行是嵌入的图表/图片/文件，必须逐字保留在输出中，不得删改' : 'Restructure the whole document: output a replaceAll operation if most of it changes, otherwise several replace operations. [DocFlow-Embed / DocFlow-Image / DocFlow-File] placeholder lines are embedded diagrams/images/files and must be kept verbatim in the output', editMode: true },
      { key: 'fix', label: zh ? '修正错别字' : 'Fix typos', instruction: zh ? '请找出并修正文档中的错别字与标点错误，逐条输出 replace 编辑指令' : 'Find and fix typos and punctuation errors, one replace instruction each', editMode: true },
      { key: 'summary', label: t(locale, 'aiEditSummary'), instruction: zh ? '请总结当前文档' : 'Summarize the current document', editMode: false },
      'divider',
      { key: 'custom', label: t(locale, 'aiEditCustom'), focus: true },
    ],
    chat: [
      { key: 'summary', label: t(locale, 'aiEditSummary'), instruction: zh ? '请总结当前文档' : 'Summarize the current document', editMode: false },
      { key: 'translate-en', label: t(locale, 'aiEditTranslateEn'), instruction: zh ? '把文档内容翻译成英文' : 'Translate the document into English', editMode: false },
      'divider',
      { key: 'custom', label: t(locale, 'aiEditCustom'), focus: true },
    ],
  }
  const items: MenuProps['items'] = itemsByKind[kind].map((item, i) =>
    item === 'divider' ? { key: `d${i}`, type: 'divider' as const } : { key: item.key, label: item.label },
  )
  const onMenuClick: MenuProps['onClick'] = ({ key }) => {
    for (const item of itemsByKind[kind]) {
      if (item === 'divider' || item.key !== key) continue
      if ('focus' in item) onQuick({ kind: 'focus' })
      else onQuick({ kind: 'send', instruction: item.instruction ?? '', editMode: !!item.editMode })
    }
  }
  return (
    <SplitButton
      size="small"
      type={open ? 'primary' : 'default'}
      menu={{ items, onClick: onMenuClick }}
      arrowLabel={zh ? '快捷指令' : 'Quick actions'}
      disabled={disabled}
      onClick={onToggle}
    >
      <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
      <span>{zh ? 'AI 对话' : 'AI chat'}</span>
    </SplitButton>
  )
}

/**
 * AI 对话编辑面板（右侧内嵌；open=false 或 AI 未启用返回 null——组件保持
 * 挂载，收起不清空会话）。宿主页提供选区读取 getTarget（无选区时 text=
 * 全文）、全文 getAllText 与结果落盘通道：onApply（insert=光标/选区末尾
 * 插入；replace=替换选区）、onAppend（无选区时追加文档末尾）；版本保护
 * 需要 ensureSaved（应用前确保已保存并返回应用前版本）、reload（撤销回退
 * 后刷新编辑器内容）与 fileId（restoreVersion 用）。
 */
export default function AIEditChat({
  open,
  onClose,
  getTarget,
  getAllText,
  onApply,
  onAppend,
  fileId,
  ensureSaved,
  reload,
  quickCommand,
  onQuickConsumed,
  applyKind = 'text',
  outputFormat = 'plaintext',
  agentTools,
  agentSystemExtra,
}: {
  open: boolean
  onClose: () => void
  /** 当前选区（text 为选中文本；无选区时返回全文与 hasSelection=false）。 */
  getTarget: () => AIEditTarget
  /** 全文（「全文」范围与无选区回退时的输入）。 */
  getAllText: () => string
  /** 应用结果（宿主页面负责编辑器落盘与 dirty 标记）。返回 string=应用
   * 失败原因（文档未被修改，显示为 applyError）；Promise 版本供元素 JSON
   * 转换等异步落盘（text 场景维持同步 void，行为不变）。 */
  onApply: (mode: 'insert' | 'replace', output: string) => string | void | Promise<string | void>
  /** 无选区时追加到文档末尾（缺省回退 onApply('insert')）。 */
  onAppend?: (output: string) => void
  /** 文件 id（撤销时 restoreVersion 用）。 */
  fileId: string
  /** 确保当前内容已保存为新版本，返回应用前版本（null=保存失败）。 */
  ensureSaved: () => Promise<{ versionId: string; version: number } | null>
  /** 撤销回退后刷新编辑器内容（含 dirty 清理）。 */
  reload: () => Promise<void>
  /** 头部下拉快捷指令（非 null 时自动执行一次）。 */
  quickCommand?: AIQuickCommand | null
  /** 快捷指令执行后回调（宿主页清空 quickCommand）。 */
  onQuickConsumed?: () => void
  /** 应用通道（默认 text；excalidraw-json/drawio-xml 见 AIApplyKind）。 */
  applyKind?: AIApplyKind
  /** text 通道输出格式（默认 plaintext=原样纯文本输出，Monaco 等用）；
   * markdown=富文本宿主（.dfrt 编辑页）：system 指令要求输出 Markdown，
   * 宿主经 marked 转富文本 HTML 插入。仅 applyKind='text' 生效。 */
  outputFormat?: 'markdown' | 'plaintext'
  /** 宿主注册的编辑工具集（v5 内置 Agent 模式）：提供时面板切换为
   *  TOOL_CALL/FINAL 工具循环——AI 自主读文档/定位/编辑/验证（编辑范围
   *  由 AI 判断），工具经 exec 在宿主编辑器上执行，过程以工具链 UI 展示。
   *  未提供（富文本指令/白板/图表等既有直编通道）保持原单轮模式。 */
  agentTools?: AIEditTool[]
  /** Agent 模式 system 提示词的宿主附加段（白板/图表等工具载荷格式规范、
   *  宿主特定约束），拼在工具清单与输出格式约定之间。 */
  agentSystemExtra?: string
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const aiOn = useAIEnabled()
  const { message } = AntdApp.useApp()
  const [turns, setTurns] = useState<ChatTurn[]>([])
  // 会话持久化：按 fileId 存 localStorage（docflow.ai.edit.conv.{fileId}），
  // 面板重开时恢复最近一次会话（无 fileId 不持久化）；保存时剥离流式/进行
  // 中等瞬态字段并截断到最近 40 条，避免存储配额问题。
  const convKey = fileId ? `docflow.ai.edit.conv.${fileId}` : ''
  useEffect(() => {
    if (!convKey) return
    try {
      const raw = window.localStorage.getItem(convKey)
      if (raw) {
        const saved = JSON.parse(raw) as ChatTurn[]
        if (Array.isArray(saved) && saved.length > 0) setTurns(saved)
      }
    } catch {
      /* 损坏的记录忽略 */
    }
    // 仅在挂载时恢复一次（convKey 变化=切换文件场景，恢复新文件会话）。
  }, [convKey])
  useEffect(() => {
    if (!convKey) return
    // 空会话也写入（清空后重开不再复活旧消息）；进行中的回合（流式/应用
    // 中）不落盘，等完成后下一次更新再存。
    const busyTurn = turns.some((x) => x.streaming || x.applying)
    if (busyTurn) return
    try {
      const persist = turns.slice(-40).map((x) => ({ ...x, streaming: undefined, applying: undefined }))
      window.localStorage.setItem(convKey, JSON.stringify(persist))
    } catch {
      /* 配额/序列化失败忽略 */
    }
  }, [convKey, turns])
  const [input, setInput] = useState('')
  const [scope, setScope] = useState<ChatScope>('selection')
  const [hasSelection, setHasSelection] = useState(false)
  const [selectionLen, setSelectionLen] = useState(0)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  /** 面板模式：可修改（默认）/仅对话（ref 供快捷指令立即生效）。 */
  const [mode, setMode] = useState<ChatMode>('edit')
  const modeRef = useRef<ChatMode>('edit')
  /** 正在执行撤销的回合 id（单飞：同一时间只允许一次版本回退）。 */
  const [undoingId, setUndoingId] = useState<number | null>(null)
  // 联网/思考开关：与全局助手共用 localStorage key 与默认逻辑
  //（所选模型从 docflow.ai.model 读取，无记忆时回落 default_models.chat；
  //  模型列表仅用于评估思考能力）。
  const [models, setModels] = useState<AIModelOption[]>([])
  const [modelKey, setModelKey] = useState(() => {
    try {
      return window.localStorage.getItem(AI_MODEL_STORAGE_KEY) ?? ''
    } catch {
      return ''
    }
  })
  // 用户是否显式选过模型（与全局助手同一 localStorage 记忆判定）。
  const [modelExplicit, setModelExplicit] = useState(() => {
    try {
      return (window.localStorage.getItem(AI_MODEL_STORAGE_KEY) ?? '') !== ''
    } catch {
      return false
    }
  })
  const toggles = useAIChatToggles(models, modelKey)
  // 文件引用（v3.9）：随指令附带其它文件全文（context.fileIds 注入 system
  // 上下文；当前文档本身已是主上下文，引用的是「其它」文件）。
  const [attached, setAttached] = useState<AIAttachFile[]>([])
  const [attachOpen, setAttachOpen] = useState(false)
  const [attachQuery, setAttachQuery] = useState('')
  const [attachItems, setAttachItems] = useState<Array<{ id: string; name: string }>>([])
  const [attachLoading, setAttachLoading] = useState(false)
  const toggleAttach = (f: { id: string; name: string }) => {
    setAttached((prev) => (prev.some((x) => x.fileId === f.id)
      ? prev.filter((x) => x.fileId !== f.id)
      : [...prev, { fileId: f.id, fileName: f.name }]))
  }
  // 引用候选（空关键词 = 最近访问；否则 350ms 防抖全文搜索）。
  useEffect(() => {
    if (!attachOpen) return
    let alive = true
    const q = attachQuery.trim()
    const run = (pr: Promise<Array<{ id: string; name: string; type?: string }>>) => {
      setAttachLoading(true)
      void pr
        .then((items) => { if (alive) setAttachItems(items.filter((x) => x.type !== 'folder').map((x) => ({ id: x.id, name: x.name }))) })
        .catch(() => { if (alive) setAttachItems([]) })
        .finally(() => { if (alive) setAttachLoading(false) })
    }
    if (!q) {
      run(listFiles(null, { recent: true, limit: 20 }))
      return () => { alive = false }
    }
    const timer = window.setTimeout(() => run(searchFiles(q, 20)), 350)
    return () => {
      alive = false
      window.clearTimeout(timer)
    }
  }, [attachOpen, attachQuery])
  const turnIdRef = useRef(0)
  const abortRef = useRef<AbortController | null>(null)
  const listRef = useRef<HTMLDivElement | null>(null)
  const inputRef = useRef<TextAreaRef | null>(null)
  // 命令式访问器经 ref 取（宿主每渲染重建函数，避免 effect 反复重挂）。
  const getTargetRef = useRef(getTarget)
  getTargetRef.current = getTarget
  const getAllTextRef = useRef(getAllText)
  getAllTextRef.current = getAllText
  const onApplyRef = useRef(onApply)
  onApplyRef.current = onApply
  const onAppendRef = useRef(onAppend)
  onAppendRef.current = onAppend
  const fileIdRef = useRef(fileId)
  fileIdRef.current = fileId
  const ensureSavedRef = useRef(ensureSaved)
  ensureSavedRef.current = ensureSaved
  const reloadRef = useRef(reload)
  reloadRef.current = reload

  // 卸载清理：中止进行中的生成。
  useEffect(() => () => { abortRef.current?.abort() }, [])

  // 模型列表（仅用于评估思考开关可用性与显式携带所选模型；与全局助手共用
  // getAIModels 缓存）；无本地记忆时回落 default_models.chat 默认模型。
  useEffect(() => {
    if (!aiOn) return
    void getAIModels().then((list) => {
      setModels(list)
      setModelKey((cur) => {
        if (cur && list.some((m) => m.id === cur)) return cur
        const dkey = defaultAIModelKey()
        return dkey && list.some((m) => m.id === dkey) ? dkey : cur
      })
    })
  }, [aiOn])

  // 当前文件名（技能 {file} 占位符替换用）：宿主仅传 fileId，面板打开时按
  // GET /files/:id 解析一次；失败置空（占位符按空处理，不新增编辑器依赖）。
  const [fileName, setFileName] = useState('')
  useEffect(() => {
    if (!open || !aiOn || !fileId) return
    let alive = true
    getFileMeta(fileId)
      .then((meta) => {
        if (alive) setFileName(meta.name)
      })
      .catch(() => {
        if (alive) setFileName('')
      })
    return () => {
      alive = false
    }
  }, [open, aiOn, fileId])

  // 新回合/流式更新时滚动到底部。
  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight })
  }, [turns])

  // 面板打开期间轻量轮询选区（「选区」选项禁用态、范围提示联动）。
  useEffect(() => {
    if (!open || !aiOn) return
    const check = () => {
      const tgt = getTargetRef.current()
      setHasSelection(tgt.hasSelection)
      setSelectionLen(tgt.hasSelection ? tgt.text.length : 0)
    }
    check()
    const timer = window.setInterval(check, 1000)
    return () => window.clearInterval(timer)
  }, [open, aiOn])

  // 选区丢失时回退「全文」（「选区」选项已禁用，避免悬空取值）。
  useEffect(() => {
    if (!hasSelection && scope === 'selection') setScope('full')
  }, [hasSelection, scope])

  // 头部下拉快捷指令：send=切模式并自动发送；focus=聚焦输入框。
  useEffect(() => {
    if (!open || !aiOn || !quickCommand) return
    onQuickConsumed?.()
    if (quickCommand.kind === 'focus') {
      inputRef.current?.focus()
      return
    }
    // 快捷指令自带处理模式：续写/润色→可修改（自动应用）；摘要/翻译→仅对话。
    const m: ChatMode = !quickCommand.editMode ? 'chat' : 'edit'
    setMode(m)
    modeRef.current = m
    void send(quickCommand.instruction)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [quickCommand, open, aiOn])

  /** 切换面板模式（可修改/仅对话）。 */
  const changeMode = (m: ChatMode) => {
    setMode(m)
    modeRef.current = m
  }

  // ---- v5 内置 Agent 工具循环（agentTools 提供时启用） ----
  // 协议与 Office 插件一致：AI 每轮输出单行 TOOL_CALL {...}（一次一个工具）
  // 或 FINAL 总结；exec 后结果以 TOOL_RESULT 回喂继续，直至 FINAL/上限。
  // 工具过程复用 turn.toolCalls（AIToolChain：running→success/error）。
  const agentToolsRef = useRef(agentTools)
  agentToolsRef.current = agentTools
  const agentSystemExtraRef = useRef(agentSystemExtra)
  agentSystemExtraRef.current = agentSystemExtra

// 括号深度扫描提取 TOOL_CALL {...}（字符串感知：引号内的 {} 与转义不计
// 深度，支持嵌套/跨行；从最后一个候选向前取首个可解析的——推理模型在
// 工具行外漏思考散文、重复多行时的鲁棒解析，与 Office 插件同实现）。
  function extractCallJson(t: string): { tool: string; args: Record<string, unknown> } | null {
    let k = 0
    const found: string[] = []
    while (found.length < 8) {
      const at = t.indexOf('TOOL_CALL', k)
      if (at < 0) break
      const brace = t.indexOf('{', at)
      if (brace < 0) break
      let depth = 0, inStr = false, escp = false, end = -1
      for (let i2 = brace; i2 < t.length; i2++) {
        const ch = t.charAt(i2)
        if (escp) { escp = false; continue }
        if (ch === '\\') { if (inStr) escp = true; continue }
        if (ch === '"') { inStr = !inStr; continue }
        if (inStr) continue
        if (ch === '{') depth++
        else if (ch === '}') { depth--; if (depth === 0) { end = i2; break } }
      }
      if (end < 0) break
      found.push(t.slice(brace, end + 1))
      k = end + 1
    }
    for (let j2 = found.length - 1; j2 >= 0; j2--) {
      try {
        const c = JSON.parse(found[j2]) as { tool: string; args?: Record<string, unknown> }
        if (c && typeof c.tool === 'string') return { tool: c.tool, args: c.args ?? {} }
      } catch { /* 候选不完整 → 跳过 */ }
    }
    return null
  }
  const parseAgentReply = (text: string): { type: 'final'; text: string } | { type: 'tool'; call: { tool: string; args: Record<string, unknown> } } => {
    const t = text.trim()
    if (t.startsWith('FINAL')) return { type: 'final', text: t.slice(5).trim() || t }
    const call = extractCallJson(t)
    if (call) return { type: 'tool', call }
    return { type: 'final', text: t }
  }

  const patchLastTurn = (turnId: number, patch: (x: ChatTurn) => Partial<ChatTurn>) => {
    setTurns((prev) => prev.map((x) => (x.id === turnId ? { ...x, ...patch(x) } : x)))
  }

  /** Agent 模式发送：工具循环 + FINAL；过程写入助手回合（toolCalls 链）。 */
  const sendAgent = async (question: string) => {
    const tools = agentToolsRef.current ?? []
    const hostLabel = applyKind === 'excalidraw-json'
      ? 'Excalidraw 白板'
      : applyKind === 'drawio-xml'
        ? 'draw.io 图表'
        : applyKind === 'richtext-patch'
          ? '富文本文档'
          : '文本编辑器'
    const extra = agentSystemExtraRef.current
    const sys = [
      `你是 DocFlow 内置文档编辑 Agent（宿主：${hostLabel}）。通过调用工具直接操作当前文档，用户只描述意图。`,
      '',
      '可用工具（每次回复恰好一行 TOOL_CALL {...} 调用一个，或以 FINAL 开头给出最终答复）：',
      ...tools.map((t) => `- ${t.name}：${t.desc}`),
      ...(extra ? ['', extra] : []),
      '',
      '输出格式（硬性要求——违反则本轮无效）：每轮回复的完整内容必须恰好是一行 TOOL_CALL {...}（单个工具、合法 JSON、前后不得有任何解释/思考/推理文字），或以 FINAL 开头的最终答复。',
      '禁止输出 "I\'ll"、"Let me"、"首先"等散文开头。不要解释你要做什么——直接输出 TOOL_CALL 或 FINAL。',
      '工作方式：先读（read_*/search_*）再改；局部修改用精确工具、不要整篇重写；一次一个工具，根据结果决定下一步；失败读错误换路径；关键修改后可用读工具校验；完成后 FINAL + 简明中文总结（改了什么、在哪）。',
      'TOOL_CALL 示例：TOOL_CALL {"tool":"read_document","args":{}}',
    ].join(String.fromCharCode(10))
    const userTurnId = ++turnIdRef.current
    const asstTurnId = ++turnIdRef.current
    const tgt = getTargetRef.current()
    setInput('')
    setNotice('')
    setTurns((prev) => [
      ...prev,
      {
        id: userTurnId, role: 'user', content: question,
        note: tgt.hasSelection ? `选区 ${tgt.text.length} 字` : '全文上下文',
      },
      { id: asstTurnId, role: 'assistant', content: '', streaming: true },
    ])
    setBusy(true)
    const controller = new AbortController()
    abortRef.current = controller
    const convo: AIMessage[] = [{ role: 'system', content: sys }, { role: 'user', content: question }]
    // 白板/图表多元素插入 + 读校验轮次较多（Office 插件侧为 50）
    const MAX_ROUNDS = 24
    try {
      let finalText = ''
      for (let round = 1; round <= MAX_ROUNDS; round++) {
        let acc = ''
        await aiChat(
          { messages: convo, think: toggles.think ? true : undefined, web_search: toggles.web ? true : undefined },
          {
            onDelta: (chunk) => {
              acc += chunk
              patchLastTurn(asstTurnId, () => ({ content: acc }))
            },
            onThinking: (th) => {
              patchLastTurn(asstTurnId, (x) => ({
                thinking: (x.thinking ?? '') + th,
                thinkingStartedAt: x.thinkingStartedAt ?? Date.now(),
              }))
            },
            onMeta: () => {},
            onTool: () => {},
            onToolResult: () => {},
            onSources: () => {},
          },
          controller.signal,
        )
        const parsed = parseAgentReply(acc)
        if (parsed.type === 'final') {
          // 纯散文回复（无 TOOL_CALL / FINAL 标记）→ 纠正后重试（≤2 次）
          if (!acc.trim().startsWith('FINAL') && round < MAX_ROUNDS - 2) {
            convo.push({ role: 'assistant', content: acc.slice(0, 200) })
            convo.push({ role: 'user', content: '你刚才输出了散文而非 TOOL_CALL。请严格按格式输出：恰好一行 TOOL_CALL {"tool":"工具名","args":{...}}，不要任何解释文字。' })
            continue
          }
          finalText = parsed.text
          break
        }
        const tool = parsed.call.tool
        const args = parsed.call.args
        const def = tools.find((t) => t.name === tool)
        // 工具链条目：running → 终态（exec 结果/错误）
        const entryId = Date.now() + round
        patchLastTurn(asstTurnId, (x) => ({
          toolCalls: [...(x.toolCalls ?? []), { label: def ? def.label(args) : `${tool}`, server: 'docflow', tool, input: JSON.stringify(args).slice(0, 400), status: 'running' as const, _id: entryId }],
        }))
        let result: { ok: boolean; data?: unknown; error?: string }
        if (!def) {
          result = { ok: false, error: `未知工具 ${tool}（可用：${tools.map((t) => t.name).join(', ')}）` }
        } else {
          try {
            result = await def.exec(args)
          } catch (err) {
            result = { ok: false, error: err instanceof Error ? err.message : String(err) }
          }
        }
        patchLastTurn(asstTurnId, (x) => ({
          toolCalls: (x.toolCalls ?? []).map((e) => (
            (e as { _id?: number })._id === entryId
              ? { ...e, status: result.ok ? ('success' as const) : ('error' as const), output: result.ok ? JSON.stringify(result.data ?? { ok: true }).slice(0, 400) : (result.error ?? '失败') }
              : e
          )),
        }))
        convo.push({ role: 'assistant', content: `TOOL_CALL ${JSON.stringify({ tool, args })}` })
        convo.push({ role: 'user', content: `TOOL_RESULT ${JSON.stringify(result).slice(0, 3000)}` })
      }
      patchLastTurn(asstTurnId, (x) => ({
        content: finalText || '（未产生最终答复）',
        streaming: false,
        thinkingMS: x.thinkingStartedAt ? Math.max(0, Date.now() - x.thinkingStartedAt) : undefined,
        note: finalText ? undefined : '已达工具轮次上限，已执行操作保留',
      }))
    } catch (err) {
      const aborted = err instanceof Error && err.name === 'AbortError'
      patchLastTurn(asstTurnId, (x) => ({
        streaming: false,
        note: aborted ? '已停止（已执行操作保留）' : undefined,
        error: aborted ? undefined : (err instanceof Error ? err.message : t(locale, 'aiAssistantErr')),
        thinkingMS: x.thinkingStartedAt ? Math.max(0, Date.now() - x.thinkingStartedAt) : undefined,
      }))
    } finally {
      abortRef.current = null
      setBusy(false)
    }
  }

  /** 发送一轮：每轮独立构造 prompt（系统约束 + 最近 2 轮历史 + 指令与
   * 当前选区/全文上下文），SSE 流式渲染；停止/失败落在助手回合上。
   * 可修改模式下正常完成后自动应用（见 autoApply）；中止/失败不应用。 */
  const send = async (question: string) => {
    const text = question.trim()
    if (!text || busy) return
    if (agentTools && agentTools.length > 0) { await sendAgent(text); return }
    // 以发送时刻的模式为准（流式期间切换不影响本轮）。
    const applyMode = modeRef.current
    const tgt = getTargetRef.current()
    const useSelection = scope === 'selection' && tgt.hasSelection
    const full = useSelection ? tgt.text : getAllTextRef.current()
    // 空上下文：文本编辑页维持原拦截；excalidraw/drawio/仅对话场景允许无
    // 上下文发送（空白画布从零生成、Office 无转换文本时直接提问）。
    if (!full.trim() && applyKind === 'text') {
      setNotice(zh ? '文档内容为空，无法作为上下文发送' : 'Nothing to send: the document is empty')
      return
    }
    const truncated = full.length > MAX_CONTEXT_CHARS
    const content = truncated ? full.slice(0, MAX_CONTEXT_CHARS) : full
    const scopeLabel = useSelection ? (zh ? '选区' : 'selection') : (zh ? '全文' : 'whole document')
    const truncNote = truncated
      ? (zh ? `，已截断至前 ${MAX_CONTEXT_CHARS} 字符` : `, truncated to the first ${MAX_CONTEXT_CHARS} chars`)
      : ''
    // 最近 2 轮问答历史（跳过出错/空回合；每轮仍重新携带当前上下文）。
    const history: AIMessage[] = turns
      .filter((x) => !x.error && x.content)
      .slice(-HISTORY_ROUNDS * 2)
      .map((x) => ({ role: x.role, content: x.content }))
    const system = applyKind === 'excalidraw-json'
      ? (zh
        ? `你是白板图表助手。请按用户需求直接输出 Excalidraw 元素 JSON 数组（rectangle/ellipse/diamond/text/arrow/line 元素骨架），只输出一个 \`\`\`excalidraw-json 代码块（以 [ 开头、] 结尾），代码块之外不要任何解释——该 JSON 将被直接转换为白板原生元素插入画布。\n\n${EXCALIDRAW_JSON_GUIDE}`
        : `You are a whiteboard diagram assistant. Output Excalidraw element JSON directly per the request (rectangle/ellipse/diamond/text/arrow/line skeletons) — exactly one \`\`\`excalidraw-json fenced block (starting with [ and ending with ]), no explanations outside it. The JSON is converted into native whiteboard elements and inserted onto the canvas.\n\n${EXCALIDRAW_JSON_GUIDE}`)
      : applyKind === 'drawio-xml'
        ? (zh
          ? `你是 draw.io 图表助手。基于给定的当前图表 XML，按指令输出修改后的完整 drawio XML（以 <mxGraphModel>...</mxGraphModel> 或 <mxfile>...</mxfile> 包裹）。只输出 XML 本身，不要解释。\n\n${DRAWIO_XML_GUIDE}`
          : `You are a draw.io diagram assistant. Based on the given current diagram XML, output the complete modified drawio XML (wrapped in <mxGraphModel>...</mxGraphModel> or <mxfile>...</mxfile>). Output only the XML itself, no explanations.\n\n${DRAWIO_XML_GUIDE}`)
        : applyKind === 'richtext-patch'
          ? (zh
            ? `你是富文本文档编辑助手。请按用户指令对给定文档进行修改（新增、插入、删除、替换），修改以下述「编辑指令」表达——系统会自动定位并逐条执行，不要直接输出修改后的全文。文档中的 [DocFlow-Embed / DocFlow-Image / DocFlow-File] 占位行是嵌入的图表/白板/图片/文件块，属受保护内容，必须逐字保留在编辑指令里。在对话回复中引用这些嵌入内容时，用标题指代（如「流程图"新图表"」），不要把 [DocFlow-...] 占位语法复制到对话正文。\n\n${RICHTEXT_PATCH_GUIDE}`
            : `You are a rich text document editing assistant. Apply the user's requested changes (add, insert, delete, replace) as edit instructions per the protocol below — they are located and executed automatically; do NOT output the whole modified document. [DocFlow-Embed / DocFlow-Image / DocFlow-File] placeholder lines in the document are protected embedded blocks (diagrams/whiteboards/images/files) and must be preserved verbatim in edit instructions. When referencing them in conversation replies, use their titles — NEVER copy the raw [DocFlow-...] syntax into your response text.\n\n${RICHTEXT_PATCH_GUIDE}`)
          : outputFormat === 'markdown'
          ? (zh
            ? '你是富文本文档写作助手。请按指令处理给定文本，仅输出最终内容本身：不要解释、不要说明。请用 Markdown 输出结果（标题 #/##/###、有序与无序列表、表格、代码块、加粗、斜体、链接等）——内容将被转换为富文本样式插入文档。'
            : 'You are a rich text document writing assistant. Process the given text per the instruction and output only the final content itself: no explanations. Write the result in Markdown (headings #/##/###, ordered and unordered lists, tables, code blocks, bold, italic, links, etc.) — it will be converted into rich text styles and inserted into the document.')
          : zh
            ? '你是文档写作助手。请按指令处理给定文本，仅输出最终内容本身：不要解释、不要说明，不要使用代码围栏。可以输出 Markdown 格式。'
            : 'You are a writing assistant. Process the given text per the instruction and output only the final content itself: no explanations and no code fences. Markdown formatting is allowed.'
    const contextLabel = applyKind === 'excalidraw-json'
      ? (zh ? '当前白板内容摘要' : 'Current whiteboard summary')
      : applyKind === 'drawio-xml'
        ? (zh ? '当前图表 XML' : 'Current diagram XML')
        : (zh ? '待处理文本' : 'Text to process')
    const userContent = full.trim()
      ? `${zh ? '指令' : 'Instruction'}：${text}\n\n${contextLabel}（${scopeLabel}${truncNote}）：\n<text>\n${content}\n</text>`
      : `${zh ? '指令' : 'Instruction'}：${text}`
    const userTurnId = ++turnIdRef.current
    const asstTurnId = ++turnIdRef.current
    const sentFiles = attached.length > 0 ? [...attached] : undefined
    setInput('')
    setNotice('')
    setAttached([])
    setTurns((prev) => [
      ...prev,
      {
        id: userTurnId,
        role: 'user',
        content: text,
        files: sentFiles,
        note: full.trim()
          ? `${scopeLabel} · ${full.length} ${zh ? '字符' : 'chars'}${truncated ? (zh ? '（已截断）' : ' (truncated)') : ''}`
          : (zh ? '无文档上下文' : 'No document context'),
      },
      { id: asstTurnId, role: 'assistant', content: '', streaming: true },
    ])
    setBusy(true)
    const controller = new AbortController()
    abortRef.current = controller
    let result = ''
    // 所选模型（本地记忆 ?? default_models.chat）：显式携带 providerId/model。
    const selectedModel = modelKey ? models.find((m) => m.id === modelKey) ?? null : null
    try {
      await aiChat(
        {
          messages: [
            { role: 'system', content: system },
            ...history,
            { role: 'user', content: userContent },
          ],
          providerId: selectedModel?.providerId || undefined,
          model: selectedModel ? { providerId: selectedModel.providerId, modelId: selectedModel.model } : undefined,
          // 联网/思考开关：选中时携带（后端未配置/模型不支持时静默忽略）。
          web_search: toggles.web ? true : undefined,
          think: toggles.think ? true : undefined,
          // MCP 工具开关：开启时允许调用平台配置的外部 MCP 服务器工具。
          use_mcp: toggles.mcp ? true : undefined,
          // 我的文件（RAG 引用本人文档）：默认开（后端就绪前透传、忽略）。
          include_docs: toggles.docs ? true : undefined,
          // 引用文件全文注入上下文（context.fileIds；aiChat 空数组不下发）。
          fileIds: attached.length > 0 ? attached.map((f) => f.fileId) : undefined,
        },
        {
          onMeta: (meta) => {
            // 联网来源：SSE meta.sources 宽松读取（与全局助手同一归一化）。
            const ws = normalizeWebSources((meta as { sources?: unknown }).sources)
            if (ws.length > 0) {
              setTurns((prev) => prev.map((x) => (x.id === asstTurnId ? { ...x, webSources: ws } : x)))
            }
          },
          onDelta: (chunk) => {
            result += chunk
            setTurns((prev) => {
              const next = [...prev]
              const last = next[next.length - 1]
              next[next.length - 1] = { ...last, content: last.content + chunk }
              return next
            })
          },
          // 推理思考增量：聚合到独立折叠区（首个增量记录起始时间）。
          onThinking: (text) => {
            setTurns((prev) => {
              const next = [...prev]
              const last = next[next.length - 1]
              next[next.length - 1] = {
                ...last,
                thinking: (last.thinking ?? '') + text,
                thinkingStartedAt: last.thinkingStartedAt ?? Date.now(),
              }
              return next
            })
          },
          // 工具调用生命周期：执行前追加 running 条目，执行后合并终态/摘要。
          onTool: (tool) => {
            setTurns((prev) => prev.map((x) => (x.id === asstTurnId ? { ...x, toolCalls: [...(x.toolCalls ?? []), toolEntryFrom(tool)] } : x)))
          },
          onToolResult: (r) => {
            setTurns((prev) => prev.map((x) => (x.id === asstTurnId ? { ...x, toolCalls: applyToolResult(x.toolCalls ?? [], r) } : x)))
          },
        },
        controller.signal,
      )
      // 正常完成（未中止）且可修改模式 → 自动应用（版本保护内置于 autoApply）。
      if (applyMode === 'edit' && result.trim()) await autoApply(asstTurnId, result)
    } catch (err) {
      const aborted = err instanceof Error && err.name === 'AbortError'
      setTurns((prev) => {
        const next = [...prev]
        const last = next[next.length - 1]
        next[next.length - 1] = aborted
          ? { ...last, note: zh ? '已停止生成（未应用）' : 'Generation stopped (not applied)' }
          : { ...last, error: err instanceof Error ? err.message : t(locale, 'aiAssistantErr') }
        return next
      })
    } finally {
      abortRef.current = null
      setTurns((prev) => {
        const next = [...prev]
        const last = next[next.length - 1]
        next[next.length - 1] = last.thinkingStartedAt
          ? { ...last, streaming: false, thinkingMS: Math.max(0, Date.now() - last.thinkingStartedAt) }
          : { ...last, streaming: false }
        return next
      })
      setBusy(false)
    }
  }

  /** 自动应用回复到文档（版本保护）：
   * 1) 应用前先 ensureSaved——有未保存修改先保存，取「应用前版本」；
   * 2) 落盘：text 通道——有选区→替换选区（onApply replace）、无选区→追加
   *    文档末尾（onAppend，缺省回退 onApply insert）；excalidraw-json/
   *    drawio-xml 通道——从回复提取元素 JSON/drawio XML，统一 onApply
   *    ('replace', 提取结果)，宿主返回/抛出错误则显示 applyError（文档未被
   *    修改）；
   * 3) 在消息上记录修改点（版本/时间/摘要），供「撤销此修改」回退。 */
  const autoApply = async (turnId: number, content: string) => {
    setTurns((prev) => prev.map((x) => (x.id === turnId ? { ...x, applying: true } : x)))
    const saved = await ensureSavedRef.current()
    if (!saved) {
      setTurns((prev) => prev.map((x) => (x.id === turnId
        ? { ...x, applying: false, applyError: zh ? '自动应用前保存失败，文档未被修改；请手动保存后重试' : 'Failed to save before applying; the document was left unchanged. Save manually and retry' }
        : x)))
      void message.error(zh ? '保存失败，本次回复未应用到文档' : 'Save failed; the reply was not applied')
      return
    }
    // 应用摘要与内容：excalidraw/drawio 通道先提取目标载荷，失败不动文档；
    // richtext-patch 通道回复原文透传宿主执行器（docflow-edit 围栏解析、
    // 定位执行、围栏缺失回落追加均在宿主 applyRichTextPatch 内完成）。
    let appliedContent = content
    if (applyKind === 'excalidraw-json' || applyKind === 'drawio-xml') {
      const extracted = applyKind === 'excalidraw-json' ? extractExcalidrawJSON(content) : extractDrawioXML(content)
      if (!extracted) {
        const why = applyKind === 'excalidraw-json'
          ? (zh ? '未识别到 excalidraw 元素 JSON 数组，画布未被修改' : 'No excalidraw element JSON array detected; the canvas was left unchanged')
          : (zh ? '未识别到 drawio XML，文档未被修改' : 'No drawio XML detected; the document was left unchanged')
        setTurns((prev) => prev.map((x) => (x.id === turnId ? { ...x, applying: false, applyError: why } : x)))
        return
      }
      appliedContent = extracted
    }
    let applyFail = ''
    try {
      if (applyKind !== 'text') {
        const r = await onApplyRef.current('replace', appliedContent)
        if (typeof r === 'string' && r) applyFail = r
      } else {
        const tgt = getTargetRef.current()
        if (tgt.hasSelection) onApplyRef.current('replace', appliedContent)
        else if (onAppendRef.current) onAppendRef.current(appliedContent)
        else onApplyRef.current('insert', appliedContent)
      }
    } catch (err) {
      applyFail = err instanceof Error ? err.message : (zh ? '应用失败，文档未被修改' : 'Failed to apply; the document was left unchanged')
    }
    if (applyFail) {
      setTurns((prev) => prev.map((x) => (x.id === turnId ? { ...x, applying: false, applyError: applyFail } : x)))
      void message.error(applyFail)
      return
    }
    const summary = appliedContent.replace(/\s+/g, ' ').trim().slice(0, APPLY_SUMMARY_CHARS)
    setTurns((prev) => prev.map((x) => (x.id === turnId
      ? {
          ...x,
          applying: false,
          applied: {
            versionId: saved.versionId,
            version: saved.version,
            summary,
            time: new Date().toLocaleTimeString(),
            revoked: false,
          },
        }
      : x)))
    void message.success(zh
      ? `已应用到文档（基线版本 v${saved.version}，可撤销）`
      : `Applied to the document (baseline v${saved.version}, revertible)`)
  }

  /** 撤销一次自动应用：先保存当前未保存修改（含该次 AI 输出，避免丢弃），
   * 再 restoreVersion 回退到应用前版本，reload 刷新编辑器并标记「已撤销」。
   * 诚实边界：回退的是整个文档到应用前版本（此后的其它修改一并回退），
   * 并非仅移除该段 AI 文字——按钮 Tooltip 已注明。 */
  const undoApply = async (turn: ChatTurn) => {
    const rec = turn.applied
    if (!rec || rec.revoked || undoingId !== null) return
    setUndoingId(turn.id)
    try {
      const saved = await ensureSavedRef.current()
      if (!saved) throw new Error(zh ? '保存当前修改失败，为避免丢失内容未执行回退' : 'Failed to save current changes; revert aborted')
      await restoreVersion(fileIdRef.current, rec.versionId)
      await reloadRef.current()
      setTurns((prev) => prev.map((x) => (x.id === turn.id && x.applied
        ? { ...x, applied: { ...x.applied, revoked: true } }
        : x)))
      void message.success(zh ? `已撤销：文档已回退到 v${rec.version}` : `Reverted: the document is back to v${rec.version}`)
    } catch (err) {
      void message.error(err instanceof Error ? err.message : (zh ? '版本恢复失败' : 'Failed to restore the version'))
    } finally {
      setUndoingId(null)
    }
  }

  const copy = (content: string) => {
    void navigator.clipboard.writeText(content)
    void message.success(zh ? '已复制' : 'Copied')
  }

  const stop = () => {
    abortRef.current?.abort()
  }

  if (!aiOn || !open) return null

  const scopeHint = applyKind === 'excalidraw-json'
    ? (zh ? '白板无选区，将使用画布内容摘要' : 'No selection; the whiteboard summary will be used')
    : applyKind === 'drawio-xml'
      ? (zh ? '图表无选区，将使用当前 XML' : 'No selection; the current XML will be used')
      : hasSelection
        ? (zh ? `已选 ${selectionLen} 字符` : `${selectionLen} chars selected`)
        : (zh ? '未选中文本，将使用全文' : 'No selection; the whole document will be used')
  const modeHint = mode === 'edit'
    ? (applyKind === 'excalidraw-json'
      ? (zh ? '回复自动转为白板元素插入，可撤销' : 'Auto-insert as whiteboard elements, revertible')
      : applyKind === 'drawio-xml'
        ? (zh ? '回复自动替换图表内容，可撤销' : 'Auto-replace the diagram, revertible')
        : applyKind === 'richtext-patch'
          ? (zh ? '编辑指令自动定位并应用，可撤销' : 'Edit instructions auto-applied, revertible')
          : (zh ? '回复自动应用，可撤销' : 'Auto-apply, revertible'))
    : (zh ? '纯输出，不改文档' : 'Output only')
  const modeTip = mode === 'edit'
    ? (applyKind === 'excalidraw-json'
      ? (zh
        ? '可修改：AI 回复完成后，其中的 excalidraw 元素 JSON 会被转换为白板原生元素（矩形/椭圆/菱形/文本/箭头等）追加插入画布（不覆盖现有内容）。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
        : 'Can edit: when the reply finishes, its excalidraw element JSON is converted into native whiteboard elements (rectangles/ellipses/diamonds/text/arrows) and appended to the canvas (existing content is kept). A new version is saved first as the revert baseline.')
      : applyKind === 'drawio-xml'
        ? (zh
          ? '可修改：AI 回复完成后，生成的 drawio XML 将整体替换画布内容。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
          : 'Can edit: when the reply finishes, the generated drawio XML replaces the whole canvas. A new version is saved first as the revert baseline.')
        : applyKind === 'richtext-patch'
          ? (zh
            ? '可修改：AI 回复完成后，围栏内的编辑指令（替换/插入/删除/整篇重写）会按定位原文自动应用到文档。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
            : 'Can edit: when the reply finishes, the fenced edit instructions (replace/insert/delete/full rewrite) are located in the document and applied automatically. A new version is saved first as the revert baseline.')
          : (zh
            ? '可修改：AI 回复完成后自动应用到文档（有选区替换选区，无选区追加到文档末尾）。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
            : 'Can edit: replies are applied automatically when finished (replace the selection, or append to the end without one). A new version is saved first as the revert baseline; each applied message can be reverted.'))
    : (zh
      ? '仅对话：AI 回复只在对话中展示，不改动文档内容。'
      : 'Chat only: replies stay in the conversation; the document is never modified.')
  const placeholder = applyKind === 'excalidraw-json'
    ? (zh ? '描述想要的图（AI 生成白板原生元素插入画布），Enter 发送' : 'Describe the diagram you want (AI generates native whiteboard elements), Enter to send')
    : applyKind === 'drawio-xml'
      ? (zh ? '描述要生成的图表或修改（AI 输出 drawio XML），Enter 发送' : 'Describe the diagram or change (AI outputs drawio XML), Enter to send')
      : applyKind === 'richtext-patch'
        ? (zh ? '描述要做的修改（AI 输出编辑指令并自动应用），Enter 发送' : 'Describe the change (AI emits edit instructions, auto-applied), Enter to send')
        : (zh ? '输入指令，Enter 发送（Shift+Enter 换行）' : 'Type an instruction, Enter to send (Shift+Enter for newline)')
  const emptyHint = applyKind === 'excalidraw-json'
      ? (zh
        ? '描述想要的图，例如「画一个用户注册流程图」「画一个三层架构图」。「可修改」模式下 AI 生成的 excalidraw 元素 JSON 会自动转换为白板原生元素插入画布（可撤销）。'
        : 'Describe a diagram, e.g. “draw a user sign-up flowchart”. In “Can edit” mode the generated excalidraw element JSON is converted into native whiteboard elements and inserted (revertible).')
      : applyKind === 'drawio-xml'
        ? (zh
          ? '描述想要的图表或修改，例如「生成一个微服务架构图」「把泳道改成三条」。「可修改」模式下 AI 生成的 drawio XML 会自动替换画布内容（可撤销）。'
          : 'Describe the diagram or change, e.g. “generate a microservice architecture diagram”. In “Can edit” mode the generated drawio XML replaces the canvas (revertible).')
        : applyKind === 'richtext-patch'
          ? (zh
            ? '描述要做的修改，例如「把第二段改得更简洁」「删除小结一节」「在开头插入一段引言」。「可修改」模式下 AI 输出编辑指令（替换/插入/删除/整篇重写），自动定位原文并应用到文档（可撤销）。'
            : 'Describe the change, e.g. “make the second paragraph more concise”, “delete the summary section”. In “Can edit” mode the AI emits edit instructions (replace/insert/delete/full rewrite) that are located in the document and applied automatically (revertible).')
          : (zh
            ? '输入指令让 AI 基于选区或全文创作/改写，例如「把选中的这段改得更简洁」「续写下一节」「生成一个对比表格」。「可修改」模式下回复会自动应用到文档（可撤销），「仅对话」模式只在对话中输出。'
            : 'Ask the AI to write or rewrite the selection / whole document. In “Can edit” mode replies are applied automatically (revertible); “Chat only” never modifies the document.')

  return (
    <aside className="ai-edit-chat-panel" aria-label={zh ? 'AI 对话' : 'AI chat'}>
      <div className="ai-edit-chat-head">
        <Sparkles size={14} strokeWidth={2} aria-hidden="true" />
        <span className="ai-edit-chat-title">{zh ? 'AI 对话' : 'AI chat'}</span>
        <span className="ai-edit-chat-head-ops">
          <Tooltip title={zh ? '清空会话' : 'Clear conversation'}>
            <Button
              size="small"
              type="text"
              icon={<Trash2 size={13} strokeWidth={2} />}
              disabled={turns.length === 0}
              onClick={() => { setTurns([]); setNotice('') }}
              aria-label={zh ? '清空会话' : 'Clear conversation'}
            />
          </Tooltip>
          <Button
            size="small"
            type="text"
            icon={<X size={14} strokeWidth={2} />}
            onClick={onClose}
            aria-label={zh ? '收起面板' : 'Close panel'}
          />
        </span>
      </div>
      {/* 模式 + 上下文范围同行（v3.9：原两行合并省一行——可修改/仅对话 +
          选区/全文同排；模型选择移入底部输入区工具栏，发送键最右）。 */}
      <div className="ai-edit-chat-mode">
        <Segmented
          size="small"
          value={mode}
          onChange={(v) => changeMode(v as ChatMode)}
          options={[
            { label: zh ? '可修改' : 'Can edit', value: 'edit' },
            { label: zh ? '仅对话' : 'Chat only', value: 'chat' },
          ]}
        />
        {(applyKind === 'text' || applyKind === 'richtext-patch') && (
          <Segmented
            size="small"
            value={scope}
            onChange={(v) => setScope(v as ChatScope)}
            options={[
              { label: zh ? '选区' : 'Selection', value: 'selection', disabled: !hasSelection },
              { label: zh ? '全文' : 'Whole doc', value: 'full' },
            ]}
          />
        )}
        <span className="muted ai-edit-chat-scope-hint">{scopeHint}</span>
        {/* v3.7：AIChatToggleBar 移到 chat-tools-bar（输入框内底部工具栏），
            不再在面板顶部平铺。 */}
        {/* 模式说明收起为悬浮图标（hover 显示完整说明），避免平铺文案占用输入区空间。 */}
        <Popover
          content={<div className="ai-edit-chat-mode-popover"><strong>{modeHint}</strong><div className="muted">{modeTip}</div></div>}
          trigger="hover"
          placement="topRight"
        >
          <span className="ai-edit-chat-mode-hint" role="button" tabIndex={0} aria-label={modeHint}>
            <HelpCircle size={14} />
          </span>
        </Popover>
      </div>
      <div className="ai-edit-chat-thread" ref={listRef}>
        {turns.length === 0 && (
          <div className="ai-edit-chat-empty muted">{emptyHint}</div>
        )}
        {turns.map((turn) => (
          <div key={turn.id} className={`ai-turn ai-turn-${turn.role}`}>
            {turn.role === 'user' ? (
              <div className="ai-bubble ai-bubble-user">
                {turn.content}
                {turn.files && turn.files.length > 0 && (
                  <span className="ai-turn-files">
                    {turn.files.map((f) => (
                      <span key={f.fileId} className="ai-turn-file-chip" title={f.fileName}>
                        <Paperclip size={10} strokeWidth={2} aria-hidden="true" />
                        <span>{f.fileName}</span>
                      </span>
                    ))}
                  </span>
                )}
                {turn.note && <div className="ai-edit-chat-note">{turn.note}</div>}
              </div>
            ) : (
              <>
                {/* 助手头像：与全局 AI 助手同款 Sparkles 圆标。 */}
                <div className="ai-avatar" aria-hidden="true">
                  <Sparkles size={13} strokeWidth={2} />
                </div>
                <div className="ai-bubble ai-bubble-assistant">
                  {/* 推理思考折叠区（流式展开跟随、完成自动收起并展示用时）。 */}
                  <AIChatThinking text={turn.thinking ?? ''} streaming={turn.streaming} thinkingMS={turn.thinkingMS} zh={zh} />
                  {turn.content ? (
                    // payload 通道自动应用成功后收起原始回复（大段 JSON/XML 不
                    // 再整屏裸输出）；仅对话/失败/流式期间保持原样渲染。
                    (applyKind === 'excalidraw-json' || applyKind === 'drawio-xml' || applyKind === 'richtext-patch') && turn.applied && !turn.streaming ? (
                      <AppliedReplyView
                        content={turn.content}
                        zh={zh}
                        label={applyKind === 'excalidraw-json'
                          ? (zh ? '白板元素已生成并插入画布' : 'Whiteboard elements generated and inserted')
                          : applyKind === 'drawio-xml'
                            ? (zh ? '图表已生成并替换画布' : 'Diagram generated and applied to the canvas')
                            : (zh ? '编辑指令已应用到文档' : 'Edit instructions applied to the document')}
                      />
                    ) : (
                      <AIMarkdown text={turn.content} zh={zh} streaming={turn.streaming} />
                    )
                  ) : turn.streaming ? (
                    <span className="ai-thinking">{t(locale, 'aiAssistantGenerating')}</span>
                  ) : (
                    // 兜底：流结束但无正文/错误/停止标记——可见提示而非空白。
                    <span className="muted">{zh ? '（模型未返回内容）' : '(no content returned)'}</span>
                  )}
                  {turn.streaming && turn.content && <span className="ai-caret" aria-hidden="true" />}
                  {turn.error && <div className="ai-error-msg">{turn.error}</div>}
                  {/* 外部工具调用（MCP）：Wrench 小标签逐条列出（顺序保留）。 */}
                  {turn.toolCalls && <AIToolCalls toolCalls={turn.toolCalls} zh={zh} />}
                  {turn.note && <div className="ai-edit-chat-note muted">{turn.note}</div>}
                  {!turn.streaming && turn.content && (
                    <div className="ai-edit-chat-actions">
                      <Button size="small" type="text" icon={<Copy size={13} strokeWidth={2} />} onClick={() => copy(turn.content)}>
                        {zh ? '复制' : 'Copy'}
                      </Button>
                    </div>
                  )}
                  {/* 自动应用状态：进行中 / 修改点（可撤销）/ 已撤销 / 失败。 */}
                  {turn.applying && (
                    <div className="ai-edit-chat-note muted">
                      {zh ? '正在保存并应用到文档…' : 'Saving and applying to the document…'}
                    </div>
                  )}
                  {turn.applyError && <div className="ai-edit-chat-note error-text">{turn.applyError}</div>}
                  {turn.applied && !turn.applied.revoked && (
                    <div className="ai-applied-bar">
                      <span className="ai-applied-note">
                        {zh ? '已应用于文档' : 'Applied to document'}
                        {' · v'}{turn.applied.version} · {turn.applied.time}
                        {turn.applied.summary && (
                          <span className="ai-applied-summary" title={turn.applied.summary}>
                            {turn.applied.summary}
                            {turn.applied.summary.length >= APPLY_SUMMARY_CHARS ? '…' : ''}
                          </span>
                        )}
                      </span>
                      <Tooltip title={zh
                        ? `撤销 = 将整个文档回退到应用前版本 v${turn.applied.version}：此后的其它修改（含其它 AI 应用）也会一并回退，并非仅移除该段文字。回退前会先保存当前未保存修改。`
                        : `Revert = restore the whole document to the pre-apply version v${turn.applied.version}; later changes (including other AI edits) are reverted too, not just this snippet. Unsaved changes are saved first.`}>
                        <Button
                          size="small"
                          type="link"
                          danger
                          loading={undoingId === turn.id}
                          onClick={() => void undoApply(turn)}
                        >
                          {zh ? '撤销此修改' : 'Revert'}
                        </Button>
                      </Tooltip>
                    </div>
                  )}
                  {turn.applied?.revoked && (
                    <div className="ai-applied-revoked">
                      {zh ? `已撤销（文档已回退至 v${turn.applied.version}）` : `Reverted (document restored to v${turn.applied.version})`}
                    </div>
                  )}
                  {/* 联网搜索来源：折叠列表（编号 + 标题超链接）。 */}
                  {turn.webSources && <AIWebSources sources={turn.webSources} zh={zh} />}
                </div>
              </>
            )}
          </div>
        ))}
      </div>
      {/* 输入区（v3.9：Cherry Studio 式统一框——引用 chips + 输入框 + 单行
          工具栏（技能/引用文件/开关组 + 模型选择 + 发送键最右端）。 */}
      <div className="ai-edit-chat-composer">
        {notice && <div className="ai-edit-chat-notice error-text">{notice}</div>}
        {attached.length > 0 && (
          <div className="ai-attach-chips">
            {attached.map((f) => (
              <span key={f.fileId} className="ai-attach-chip">
                <Paperclip size={10} strokeWidth={2} aria-hidden="true" />
                <span className="ai-attach-chip-name" title={f.fileName}>{f.fileName}</span>
                <button type="button" aria-label={zh ? '移除引用' : 'Remove reference'} onClick={() => toggleAttach({ id: f.fileId, name: f.fileName })}>×</button>
              </span>
            ))}
          </div>
        )}
        <div className="chat-input-box">
          <Input.TextArea
            ref={inputRef}
            autoSize={{ minRows: 1, maxRows: 5 }}
            value={input}
            placeholder={placeholder}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              // Enter 发送 / Shift+Enter 换行；输入法组合中 Enter 不发送（同 AI 助手）。
              if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
                e.preventDefault()
                void send(input)
              }
            }}
          />
          <div className="chat-tools-bar">
            {/* 左：技能 + 引用文件 + 开关组（联网/思考/MCP/我的文件）。 */}
            <AISkillButton
              zh={zh}
              onPick={(s) => {
                const tgt = getTargetRef.current()
                const selection = tgt.hasSelection ? tgt.text : ''
                if (!selection && s.prompt.includes('{selection}')) {
                  void message.warning(zh ? '未选中内容，{selection} 已置空' : 'No selection; {selection} was left empty')
                }
                setInput(renderSkillPrompt(s.prompt, { selection, file: fileName }))
              }}
            />
            {/* 引用文件（v3.9）：其它文件全文注入上下文（context.fileIds）。 */}
            <Popover
              trigger="click"
              placement="topLeft"
              arrow={false}
              open={attachOpen}
              onOpenChange={(next) => {
                setAttachOpen(next)
                if (next) setAttachQuery('')
              }}
              content={
                <div className="ai-attach-pop">
                  <Input
                    allowClear
                    size="small"
                    value={attachQuery}
                    onChange={(e) => setAttachQuery(e.target.value)}
                    placeholder={zh ? '搜索文件（留空 = 最近访问）' : 'Search files (empty = recent)'}
                    prefix={<Paperclip size={12} strokeWidth={2} aria-hidden="true" />}
                  />
                  <div className="ai-attach-list">
                    {attachLoading && <div className="ai-attach-state muted">{zh ? '加载中…' : 'Loading…'}</div>}
                    {!attachLoading && attachItems.length === 0 && <div className="ai-attach-state muted">{zh ? '没有匹配的文件' : 'No matching files'}</div>}
                    {attachItems.map((item) => {
                      const selected = attached.some((f) => f.fileId === item.id)
                      return (
                        <button
                          key={item.id}
                          type="button"
                          className={`ai-attach-item${selected ? ' selected' : ''}`}
                          onClick={() => toggleAttach(item)}
                        >
                          <FileText size={13} strokeWidth={2} aria-hidden="true" />
                          <span className="name" title={item.name}>{item.name}</span>
                          <Check size={13} strokeWidth={2} aria-hidden="true" className="check" />
                        </button>
                      )
                    })}
                  </div>
                  <div className="ai-attach-state muted">{zh ? '引用文件全文将随指令一并作为上下文发送' : 'Referenced files are sent as extra context'}</div>
                </div>
              }
            >
              <Button size="small" type="text" className="ai-attach-btn" aria-label={zh ? '引用文件' : 'Attach files'} title={zh ? '引用文件（其它文件全文作为附加上下文）' : 'Attach files (full text as extra context)'}>
                <Paperclip size={14} strokeWidth={2} aria-hidden="true" />
              </Button>
            </Popover>
            <AIChatToggleBar
              zh={zh}
              web={toggles.web}
              think={toggles.think}
              thinkBlocked={toggles.thinkBlocked}
              mcp={toggles.mcp}
              mcpAvailable={toggles.mcpAvailable}
              docs={toggles.docs}
              docsAvailable={toggles.docsAvailable}
              onWeb={toggles.setWeb}
              onThink={toggles.setThink}
              onMcp={toggles.setMcp}
              onDocs={toggles.setDocs}
            />
            {/* 右：模型选择 + 发送/停止（最右端，与 AI 助理/创作同款样式）。 */}
            <div className="chat-tools-right">
              {models.length > 0 && (
                <AIModelSelect
                  models={models}
                  modelKey={modelKey}
                  modelExplicit={modelExplicit}
                  onSelect={(v) => {
                    setModelKey(v)
                    setModelExplicit(true)
                  }}
                  zh={zh}
                />
              )}
            </div>
            <span className="chat-send-btn-wrap">
              {busy ? (
                <button type="button" className="chat-stop-btn" aria-label={zh ? '停止生成' : 'Stop generating'} title={zh ? '停止生成' : 'Stop generating'} onClick={stop}>
                  {CHAT_STOP_ICON}
                </button>
              ) : (
                <button type="button" className="chat-send-btn" disabled={!input.trim()} aria-label={t(locale, 'aiAssistantSend')} title={t(locale, 'aiAssistantSend')} onClick={() => void send(input)}>
                  {CHAT_SEND_ICON}
                </button>
              )}
            </span>
          </div>
        </div>
      </div>
    </aside>
  )
}
