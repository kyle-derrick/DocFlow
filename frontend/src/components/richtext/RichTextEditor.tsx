// DocFlow 富文本编辑器（Tiptap v2，.dfrt 专属格式：Tiptap JSON 存储）：
// - 受控语义：initialJSON 仅装载期生效（父组件用 key 重挂载换内容），
//   用户编辑经 onChange(json) 全量回吐最新文档 JSON（JSON.stringify(getJSON())）；
// - readonly 态复用同一渲染（editable=false，隐藏工具栏/菜单）；公开分享
//   态经 publicBase 提供分享树 raw 基址，引用节点改走 raw/share 解析；
// - 引用节点体系（均存 file-id 引用，编辑源文件后引用处刷新即可实时更新）：
//   docflowEmbed（drawio/excalidraw/office/web/file 块级嵌入，见 DocflowEmbed.ts）、
//   docflowImage（图片块：渲染时换 blob/raw URL + 宽度 25/50/75/100%）、
//   docflowFileCard（内联文件卡片：图标+名称+大小，点击查看）；
// - 粘贴/拖入：图片自动上传到文档所在目录 assets/ 插图片块；其他文件上传
//   插文件卡片；富文本 HTML 清理样式保留结构，远程图片尝试重上传；
// - 选中浮动工具条（BubbleMenu）：文字格式（加粗/斜体/高亮/链接/代码）+
//   内联文件卡片删除；图片/嵌入块的操作条随节点选中浮出（NodeView 内）；
//   v2.7 编辑态右键菜单（antd Menu，与文件页右键同风格）：撤销重做/粗斜/
//   标题/列表/高亮/代码/链接 + 插入（图片/文件/表格/分割线/代码块）+ 嵌入
//   （drawio/白板）；查看页（readonly）不接管右键。
import {
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react'
import type { ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { BubbleMenu, EditorContent, useEditor } from '@tiptap/react'
import type { Editor } from '@tiptap/react'
import type { Transaction } from '@tiptap/pm/state'
import { NodeSelection } from '@tiptap/pm/state'
import { Fragment } from '@tiptap/pm/model'
import { Step } from '@tiptap/pm/transform'
import { receiveTransaction, sendableSteps } from '@tiptap/pm/collab'
import { App as AntdApp, Button, Dropdown, Input, Menu, Modal as AntdModal, Popover, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import {
  Bold,
  Braces,
  ChevronDown,
  Code,
  File as FileIcon,
  Heading1,
  Heading2,
  Heading3,
  Highlighter,
  Image as ImageIcon,
  Italic,
  Link2,
  List,
  MessageSquare,
  ListOrdered,
  ListTodo,
  Minus,
  Network,
  PenLine,
  Pilcrow,
  Redo2,
  Clipboard,
  Scissors,
  Strikethrough,
  Table as TableIcon,
  TextQuote,
  Trash2,
  Underline as UnderlineIcon,
  Undo2,
} from 'lucide-react'
import StarterKit from '@tiptap/starter-kit'
import { Mark } from '@tiptap/core'
import Underline from '@tiptap/extension-underline'
import Highlight from '@tiptap/extension-highlight'
import Link from '@tiptap/extension-link'
import Image from '@tiptap/extension-image'
import TaskList from '@tiptap/extension-task-list'
import TaskItem from '@tiptap/extension-task-item'
import Table from '@tiptap/extension-table'
import TableRow from '@tiptap/extension-table-row'
import TableHeader from '@tiptap/extension-table-header'
import TableCell from '@tiptap/extension-table-cell'
import Placeholder from '@tiptap/extension-placeholder'
import CodeBlockLowlight from '@tiptap/extension-code-block-lowlight'
import { common, createLowlight } from 'lowlight'
import { getFileMeta, resolveNamespaceOf, uploadFile, ApiError, createDocumentComment, replyDocumentComment, updateDocumentComment, deleteDocumentComment, currentUserId, getMe, websocketToken } from '../../api'
import type { DocumentComment } from '../../api'
import { useLocale } from '../../i18n'
import { clampFixedMenu, promptViaModal } from '../FileBrowser'
import DocflowEmbed from './DocflowEmbed'
import DocflowFileCard from './DocflowFileCard'
import DocflowImage from './DocflowImage'
import { OPEN_FILE_CARD_EVENT, CARD_CONTEXT_EVENT } from './FileCardView'
import { REPLACE_EMBED_EVENT } from './EmbedView'
import { resolveFileById } from '../../api'
import { editorRouteFor } from '../../openers'
import { EmbedKind } from './markdownRoundtrip'
import type { DocflowEmbedAttrs } from './DocflowEmbed'
import FilePickerModal, { PickerFilter, ensureAssetsFolder } from './FilePickerModal'
import type { PickedFile } from './FilePickerModal'
import { RichTextPublicProvider } from './RichTextPublicContext'
import type { RichTextPublicBase } from './RichTextPublicContext'
import { createSlashMenuExtension } from './SlashMenu'
import type { SlashMenuCallbacks } from './SlashMenu'
import { sanitizePastedHTML, rewriteRemoteImages } from './pasteHTML'
import {
  CollabSession,
  avatarCharOf,
  clearRemoteCarets,
  collabColorFor,
  collabWsUrl,
  createCollabExtension,
  randomCollabClientID,
  remoteCaretsKey,
  removeRemoteCaret,
  setRemoteCaret,
} from './CollabSession'
import type { CollabParticipant, CollabSnapshot, CollabStatus } from './CollabSession'

export interface RichTextEditorProps {
  /** 装载期使用的文档 JSON（Tiptap doc 序列化串；外部更新不自动同步，用 key 重挂载）。 */
  initialJSON: string
  /** 用户编辑后回调（最新文档 JSON 字符串）。 */
  onChange?: (json: string) => void
  readonly?: boolean
  /** dfdoc 文件自身 ID：图片上传时解析其所在目录（其下 assets/ 子目录）。 */
  fileId?: string
  versionId?: string
  comments?: DocumentComment[]
  onCommentsChange?: () => void
  onCommentError?: (error: string) => void
  /** 公开分享渲染态：提供分享树 raw 基址与文档自身路径（readonly 配合使用）。 */
  publicBase?: RichTextPublicBase
  /** 编辑器实例就绪/销毁回调（编辑器 AI 等宿主能力挂接用）。 */
  onEditor?: (editor: Editor | null) => void
  /** 多人实时协作（连 /api/v1/collab/:fileId/ws；失败自动回退单机编辑，编辑器不销毁）。 */
  collab?: boolean
  /** 协作状态快照回调（status/participants/self/leader/message；卸载时回 null）。 */
  onCollabState?: (snapshot: CollabSnapshot | null) => void
}

/** 解析 .dfdoc 内容：JSON 合法且为 {type:'doc'} 时原样使用，否则回退空段（不抛错——查看态损坏内容仍可打开编辑修复）。 */
function parseDocJSON(text: string): Record<string, unknown> {
  const trimmed = text.trim()
  if (trimmed) {
    try {
      const parsed = JSON.parse(trimmed) as Record<string, unknown>
      if (parsed && parsed.type === 'doc') return parsed
    } catch {
      /* 损坏内容回退空文档 */
    }
  }
  return { type: 'doc', content: [{ type: 'paragraph' }] }
}

const IMAGE_MIME = /^image\//

const CommentMark = Mark.create({
  name: 'comment',
  inclusive: false,
  addAttributes() {
    return { id: { default: '' }, text: { default: '' } }
  },
  parseHTML() { return [{ tag: 'span[data-comment-id]' }] },
  renderHTML({ HTMLAttributes }) {
    return ['span', { ...HTMLAttributes, 'data-comment-id': HTMLAttributes.id, class: 'rich-text-comment' }, 0]
  },
})

/** 文件卡片点击 → 查看弹窗内容（FileViewerDispatch 懒加载，避开
 * ViewerPage→DfdocEditorPage→RichTextEditor 静态循环依赖）。 */
const FileViewerDispatch = lazy(() =>
  import('../../pages/ViewerPage').then((m) => ({ default: m.FileViewerDispatch })),
)

/** 文件选择器请求：filter 类型 + 可选替换目标（嵌入块「替换」按钮发起；
 * pos 为发起替换的节点位置，按位精准替换防同源多块误替换）。 */
interface PickerRequest {
  filter: PickerFilter
  replaceOf?: { fileId: string; kind: string; pos: number }
}

/** 嵌入块替换的文件选择器过滤条件（与嵌入类型一致）。 */
function embedPickerFilter(kind: string): PickerFilter {
  if (kind === 'drawio') return 'drawio'
  if (kind === 'excalidraw') return 'excalidraw'
  return 'any'
}

/** 文件名加唯一后缀（base-xxxx.ext；随机 4-6 位，冲突域内近似唯一）。 */
function uniqueFileName(name: string): string {
  const suffix = Math.random().toString(36).slice(2, 8)
  const i = name.lastIndexOf('.')
  return i > 0 ? `${name.slice(0, i)}-${suffix}${name.slice(i)}` : `${name}-${suffix}`
}

export default function RichTextEditor({
  initialJSON,
  onChange,
  readonly = false,
  fileId,
  versionId,
  comments = [],
  onCommentsChange,
  onCommentError,
  publicBase,
  onEditor,
  collab = false,
  onCollabState,
}: RichTextEditorProps) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const { modal: antdModal } = AntdApp.useApp()
  const editorRef = useRef<Editor | null>(null)
  const onChangeRef = useRef(onChange)
  onChangeRef.current = onChange

  const [picker, setPicker] = useState<PickerRequest | null>(null)
  const [mdParentId, setMdParentId] = useState<string | null | undefined>(undefined)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')
  // 文件卡片查看弹窗（卡片点击经全局事件上提，编辑器层统一渲染）。
  const [cardView, setCardView] = useState<{ fileId: string; name: string } | null>(null)
  // 编辑器右键菜单（编辑态：与文件页右键同风格的 antd Menu 固定浮层）。
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number } | null>(null)
  // 文件卡片右键菜单（编辑态：「编辑（新窗口）」）。
  const [cardCtx, setCardCtx] = useState<{ fileId: string; name: string; x: number; y: number } | null>(null)
  // 工具栏链接 Popover：输入 URL 的受控态。
  const [linkOpen, setLinkOpen] = useState(false)
  const [linkUrl, setLinkUrl] = useState('https://')
  const [outline, setOutline] = useState<Array<{ level: number; text: string; pos: number }>>([])
  // 多人实时协作状态（null = 未启用 collab；离线/失败即回退单机编辑）。
  const [collabStatus, setCollabStatus] = useState<CollabStatus | null>(null)
  const [collabParticipants, setCollabParticipants] = useState<CollabParticipant[]>([])
  // 协作编辑器基座（起点文档 JSON + 对应权威版本）：协作模式 bootstrap 完成
  // （init/init-doc/失败回退）前为 null，编辑器挂起渲染加载提示；非协作模式
  // 恒为 {json:initialJSON, version:0}。基座落位后随 useEditor deps 重建编辑器。
  const [collabBase, setCollabBase] = useState<{ json: string; version: number } | null>(
    collab && !readonly ? null : { json: initialJSON, version: 0 },
  )
  // 协作会话（bootstrap 建立后常驻；sessionSeq 在会话重建时自增以重挂 wiring）。
  const sessionRef = useRef<CollabSession | null>(null)
  const [sessionSeq, setSessionSeq] = useState(0)
  // 协作提示文案（快照上提用）与基座是否已落位（跨会话重建持久，防重置内容）。
  const collabMessageRef = useRef('')
  const collabInitedRef = useRef(false)
  const initialJSONRef = useRef(initialJSON)
  initialJSONRef.current = initialJSON
  const [commentBusy, setCommentBusy] = useState(false)
  const commentAction = async (action: () => Promise<unknown>) => {
    setCommentBusy(true)
    try { await action(); onCommentsChange?.(); onCommentError?.('') }
    catch (err) { onCommentError?.(err instanceof Error ? err.message : (zh ? '评论操作失败' : 'Comment action failed')) }
    finally { setCommentBusy(false) }
  }

  const callbacksRef = useRef<SlashMenuCallbacks>({
    onInsertEmbed: (filter) => setPicker({ filter }),
    promptLink: () =>
      promptViaModal(antdModal, {
        title: zh ? '插入链接' : 'Insert link',
        label: zh ? '链接地址（留空取消）' : 'Link URL (empty to cancel)',
        initialValue: 'https://',
        okText: zh ? '确定' : 'OK',
        cancelText: zh ? '取消' : 'Cancel',
      }),
    isZh: zh,
  })
  useEffect(() => {
    callbacksRef.current.isZh = zh
  }, [zh])

  // 卡片点击（查看弹窗）、卡片右键（编辑菜单）与嵌入块「替换」：NodeView
  // 内不渲染 Modal（ProseMirror 搬移 DOM 会与 React 冲突崩溃），经全局事件
  // 上提到编辑器层统一处理。
  useEffect(() => {
    const onOpenCard = (e: Event) => {
      const detail = (e as CustomEvent<{ fileId: string; name: string }>).detail
      if (detail?.fileId) setCardView({ fileId: detail.fileId, name: detail.name || '' })
    }
    const onCardContext = (e: Event) => {
      const detail = (e as CustomEvent<{ fileId: string; name: string; x: number; y: number }>).detail
      if (detail?.fileId) setCardCtx({ fileId: detail.fileId, name: detail.name || '', x: detail.x, y: detail.y })
    }
    const onReplaceEmbed = (e: Event) => {
      const detail = (e as CustomEvent<{ fileId: string; kind: string; pos?: number }>).detail
      if (detail?.fileId) setPicker({ filter: embedPickerFilter(detail.kind), replaceOf: { fileId: detail.fileId, kind: detail.kind, pos: detail.pos ?? -1 } })
    }
    const onPointerDown = (e: Event) => {
      // 菜单浮层内按下不关闭：mousedown 先于 click 关掉浮层会使菜单项
      // onClick 落空（元素在 mouseup 前被卸载）。
      const t = e.target as HTMLElement | null
      if (t && typeof t.closest === 'function' && t.closest('.ctx-menu')) return
      setCtxMenu(null)
      setCardCtx(null)
    }
    window.addEventListener(OPEN_FILE_CARD_EVENT, onOpenCard)
    window.addEventListener(CARD_CONTEXT_EVENT, onCardContext)
    window.addEventListener(REPLACE_EMBED_EVENT, onReplaceEmbed)
    window.addEventListener('mousedown', onPointerDown)
    return () => {
      window.removeEventListener(OPEN_FILE_CARD_EVENT, onOpenCard)
      window.removeEventListener(CARD_CONTEXT_EVENT, onCardContext)
      window.removeEventListener(REPLACE_EMBED_EVENT, onReplaceEmbed)
      window.removeEventListener('mousedown', onPointerDown)
    }
  }, [])

  // 文档所在目录（图片上传目标 assets/ 的父目录）。
  useEffect(() => {
    if (!fileId) {
      setMdParentId(null)
      return
    }
    let alive = true
    void getFileMeta(fileId)
      .then((meta) => { if (alive) setMdParentId(meta.parent_id) })
      .catch(() => { if (alive) setMdParentId(null) })
    return () => { alive = false }
  }, [fileId])

  /** 定位/创建 assets 目录并返回其 ID（失败抛错）。 */
  const locateAssets = async (): Promise<string> => {
    const parentId = mdParentId ?? null
    const ns = parentId ? await resolveNamespaceOf(parentId) : null
    const assetsId = await ensureAssetsFolder(parentId, ns)
    if (!assetsId) throw new Error(zh ? '无法定位 assets 目录' : 'Cannot locate assets folder')
    return assetsId
  }

  /** 上传文件到 assets/，返回 { id, name, size }。
   * 同名冲突（assets/ 下已有同名文件，如重复粘贴截图 image.png）自动改名
   * 重试（base-xxxx.ext 唯一后缀），保证生成唯一文件名不再 409。 */
  const uploadToAssets = async (file: File) => {
    const assetsId = await locateAssets()
    let current = file
    let lastErr: unknown
    for (let attempt = 0; attempt < 3; attempt++) {
      try {
        const session = await uploadFile(current, assetsId, () => {})
        if (!session.file_id) throw new Error(zh ? '上传完成但未返回文件 ID' : 'Upload finished without file id')
        return { id: session.file_id, name: current.name, size: current.size }
      } catch (err) {
        lastErr = err
        const conflict = err instanceof ApiError && (err.status === 409 || /conflict|同名|exists/i.test(err.message))
        if (!conflict) throw err
        current = new File([file], uniqueFileName(current.name), { type: file.type || undefined, lastModified: file.lastModified })
      }
    }
    throw lastErr instanceof Error ? lastErr : new Error(zh ? '文件上传失败' : 'File upload failed')
  }

  /** 图片上传：assets 目录定位/创建 → 上传 → 在粘贴/拖入时的选区位置插入
   * docflowImage 图片块（上传耗时数秒，期间用户可能移动光标——按捕获的
   * 位置插入，保证「光标处立即显示」语义）。 */
  const uploadImages = async (files: File[], at?: { from: number; to: number }) => {
    const editor = editorRef.current
    if (!editor) return
    for (const file of files) {
      if (!IMAGE_MIME.test(file.type)) continue
      setStatus(zh ? `正在上传「${file.name}」…` : `Uploading "${file.name}"…`)
      setError('')
      try {
        const uploaded = await uploadToAssets(file)
        insertImageRef(uploaded.id, uploaded.name, at)
      } catch (err) {
        setError(err instanceof Error ? err.message : (zh ? '图片上传失败' : 'Image upload failed'))
      }
    }
    setStatus('')
  }

  /** 其他文件拖入/粘贴：上传到 assets/ → 插入内联文件卡片。 */
  const uploadFilesAsCards = async (files: File[]) => {
    const editor = editorRef.current
    if (!editor) return
    for (const file of files) {
      if (IMAGE_MIME.test(file.type) || file.size === 0) continue
      setStatus(zh ? `正在上传「${file.name}」…` : `Uploading "${file.name}"…`)
      setError('')
      try {
        const uploaded = await uploadToAssets(file)
        insertFileCard(uploaded.id, uploaded.name, uploaded.size)
      } catch (err) {
        setError(err instanceof Error ? err.message : (zh ? '文件上传失败' : 'File upload failed'))
      }
    }
    setStatus('')
  }

  /** 粘贴富文本（Word/网页）：清理样式保留结构，远程/内嵌图片尝试重上传
   *（CORS 失败的远程图保留原 URL；file:// 等本地引用图移除；blob: 图优先
   * 消费剪贴板随附的图片文件重上传——跨页 blob URL 不可 fetch，直接保留
   * 会是永不显示的死链，即「粘贴后图片不显示」的根因）。 */
  const pasteRichHTML = async (html: string, clipboardFiles: File[] = []) => {
    const editor = editorRef.current
    if (!editor) return
    const range = { from: editor.state.selection.from, to: editor.state.selection.to }
    setStatus(zh ? '正在处理粘贴内容…' : 'Processing pasted content…')
    setError('')
    try {
      const cleaned = sanitizePastedHTML(html)
      const rewritten = await rewriteRemoteImages(cleaned, async (file) => {
        const uploaded = await uploadToAssets(file)
        return uploaded
      }, clipboardFiles)
      editor.chain().focus().insertContentAt(range, rewritten).run()
    } catch (err) {
      setError(err instanceof Error ? err.message : (zh ? '粘贴内容处理失败' : 'Failed to process pasted content'))
      // 兜底：插入清理后的 HTML（不做图片重上传）。
      try {
        editor.chain().focus().insertContentAt(range, sanitizePastedHTML(html)).run()
      } catch {
        /* 忽略 */
      }
    }
    setStatus('')
  }

  const insertEmbed = (kind: EmbedKind, id: string, title: string) => {
    editorRef.current
      ?.chain()
      .focus()
      .insertContent([
        { type: 'docflowEmbed', attrs: { kind, fileId: id, title } },
        { type: 'paragraph' },
      ])
      .run()
  }

  /** 插入图片块（fileId 引用，渲染时现取 URL）；at 提供时在该选区位置插入
   *（粘贴/拖入捕获的位置，防止上传耗时期间光标漂移导致插错位置）。 */
  const insertImageRef = (id: string, title: string, at?: { from: number; to: number }) => {
    const editor = editorRef.current
    if (!editor) return
    const content = [
      { type: 'docflowImage', attrs: { fileId: id, title, width: 100 } },
      { type: 'paragraph' },
    ]
    if (at) {
      const pos = {
        from: Math.min(at.from, editor.state.doc.content.size),
        to: Math.min(at.to, editor.state.doc.content.size),
      }
      editor.chain().focus().insertContentAt(pos, content).run()
      return
    }
    editor.chain().focus().insertContent(content).run()
  }

  /** 插入内联文件卡片（size 缺省 0，NodeView 懒拉元数据补齐）。 */
  const insertFileCard = (id: string, title: string, size = 0) => {
    editorRef.current
      ?.chain()
      .focus()
      .insertContent({ type: 'docflowFileCard', attrs: { fileId: id, title, size } })
      .run()
  }

  const lowlight = useMemo(() => createLowlight(common), [])
  // 协作扩展：prosemirror-collab({clientID,version}) 插件 + 远程光标插件
  //（仅 collab 编辑态且基座已落位；version 为基座权威版本）。bootstrap 期间
  // 的占位编辑器不装协作插件（不可见、不参与收发）。
  const collabClientID = useMemo(() => randomCollabClientID(), [])
  const collabExtension = useMemo(
    () => (collab && !readonly && collabBase ? createCollabExtension(collabClientID, collabBase.version) : null),
    [collab, readonly, collabClientID, collabBase],
  )
  const editor = useEditor({
    extensions: [
      StarterKit.configure({
        codeBlock: false,
        heading: { levels: [1, 2, 3, 4, 5, 6] },
      }),
      CodeBlockLowlight.configure({ lowlight }),
      Underline,
      Highlight.configure({ multicolor: false }),
      Link.configure({ openOnClick: false, autolink: true }),
      Image.configure({ inline: false }),
      TaskList,
      TaskItem.configure({ nested: true }),
      Table.configure({ resizable: false }),
      TableRow,
      TableHeader,
      TableCell,
      Placeholder.configure({
        placeholder: zh ? '输入 / 唤起插入菜单…' : 'Type / for commands…',
        showOnlyWhenEditable: true,
      }),
      DocflowEmbed,
      DocflowFileCard,
      DocflowImage,
      CommentMark,
      createSlashMenuExtension(callbacksRef),
      ...(collabExtension ? [collabExtension] : []),
    ],
    // 协作 bootstrap 期间用空段占位（编辑器不渲染，基座落位后随 deps 重建）。
    content: parseDocJSON(collabBase ? collabBase.json : (collab && !readonly ? '' : initialJSON)),
    editable: !readonly,
    editorProps: {
      attributes: { class: 'rich-text-content', spellcheck: 'false' },
      handlePaste: (_view, event) => {
        const files = Array.from(event.clipboardData?.files ?? [])
        const images = files.filter((f) => IMAGE_MIME.test(f.type))
        const others = files.filter((f) => !IMAGE_MIME.test(f.type) && f.size > 0)
        // 粘贴/拖入位置（上传异步完成后仍按此位置插入，防光标漂移）。
        const selection = editorRef.current
          ? { from: editorRef.current.state.selection.from, to: editorRef.current.state.selection.to }
          : undefined
        // 剪贴板文件（截图等）：上传插入；纯文件（无 HTML 结构）时直接消费。
        if (images.length > 0 && !event.clipboardData?.getData('text/html')) {
          void uploadImages(images, selection)
          return true
        }
        if (others.length > 0 && !event.clipboardData?.getData('text/html')) {
          void uploadFilesAsCards(others)
          return true
        }
        // 富文本 HTML：清理 + 图片重上传（截图文件与 HTML 并存时优先保结构，
        // HTML 内远程/blob 图走重上传链路——blob 图消费剪贴板随附文件）。
        const html = event.clipboardData?.getData('text/html')
        if (html && /<img[\s>]/i.test(html)) {
          void pasteRichHTML(html, files)
          return true
        }
        if (html) {
          // 清理样式保留结构（无图片时同步完成，无感插入）。
          const cleaned = sanitizePastedHTML(html)
          if (cleaned !== html) {
            editorRef.current?.chain().focus().insertContent(cleaned).run()
            return true
          }
        }
        return false
      },
      handleDrop: (view, event, _slice, moved) => {
        if (moved) return false
        const files = Array.from(event.dataTransfer?.files ?? [])
        if (files.length === 0) return false
        event.preventDefault()
        const coords = view.posAtCoords({ left: event.clientX, top: event.clientY })
        let selection: { from: number; to: number } | undefined
        if (coords) {
          editorRef.current?.chain().focus().setTextSelection(coords.pos).run()
          selection = { from: coords.pos, to: coords.pos }
        }
        const images = files.filter((f) => IMAGE_MIME.test(f.type))
        const others = files.filter((f) => !IMAGE_MIME.test(f.type) && f.size > 0)
        if (images.length > 0) void uploadImages(images, selection)
        if (others.length > 0) void uploadFilesAsCards(others)
        return true
      },
    },
    // deps：协作基座落位（null → init/init-doc/失败回退值）时以起点文档重建
    // 编辑器（collab 插件带基座版本，版本计数与服务端权威序列对齐）。
  }, [collabBase])
  editorRef.current = editor
  // 宿主编辑器实例回调（编辑器 AI 挂接）：就绪/销毁均通知。
  const onEditorRef = useRef(onEditor)
  onEditorRef.current = onEditor
  useEffect(() => {
    onEditorRef.current?.(editor)
    return () => {
      if (editor) onEditorRef.current?.(null)
    }
  }, [editor])

  // 编辑 → 文档 JSON 回吐（仅文档真实变化）。
  useEffect(() => {
    if (!editor) return
    const handler = ({ transaction }: { transaction: Transaction }) => {
      if (!transaction.docChanged) return
      onChangeRef.current?.(JSON.stringify(editor.getJSON()))
      const next: Array<{ level: number; text: string; pos: number }> = []
      editor.state.doc.descendants((node, pos) => {
        if (node.type.name === 'heading') next.push({ level: Number(node.attrs.level), text: node.textContent, pos })
      })
      setOutline(next)
    }
    editor.on('update', handler)
    const initial: Array<{ level: number; text: string; pos: number }> = []
    editor.state.doc.descendants((node, pos) => {
      if (node.type.name === 'heading') initial.push({ level: Number(node.attrs.level), text: node.textContent, pos })
    })
    setOutline(initial)
    return () => {
      editor.off('update', handler)
    }
  }, [editor])

  useEffect(() => {
    if (!editor || readonly) return
    const root = editor.view.dom
    Array.from(root.children).forEach((child, index) => {
      const element = child as HTMLElement
      element.dataset.topBlock = String(index)
      element.draggable = true
    })
  }, [editor, readonly, outline])

  useEffect(() => {
    editor?.setEditable(!readonly)
  }, [editor, readonly])

  // ---- 多人实时协作（collab=true）：三层解耦 ----
  // a. bootstrap（不依赖编辑器）：建 WS 会话（getMe 取名/颜色、token 携带
  //    同旧版），构造期回调维护状态区/成员/错误提示；onInit/onInitDoc 落
  //    collabBase 基座（起点文档 + 权威版本），WS 持续失败（连续重连超
  //    3 次）或致命 error 经 onFatal 回退单机基座（version=0）解除挂起；
  // b. 编辑器基座：基座未落位时挂起渲染「协作连接中…」加载提示；
  // c. wiring（编辑器 + 会话就绪）：attachStepsSink 应用远端 steps、
  //    attachHandlers 处理 sync 流程（leader 快照上报）、transaction 发送
  //    与 presence。会话与编辑器解耦：WS 失败/离线仅停止协作（状态区
  //    「协作离线」），本地编辑照常（collab 插件隔离他人 steps，undo 不受
  //    影响）；onChange 全文回吐不变，宿主保存逻辑不受影响。重连后版本
  //    不一致无法追赶 steps → 提示刷新。
  const onCollabStateRef = useRef(onCollabState)
  onCollabStateRef.current = onCollabState

  /** 协作状态快照上提（状态区/成员/leader/提示文案；卸载时回 null）。 */
  const pushCollabSnapshot = useCallback(() => {
    const session = sessionRef.current
    onCollabStateRef.current?.({
      status: session ? session.currentStatus : 'offline',
      participants: session ? session.currentParticipants : [],
      selfConnId: session ? session.currentSelfConnId : null,
      leaderConnId: session ? session.leaderConnId : null,
      message: collabMessageRef.current,
    })
  }, [])
  const setCollabMessage = useCallback((text: string) => {
    collabMessageRef.current = text
    pushCollabSnapshot()
  }, [pushCollabSnapshot])

  // a. bootstrap：会话生命周期（不依赖编辑器；基座落位前远端 steps 在会话内排队）。
  useEffect(() => {
    if (!collab || readonly || !fileId) {
      setCollabStatus(null)
      setCollabParticipants([])
      return
    }
    let alive = true
    const userId = currentUserId()
    const color = collabColorFor(userId ?? 'anonymous')
    collabMessageRef.current = ''

    /** 基座落位（幂等：仅首次生效——跨会话重建/重连不重置编辑器内容）。 */
    const settleBase = (base: { json: string; version: number }): boolean => {
      if (collabInitedRef.current) return false
      collabInitedRef.current = true
      setCollabBase(base)
      return true
    }
    /** 重连漂移检测：服务端版本与断线前不一致 = 期间有未同步 steps（无追赶能力）。 */
    const checkDrift = (version: number) => {
      const session = sessionRef.current
      if (session && session.lastServerVersion !== null && version !== session.lastServerVersion) {
        setCollabMessage(zh ? '协作已重连，文档与服务端版本不一致，请刷新页面完成同步' : 'Collab reconnected with version drift; refresh to resync')
      }
    }

    const start = (name: string) => {
      if (!alive) return
      const session = new CollabSession({
        url: collabWsUrl(fileId),
        // tokenProvider 动态取（token 恢复前为 null 时 CollabSession 短退避
        // 等待，避免旧 null 快照 401 死循环）。
        tokenProvider: websocketToken,
        userId,
        name,
        color,
        maxFailedAttempts: 3,
        onStatus: (status) => {
          if (!alive) return
          setCollabStatus(status)
          if (status === 'connected') setCollabMessage('')
        },
        onInit: (version) => {
          // 空房首成员：磁盘文档为基座；重连则比较版本漂移提示刷新。
          if (!settleBase({ json: initialJSONRef.current, version })) checkDrift(version)
        },
        onInitDoc: (doc, version) => {
          // 非空房加入者：leader 实时文档为基座；重连同样比较漂移。
          if (!settleBase({ json: JSON.stringify(doc), version })) checkDrift(version)
        },
        onPresence: (p) => {
          if (!alive) return
          const editor = editorRef.current
          const current = sessionRef.current
          if (!editor || !current || p.connId === current.currentSelfConnId) return
          setRemoteCaret(editor.view, {
            connId: p.connId,
            name: p.name,
            color: p.color,
            anchor: p.selection ? p.selection.anchor : -1,
            head: p.selection ? p.selection.head : -1,
          })
        },
        onParticipants: (participants) => {
          if (!alive) return
          setCollabParticipants(participants)
          // 成员列表已不含的光标一并清除（leave 之外的一致性兜底）。
          const editor = editorRef.current
          if (editor) {
            const ids = new Set(participants.map((p) => p.connId))
            remoteCaretsKey.getState(editor.state)?.carets.forEach((_entry, connId) => {
              if (!ids.has(connId)) removeRemoteCaret(editor.view, connId)
            })
          }
          pushCollabSnapshot()
        },
        onLeave: (connId) => {
          const editor = editorRef.current
          if (alive && editor) removeRemoteCaret(editor.view, connId)
        },
        onError: (message) => {
          if (alive) setCollabMessage(message)
        },
        onFatal: (reason) => {
          // 连接已停（error 信封 / 连续重连超 3 次失败）：编辑器保持单机可用；
          // 基座仍未落位（一直在「协作连接中…」）时立即落单机基座解除挂起。
          if (!alive) return
          setCollabMessage(/reconnect failed/i.test(reason)
            ? (zh ? '协作连接失败，已回退单机编辑，可继续编辑并手动保存' : 'Collaboration connection failed; fell back to standalone editing')
            : reason)
          settleBase({ json: initialJSONRef.current, version: 0 })
        },
      })
      sessionRef.current = session
      setSessionSeq((n) => n + 1)
      setCollabStatus('connecting')
      session.connect()
      pushCollabSnapshot()
    }

    // 展示名：档案昵称 → 用户名 → userId 前 8 位（getMe 失败不阻断协作）。
    void getMe()
      .then((me) => {
        if (alive) start(me.profile.nickname || me.username)
      })
      .catch(() => {
        if (alive) start(userId ? userId.slice(0, 8) : (zh ? '匿名' : 'Guest'))
      })

    return () => {
      alive = false
      sessionRef.current?.close()
      sessionRef.current = null
      const current = editorRef.current
      if (current && !current.isDestroyed) clearRemoteCarets(current.view)
      setCollabStatus(null)
      setCollabParticipants([])
      onCollabStateRef.current?.(null)
    }
  }, [collab, readonly, fileId, zh, pushCollabSnapshot, setCollabMessage])

  // c. wiring：编辑器（基座已落位、collab 插件就绪）+ 会话可用后挂接
  // steps 应用、sync 流程与发送逻辑；deps 含 sessionSeq（会话重建时对
  // 新会话重新挂接）与 collabBase/editor（基座落位重建编辑器时重挂）。
  useEffect(() => {
    if (!collab || readonly || !fileId || !editor || !collabBase) return
    const session = sessionRef.current
    if (!session) return
    let alive = true
    const clientID = collabClientID
    // 本端已应用的最新权威版本（基座版本起步；应用远端 steps/sync-end 推进）。
    let appliedVersion = collabBase.version
    let lastSentStep: unknown = null
    let presenceTimer: number | null = null
    let lastPresenceSentAt = 0
    let lastPresence: { anchor: number; head: number } | null = null
    let syncPollTimer: number | null = null

    /** 应用远端 steps（Step.fromJSON → receiveTransaction → dispatch）。
     * 自己的 echo 也走 receiveTransaction：其自会识别自己前缀的 steps 仅
     * 做确认（清 sendable、推进版本）不重复应用——这正是「客户端按
     * clientID 忽略自己的 steps」的协议约定；映射失败提示刷新。 */
    const applyRemoteSteps = (steps: unknown[], clientIDs: number[]) => {
      try {
        const parsed = steps.map((s) => Step.fromJSON(editor.schema, s))
        const tr = receiveTransaction(editor.state, parsed, clientIDs)
        editor.view.dispatch(tr)
      } catch {
        setCollabMessage(zh ? '协作变更应用失败，建议刷新页面后重试' : 'Failed to apply collab changes; refresh recommended')
      }
    }

    /** 发送未确认 steps（末位 step 引用去重；发送失败保持 sendable 重试）。 */
    const flushSendable = () => {
      const sendable = sendableSteps(editor.state)
      if (!sendable || sendable.steps.length === 0) return
      const last = sendable.steps[sendable.steps.length - 1]
      if (last === lastSentStep) return
      if (session.sendSteps(sendable.steps.map((s) => s.toJSON()), clientID, sendable.version)) lastSentStep = last
    }

    session.attachStepsSink((version, steps, clientIDs) => {
      if (version <= appliedVersion) return // 已含于基座，防重复应用
      applyRemoteSteps(steps, clientIDs)
      appliedVersion = version
    })

    session.attachHandlers({
      // 暂停由 session.sendsHeld 表达（onTransaction 发送侧跳过非 leader）。
      onSyncBegin: () => {},
      onSyncEnd: (version) => {
        if (version > appliedVersion) appliedVersion = version
        flushSendable() // 恢复发送：立即补发暂停期间积压的 sendable
      },
      onSyncRequest: (target) => {
        // leader 冲账上报：先立即发送 sendable，再 80ms 轮询直至排空或 2s
        // 超时，随后上报实时快照（版本落后会被服务端判 stale 并重发
        // sync-request，届时再次进入本流程直至追平）。
        flushSendable()
        if (syncPollTimer !== null) window.clearInterval(syncPollTimer)
        const startedAt = Date.now()
        syncPollTimer = window.setInterval(() => {
          const sendable = sendableSteps(editor.state)
          if (!alive || !sendable || sendable.steps.length === 0 || Date.now() - startedAt >= 2000) {
            if (syncPollTimer !== null) {
              window.clearInterval(syncPollTimer)
              syncPollTimer = null
            }
            if (alive) session.sendSnapshot(editor.getJSON(), appliedVersion, target)
          }
        }, 80)
      },
    })

    /** 每次 transaction 后：发送未确认 steps（sync 暂停期间非 leader 跳过，
     * steps 留 sendable 待 sync-end 补发）+ presence 节流 300ms。 */
    const onTransaction = ({ transaction }: { transaction: Transaction }) => {
      if (session.currentStatus !== 'connected') return
      const held = session.sendsHeld && session.leaderConnId !== session.currentSelfConnId
      if (!held) flushSendable()
      if (transaction.selectionSet || transaction.docChanged) {
        const sel = editor.state.selection
        if (!lastPresence || sel.anchor !== lastPresence.anchor || sel.head !== lastPresence.head) {
          const fire = () => {
            presenceTimer = null
            if (!alive) return
            const s = editor.state.selection
            lastPresence = { anchor: s.anchor, head: s.head }
            lastPresenceSentAt = Date.now()
            session.sendPresence({ anchor: s.anchor, head: s.head })
          }
          if (Date.now() - lastPresenceSentAt >= 300) fire()
          else if (presenceTimer === null) presenceTimer = window.setTimeout(fire, 300)
        }
      }
    }
    editor.on('transaction', onTransaction)

    return () => {
      alive = false
      editor.off('transaction', onTransaction)
      if (presenceTimer !== null) window.clearTimeout(presenceTimer)
      if (syncPollTimer !== null) window.clearInterval(syncPollTimer)
    }
  }, [collab, readonly, fileId, editor, collabBase, sessionSeq, zh, collabClientID, setCollabMessage])

  // 右键/卡片菜单浮层：渲染后按实测尺寸收缩进视口（与文件页右键菜单同法）。
  const ctxMenuRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!ctxMenu) return
    const id = window.requestAnimationFrame(() => {
      if (ctxMenuRef.current) clampFixedMenu(ctxMenuRef.current, ctxMenu.x, ctxMenu.y)
    })
    return () => window.cancelAnimationFrame(id)
  }, [ctxMenu])
  const cardCtxRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    if (!cardCtx) return
    const id = window.requestAnimationFrame(() => {
      if (cardCtxRef.current) clampFixedMenu(cardCtxRef.current, cardCtx.x, cardCtx.y)
    })
    return () => window.cancelAnimationFrame(id)
  }, [cardCtx])

  /** 编辑器右键菜单项（与工具栏同能力，antd Menu：查看页不渲染右键）。 */
  const editorCtxItems = (): MenuProps['items'] => [
    { key: 'copy', icon: icon(Clipboard), label: zh ? '复制' : 'Copy' },
    { key: 'cut', icon: icon(Scissors), label: zh ? '剪切' : 'Cut' },
    { key: 'paste', label: zh ? '粘贴' : 'Paste' },
    { type: 'divider' },
    { key: 'bold', label: zh ? '加粗' : 'Bold' },
    { key: 'italic', label: zh ? '斜体' : 'Italic' },
    { key: 'highlight', label: zh ? '高亮' : 'Highlight' },
    { key: 'code', label: zh ? '行内代码' : 'Inline code' },
    { key: 'link', label: editor?.isActive('link') ? (zh ? '移除链接' : 'Remove link') : (zh ? '链接…' : 'Link…') },
    { type: 'divider' },
    { key: 'h1', label: zh ? '标题 1' : 'Heading 1' },
    { key: 'h2', label: zh ? '标题 2' : 'Heading 2' },
    { key: 'h3', label: zh ? '标题 3' : 'Heading 3' },
    { key: 'ul', label: zh ? '无序列表' : 'Bullet list' },
    { key: 'ol', label: zh ? '有序列表' : 'Ordered list' },
    { type: 'divider' },
    {
      key: 'insert',
      label: zh ? '插入' : 'Insert',
      children: [
        { key: 'ins:image', label: zh ? '图片（上传到 assets/）' : 'Image (upload to assets/)' },
        { key: 'ins:file', label: zh ? '文件（内联卡片）' : 'File (inline card)' },
        { key: 'ins:table', label: zh ? '表格（3×3）' : 'Table (3×3)' },
        { key: 'ins:hr', label: zh ? '分割线' : 'Divider' },
        { key: 'ins:codeblock', label: zh ? '代码块' : 'Code block' },
      ],
    },
    {
      key: 'embed',
      label: zh ? '嵌入' : 'Embed',
      children: [
        { key: 'emb:drawio', label: zh ? 'draw.io 图表' : 'draw.io diagram' },
        { key: 'emb:excalidraw', label: zh ? '白板（Excalidraw）' : 'Whiteboard (Excalidraw)' },
      ],
    },
  ]

  /** 右键菜单执行（键见 editorCtxItems；执行后收起并保持编辑器焦点）。 */
  const runCtxAction = (key: string) => {
    setCtxMenu(null)
    if (!editor) return
    switch (key) {
      case 'copy': {
        const { from, to } = editor.state.selection
        const text = editor.state.doc.textBetween(from, to, '\n')
        if (text) void navigator.clipboard?.writeText(text)
        break
      }
      case 'cut': {
        const { from, to } = editor.state.selection
        const text = editor.state.doc.textBetween(from, to, '\n')
        if (text) {
          void navigator.clipboard?.writeText(text)
          chain().deleteSelection().run()
        }
        break
      }
      case 'paste': {
        void navigator.clipboard?.readText().then((text) => {
          if (text) editor.chain().focus().insertContent(text).run()
        }).catch(() => setError(zh ? '浏览器未授予剪贴板读取权限，请使用 Ctrl+V' : 'Clipboard permission denied; use Ctrl+V'))
        break
      }
      case 'bold': chain().toggleBold().run(); break
      case 'italic': chain().toggleItalic().run(); break
      case 'highlight': chain().toggleHighlight().run(); break
      case 'code': chain().toggleCode().run(); break
      case 'link':
        if (editor.isActive('link')) chain().unsetLink().run()
        else onToolbarLink()
        break
      case 'h1': chain().toggleHeading({ level: 1 }).run(); break
      case 'h2': chain().toggleHeading({ level: 2 }).run(); break
      case 'h3': chain().toggleHeading({ level: 3 }).run(); break
      case 'ul': chain().toggleBulletList().run(); break
      case 'ol': chain().toggleOrderedList().run(); break
      case 'ins:image': setPicker({ filter: 'image' }); break
      case 'ins:file': setPicker({ filter: 'any' }); break
      case 'ins:table': chain().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run(); break
      case 'ins:hr': chain().setHorizontalRule().run(); break
      case 'ins:codeblock': chain().toggleCodeBlock().run(); break
      case 'emb:drawio': setPicker({ filter: 'drawio' }); break
      case 'emb:excalidraw': setPicker({ filter: 'excalidraw' }); break
    }
  }

  /** 卡片右键「编辑（新窗口）」：按扩展名分发编辑器路由。 */
  const openCardEditor = (target: { fileId: string; name: string }) => {
    const base = editorRouteFor(target.name, 'edit')
    const url = new URL(`${base}/${target.fileId}`, window.location.origin)
    url.searchParams.set('returnTo', `${window.location.pathname}${window.location.search}`)
    window.open(`${url.pathname}${url.search}`, '_blank', 'noopener')
  }

  const pickHandler = (request: PickerRequest, file: PickedFile) => {
    setPicker(null)
    if (request.replaceOf) {
      replaceEmbedByFileId(request.replaceOf, file)
      return
    }
    const filter = request.filter
    if (filter === 'image') {
      insertImageRef(file.id, file.name)
      return
    }
    if (filter === 'drawio' || filter === 'excalidraw') {
      insertEmbed(filter, file.id, file.name)
      return
    }
    // 其他文件 → 内联文件卡片。
    insertFileCard(file.id, file.name)
  }

  /** 替换嵌入块引用（kind/fileId/title 覆写，保留其余 attrs）。
   * 优先按发起替换时记录的节点位置（pos）精准替换——按 fileId 全文扫描
   * 「首个命中」在多个嵌入块引用同一文件时会替换错节点（偶发被替换 bug
   * 根因）；pos 失效（文档已变）时回退扫描。替换后断言 file-id 已生效，
   * 未生效（竞态）再补一次覆盖写。 */
  const replaceEmbedByFileId = (target: { fileId: string; kind: string; pos: number }, file: PickedFile) => {
    const editor = editorRef.current
    if (!editor) return
    let pos = -1
    if (target.pos >= 0) {
      const at = editor.state.doc.nodeAt(Math.min(target.pos, editor.state.doc.content.size - 1))
      if (at && at.type.name === 'docflowEmbed' && (at.attrs as DocflowEmbedAttrs).fileId === target.fileId) {
        pos = Math.min(target.pos, editor.state.doc.content.size - 1)
      }
    }
    if (pos < 0) {
      editor.state.doc.descendants((node, p) => {
        if (pos >= 0 || node.type.name !== 'docflowEmbed') return
        const a = node.attrs as DocflowEmbedAttrs
        if (a.fileId !== target.fileId) return
        pos = p
      })
    }
    if (pos < 0) return
    const prev = editor.state.doc.nodeAt(pos)?.attrs as DocflowEmbedAttrs | undefined
    if (!prev) return
    const finalPos = pos
    editor
      .chain()
      .focus()
      .command(({ tr }) => {
        tr.setNodeMarkup(finalPos, undefined, { ...prev, kind: target.kind, fileId: file.id, title: file.name })
        return true
      })
      .run()
    // 断言替换生效（attrs 已指向新 file-id）；未生效（NodeView 复用竞态等）
    // 时按新 file-id 再定位补写一次。
    const after = editor.state.doc.nodeAt(finalPos)?.attrs as DocflowEmbedAttrs | undefined
    if (after && after.fileId !== file.id) {
      editor.state.doc.descendants((node, p) => {
        if (node.type.name !== 'docflowEmbed') return
        const a = node.attrs as DocflowEmbedAttrs
        if (a.fileId === target.fileId) {
          editor
            .chain()
            .command(({ tr }) => {
              tr.setNodeMarkup(p, undefined, { ...a, kind: target.kind, fileId: file.id, title: file.name })
              return true
            })
            .run()
        }
      })
    }
  }

  // ---- 工具栏（antd Button + Tooltip，lucide 图标；激活态高亮） ----

  const chain = () => editor!.chain().focus()
  const toolbarBtn = (
    key: string,
    label: ReactNode,
    title: string,
    active: boolean,
    disabled: boolean,
    run: () => unknown,
  ) => (
    <Tooltip key={key} title={title}>
      <Button
        type="text"
        size="small"
        className={`rich-text-tbtn${active ? ' active' : ''}`}
        disabled={disabled}
        aria-label={title}
        onMouseDown={(e) => e.preventDefault()}
        onClick={() => run()}
      >
        {label}
      </Button>
    </Tooltip>
  )

  const icon = (I: typeof Bold) => <I size={14} strokeWidth={2} aria-hidden="true" />

  // 链接：已有链接 → 直接移除；无选区/有选区 → Popover 输入 URL（回车或确定应用）。
  const applyLink = () => {
    if (!editor) return
    const href = linkUrl.trim()
    setLinkOpen(false)
    if (!href) return
    if (editor.state.selection.empty) {
      chain().insertContent({ type: 'text', text: href, marks: [{ type: 'link', attrs: { href } }] }).run()
    } else {
      chain().extendMarkRange('link').setLink({ href }).run()
    }
  }

  const onToolbarLink = () => {
    if (!editor) return
    if (editor.isActive('link')) {
      chain().unsetLink().run()
      return
    }
    setLinkUrl((editor.getAttributes('link').href as string | undefined) || 'https://')
    setLinkOpen(true)
  }

  // ---- BubbleMenu（选中浮动工具条） ----
  // 文字选区：加粗/斜体/高亮/代码/链接；内联文件卡片节点选中：删除。
  // 图片块/嵌入块选中时的操作条（宽度/编辑/替换/删除）由各自 NodeView 浮出
  //（同屏避免双浮层，故对这两类节点不显示）。
  // 注意：BubbleMenu 必须常挂载、仅以 shouldShow 控制显隐——条件卸载时
  // tippy 已搬移其容器 DOM，React commitDeletion 会触发 removeChild
  // NotFoundError 使整棵编辑器树崩溃（实测）。
  const bubbleStateOf = (ed: Editor | null): 'text' | 'card' | null => {
    if (!ed || !ed.isEditable) return null
    const selection = ed.state.selection
    if (selection instanceof NodeSelection) {
      const name = selection.node.type.name
      if (name === 'docflowFileCard') return 'card'
      return null
    }
    if (!selection.empty && ed.view.hasFocus() && !ed.isActive('codeBlock')) return 'text'
    return null
  }
  const bubbleMode = bubbleStateOf(editor)

  const addComment = async () => {
    if (!editor || editor.state.selection.empty) return
    const text = await promptViaModal(antdModal, {
      title: zh ? '添加评论' : 'Add comment',
      label: zh ? '评论内容' : 'Comment',
      initialValue: '',
      okText: zh ? '添加' : 'Add',
      cancelText: zh ? '取消' : 'Cancel',
    })
    if (!text?.trim()) return
    const { from, to } = editor.state.selection
    const quote = editor.state.doc.textBetween(from, to, ' ').slice(0, 1000)
    if (!fileId || !versionId) { onCommentError?.(zh ? '文档版本尚未就绪' : 'Document version is not ready'); return }
    await commentAction(async () => {
      const created = await createDocumentComment(fileId, { version_id: versionId, anchor_from: from, anchor_to: to, quote, body: text.trim() })
      editor.chain().focus().setMark('comment', { id: created.id, text: created.body }).run()
    })
  }

  const resolveComment = (comment: DocumentComment) => commentAction(() => updateDocumentComment(comment.id, { status: comment.status === 'resolved' ? 'open' : 'resolved' }))
  const replyToComment = async (comment: DocumentComment) => {
    const body = await promptViaModal(antdModal, { title: zh ? '回复评论' : 'Reply', label: zh ? '回复内容' : 'Reply', initialValue: '', okText: zh ? '回复' : 'Reply', cancelText: zh ? '取消' : 'Cancel' })
    if (body?.trim()) await commentAction(() => replyDocumentComment(comment.id, body.trim()))
  }
  const removeComment = (comment: DocumentComment) => commentAction(() => deleteDocumentComment(comment.id))

  const moveTopBlock = (fromIndex: number, toIndex: number) => {
    if (!editor || fromIndex === toIndex || fromIndex < 0 || toIndex < 0) return
    const nodes = Array.from({ length: editor.state.doc.childCount }, (_, i) => editor.state.doc.child(i))
    if (!nodes[fromIndex] || !nodes[toIndex]) return
    const moved = nodes.splice(fromIndex, 1)[0]
    nodes.splice(toIndex, 0, moved)
    editor.view.dispatch(editor.state.tr.replaceWith(0, editor.state.doc.content.size, Fragment.from(nodes)))
  }

  const body = (
    <div className={`rich-text-wrap${readonly ? ' readonly' : ''}`}>
      {!readonly && editor && (
        <div className="rich-text-toolbar" contentEditable={false}>
          {collabStatus === null ? (
            <span className="rich-text-collab-state" title={zh ? '多人实时编辑尚未启用' : 'Realtime collaboration is not enabled'}>{zh ? '协作暂未启用' : 'Collaboration unavailable'}</span>
          ) : (
            <span
              className={`rich-text-collab-state on ${collabStatus}`}
              title={zh ? '多人实时协作（绿=在线，黄=连接中，灰=离线）' : 'Realtime collaboration (green=online, yellow=connecting, gray=offline)'}
            >
              <span className={`rich-text-collab-dot ${collabStatus}`} aria-hidden="true" />
              <span className="rich-text-collab-avatars">
                {collabParticipants.slice(0, 8).map((p) => (
                  <span
                    key={p.connId}
                    className="rich-text-collab-avatar"
                    style={{ background: p.color }}
                    title={`${p.name}${p.leader ? (zh ? '（主持）' : ' (host)') : ''}`}
                  >
                    {p.leader && <span className="rich-text-collab-star" aria-hidden="true">★</span>}
                    {avatarCharOf(p.name)}
                  </span>
                ))}
                {collabParticipants.length > 8 && (
                  <span className="rich-text-collab-avatar more" title={collabParticipants.slice(8).map((p) => p.name).join(', ')}>
                    +{collabParticipants.length - 8}
                  </span>
                )}
              </span>
              {collabStatus === 'connected' ? (zh ? '协作已启用' : 'Collaboration on')
                : collabStatus === 'connecting' ? (zh ? '协作连接中…' : 'Connecting…')
                : (zh ? '协作离线' : 'Offline')}
            </span>
          )}
          <div className="rich-text-toolbar-group">
            {toolbarBtn('undo', icon(Undo2), zh ? '撤销' : 'Undo', false, !editor.can().undo(), () => chain().undo().run())}
            {toolbarBtn('redo', icon(Redo2), zh ? '重做' : 'Redo', false, !editor.can().redo(), () => chain().redo().run())}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('paragraph', icon(Pilcrow), zh ? '正文' : 'Paragraph', editor.isActive('paragraph') && !editor.isActive('heading'), false, () => chain().setParagraph().run())}
            {toolbarBtn('h1', icon(Heading1), zh ? '标题 1' : 'Heading 1', editor.isActive('heading', { level: 1 }), false, () => chain().toggleHeading({ level: 1 }).run())}
            {toolbarBtn('h2', icon(Heading2), zh ? '标题 2' : 'Heading 2', editor.isActive('heading', { level: 2 }), false, () => chain().toggleHeading({ level: 2 }).run())}
            {toolbarBtn('h3', icon(Heading3), zh ? '标题 3' : 'Heading 3', editor.isActive('heading', { level: 3 }), false, () => chain().toggleHeading({ level: 3 }).run())}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('bold', icon(Bold), zh ? '加粗' : 'Bold', editor.isActive('bold'), false, () => chain().toggleBold().run())}
            {toolbarBtn('italic', icon(Italic), zh ? '斜体' : 'Italic', editor.isActive('italic'), false, () => chain().toggleItalic().run())}
            {toolbarBtn('underline', icon(UnderlineIcon), zh ? '下划线' : 'Underline', editor.isActive('underline'), false, () => chain().toggleUnderline().run())}
            {toolbarBtn('strike', icon(Strikethrough), zh ? '删除线' : 'Strikethrough', editor.isActive('strike'), false, () => chain().toggleStrike().run())}
            {toolbarBtn('highlight', icon(Highlighter), zh ? '高亮' : 'Highlight', editor.isActive('highlight'), false, () => chain().toggleHighlight().run())}
            {toolbarBtn('code', icon(Code), zh ? '行内代码' : 'Inline code', editor.isActive('code'), false, () => chain().toggleCode().run())}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('comment', icon(MessageSquare), zh ? '添加评论' : 'Add comment', false, editor.state.selection.empty, () => void addComment())}
            {toolbarBtn('ul', icon(List), zh ? '无序列表' : 'Bullet list', editor.isActive('bulletList'), false, () => chain().toggleBulletList().run())}
            {toolbarBtn('ol', icon(ListOrdered), zh ? '有序列表' : 'Ordered list', editor.isActive('orderedList'), false, () => chain().toggleOrderedList().run())}
            {toolbarBtn('task', icon(ListTodo), zh ? '任务列表' : 'Task list', editor.isActive('taskList'), false, () => chain().toggleTaskList().run())}
            {toolbarBtn('quote', icon(TextQuote), zh ? '引用' : 'Quote', editor.isActive('blockquote'), false, () => chain().toggleBlockquote().run())}
          </div>
          <Popover
            open={linkOpen}
            onOpenChange={setLinkOpen}
            trigger={[]}
            placement="bottom"
            content={
              <div style={{ display: 'flex', gap: 8, padding: 4 }} contentEditable={false}>
                <Input
                  size="small"
                  autoFocus
                  value={linkUrl}
                  onChange={(e) => setLinkUrl(e.target.value)}
                  placeholder="https://"
                  onPressEnter={applyLink}
                  style={{ width: 280 }}
                />
                <Button size="small" type="primary" onMouseDown={(e) => e.preventDefault()} onClick={applyLink}>
                  {zh ? '应用' : 'Apply'}
                </Button>
              </div>
            }
          >
            <Button
              type="text"
              size="small"
              className={`rich-text-tbtn${editor.isActive('link') ? ' active' : ''}`}
              onMouseDown={(e) => e.preventDefault()}
              onClick={onToolbarLink}
              title={editor.isActive('link') ? (zh ? '移除链接' : 'Remove link') : (zh ? '插入链接' : 'Insert link')}
            >
              {icon(Link2)}
            </Button>
          </Popover>
          {/* 插入下拉：文件/图片/链接/表格/分割线/代码块。 */}
          <div className="rich-text-toolbar-group">
            <Dropdown
              trigger={['click']}
              menu={{
                items: [
                  { key: 'image', icon: icon(ImageIcon), label: zh ? '图片（上传到 assets/）' : 'Image (upload to assets/)' },
                  { key: 'file', icon: icon(FileIcon), label: zh ? '文件（内联卡片）' : 'File (inline card)' },
                  { key: 'link', icon: icon(Link2), label: zh ? '链接' : 'Link' },
                  { type: 'divider' },
                  { key: 'table', icon: icon(TableIcon), label: zh ? '表格（3×3）' : 'Table (3×3)' },
                  { key: 'hr', icon: icon(Minus), label: zh ? '分割线' : 'Divider' },
                  { key: 'codeblock', icon: icon(Braces), label: zh ? '代码块' : 'Code block' },
                ],
                onClick: ({ key }) => {
                  if (key === 'image') setPicker({ filter: 'image' })
                  else if (key === 'file') setPicker({ filter: 'any' })
                  else if (key === 'link') onToolbarLink()
                  else if (key === 'table') chain().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run()
                  else if (key === 'hr') chain().setHorizontalRule().run()
                  else if (key === 'codeblock') chain().toggleCodeBlock().run()
                },
              }}
            >
              <Button type="text" size="small" className="rich-text-tbtn" onMouseDown={(e) => e.preventDefault()}>
                {zh ? '插入' : 'Insert'} <ChevronDown size={12} strokeWidth={2} aria-hidden="true" />
              </Button>
            </Dropdown>
          </div>
          {/* 嵌入下拉：draw.io 图表 / 白板（Excalidraw）。 */}
          <div className="rich-text-toolbar-group">
            <Dropdown
              trigger={['click']}
              menu={{
                items: [
                  { key: 'drawio', icon: icon(Network), label: zh ? 'draw.io 图表' : 'draw.io diagram' },
                  { key: 'excalidraw', icon: icon(PenLine), label: zh ? '白板（Excalidraw）' : 'Whiteboard (Excalidraw)' },
                ],
                onClick: ({ key }) => setPicker({ filter: key as PickerFilter }),
              }}
            >
              <Button type="text" size="small" className="rich-text-tbtn" onMouseDown={(e) => e.preventDefault()}>
                {zh ? '嵌入' : 'Embed'} <ChevronDown size={12} strokeWidth={2} aria-hidden="true" />
              </Button>
            </Dropdown>
          </div>
          {editor.isActive('table') && (
            <div className="rich-text-toolbar-group">
              {toolbarBtn('row-add', zh ? '行+' : 'Row+', zh ? '下方插入行' : 'Add row below', false, false, () => chain().addRowAfter().run())}
              {toolbarBtn('row-del', <><Trash2 size={14} strokeWidth={2} aria-hidden="true" />{zh ? '行' : ' row'}</>, zh ? '删除当前行' : 'Delete row', false, false, () => chain().deleteRow().run())}
              {toolbarBtn('col-add', zh ? '列+' : 'Col+', zh ? '右侧插入列' : 'Add column after', false, false, () => chain().addColumnAfter().run())}
              {toolbarBtn('col-del', <><Trash2 size={14} strokeWidth={2} aria-hidden="true" />{zh ? '列' : ' col'}</>, zh ? '删除当前列' : 'Delete column', false, false, () => chain().deleteColumn().run())}
              {toolbarBtn('table-del', zh ? '删表格' : 'Del table', zh ? '删除表格' : 'Delete table', false, false, () => chain().deleteTable().run())}
            </div>
          )}
        </div>
      )}
      {(status || error) && (
        <div className={`rich-text-status${error ? ' error' : ''}`} contentEditable={false}>
          {error || status}
          {error && (
            <Button type="text" size="small" onClick={() => setError('')} aria-label={zh ? '关闭' : 'Close'}>×</Button>
          )}
        </div>
      )}
      {/* 编辑器主体：编辑态接管 contextmenu 弹自有右键菜单（与文件页右键
          同风格 antd Menu）；查看页（readonly）不接管，保持浏览器原生。 */}
      {!readonly && outline.length > 0 && (
        <aside className="rich-text-outline" aria-label={zh ? '文档大纲' : 'Document outline'}>
          <strong>{zh ? '大纲' : 'Outline'}</strong>
          {outline.map((item, index) => (
            <button key={`${item.pos}-${index}`} className={`rich-text-outline-item level-${item.level}`} onClick={() => {
              editor?.chain().focus().setTextSelection(item.pos + 1).run()
              editor?.view.dom.querySelectorAll('h1,h2,h3,h4,h5,h6')[index]?.scrollIntoView({ behavior: 'smooth', block: 'center' })
            }}>{item.text || (zh ? '未命名标题' : 'Untitled')}</button>
          ))}
        </aside>
      )}
      {!readonly && comments.length > 0 && (
        <aside className="rich-text-comments" aria-label={zh ? '评论' : 'Comments'}>
          <strong>{zh ? '评论' : 'Comments'} ({comments.length})</strong>
          {comments.filter((c) => !c.parent_id).map((comment) => (
            <div className={`rich-text-comment-item${comment.status === 'resolved' ? ' resolved' : ''}`} key={comment.id}>
              <div className="rich-text-comment-quote">{comment.quote}</div>
              <div>{comment.body}</div>
              <div className="rich-text-comment-actions">
                <Button size="small" disabled={commentBusy} onClick={() => void resolveComment(comment)}>{comment.status === 'resolved' ? (zh ? '重开' : 'Reopen') : (zh ? '解决' : 'Resolve')}</Button>
                <Button size="small" disabled={commentBusy} onClick={() => void replyToComment(comment)}>{zh ? '回复' : 'Reply'}</Button>
                <Button size="small" danger disabled={commentBusy} onClick={() => void removeComment(comment)}>{zh ? '删除' : 'Delete'}</Button>
              </div>
              {comments.filter((reply) => reply.parent_id === comment.id).map((reply) => <div className="rich-text-comment-reply" key={reply.id}>{reply.body}</div>)}
            </div>
          ))}
        </aside>
      )}
      <div
        className="rich-text-editor-host"
        onDragStart={(e) => {
          if (readonly) return
          const target = (e.target as HTMLElement).closest('[data-top-block]') as HTMLElement | null
          if (target) e.dataTransfer.setData('text/plain', target.dataset.topBlock || '')
        }}
        onDragOver={(e) => { if (!readonly && (e.target as HTMLElement).closest('[data-top-block]')) e.preventDefault() }}
        onDrop={(e) => {
          if (readonly) return
          const target = (e.target as HTMLElement).closest('[data-top-block]') as HTMLElement | null
          if (!target) return
          const from = Number(e.dataTransfer.getData('text/plain'))
          const to = Number(target.dataset.topBlock)
          if (Number.isFinite(from) && Number.isFinite(to)) moveTopBlock(from, to)
        }}
        onContextMenu={(e) => {
          if (readonly) return
          e.preventDefault()
          setCtxMenu({ x: e.clientX, y: e.clientY })
        }}
      >
        {/* 协作 bootstrap 未完成（基座未落位）时挂起编辑器，渲染加载提示；
            WS 持续失败经 onFatal 落单机基座后照常渲染（状态区「协作离线」）。 */}
        {collab && !readonly && collabBase === null ? (
          <div className="text-editor-state" contentEditable={false}>
            {zh ? '协作连接中…' : 'Connecting to collaboration…'}
          </div>
        ) : (
          <EditorContent editor={editor} className="rich-text-editor-body" />
        )}
      </div>
      {/* 右键菜单浮层（antd Menu，与文件页 .ctx-antd-menu 同视觉）。经 Portal
          挂 body：直接渲染在 .rich-text-wrap 内会排在 BubbleMenu（tippy 已把
          容器 DOM 搬到 body）之后，React 插入新兄弟时 insertBefore 目标不在
          父节点内 → NotFoundError 整树崩溃（同 BubbleMenu 卸载崩溃同源）。 */}
      {ctxMenu && createPortal(
        <div
          ref={ctxMenuRef}
          className="ctx-menu"
          role="menu"
          style={{ left: `${ctxMenu.x}px`, top: `${ctxMenu.y}px` }}
          onClick={() => setCtxMenu(null)}
        >
          <Menu
            className="ctx-antd-menu"
            mode="vertical"
            selectable={false}
            items={editorCtxItems()}
            onClick={({ key }) => runCtxAction(key)}
          />
        </div>,
        document.body,
      )}
      {/* 文件卡片右键菜单：「编辑（新窗口）」（Portal 挂 body，同上）。 */}
      {cardCtx && createPortal(
        <div
          ref={cardCtxRef}
          className="ctx-menu"
          role="menu"
          style={{ left: `${cardCtx.x}px`, top: `${cardCtx.y}px` }}
          onClick={() => setCardCtx(null)}
        >
          <Menu
            className="ctx-antd-menu"
            mode="vertical"
            selectable={false}
            items={[
              { key: 'view', label: zh ? '查看' : 'View' },
              { key: 'edit', label: zh ? '编辑（新窗口）' : 'Edit in new window' },
            ]}
            onClick={({ key }) => {
              const target = cardCtx
              setCardCtx(null)
              if (!target) return
              if (key === 'edit') openCardEditor(target)
              else setCardView({ fileId: target.fileId, name: target.name })
            }}
          />
        </div>,
        document.body,
      )}
      {/* 常挂载：显隐交给 shouldShow（条件卸载会因 tippy 搬移 DOM 崩溃）。 */}
      {editor && !readonly && (
        <BubbleMenu
          editor={editor}
          tippyOptions={{ placement: 'top', offset: [0, 8] }}
          shouldShow={({ editor: ed }) => bubbleStateOf(ed) !== null}
          className="rich-text-bubble"
        >
          {bubbleMode === 'text' ? (
            <>
              {toolbarBtn('bb', icon(Bold), zh ? '加粗' : 'Bold', editor.isActive('bold'), false, () => chain().toggleBold().run())}
              {toolbarBtn('bi', icon(Italic), zh ? '斜体' : 'Italic', editor.isActive('italic'), false, () => chain().toggleItalic().run())}
              {toolbarBtn('bhl', icon(Highlighter), zh ? '高亮' : 'Highlight', editor.isActive('highlight'), false, () => chain().toggleHighlight().run())}
              {toolbarBtn('bcd', icon(Code), zh ? '代码' : 'Code', editor.isActive('code'), false, () => chain().toggleCode().run())}
              {toolbarBtn('blink', icon(Link2), editor.isActive('link') ? (zh ? '移除链接' : 'Remove link') : (zh ? '链接' : 'Link'), editor.isActive('link'), false, () => {
                if (editor.isActive('link')) chain().unsetLink().run()
                else onToolbarLink()
              })}
            </>
          ) : (
            <>
              <span className="rich-text-bubble-label" contentEditable={false}>{zh ? '文件卡片' : 'File card'}</span>
              {toolbarBtn('bcard-del', icon(Trash2), zh ? '删除卡片' : 'Delete card', false, false, () => chain().deleteSelection().run())}
            </>
          )}
        </BubbleMenu>
      )}
      {picker && (
        <FilePickerModal
          open
          filter={picker.filter}
          allowCreate={picker.filter === 'drawio' || picker.filter === 'excalidraw'}
          uploadParentId={mdParentId ?? null}
          onPick={(file) => pickHandler(picker, file)}
          onClose={() => setPicker(null)}
        />
      )}
      {/* 文件卡片查看弹窗（卡片点击事件上提；FileViewerDispatch 懒加载）。 */}
      {cardView && (
        <AntdModal
          open
          centered
          footer={null}
          width="min(1180px, 94vw)"
          title={<span className="share-entry-modal-title">查看「{cardView.name || cardView.fileId.slice(0, 8)}」</span>}
          styles={{ body: { overflow: 'auto', height: 'min(76vh, 760px)', maxHeight: 'min(76vh, 760px)' } }}
          classNames={{
            header: 'docflow-modal-header',
            title: 'docflow-modal-title',
            body: 'docflow-modal-body',
            close: 'docflow-modal-close',
          }}
          className="docflow-modal modal-viewer"
          onCancel={() => setCardView(null)}
        >
          <div className="preview-embed">
            <Suspense fallback={<div className="text-editor-state">{zh ? '正在加载查看器…' : 'Loading viewer…'}</div>}>
              <FileViewerDispatch
                fileId={cardView.fileId}
                name={cardView.name || cardView.fileId.slice(0, 8)}
                resolveRawUrl={async () => {
                  try {
                    const r = await resolveFileById(cardView.fileId)
                    return r.raw_url
                  } catch {
                    return null
                  }
                }}
              />
            </Suspense>
          </div>
          <div className="preview-foot">
            <Button size="small" onClick={() => window.open(`/view/${cardView.fileId}`, '_blank', 'noopener')}>
              {zh ? '新窗口查看' : 'View in new window'}
            </Button>
            <Button size="small" onClick={() => openCardEditor(cardView)}>
              {zh ? '新窗口编辑' : 'Edit in new window'}
            </Button>
          </div>
        </AntdModal>
      )}
    </div>
  )

  return publicBase ? <RichTextPublicProvider value={publicBase}>{body}</RichTextPublicProvider> : body
}
