// DocFlow 富文本编辑器（Tiptap v2 + tiptap-markdown，Markdown 存储）：
// - 受控语义：initialMarkdown 仅装载期生效（父组件用 key 重挂载换内容），
//   用户编辑经 onChange(md) 全量回吐最新 markdown；
// - readonly 态复用同一渲染（editable=false，隐藏工具栏/菜单）；
// - 往返保障：装载后 serialize(parse(initial)) 与源文归一比较，不一致时
//   回调 onRoundtripFail 由父组件降级源码模式（内容保真优先）；
// - 嵌入块（drawio/excalidraw/office/web/file）以 fenced code block 存储
//   （见 markdownRoundtrip.ts 约定）；slash 菜单插入，图片另支持粘贴/拖拽
//   上传到 md 所在目录的 assets/ 子目录。
import { useEffect, useMemo, useRef, useState } from 'react'
import { EditorContent, useEditor } from '@tiptap/react'
import type { Editor } from '@tiptap/react'
import type { Transaction } from '@tiptap/pm/state'
import StarterKit from '@tiptap/starter-kit'
import Underline from '@tiptap/extension-underline'
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
import { Markdown } from 'tiptap-markdown'
import { getFileMeta, resolveNamespaceOf, uploadFile } from '../../api'
import { useLocale } from '../../i18n'
import DocflowEmbed from './DocflowEmbed'
import { EmbedKind, markdownRoundTripMatches } from './markdownRoundtrip'
import FilePickerModal, { PickerFilter, ensureAssetsFolder } from './FilePickerModal'
import type { PickedFile } from './FilePickerModal'
import { createSlashMenuExtension } from './SlashMenu'
import type { SlashMenuCallbacks } from './SlashMenu'

export interface RichTextEditorProps {
  /** 装载期使用的 markdown 内容（外部更新不会自动同步，用 key 重挂载）。 */
  initialMarkdown: string
  /** 用户编辑后回调（最新全文 markdown）。 */
  onChange?: (md: string) => void
  readonly?: boolean
  /** md 文件自身 ID：图片上传时解析其所在目录（其下 assets/ 子目录）。 */
  fileId?: string
  /** 初始往返校验失败（内容无法无损进富文本）时回调，父组件应降级源码模式。 */
  onRoundtripFail?: () => void
}

/** tiptap-markdown storage 的最小结构（getMarkdown 序列化出口）。 */
interface MarkdownStorage {
  getMarkdown: () => string
}

function storageOf(editor: Editor): MarkdownStorage | null {
  return (editor.storage as Record<string, unknown>).markdown as MarkdownStorage | null
}

const IMAGE_MIME = /^image\//

export default function RichTextEditor({
  initialMarkdown,
  onChange,
  readonly = false,
  fileId,
  onRoundtripFail,
}: RichTextEditorProps) {
  const locale = useLocale()
  const zh = locale === 'zh-CN'
  const editorRef = useRef<Editor | null>(null)
  const onChangeRef = useRef(onChange)
  const onRoundtripFailRef = useRef(onRoundtripFail)
  onChangeRef.current = onChange
  onRoundtripFailRef.current = onRoundtripFail

  const [picker, setPicker] = useState<PickerFilter | null>(null)
  const [mdParentId, setMdParentId] = useState<string | null | undefined>(undefined)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')

  const callbacksRef = useRef<SlashMenuCallbacks>({
    onInsertEmbed: (filter) => setPicker(filter),
    isZh: zh,
  })
  useEffect(() => {
    callbacksRef.current.isZh = zh
  }, [zh])

  // md 文件所在目录（图片上传目标 assets/ 的父目录）。
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

  /** 图片上传：assets 目录定位/创建（按 md 所在目录探测个人/团队空间分发
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
      Markdown.configure({
        html: false,
        linkify: false,
        breaks: false,
        // 粘贴纯文本按 markdown 解析（复制时剪贴板文本同为 markdown）。
        transformPastedText: true,
        transformCopiedText: true,
      }),
      createSlashMenuExtension(callbacksRef),
    ],
    content: initialMarkdown,
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

  // 编辑 → markdown 回吐（仅文档真实变化）。
  useEffect(() => {
    if (!editor) return
    const handler = ({ transaction }: { transaction: Transaction }) => {
      if (!transaction.docChanged) return
      const md = storageOf(editor)
      if (md) onChangeRef.current?.(md.getMarkdown())
    }
    editor.on('update', handler)
    return () => {
      editor.off('update', handler)
    }
  }, [editor])

  // 装载后往返校验：失败则通知父组件降级（本组件随后会被卸载）。
  useEffect(() => {
    if (!editor) return
    const md = storageOf(editor)
    if (!md) return
    if (!markdownRoundTripMatches(initialMarkdown, md.getMarkdown())) {
      onRoundtripFailRef.current?.()
    }
    // 仅装载期校验一次（initialMarkdown 变化由父组件 key 重挂载承担）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editor])

  useEffect(() => {
    editor?.setEditable(!readonly)
  }, [editor, readonly])

  const pickHandler = (filter: PickerFilter, file: PickedFile) => {
    setPicker(null)
    const kind: EmbedKind = filter === 'drawio' ? 'drawio' : filter === 'excalidraw' ? 'excalidraw' : 'file'
    insertEmbed(kind, file.id, file.name)
  }

  // ---- 工具栏 ----

  const chain = () => editor!.chain().focus()
  const toolbarBtn = (
    key: string,
    label: string,
    title: string,
    active: boolean,
    disabled: boolean,
    run: () => unknown,
  ) => (
    <button
      key={key}
      type="button"
      className={`btn small ghost${active ? ' active' : ''}`}
      title={title}
      disabled={disabled}
      onMouseDown={(e) => e.preventDefault()}
      onClick={() => run()}
    >
      {label}
    </button>
  )

  const onToolbarLink = () => {
    if (!editor) return
    if (editor.isActive('link')) {
      chain().unsetLink().run()
      return
    }
    const url = window.prompt(zh ? '链接地址（留空取消）' : 'Link URL (empty to cancel)', 'https://')
    const href = url?.trim()
    if (!href) return
    if (editor.state.selection.empty) {
      chain().insertContent({ type: 'text', text: href, marks: [{ type: 'link', attrs: { href } }] }).run()
    } else {
      chain().extendMarkRange('link').setLink({ href }).run()
    }
  }

  return (
    <div className={`rich-text-wrap${readonly ? ' readonly' : ''}`}>
      {!readonly && editor && (
        <div className="rich-text-toolbar" contentEditable={false}>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('undo', '↶', zh ? '撤销' : 'Undo', false, !editor.can().undo(), () => chain().undo().run())}
            {toolbarBtn('redo', '↷', zh ? '重做' : 'Redo', false, !editor.can().redo(), () => chain().redo().run())}
          </div>
          <div className="rich-text-toolbar-group">
            {([1, 2, 3] as const).map((level) =>
              toolbarBtn(
                `h${level}`,
                `H${level}`,
                zh ? `标题 ${level}` : `Heading ${level}`,
                editor.isActive('heading', { level }),
                false,
                () => chain().toggleHeading({ level }).run(),
              ),
            )}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('bold', 'B', zh ? '加粗' : 'Bold', editor.isActive('bold'), false, () => chain().toggleBold().run())}
            {toolbarBtn('italic', 'I', zh ? '斜体' : 'Italic', editor.isActive('italic'), false, () => chain().toggleItalic().run())}
            {toolbarBtn('underline', 'U', zh ? '下划线' : 'Underline', editor.isActive('underline'), false, () => chain().toggleUnderline().run())}
            {toolbarBtn('code', '</>', zh ? '行内代码' : 'Inline code', editor.isActive('code'), false, () => chain().toggleCode().run())}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('ul', '•', zh ? '无序列表' : 'Bullet list', editor.isActive('bulletList'), false, () => chain().toggleBulletList().run())}
            {toolbarBtn('ol', '1.', zh ? '有序列表' : 'Ordered list', editor.isActive('orderedList'), false, () => chain().toggleOrderedList().run())}
            {toolbarBtn('task', '☑', zh ? '任务列表' : 'Task list', editor.isActive('taskList'), false, () => chain().toggleTaskList().run())}
            {toolbarBtn('quote', '❝', zh ? '引用' : 'Quote', editor.isActive('blockquote'), false, () => chain().toggleBlockquote().run())}
          </div>
          <div className="rich-text-toolbar-group">
            {toolbarBtn('codeblock', '{}', zh ? '代码块' : 'Code block', editor.isActive('codeBlock'), false, () => chain().toggleCodeBlock().run())}
            {toolbarBtn('link', '🔗', zh ? '链接' : 'Link', editor.isActive('link'), false, onToolbarLink)}
            {toolbarBtn('table', '▦', zh ? '插入表格' : 'Insert table', false, false, () =>
              chain().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run(),
            )}
          </div>
          {editor.isActive('table') && (
            <div className="rich-text-toolbar-group">
              {toolbarBtn('row-add', zh ? '行+' : 'Row+', zh ? '下方插入行' : 'Add row below', false, false, () => chain().addRowAfter().run())}
              {toolbarBtn('row-del', zh ? '删行' : 'Del row', zh ? '删除当前行' : 'Delete row', false, false, () => chain().deleteRow().run())}
              {toolbarBtn('col-add', zh ? '列+' : 'Col+', zh ? '右侧插入列' : 'Add column after', false, false, () => chain().addColumnAfter().run())}
              {toolbarBtn('col-del', zh ? '删列' : 'Del col', zh ? '删除当前列' : 'Delete column', false, false, () => chain().deleteColumn().run())}
              {toolbarBtn('table-del', zh ? '删表格' : 'Del table', zh ? '删除表格' : 'Delete table', false, false, () => chain().deleteTable().run())}
            </div>
          )}
        </div>
      )}
      {(status || error) && (
        <div className={`rich-text-status${error ? ' error' : ''}`} contentEditable={false}>
          {error || status}
          {error && (
            <button type="button" className="btn ghost small" onClick={() => setError('')}>×</button>
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

