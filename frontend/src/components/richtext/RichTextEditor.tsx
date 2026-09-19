// DocFlow 富文本编辑器（Tiptap v2，.dfdoc 专属格式：Tiptap JSON 存储）：
// - 受控语义：initialJSON 仅装载期生效（父组件用 key 重挂载换内容），
//   用户编辑经 onChange(json) 全量回吐最新文档 JSON（JSON.stringify(getJSON())）；
// - readonly 态复用同一渲染（editable=false，隐藏工具栏/菜单）；
// - 嵌入块（drawio/excalidraw/office/web/file 引用文件节点）以 docflowEmbed
//   自定义节点原生存进 JSON（见 DocflowEmbed.ts）；slash 菜单插入，图片
//   另支持粘贴/拖拽上传到文档所在目录的 assets/ 子目录；
// - 历史 .md 内容不再进本编辑器（.md 已回归 Monaco 源码编辑）。
import { useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { EditorContent, useEditor } from '@tiptap/react'
import type { Editor } from '@tiptap/react'
import type { Transaction } from '@tiptap/pm/state'
import { App as AntdApp, Button, Input, Popover, Tooltip } from 'antd'
import {
  Bold,
  Braces,
  Code,
  Heading1,
  Heading2,
  Heading3,
  Highlighter,
  Image as ImageIcon,
  Italic,
  Link2,
  List,
  ListOrdered,
  ListTodo,
  Minus,
  Pilcrow,
  Redo2,
  Strikethrough,
  Table as TableIcon,
  TextQuote,
  Trash2,
  Underline as UnderlineIcon,
  Undo2,
} from 'lucide-react'
import StarterKit from '@tiptap/starter-kit'
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
import { getFileMeta, resolveNamespaceOf, uploadFile } from '../../api'
import { useLocale } from '../../i18n'
import { promptViaModal } from '../FileBrowser'
import DocflowEmbed from './DocflowEmbed'
import { EmbedKind } from './markdownRoundtrip'
import FilePickerModal, { PickerFilter, ensureAssetsFolder } from './FilePickerModal'
import type { PickedFile } from './FilePickerModal'
import { createSlashMenuExtension } from './SlashMenu'
import type { SlashMenuCallbacks } from './SlashMenu'

export interface RichTextEditorProps {
  /** 装载期使用的文档 JSON（Tiptap doc 序列化串；外部更新不自动同步，用 key 重挂载）。 */
  initialJSON: string
  /** 用户编辑后回调（最新文档 JSON 字符串）。 */
  onChange?: (json: string) => void
  readonly?: boolean
  /** dfdoc 文件自身 ID：图片上传时解析其所在目录（其下 assets/ 子目录）。 */
  fileId?: string
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

export default function RichTextEditor({
  initialJSON,
  onChange,
  readonly = false,
  fileId,
}: RichTextEditorProps) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const { modal: antdModal } = AntdApp.useApp()
  const editorRef = useRef<Editor | null>(null)
  const onChangeRef = useRef(onChange)
  onChangeRef.current = onChange

  const [picker, setPicker] = useState<PickerFilter | null>(null)
  const [mdParentId, setMdParentId] = useState<string | null | undefined>(undefined)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')
  // 工具栏链接 Popover：输入 URL 的受控态。
  const [linkOpen, setLinkOpen] = useState(false)
  const [linkUrl, setLinkUrl] = useState('https://')

  const callbacksRef = useRef<SlashMenuCallbacks>({
    onInsertEmbed: (filter) => setPicker(filter),
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

  /** 图片上传：assets 目录定位/创建（按文档所在目录探测个人/团队空间分发
   * 端点）→ 上传 → 插入 file 嵌入块。 */
  const uploadImages = async (files: File[]) => {
    const editor = editorRef.current
    if (!editor) return
    for (const file of files) {
      if (!IMAGE_MIME.test(file.type)) continue
      setStatus(zh ? `正在上传「${file.name}」…` : `Uploading "${file.name}"…`)
      setError('')
      try {
        const parentId = mdParentId ?? null
        const ns = parentId ? await resolveNamespaceOf(parentId) : null
        const assetsId = await ensureAssetsFolder(parentId, ns)
        if (!assetsId) throw new Error(zh ? '无法定位 assets 目录' : 'Cannot locate assets folder')
        const session = await uploadFile(file, assetsId, () => {})
        if (!session.file_id) throw new Error(zh ? '上传完成但未返回文件 ID' : 'Upload finished without file id')
        insertEmbed('file', session.file_id, file.name)
      } catch (err) {
        setError(err instanceof Error ? err.message : (zh ? '图片上传失败' : 'Image upload failed'))
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

  const lowlight = useMemo(() => createLowlight(common), [])
  const editor = useEditor({
    extensions: [
      StarterKit.configure({
        codeBlock: false,
        heading: { levels: [1, 2, 3] },
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
      createSlashMenuExtension(callbacksRef),
    ],
    content: parseDocJSON(initialJSON),
    editable: !readonly,
    editorProps: {
      attributes: { class: 'rich-text-content', spellcheck: 'false' },
      handlePaste: (_view, event) => {
        const images = Array.from(event.clipboardData?.files ?? []).filter((f) => IMAGE_MIME.test(f.type))
        if (images.length === 0) return false
        void uploadImages(images)
        return true
      },
      handleDrop: (view, event, _slice, moved) => {
        if (moved) return false
        const files = Array.from(event.dataTransfer?.files ?? [])
        const images = files.filter((f) => IMAGE_MIME.test(f.type))
        if (images.length === 0) return false
        event.preventDefault()
        const coords = view.posAtCoords({ left: event.clientX, top: event.clientY })
        if (coords) {
          editorRef.current?.chain().focus().setTextSelection(coords.pos).run()
        }
        void uploadImages(images)
        return true
      },
    },
  })
  editorRef.current = editor

  // 编辑 → 文档 JSON 回吐（仅文档真实变化）。
  useEffect(() => {
    if (!editor) return
    const handler = ({ transaction }: { transaction: Transaction }) => {
      if (!transaction.docChanged) return
      onChangeRef.current?.(JSON.stringify(editor.getJSON()))
    }
    editor.on('update', handler)
    return () => {
      editor.off('update', handler)
    }
  }, [editor])

  useEffect(() => {
    editor?.setEditable(!readonly)
  }, [editor, readonly])

  const pickHandler = (filter: PickerFilter, file: PickedFile) => {
    setPicker(null)
    const kind: EmbedKind = filter === 'drawio' ? 'drawio' : filter === 'excalidraw' ? 'excalidraw' : 'file'
    insertEmbed(kind, file.id, file.name)
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

  return (
    <div className={`rich-text-wrap${readonly ? ' readonly' : ''}`}>
      {!readonly && editor && (
        <div className="rich-text-toolbar" contentEditable={false}>
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
            {toolbarBtn('ul', icon(List), zh ? '无序列表' : 'Bullet list', editor.isActive('bulletList'), false, () => chain().toggleBulletList().run())}
            {toolbarBtn('ol', icon(ListOrdered), zh ? '有序列表' : 'Ordered list', editor.isActive('orderedList'), false, () => chain().toggleOrderedList().run())}
            {toolbarBtn('task', icon(ListTodo), zh ? '任务列表' : 'Task list', editor.isActive('taskList'), false, () => chain().toggleTaskList().run())}
            {toolbarBtn('quote', icon(TextQuote), zh ? '引用' : 'Quote', editor.isActive('blockquote'), false, () => chain().toggleBlockquote().run())}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('table', icon(TableIcon), zh ? '插入表格（3×3）' : 'Insert table (3×3)', false, false, () =>
              chain().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run(),
            )}
            {toolbarBtn('hr', icon(Minus), zh ? '分割线' : 'Divider', false, false, () => chain().setHorizontalRule().run())}
            {toolbarBtn('codeblock', icon(Braces), zh ? '代码块' : 'Code block', editor.isActive('codeBlock'), false, () => chain().toggleCodeBlock().run())}
          </div>
          <div className="rich-text-toolbar-group">
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
                title={editor.isActive('link') ? (zh ? '移除链接' : 'Remove link') : zh ? '插入链接' : 'Insert link'}
              >
                {icon(Link2)}
              </Button>
            </Popover>
            {toolbarBtn('image', icon(ImageIcon), zh ? '插入图片（上传到 assets/）' : 'Insert image (upload to assets/)', false, false, () => setPicker('image'))}
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
      <EditorContent editor={editor} className="rich-text-editor-body" />
      {picker && (
        <FilePickerModal
          open
          filter={picker}
          uploadParentId={mdParentId ?? null}
          onPick={(file) => pickHandler(picker, file)}
          onClose={() => setPicker(null)}
        />
      )}
    </div>
  )
}

