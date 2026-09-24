// 富文本文档页（.dfrt 主后缀 / .dfdoc 兼容别名；路由 /dfdoc/:fileId）：
// DocFlow 专属富文本格式，Tiptap JSON 存储（见 RichTextEditor）——编辑态
// 加载 JSON 进 Tiptap，保存整篇回写新版本；查看态 readonly 渲染同一编辑器
//（嵌入块内联渲染 drawio/白板/图片等）。by-path 路由经 prop 传入 file_id。
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react'
import { App as AntdApp, Button } from 'antd'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { marked } from 'marked'
import { fetchFileText, getFileMeta, listDocumentComments, uploadFileVersion } from '../api'
import type { DocumentComment } from '../api'
import type { AIEditTarget } from '../components/AIEdit'
import AIEditChat, { AIEditChatButton } from '../components/AIEditChat'
import type { AIQuickCommand } from '../components/AIEditChat'
import { closeEditorWithFallback, safeReturnTo } from '../editorNavigation'
import { MessageKey, t, useLocale } from '../i18n'
import type { Editor as TiptapEditor } from '@tiptap/react'
import type { Node as ProseMirrorNode } from '@tiptap/pm/model'
import type { CollabSnapshot } from '../components/richtext/CollabSession'

// 富文本编辑器（Tiptap + lowlight 产物 1MB+）懒加载独立 chunk。
const RichTextEditor = lazy(() => import('../components/richtext/RichTextEditor'))

// ---- 富文本指令式编辑（AIEditChat applyKind=richtext-patch）----
// AI 回复 ```docflow-edit 围栏内的 JSON 编辑指令数组（replace/insert/delete/
// replaceAll），由执行器 applyRichTextPatch 在 Tiptap 文档中按定位原文精确
// 匹配逐条落盘（增删改插，而非整段追加）。

/** 单条编辑指令（宽松类型：字段存在性由执行器逐条校验）。 */
type RichTextPatchOp = { op?: unknown; find?: unknown; after?: unknown; text?: unknown }

/** 提取回复中全部 ```docflow-edit 围栏的候选指令体：逐个非贪婪截取 + 一个
 * 贪婪兜底（text 值内嵌 ``` 代码块时非贪婪会提前截断切碎 JSON，贪婪整段取
 * 首围栏起到末个 ```），任一候选可解析即用。空数组=无围栏（回落追加）。 */
function docflowEditFenceCandidates(raw: string): string[] {
  const out: string[] = []
  const re = /```docflow-edit[^\n]*\n([\s\S]*?)(?:```|$)/gi
  for (let m = re.exec(raw); m; m = re.exec(raw)) {
    if (m[1].trim()) out.push(m[1].trim())
  }
  const greedy = /```docflow-edit[^\n]*\n([\s\S]*?)\n?```[ \t]*$/i.exec(raw.trim())
  const body = greedy?.[1]?.trim()
  if (body && !out.includes(body)) out.push(body)
  return out
}

// ---- AI 上下文占位行（受保护内容标记）----
// 嵌入块（docflowEmbed）/文档图片（docflowImage）/文件卡片（docflowFileCard）
// 是 atom 节点，纯文本序列化（getText）下对 AI 完全不可见——AI 一旦输出
// replaceAll/大段 replace，这些节点即被 markdown HTML 整体覆盖丢失。
// 方案：序列化时输出占位行（[DocFlow-Embed kind="…" fileId="…" title="…"] 等），
// 提示词约束 AI 逐字保留；执行器把占位行还原为对应节点 HTML（Tiptap 按
// parseHTML 约定重建节点）；replaceAll 后扫描缺失节点按原顺序追加文末保底。

/** 占位行 title 值转义（" 与 \；还原侧配对）。 */
function escapePlaceholderTitle(title: string): string {
  return String(title ?? '').replace(/\\/g, '\\\\').replace(/"/g, '\\"')
}

/** 占位行 title 值反转义。 */
function unescapePlaceholderTitle(title: string): string {
  return String(title ?? '').replace(/\\(.)/g, '$1')
}

/** 特殊节点 → 占位行（非特殊节点返回 null）。 */
function specialNodePlaceholder(node: ProseMirrorNode): string | null {
  const attrs = node.attrs as { kind?: string; fileId?: string; title?: string }
  const fid = String(attrs.fileId ?? '')
  const title = escapePlaceholderTitle(attrs.title ?? '')
  if (node.type.name === 'docflowEmbed') {
    return `[DocFlow-Embed kind="${String(attrs.kind ?? 'file')}" fileId="${fid}" title="${title}"]`
  }
  if (node.type.name === 'docflowImage') {
    return `[DocFlow-Image fileId="${fid}" title="${title}"]`
  }
  if (node.type.name === 'docflowFileCard') {
    return `[DocFlow-File fileId="${fid}" title="${title}"]`
  }
  return null
}

/** 文档（或 [from,to] 选区范围）序列化为带占位行的 AI 上下文纯文本：
 * 块边界以 '\n\n' 分隔（与 getText 一致），特殊节点占位行原样带出。 */
function docToAIText(doc: ProseMirrorNode, from?: number, to?: number): string {
  const lo = from ?? 0
  const hi = to ?? doc.content.size
  let out = ''
  let lastBlock: ProseMirrorNode | null = null
  doc.descendants((node, pos) => {
    if (pos >= hi || pos + node.nodeSize <= lo) return false
    const block = doc.resolve(pos).parent
    if (out && lastBlock !== null && block !== lastBlock) out += '\n\n'
    lastBlock = block
    const ph = specialNodePlaceholder(node)
    if (ph) {
      out += ph
      return false
    }
    if (node.isText && node.text) out += node.text
    return true
  })
  return out
}

/** 特殊节点快照条目（replaceAll 保底找回用）。 */
interface SpecialNodeSnapshot { type: string; attrs: Record<string, unknown> }

/** 收集文档内全部特殊节点（docflowEmbed/Image/FileCard）按出现顺序。 */
function collectSpecialNodes(doc: ProseMirrorNode): SpecialNodeSnapshot[] {
  const out: SpecialNodeSnapshot[] = []
  doc.descendants((node) => {
    const ph = specialNodePlaceholder(node)
    if (ph) out.push({ type: node.type.name, attrs: { ...node.attrs } as Record<string, unknown> })
    return !ph
  })
  return out
}

/** 特殊节点去重键（type+fileId：同文件多处引用按存在性判断）。 */
function specialNodeKey(n: SpecialNodeSnapshot): string {
  return `${n.type}:${String((n.attrs as { fileId?: string }).fileId ?? '')}`
}

/** HTML 属性值转义。 */
function escapeHTMLAttr(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

/** 把 AI 输出 markdown 中的占位行还原为对应节点的 HTML（marked 解析前）：
 * - Embed/Image 为块级 div（data-docflow-embed / data-docflow-image 属性，
 *   与 DocflowEmbed.ts / DocflowImage.ts 的 parseHTML 约定一致）；
 * - FileCard 为内联 span（data-docflow-file）。 */
function hydratePlaceholders(md: string): string {
  const title = '((?:\\\\.|[^"\\\\])*)'
  return md
    .replace(new RegExp(`\\[DocFlow-Embed kind="([^"]*)" fileId="([^"]*)" title=${title}\\]`, 'g'),
      (_m, kind: string, fid: string, t: string) =>
        `<div data-docflow-embed="${escapeHTMLAttr(kind)}" data-file-id="${escapeHTMLAttr(fid)}" data-title="${escapeHTMLAttr(unescapePlaceholderTitle(t))}" data-width="100"></div>`)
    .replace(new RegExp(`\\[DocFlow-Image fileId="([^"]*)" title=${title}\\]`, 'g'),
      (_m, fid: string, t: string) =>
        `<div data-docflow-image="1" data-file-id="${escapeHTMLAttr(fid)}" data-title="${escapeHTMLAttr(unescapePlaceholderTitle(t))}" data-width="100"></div>`)
    .replace(new RegExp(`\\[DocFlow-File fileId="([^"]*)" title=${title}\\]`, 'g'),
      (_m, fid: string, t: string) =>
        `<span data-docflow-file="1" data-file-id="${escapeHTMLAttr(fid)}" data-title="${escapeHTMLAttr(unescapePlaceholderTitle(t))}"></span>`)
}

/** 在 ProseMirror 文档中按纯文本查找 needle（首个匹配，首尾空白不参与）：
 * 遍历 text 节点拼接连续纯文本并维护「字符偏移→文档位置」映射——同块内
 * 相邻 text 节点无缝拼接（支持跨加粗等格式节点的连续匹配），块边界插入
 * '\n\n'（与 editor.getText() 全文上下文一致）。精确 indexOf 未命中时做
 * 空白折叠兜底（全文 '\n\n' 与选区 '\n' 分隔、连续空格差异归一）。返回匹
 * 配的 {from,to} 文档位置；未命中返回 null。 */
function docFindText(doc: ProseMirrorNode, needleRaw: string): { from: number; to: number } | null {
  const needle = needleRaw.trim()
  if (!needle) return null
  let raw = ''
  const segs: Array<{ start: number; end: number; from: number; to: number }> = []
  let lastBlock: ProseMirrorNode | null = null
  doc.descendants((node, pos) => {
    if (!node.isText || !node.text) return
    const block = doc.resolve(pos).parent
    if (lastBlock !== null && block !== lastBlock) raw += '\n\n'
    lastBlock = block
    segs.push({ start: raw.length, end: raw.length + node.text.length, from: pos, to: pos + node.text.length })
    raw += node.text
  })
  if (!segs.length) return null
  // 字符偏移→文档位置（落在块边界分隔符上时取相邻文本节点端点）。
  const mapPos = (offset: number): number => {
    for (const seg of segs) {
      if (offset < seg.start) return seg.from
      if (offset <= seg.end) return seg.from + (offset - seg.start)
    }
    return segs[segs.length - 1].to
  }
  const hit = raw.indexOf(needle)
  if (hit >= 0) return { from: mapPos(hit), to: mapPos(hit + needle.length) }
  // 空白折叠兜底：norm 为折叠后文本，normIdx[i] 为第 i 个字符在 raw 的偏移
  //（合成空格记 -1，真实字符记原偏移）。
  let norm = ''
  const normIdx: number[] = []
  let gap = false
  for (let i = 0; i < raw.length; i++) {
    if (/\s/.test(raw[i])) {
      if (norm) gap = true
      continue
    }
    if (gap) {
      norm += ' '
      normIdx.push(-1)
      gap = false
    }
    norm += raw[i]
    normIdx.push(i)
  }
  const normNeedle = needle.replace(/\s+/g, ' ').trim()
  const idx = normNeedle ? norm.indexOf(normNeedle) : -1
  if (idx >= 0) {
    const start = normIdx[idx]
    const end = normIdx[idx + normNeedle.length - 1]
    if (start !== undefined && end !== undefined && start >= 0 && end >= 0) {
      return { from: mapPos(start), to: mapPos(end + 1) }
    }
  }
  return null
}

export default function DfdocEditorPage({
  mode,
  fileId: fileIdProp,
}: { mode?: 'edit' | 'view'; fileId?: string }) {
  const { fileId: routeFileId = '' } = useParams()
  const fileId = fileIdProp ?? routeFileId
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const returnTo = safeReturnTo(searchParams.get('returnTo'))
  const { modal: antdModal, message } = AntdApp.useApp()
  const viewMode = mode === 'view' || searchParams.get('mode') === 'view'
  const locale = useLocale()
  const msg = (key: MessageKey) => t(locale, key)
  const [name, setName] = useState('document.dfrt')
  const [doc, setDoc] = useState('')
  const [versionId, setVersionId] = useState('')
  const [comments, setComments] = useState<DocumentComment[]>([])
  const [commentError, setCommentError] = useState('')
  const refreshComments = useCallback(async () => {
    try { setComments(await listDocumentComments(fileId)); setCommentError('') }
    catch (err) { setCommentError(err instanceof Error ? err.message : msg('loadFailed')) }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [dirty, setDirty] = useState(false)
  const dirtyRef = useRef(false)
  // 多人实时协作快照（collab WS 状态/成员/leader；null=未启用或已卸载）。
  const [collabState, setCollabState] = useState<CollabSnapshot | null>(null)
  const collabLeader = !!collabState
    && collabState.status === 'connected'
    && !!collabState.selfConnId
    && collabState.leaderConnId === collabState.selfConnId

  useEffect(() => {
    let alive = true
    setLoading(true)
    setError('')
    void Promise.all([getFileMeta(fileId), fetchFileText(fileId)])
      .then(([meta, content]) => {
        if (!alive) return
        setName(meta.name)
        setVersionId(meta.current_version?.id ?? '')
        setDoc(content)
        void refreshComments()
        dirtyRef.current = false
        setDirty(false)
      })
      .catch((err) => {
        if (alive) setError(err instanceof Error ? err.message : msg('loadFailed'))
      })
      .finally(() => {
        if (alive) setLoading(false)
      })
    return () => { alive = false }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])

  const save = useCallback(async () => {
    if (saving) return
    setSaving(true)
    setError('')
    setNotice('')
    try {
      await uploadFileVersion(new File([doc], name, { type: 'application/json' }), fileId, () => {})
      const meta = await getFileMeta(fileId)
      setVersionId(meta.current_version?.id ?? '')
      dirtyRef.current = false
      setDirty(false)
      setNotice(msg('saved'))
    } catch (err) {
      setError(err instanceof Error ? err.message : msg('saveFailed'))
    } finally {
      setSaving(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [saving, doc, name, fileId])

  // Ctrl/Cmd+S 保存（编辑态）。
  useEffect(() => {
    if (viewMode) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
        e.preventDefault()
        void save()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [save, viewMode])

  // ---- 协作 leader 自动保存：本端为 leader（participants 含 leader 标记且为
  // 自己 connId）时每 3 秒检查一次，dirty 即调既有 save()；非 leader 保留
  // 手动保存按钮（协作中他人编辑经 onChange 全量回吐同样置 dirty）。----
  const saveRef = useRef(save)
  saveRef.current = save
  useEffect(() => {
    if (viewMode || !collabLeader) return
    const timer = window.setInterval(() => {
      if (dirtyRef.current) void saveRef.current()
    }, 3000)
    return () => window.clearInterval(timer)
  }, [viewMode, collabLeader])

  // 返回（退出）：与其他编辑页一致的未保存二次确认。
  const exitWithConfirm = () => {
    if (!dirtyRef.current) {
      closeEditorWithFallback(navigate, returnTo)
      return
    }
    antdModal.confirm({
      title: locale === 'zh-CN' ? '有未保存的修改' : 'Unsaved changes',
      content: locale === 'zh-CN'
        ? '文档存在尚未保存的修改，直接退出可能丢失。仍要退出吗？'
        : 'The document has unsaved changes. Exit anyway?',
      okText: locale === 'zh-CN' ? '仍然退出' : 'Exit anyway',
      okButtonProps: { danger: true },
      cancelText: locale === 'zh-CN' ? '继续编辑' : 'Keep editing',
      onOk: () => {
        dirtyRef.current = false
        closeEditorWithFallback(navigate, returnTo)
      },
    })
  }

  useEffect(() => {
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return
      event.preventDefault()
      event.returnValue = ''
    }
    window.addEventListener('beforeunload', onBeforeUnload)
    return () => window.removeEventListener('beforeunload', onBeforeUnload)
  }, [])

  // ---- 编辑器 AI（原 AIEditMenu 快捷指令已并入「AI 对话」下拉按钮） ----
  const tiptapRef = useRef<TiptapEditor | null>(null)
  // AI 对话面板（AIEditChat）展开态（收起不清空会话）。
  const [aiChatOpen, setAiChatOpen] = useState(false)
  // 头部下拉快捷指令（打开面板后由 AIEditChat 消费一次）。
  const [aiQuick, setAiQuick] = useState<AIQuickCommand | null>(null)
  // 撤销回退后的编辑器重挂 key（Tiptap initialJSON 仅挂载时生效）。
  const [editorReloadKey, setEditorReloadKey] = useState(0)

  /** 选区读取：Tiptap state.selection（空选区回退全文）；嵌入块/图片/文件
   * 卡片经 docToAIText 序列化为占位行带出（AI 可见 + 提示词约束保留）。 */
  const aiGetTarget = (): AIEditTarget => {
    const ed = tiptapRef.current
    if (!ed) return { text: '', hasSelection: false }
    const { from, to } = ed.state.selection
    if (to > from) {
      return { text: docToAIText(ed.state.doc, from, to), hasSelection: true }
    }
    return { text: docToAIText(ed.state.doc), hasSelection: false }
  }

  /** 剥掉模型给整段回复包上的单一外层围栏（```markdown / ```md / 裸 ```）：
   * 仅当首行是围栏起始、尾行是围栏闭合且信息串为 markdown 类（或空）时剥壳
   *（正文内含的其余围栏不受影响；```js 等代码围栏不剥，保留代码块语义）——
   * 否则 marked 会把整段回复渲染成一个代码块，文档里看到的就是“原始
   * markdown 文本”（AI 重构输出样式不对的根因之一）。 */
  function stripOuterMarkdownFence(raw: string): string {
    const t = raw.trim()
    const m = /^```(?:[Mm]arkdown|[Mm]d)?[ \t]*\n([\s\S]*?)\n?```[ \t]*$/.exec(t)
    return m ? m[1] : t
  }

  /** AI 输出（markdown）→ 富文本 HTML：先经 hydratePlaceholders 把占位行
   * 还原为嵌入节点 HTML，再 marked 解析（GFM：表格/删除线等；breaks：段内
   * 单换行渲染为换行），随后 insertContent 按 Tiptap schema 将 HTML 解析为
   * 富文本节点——标题/列表/表格/代码块/加粗斜体链接等标准 markdown 全部
   * 映射为富文本样式，嵌入块/图片/文件卡片经占位行还原为原生节点。 */
  const aiMarkdownToHTML = (output: string): string =>
    marked.parse(hydratePlaceholders(stripOuterMarkdownFence(output)), { async: false, gfm: true, breaks: true })

  /** AI 编辑指令执行器（AIEditChat applyKind=richtext-patch 的 onApply 通道）：
   * - 解析回复中的 ```docflow-edit 围栏 → JSON 指令数组（非法 JSON 返回失败
   *   原因，文档不被修改）；
   * - replace：定位原文范围原位替换（marked→HTML insertContentAt(range)）；
   *   insert：after 定位末尾插入；delete：删除定位范围；replaceAll：整篇
   *   替换（0→文末，常规事务正常回吐 onChange 置 dirty）；
   * - 每条指令在「当前」文档重新定位（前序指令已改动文档），找不到→跳过并
   *   累计，结果 message 汇报「已应用 N/M 条；未定位 K 条」及未定位摘要；
   * - 全部未定位/无有效指令：返回失败原因（文档未被修改，面板显示
   *   applyError）；围栏缺失（AI 未按指令格式）：回落既有通道（选区替换/
   *   文末追加，不丢内容）并提示「已按追加处理」。
   * 版本基线由面板 autoApply 先经 aiEnsureSaved 保存；撤销走既有
   * restoreVersion + aiReload。 */
  const applyRichTextPatch = (raw: string): string | void => {
    const zh = locale === 'zh-CN'
    const ed = tiptapRef.current
    if (!ed) return zh ? '编辑器未就绪，未应用' : 'Editor not ready; not applied'
    const candidates = docflowEditFenceCandidates(raw)
    if (!candidates.length) {
      // 围栏缺失：回落现有追加/替换选区通道，内容不丢。
      const { from, to } = ed.state.selection
      const html = aiMarkdownToHTML(raw)
      if (to > from) ed.chain().focus().insertContentAt({ from, to }, html).run()
      else ed.chain().focus().setTextSelection(ed.state.doc.content.size).insertContent(html).run()
      void message.warning(zh ? 'AI 未按编辑指令格式输出，已按追加处理' : 'The AI reply was not in edit-instruction format; it was appended instead')
      return
    }
    let ops: unknown[] | null = null
    for (const cand of candidates) {
      try {
        const parsed: unknown = JSON.parse(cand)
        if (Array.isArray(parsed)) {
          ops = parsed
          break
        }
      } catch {
        // 尝试下一候选（围栏内嵌 ``` 代码块被非贪婪截断等形态）
      }
    }
    if (!ops) return zh ? '编辑指令不是合法的 JSON 数组，文档未被修改' : 'The edit instructions are not a valid JSON array; the document was left unchanged'
    let applied = 0
    let invalid = 0
    const missed: string[] = []
    for (const item of ops) {
      const op = (item ?? {}) as RichTextPatchOp
      const kind = typeof op.op === 'string' ? op.op : ''
      const text = typeof op.text === 'string' ? op.text : ''
      if (kind === 'replaceAll') {
        if (!text.trim()) {
          invalid++
          continue
        }
        // 保底：replaceAll 整篇替换会把 AI 未保留占位行的嵌入块/图片/文件
        // 卡片一并抹掉——替换前快照，替换后缺失的按原顺序追加文末（不丢内容）。
        const before = collectSpecialNodes(ed.state.doc)
        ed.chain().focus().insertContentAt({ from: 0, to: ed.state.doc.content.size }, aiMarkdownToHTML(text)).run()
        const afterKeys = new Set(collectSpecialNodes(ed.state.doc).map(specialNodeKey))
        const lost = before.filter((n) => !afterKeys.has(specialNodeKey(n)))
        if (lost.length > 0) {
          ed.chain().focus().insertContentAt(ed.state.doc.content.size, lost.map((n) => ({ type: n.type, attrs: n.attrs }))).run()
          void message.info(zh ? `已自动找回 ${lost.length} 个嵌入内容（AI 输出未保留其占位行）` : `Restored ${lost.length} embedded block(s) whose placeholders the AI dropped`)
        }
        applied++
        continue
      }
      const locate = kind === 'insert' ? op.after : op.find
      if (typeof locate !== 'string' || !locate.trim() || (kind !== 'delete' && !text.trim())) {
        invalid++
        continue
      }
      const range = docFindText(ed.state.doc, locate)
      if (!range) {
        missed.push(locate.trim().replace(/\s+/g, ' ').slice(0, 20))
        continue
      }
      if (kind === 'delete') ed.chain().focus().deleteRange(range).run()
      else if (kind === 'insert') ed.chain().focus().insertContentAt(range.to, aiMarkdownToHTML(text)).run()
      else if (kind === 'replace') ed.chain().focus().insertContentAt(range, aiMarkdownToHTML(text)).run()
      else {
        invalid++
        continue
      }
      applied++
    }
    if (applied === 0) {
      return missed.length
        ? (zh ? `未定位到任何编辑指令的原文（0/${ops.length} 条），文档未被修改` : `No instruction could be located in the document (0/${ops.length}); the document was left unchanged`)
        : (zh ? `未识别到有效的编辑指令（0/${ops.length} 条），文档未被修改` : `No valid edit instruction found (0/${ops.length}); the document was left unchanged`)
    }
    if (missed.length > 0 || invalid > 0) {
      const parts: string[] = [`${zh ? '已应用' : 'Applied'} ${applied}/${ops.length} ${zh ? '条编辑指令' : 'instructions'}`]
      if (missed.length) parts.push(`${zh ? `未定位 ${missed.length} 条` : `${missed.length} not located`}：${missed.map((s) => `「${s}」`).join(zh ? '、' : ', ')}`)
      if (invalid) parts.push(zh ? `格式无效 ${invalid} 条` : `${invalid} invalid`)
      void message.info(parts.join(zh ? '；' : '; '))
    }
  }

  /** 版本保护前置：确保当前内容已保存（有未保存修改先 save），返回应用前
   * 版本信息（null=保存失败，AIEditChat 将放弃自动应用）。 */
  const aiEnsureSaved = useCallback(async (): Promise<{ versionId: string; version: number } | null> => {
    try {
      if (dirtyRef.current) {
        await save()
        // save() 内部捕获错误不抛出：dirtyRef 仍为 true 即保存失败。
        if (dirtyRef.current) return null
      }
      const meta = await getFileMeta(fileId)
      const cur = meta.current_version
      if (!cur) return null
      setVersionId(cur.id)
      return { versionId: cur.id, version: cur.version }
    } catch {
      return null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, save])

  /** 撤销回退后刷新编辑器：重新拉取最新内容并重挂 Tiptap（initialJSON 仅
   * 挂载时生效；协作连接随重挂重建）。 */
  const aiReload = useCallback(async () => {
    const [meta, content] = await Promise.all([getFileMeta(fileId), fetchFileText(fileId)])
    setName(meta.name)
    setVersionId(meta.current_version?.id ?? '')
    setDoc(content)
    dirtyRef.current = false
    setDirty(false)
    setNotice('')
    setEditorReloadKey((k) => k + 1)
    void refreshComments()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId])

  /** 头部下拉快捷指令：打开面板并透传给 AIEditChat 自动执行。 */
  const openAiChatWith = (cmd: AIQuickCommand) => {
    setAiChatOpen(true)
    setAiQuick(cmd)
  }

  if (loading) return <main className="text-editor-page"><div className="text-editor-state">{msg('loading')}</div></main>
  if (error) return <main className="text-editor-page"><div className="banner error">{error}</div></main>

  if (viewMode) {
    return (
      <main className="text-editor-page viewer-only">
        <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
          <RichTextEditor key={fileId} initialJSON={doc} fileId={fileId} comments={comments} readonly />
        </Suspense>
      </main>
    )
  }

  return (
    <main className="text-editor-page">
      {/* 行布局：主列（头部/横幅/编辑器）+ 右侧 AI 对话面板（可收起，
          不破坏编辑区 flex 高度链；AI 未启用时面板不渲染）。 */}
      <div className="text-editor-body">
        <div className="text-editor-main">
          <header className="text-editor-head">
            <div className="text-editor-head-title">
              <div className="text-editor-back">
                <Button type="text" size="small" onClick={exitWithConfirm}>{msg('back')}</Button>
              </div>
              <div>
                <h1>{locale === 'zh-CN' ? '富文本文档' : 'Rich text document'}</h1>
                <div className="muted">
                  {name}
                  {dirty && <span className="badge uploading text-editor-dirty-badge">{locale === 'zh-CN' ? '未保存' : 'Unsaved'}</span>}
                  {collabLeader && dirty && <span className="badge uploading text-editor-dirty-badge">{locale === 'zh-CN' ? '协作自动保存' : 'Autosaving'}</span>}
                </div>
              </div>
            </div>
            <div className="editor-head-actions">
              {/* AI 对话（主点击开面板；下拉快捷指令引导走编辑指令模式：
                  润色/重构全文/修正错别字/摘要/自定义，打开面板自动发送）。 */}
              <AIEditChatButton open={aiChatOpen} onToggle={() => setAiChatOpen((v) => !v)} onQuick={openAiChatWith} kind="richtext-patch" disabled={saving} />
              <Button
                type="primary"
                size="small"
                disabled={saving}
                loading={saving}
                onClick={() => void save()}
              >
                {msg('save')}
              </Button>
            </div>
          </header>
          {notice && <div className="banner ok">{notice}</div>}
          {commentError && <div className="banner error">{commentError}</div>}
          {/* 协作提示：error 信封 / 重连超限回退单机（编辑器仍可用）/ 版本漂移建议刷新。 */}
          {collabState?.message && <div className="banner warn">{collabState.message}</div>}
          <Suspense fallback={<div className="text-editor-state">{msg('loading')}</div>}>
            <RichTextEditor
              key={`${fileId}-${editorReloadKey}`}
              initialJSON={doc}
              fileId={fileId}
              versionId={versionId}
              comments={comments}
              collab
              onCollabState={setCollabState}
              onCommentsChange={() => void refreshComments()}
              onCommentError={setCommentError}
              onEditor={(ed) => { tiptapRef.current = ed }}
              onChange={(json) => {
                setDoc(json)
                setNotice('')
                dirtyRef.current = true
                setDirty(true)
              }}
            />
          </Suspense>
        </div>
        {/* AI 对话式创作/编辑侧栏面板（applyKind=richtext-patch 指令式编辑）：
            AI 回复 ```docflow-edit 围栏内的 JSON 编辑指令数组，经
            applyRichTextPatch 在文档中按定位原文精确执行增删改插（可撤销）；
            围栏缺失时执行器回落选区替换/文末追加。可修改模式自动应用前经
            aiEnsureSaved 保存基线版本，撤销回退后 reload。 */}
        <AIEditChat
          open={aiChatOpen}
          onClose={() => setAiChatOpen(false)}
          getTarget={aiGetTarget}
          getAllText={() => aiGetTarget().text}
          onApply={(_mode, raw) => applyRichTextPatch(raw)}
          fileId={fileId}
          ensureSaved={aiEnsureSaved}
          reload={aiReload}
          quickCommand={aiQuick}
          onQuickConsumed={() => setAiQuick(null)}
          applyKind="richtext-patch"
        />
      </div>
    </main>
  )
}
