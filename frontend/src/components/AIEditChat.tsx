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
// - 应用通道 applyKind（默认 text 行为不变）：excalidraw-mermaid（白板页：
//   AI 只回 mermaid 代码块，自动应用时提取源码交宿主转为白板图形插入）、
//   drawio-xml（图表页：AI 回完整 drawio XML，自动应用时提取 XML 整体替换
//   画布）；forceChatOnly=true 恒「仅对话」（隐藏模式切换，OnlyOffice 等
//   无内容修改 API 的编辑页用）。
import { useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button, Dropdown, Input, Segmented, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import type { TextAreaRef } from 'antd/es/input/TextArea'
import { Copy, Send, Sparkles, Square, Trash2, X } from 'lucide-react'
import { aiChat, getFileMeta, restoreVersion } from '../api'
import type { AIMessage } from '../api'
import { AIChatToggleBar, AISkillButton, AIToolCalls, AIWebSources, AI_MODEL_STORAGE_KEY, defaultAIModelKey, getAIModels, normalizeWebSources, renderSkillPrompt, toolCallView, useAIChatToggles } from './AIAssistant'
import type { AIModelOption, AIToolCallView, AIWebSource } from './AIAssistant'
import AIMarkdown from './AIMarkdown'
import { useAIEnabled } from '../aiFeature'
import { t, useLocale } from '../i18n'
import type { AIEditTarget } from './AIEdit'

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
 * - excalidraw-mermaid：白板页——AI 只回 mermaid 代码块，宿主经
 *   @excalidraw/mermaid-to-excalidraw 转换为白板图形插入画布；
 * - drawio-xml：drawio 页——AI 基于当前 XML 回完整 drawio XML，宿主整体
 *   替换画布内容。两者均无选区概念，统一走 onApply('replace')。 */
export type AIApplyKind = 'text' | 'excalidraw-mermaid' | 'drawio-xml'

/** mermaid 图定义首行关键字（裸语法识别用，无 ```mermaid 围栏时兜底）。 */
const MERMAID_FIRST_LINE =
  /^\s*(graph|flowchart|sequenceDiagram|classDiagram|stateDiagram(-v2)?|erDiagram|journey|gantt|pie|mindmap|timeline|quadrantChart|gitGraph|sankey-beta|xychart-beta|block-beta|architecture-beta|requirementDiagram|C4Context|C4Container)\b/

/** 从 AI 回复中提取 mermaid 源码：优先 ```mermaid 围栏；其次任意围栏代码块
 * 内容首行命中 mermaid 语法；最后裸语法（首行命中起、空行止）。 */
function extractMermaidSource(reply: string): string {
  const fenced = /```(?:mermaid|Mermaid)[^\n]*\n([\s\S]*?)```/.exec(reply)
  if (fenced && fenced[1].trim()) return fenced[1].trim()
  const re = /```[^\n]*\n([\s\S]*?)```/g
  for (let m = re.exec(reply); m; m = re.exec(reply)) {
    const body = m[1].trim()
    if (body && MERMAID_FIRST_LINE.test(body)) return body
  }
  const lines = reply.split('\n')
  for (let i = 0; i < lines.length; i++) {
    if (!MERMAID_FIRST_LINE.test(lines[i])) continue
    const out: string[] = []
    for (let j = i; j < lines.length; j++) {
      if (!lines[j].trim()) break
      out.push(lines[j])
    }
    const text = out.join('\n').trim()
    if (text) return text
  }
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
 * extractDrawioXML/应用逻辑。 */
const DRAWIO_XML_GUIDE = `drawio XML quick reference:
- Structure: prefer the simplified form <mxGraphModel><root><mxCell id="0"/><mxCell id="1" parent="0"/>...cells...</root></mxGraphModel> (drawio auto-wraps mxfile/diagram; a full <mxfile>...</mxfile> is also accepted).
- Node (shape): <mxCell id="2" value="Label" style="..." vertex="1" parent="1"><mxGeometry x="40" y="40" width="160" height="60" as="geometry"/></mxCell>.
- Edge (connector): <mxCell id="5" value="Yes" style="..." edge="1" parent="1" source="2" target="3"><mxGeometry relative="1" as="geometry"/></mxCell>; define endpoint nodes first, then reference their ids via source/target.
- Common styles: rounded=1;whiteSpace=wrap;html=1; (rounded box) / ellipse;whiteSpace=wrap;html=1; (oval) / rhombus;whiteSpace=wrap;html=1; (decision diamond) / text;html=1; (plain label); edges default to edgeStyle=orthogonalEdgeStyle;rounded=0;. Tune with fillColor / strokeColor / fontSize / fontStyle=1 (bold).
- Layout: pick left-to-right or top-down flow; keep node gaps >=40; main-flow step 160-200, branch step 100 on the cross axis; put branch labels ("Yes"/"No") in the edge value.
- ids: incrementing numeric strings "2","3",...; keep id="0" (root) and id="1" (default parent) fixed; every cell gets parent="1" unless grouping.
- Editing an existing diagram: keep the ids of unchanged elements and only modify their style/geometry/value, or add/remove cells; never renumber the whole model.
- The XML must be well-formed; escape < > & " ' inside value attributes.`

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
  /** 联网搜索来源（SSE meta.sources 宽松归一化；底部折叠列表展示）。 */
  webSources?: AIWebSource[]
  /** 外部工具调用（SSE event:tool 逐次追加，顺序保留；Wrench 小标签展示）。 */
  toolCalls?: AIToolCallView[]
  /** 自动应用进行中（保存+落盘）。 */
  applying?: boolean
  /** 自动应用成功的修改点记录（可撤销）。 */
  applied?: AppliedRecord
  /** 自动应用失败原因（文档未被修改）。 */
  applyError?: string
}

/** 编辑器页头部「AI 对话」按钮（主点击开面板；下拉=并入的 AIEditMenu 快捷
 * 指令，AI 未启用不渲染）。kind 定制下拉指令（默认 text 保持原文案）：
 * excalidraw-mermaid / drawio-xml / chat（仅对话页）分别给出契合场景的
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
    'excalidraw-mermaid': [
      { key: 'flowchart', label: zh ? '生成流程图' : 'Flowchart', instruction: zh ? '画一个流程图（若白板上下文未给出主题，请生成一个通用的审批流程）' : 'Draw a flowchart (fall back to a generic approval flow if the whiteboard context has no topic)', editMode: true },
      { key: 'sequence', label: zh ? '生成时序图' : 'Sequence diagram', instruction: zh ? '画一个时序图（若上下文未给出主题，请生成一个用户登录/下单示例）' : 'Draw a sequence diagram (fall back to a login/checkout example if no topic in context)', editMode: true },
      { key: 'class', label: zh ? '生成类图' : 'Class diagram', instruction: zh ? '画一个类图（若上下文未给出主题，请生成一个订单领域模型示例）' : 'Draw a class diagram (fall back to an order domain model if no topic)', editMode: true },
      'divider',
      { key: 'custom', label: t(locale, 'aiEditCustom'), focus: true },
    ],
    'drawio-xml': [
      { key: 'flowchart', label: zh ? '生成流程图' : 'Flowchart', instruction: zh ? '生成一个流程图（若上下文未给出主题，请生成一个通用的审批流程）' : 'Generate a flowchart (fall back to a generic approval flow if no topic)', editMode: true },
      { key: 'architecture', label: zh ? '生成架构图' : 'Architecture diagram', instruction: zh ? '生成一个系统架构图（若上下文未给出主题，请生成一个前后端+数据库的三层架构示例）' : 'Generate an architecture diagram (fall back to a 3-tier web/database example if no topic)', editMode: true },
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
    <Dropdown.Button
      size="small"
      type={open ? 'primary' : 'default'}
      menu={{ items, onClick: onMenuClick }}
      trigger={['click']}
      disabled={disabled}
      onClick={onToggle}
    >
      <Sparkles size={13} strokeWidth={2} aria-hidden="true" />
      <span>{zh ? 'AI 对话' : 'AI chat'}</span>
    </Dropdown.Button>
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
  forceChatOnly = false,
  outputFormat = 'plaintext',
}: {
  open: boolean
  onClose: () => void
  /** 当前选区（text 为选中文本；无选区时返回全文与 hasSelection=false）。 */
  getTarget: () => AIEditTarget
  /** 全文（「全文」范围与无选区回退时的输入）。 */
  getAllText: () => string
  /** 应用结果（宿主页面负责编辑器落盘与 dirty 标记）。返回 string=应用
   * 失败原因（文档未被修改，显示为 applyError）；Promise 版本供 mermaid
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
  /** 应用通道（默认 text；excalidraw-mermaid/drawio-xml 见 AIApplyKind）。 */
  applyKind?: AIApplyKind
  /** 恒「仅对话」：隐藏模式切换（不支持改文档的宿主页，如 OnlyOffice）。 */
  forceChatOnly?: boolean
  /** text 通道输出格式（默认 plaintext=原样纯文本输出，Monaco 等用）；
   * markdown=富文本宿主（.dfrt 编辑页）：system 指令要求输出 Markdown，
   * 宿主经 marked 转富文本 HTML 插入。仅 applyKind='text' 生效。 */
  outputFormat?: 'markdown' | 'plaintext'
}) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const aiOn = useAIEnabled()
  const { message } = AntdApp.useApp()
  const [turns, setTurns] = useState<ChatTurn[]>([])
  const [input, setInput] = useState('')
  const [scope, setScope] = useState<ChatScope>('selection')
  const [hasSelection, setHasSelection] = useState(false)
  const [selectionLen, setSelectionLen] = useState(0)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  /** 面板模式：可修改（默认）/仅对话（ref 供快捷指令立即生效）；
   * forceChatOnly 恒 chat。 */
  const [mode, setMode] = useState<ChatMode>(() => (forceChatOnly ? 'chat' : 'edit'))
  const modeRef = useRef<ChatMode>(forceChatOnly ? 'chat' : 'edit')
  /** 正在执行撤销的回合 id（单飞：同一时间只允许一次版本回退）。 */
  const [undoingId, setUndoingId] = useState<number | null>(null)
  // 联网/思考开关：与全局助手 / Studio 共用 localStorage key 与默认逻辑
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
  const toggles = useAIChatToggles(models, modelKey)
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
    // forceChatOnly 宿主不支持改文档，一律仅对话。
    const m: ChatMode = forceChatOnly || !quickCommand.editMode ? 'chat' : 'edit'
    setMode(m)
    modeRef.current = m
    void send(quickCommand.instruction)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [quickCommand, open, aiOn])

  /** 切换面板模式（可修改/仅对话；forceChatOnly 下不可切换）。 */
  const changeMode = (m: ChatMode) => {
    if (forceChatOnly) return
    setMode(m)
    modeRef.current = m
  }

  /** 发送一轮：每轮独立构造 prompt（系统约束 + 最近 2 轮历史 + 指令与
   * 当前选区/全文上下文），SSE 流式渲染；停止/失败落在助手回合上。
   * 可修改模式下正常完成后自动应用（见 autoApply）；中止/失败不应用。 */
  const send = async (question: string) => {
    const text = question.trim()
    if (!text || busy) return
    // 以发送时刻的模式为准（流式期间切换不影响本轮）。
    const applyMode = modeRef.current
    const tgt = getTargetRef.current()
    const useSelection = scope === 'selection' && tgt.hasSelection
    const full = useSelection ? tgt.text : getAllTextRef.current()
    // 空上下文：文本编辑页维持原拦截；mermaid/drawio/仅对话场景允许无上下文
    // 发送（空白画布从零生成、Office 无转换文本时直接提问）。
    if (!full.trim() && applyKind === 'text' && !forceChatOnly) {
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
    const system = applyKind === 'excalidraw-mermaid'
      ? (zh
        ? '你是白板图表助手。请按用户需求生成 mermaid 图表定义（支持 flowchart、sequenceDiagram、classDiagram、stateDiagram-v2、erDiagram 等）。只输出一个 mermaid 代码块（```mermaid 开头、``` 结尾），代码块之外不要任何解释——该代码将被转换为白板图形插入画布。'
        : 'You are a whiteboard diagram assistant. Generate a mermaid diagram definition per the request (flowchart, sequenceDiagram, classDiagram, stateDiagram-v2, erDiagram are supported). Output exactly one mermaid code block (starting with ```mermaid and ending with ```), with no explanations outside it — the code will be converted into whiteboard shapes.')
      : applyKind === 'drawio-xml'
        ? (zh
          ? `你是 draw.io 图表助手。基于给定的当前图表 XML，按指令输出修改后的完整 drawio XML（以 <mxGraphModel>...</mxGraphModel> 或 <mxfile>...</mxfile> 包裹）。只输出 XML 本身，不要解释。\n\n${DRAWIO_XML_GUIDE}`
          : `You are a draw.io diagram assistant. Based on the given current diagram XML, output the complete modified drawio XML (wrapped in <mxGraphModel>...</mxGraphModel> or <mxfile>...</mxfile>). Output only the XML itself, no explanations.\n\n${DRAWIO_XML_GUIDE}`)
        : outputFormat === 'markdown'
          ? (zh
            ? '你是富文本文档写作助手。请按指令处理给定文本，仅输出最终内容本身：不要解释、不要说明。请用 Markdown 输出结果（标题 #/##/###、有序与无序列表、表格、代码块、加粗、斜体、链接等）——内容将被转换为富文本样式插入文档。'
            : 'You are a rich text document writing assistant. Process the given text per the instruction and output only the final content itself: no explanations. Write the result in Markdown (headings #/##/###, ordered and unordered lists, tables, code blocks, bold, italic, links, etc.) — it will be converted into rich text styles and inserted into the document.')
          : zh
            ? '你是文档写作助手。请按指令处理给定文本，仅输出最终内容本身：不要解释、不要说明，不要使用代码围栏。可以输出 Markdown 格式。'
            : 'You are a writing assistant. Process the given text per the instruction and output only the final content itself: no explanations and no code fences. Markdown formatting is allowed.'
    const contextLabel = applyKind === 'excalidraw-mermaid'
      ? (zh ? '当前白板内容摘要' : 'Current whiteboard summary')
      : applyKind === 'drawio-xml'
        ? (zh ? '当前图表 XML' : 'Current diagram XML')
        : (zh ? '待处理文本' : 'Text to process')
    const userContent = full.trim()
      ? `${zh ? '指令' : 'Instruction'}：${text}\n\n${contextLabel}（${scopeLabel}${truncNote}）：\n<text>\n${content}\n</text>`
      : `${zh ? '指令' : 'Instruction'}：${text}`
    const userTurnId = ++turnIdRef.current
    const asstTurnId = ++turnIdRef.current
    setInput('')
    setNotice('')
    setTurns((prev) => [
      ...prev,
      {
        id: userTurnId,
        role: 'user',
        content: text,
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
          // 外部工具（MCP）执行前逐次下发：追加到消息的工具调用列表（顺序保留）。
          onTool: (tool) => {
            setTurns((prev) => prev.map((x) => (x.id === asstTurnId ? { ...x, toolCalls: [...(x.toolCalls ?? []), toolCallView(tool)] } : x)))
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
        next[next.length - 1] = { ...last, streaming: false }
        return next
      })
      setBusy(false)
    }
  }

  /** 自动应用回复到文档（版本保护）：
   * 1) 应用前先 ensureSaved——有未保存修改先保存，取「应用前版本」；
   * 2) 落盘：text 通道——有选区→替换选区（onApply replace）、无选区→追加
   *    文档末尾（onAppend，缺省回退 onApply insert）；excalidraw-mermaid/
   *    drawio-xml 通道——从回复提取 mermaid/drawio XML，统一 onApply
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
    // 应用摘要与内容：mermaid/drawio 通道先提取目标载荷，失败不动文档。
    let appliedContent = content
    if (applyKind !== 'text') {
      const extracted = applyKind === 'excalidraw-mermaid' ? extractMermaidSource(content) : extractDrawioXML(content)
      if (!extracted) {
        const why = applyKind === 'excalidraw-mermaid'
          ? (zh ? '未识别到 mermaid 代码，文档未被修改' : 'No mermaid code detected; the document was left unchanged')
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

  const chips = forceChatOnly
    ? (zh ? ['总结要点', '翻译成英文', '润色建议', '列表化'] : ['Summarize', 'Translate to English', 'Polish suggestions', 'Turn into lists'])
    : applyKind === 'excalidraw-mermaid'
      ? (zh ? ['画一个流程图', '画一个时序图', '生成类图', '生成状态图'] : ['Draw a flowchart', 'Draw a sequence diagram', 'Class diagram', 'State diagram'])
      : applyKind === 'drawio-xml'
        ? (zh ? ['生成流程图', '生成架构图', '美化整体布局', '改为横向布局'] : ['Generate a flowchart', 'Architecture diagram', 'Clean up the layout', 'Switch to horizontal layout'])
        : zh
          ? ['续写', '扩写', '精简', '修正错别字', '翻译成英文']
          : ['Continue writing', 'Expand', 'Shorten', 'Fix typos', 'Translate to English']
  const scopeHint = applyKind === 'excalidraw-mermaid'
    ? (zh ? '白板无选区，将使用画布内容摘要' : 'No selection; the whiteboard summary will be used')
    : applyKind === 'drawio-xml'
      ? (zh ? '图表无选区，将使用当前 XML' : 'No selection; the current XML will be used')
      : hasSelection
        ? (zh ? `已选 ${selectionLen} 字符` : `${selectionLen} chars selected`)
        : (zh ? '未选中文本，将使用全文' : 'No selection; the whole document will be used')
  const modeHint = mode === 'edit'
    ? (applyKind === 'excalidraw-mermaid'
      ? (zh ? '回复自动转图形插入白板，可撤销' : 'Auto-insert as shapes, revertible')
      : applyKind === 'drawio-xml'
        ? (zh ? '回复自动替换图表内容，可撤销' : 'Auto-replace the diagram, revertible')
        : (zh ? '回复自动应用，可撤销' : 'Auto-apply, revertible'))
    : (zh ? '纯输出，不改文档' : 'Output only')
  const modeTip = mode === 'edit'
    ? (applyKind === 'excalidraw-mermaid'
      ? (zh
        ? '可修改：AI 回复完成后，其中的 mermaid 代码会被转换为白板图形并追加插入画布（不覆盖现有内容）。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
        : 'Can edit: when the reply finishes, its mermaid code is converted into whiteboard shapes and appended to the canvas (existing content is kept). A new version is saved first as the revert baseline.')
      : applyKind === 'drawio-xml'
        ? (zh
          ? '可修改：AI 回复完成后，生成的 drawio XML 将整体替换画布内容。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
          : 'Can edit: when the reply finishes, the generated drawio XML replaces the whole canvas. A new version is saved first as the revert baseline.')
        : (zh
          ? '可修改：AI 回复完成后自动应用到文档（有选区替换选区，无选区追加到文档末尾）。应用前会先保存新版本作为回退基线，可在消息下方一键撤销。'
          : 'Can edit: replies are applied automatically when finished (replace the selection, or append to the end without one). A new version is saved first as the revert baseline; each applied message can be reverted.'))
    : (zh
      ? '仅对话：AI 回复只在对话中展示，不改动文档内容。'
      : 'Chat only: replies stay in the conversation; the document is never modified.')
  const placeholder = applyKind === 'excalidraw-mermaid'
    ? (zh ? '描述想要的图（AI 生成 mermaid 并转为白板图形），Enter 发送' : 'Describe the diagram you want (AI generates mermaid → whiteboard shapes), Enter to send')
    : applyKind === 'drawio-xml'
      ? (zh ? '描述要生成的图表或修改（AI 输出 drawio XML），Enter 发送' : 'Describe the diagram or change (AI outputs drawio XML), Enter to send')
      : (zh ? '输入指令，Enter 发送（Shift+Enter 换行）' : 'Type an instruction, Enter to send (Shift+Enter for newline)')
  const emptyHint = forceChatOnly
    ? (zh
      ? '与 AI 对话讨论当前文档（仅对话，不改动文档）。文档文本经转换接口获取；暂不支持转换的格式将以无上下文对话。'
      : 'Chat with the AI about the current document (chat only, never modified). Document text is fetched via conversion; unsupported formats chat without context.')
    : applyKind === 'excalidraw-mermaid'
      ? (zh
        ? '描述想要的图，例如「画一个用户注册流程图」「订单状态流转时序图」。「可修改」模式下 AI 生成的 mermaid 会自动转换为白板图形插入画布（可撤销）。'
        : 'Describe a diagram, e.g. “draw a user sign-up flowchart”. In “Can edit” mode the generated mermaid is converted into whiteboard shapes and inserted (revertible).')
      : applyKind === 'drawio-xml'
        ? (zh
          ? '描述想要的图表或修改，例如「生成一个微服务架构图」「把泳道改成三条」。「可修改」模式下 AI 生成的 drawio XML 会自动替换画布内容（可撤销）。'
          : 'Describe the diagram or change, e.g. “generate a microservice architecture diagram”. In “Can edit” mode the generated drawio XML replaces the canvas (revertible).')
        : (zh
          ? '输入指令让 AI 基于选区或全文创作/改写，例如「把选中的这段改得更简洁」「续写下一节」「生成一个对比表格」。「可修改」模式下回复会自动应用到文档（可撤销），「仅对话」模式只在对话中输出。'
          : 'Ask the AI to write or rewrite the selection / whole document. In “Can edit” mode replies are applied automatically (revertible); “Chat only” never modifies the document.')

  return (
    <aside className="ai-edit-chat-panel" aria-label={zh ? 'AI 对话' : 'AI chat'}>
      <div className="ai-edit-chat-head">
        <Sparkles size={14} strokeWidth={2} aria-hidden="true" />
        <span className="ai-edit-chat-title">{zh ? 'AI 对话' : 'AI chat'}</span>
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
      </div>
      {/* 模式：可修改（自动应用+版本保护，默认）/ 仅对话（纯输出）；
          forceChatOnly（如 OnlyOffice 页）隐藏切换恒仅对话；
          同排放联网/思考小开关（与全局助手共用 localStorage 与默认逻辑，
          窄面板经 flex-wrap 换行不挤爆）。 */}
      <div className="ai-edit-chat-mode">
        {!forceChatOnly && (
          <Segmented
            size="small"
            value={mode}
            onChange={(v) => changeMode(v as ChatMode)}
            options={[
              { label: zh ? '可修改' : 'Can edit', value: 'edit' },
              { label: zh ? '仅对话' : 'Chat only', value: 'chat' },
            ]}
          />
        )}
        <AIChatToggleBar
          compact
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
        <Tooltip title={modeTip}>
          <span className="muted ai-edit-chat-mode-hint">{modeHint}</span>
        </Tooltip>
      </div>
      {/* 上下文范围：选区（默认，无选区禁用）/ 全文；白板/drawio 无选区
          概念（上下文恒为摘要/XML），仅展示提示不渲染切换。 */}
      <div className="ai-edit-chat-scope">
        {applyKind === 'text' && (
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
                {turn.note && <div className="ai-edit-chat-note">{turn.note}</div>}
              </div>
            ) : (
              <>
                {/* 助手头像：与全局 AI 助手同款 Sparkles 圆标。 */}
                <div className="ai-avatar" aria-hidden="true">
                  <Sparkles size={13} strokeWidth={2} />
                </div>
                <div className="ai-bubble ai-bubble-assistant">
                  {turn.content ? (
                    <AIMarkdown text={turn.content} zh={zh} streaming={turn.streaming} />
                  ) : turn.streaming ? (
                    <span className="ai-thinking">{t(locale, 'aiAssistantGenerating')}</span>
                  ) : null}
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
      {/* 快捷指令 + 输入区（与 AI 助手同款「圆角框内嵌发送/停止」形态）。 */}
      <div className="ai-edit-chat-composer">
        <div className="ai-edit-chat-chips">
          {chips.map((chip) => (
            <Button key={chip} size="small" disabled={busy} onClick={() => setInput(chip)}>{chip}</Button>
          ))}
        </div>
        {notice && <div className="ai-edit-chat-notice error-text">{notice}</div>}
        <div className="ai-input-box">
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
          <div className="ai-input-footer">
            <span className="ai-input-tools">
              {/* 平台技能模板：{selection}=当前选区（无选区置空并提示）、
                  {file}=当前文件名（面板打开时解析）。 */}
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
              <span className="ai-input-hint muted">{zh ? 'Enter 发送 · Shift + Enter 换行' : 'Enter to send · Shift+Enter for newline'}</span>
            </span>
            {busy ? (
              <Button
                className="ai-stop-btn"
                shape="circle"
                size="small"
                aria-label={zh ? '停止生成' : 'Stop generating'}
                title={zh ? '停止生成' : 'Stop generating'}
                onClick={stop}
              >
                <Square size={10} fill="currentColor" strokeWidth={0} aria-hidden="true" />
              </Button>
            ) : (
              <Button
                className="ai-send-btn"
                type="primary"
                shape="circle"
                size="small"
                disabled={!input.trim()}
                aria-label={t(locale, 'aiAssistantSend')}
                title={t(locale, 'aiAssistantSend')}
                onClick={() => void send(input)}
              >
                <Send size={13} strokeWidth={2} aria-hidden="true" />
              </Button>
            )}
          </div>
        </div>
      </div>
    </aside>
  )
}
