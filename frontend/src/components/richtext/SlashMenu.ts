// Slash 插入菜单：@tiptap/suggestion + 原生 DOM 浮层（无 tippy 依赖）。
// 触发：块内行首（或仅有空白前缀）输入 '/'；方向键/回车选择，Esc 关闭。
// drawio/白板/图片/文件引用项删除 '/' 后回调上层打开文件选择器，其余项
// 直接执行编辑器命令。
import { Extension } from '@tiptap/react'
import Suggestion from '@tiptap/suggestion'
import type { SuggestionKeyDownProps, SuggestionProps } from '@tiptap/suggestion'
import type { Editor, Range } from '@tiptap/react'
import type { PickerFilter } from './FilePickerModal'

/** 上层回调（经 ref 传递，避免编辑器重建；isZh 用于菜单文案即时语言）。 */
export interface SlashMenuCallbacks {
  onInsertEmbed: (filter: PickerFilter) => void
  isZh: boolean
}

export interface SlashMenuCallbacksRef {
  current: SlashMenuCallbacks
}

interface SlashItem {
  key: string
  icon: string
  zh: string
  en: string
  keywords: string
  command: (editor: Editor, range: Range) => void
}

function getItems(callbacksRef: SlashMenuCallbacksRef): SlashItem[] {
  const embed = (filter: PickerFilter) => (editor: Editor, range: Range) => {
    editor.chain().focus().deleteRange(range).run()
    callbacksRef.current.onInsertEmbed(filter)
  }
  const run = (fn: (chain: ReturnType<Editor['chain']>) => unknown) => (editor: Editor, range: Range) => {
    editor.chain().focus().deleteRange(range).run()
    fn(editor.chain())
  }
  return [
    { key: 'h1', icon: 'H₁', zh: '标题 1', en: 'Heading 1', keywords: 'h1 heading title', command: run((c) => c.setNode('heading', { level: 1 }).run()) },
    { key: 'h2', icon: 'H₂', zh: '标题 2', en: 'Heading 2', keywords: 'h2 heading title', command: run((c) => c.setNode('heading', { level: 2 }).run()) },
    { key: 'h3', icon: 'H₃', zh: '标题 3', en: 'Heading 3', keywords: 'h3 heading title', command: run((c) => c.setNode('heading', { level: 3 }).run()) },
    { key: 'bullet', icon: '•', zh: '无序列表', en: 'Bullet list', keywords: 'ul list 列表', command: run((c) => c.toggleBulletList().run()) },
    { key: 'ordered', icon: '1.', zh: '有序列表', en: 'Ordered list', keywords: 'ol list 编号', command: run((c) => c.toggleOrderedList().run()) },
    { key: 'task', icon: '☑', zh: '任务列表', en: 'Task list', keywords: 'task todo 任务 待办', command: run((c) => c.toggleTaskList().run()) },
    { key: 'table', icon: '▦', zh: '表格', en: 'Table', keywords: 'table 表格', command: run((c) => c.insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run()) },
    { key: 'code', icon: '{}', zh: '代码块', en: 'Code block', keywords: 'code 代码', command: run((c) => c.toggleCodeBlock().run()) },
    { key: 'quote', icon: '❝', zh: '引用', en: 'Quote', keywords: 'quote blockquote 引用', command: run((c) => c.toggleBlockquote().run()) },
    { key: 'hr', icon: '—', zh: '分割线', en: 'Divider', keywords: 'hr divider 分割线', command: run((c) => c.setHorizontalRule().run()) },
    { key: 'link', icon: '🔗', zh: '链接', en: 'Link', keywords: 'link url 链接', command: (editor, range) => {
      editor.chain().focus().deleteRange(range).run()
      const isZh = callbacksRef.current.isZh
      const url = window.prompt(isZh ? '链接地址' : 'Link URL', 'https://')
      const href = url?.trim()
      if (!href) return
      if (editor.state.selection.empty) {
        editor.chain().focus().insertContent({ type: 'text', text: href, marks: [{ type: 'link', attrs: { href } }] }).run()
      } else {
        editor.chain().focus().extendMarkRange('link').setLink({ href }).run()
      }
    } },
    { key: 'image', icon: '🖼', zh: '图片', en: 'Image', keywords: 'image picture 图片 上传', command: embed('image') },
    { key: 'drawio', icon: '▦', zh: 'draw.io 图表', en: 'draw.io diagram', keywords: 'drawio diagram 图表 流程图', command: embed('drawio') },
    { key: 'excalidraw', icon: '✎', zh: '白板', en: 'Whiteboard', keywords: 'excalidraw whiteboard 白板', command: embed('excalidraw') },
    { key: 'file', icon: '📄', zh: '文件引用', en: 'File reference', keywords: 'file 文件 引用 embed', command: embed('any') },
  ]
}

function filterItems(items: SlashItem[], query: string): SlashItem[] {
  const q = query.toLowerCase()
  if (!q) return items
  return items.filter((it) => it.zh.includes(q) || it.en.toLowerCase().includes(q) || it.keywords.includes(q))
}

/** 原生 DOM 浮层（body 直挂，clientRect 定位）。 */
function createMenuRenderer(callbacksRef: SlashMenuCallbacksRef) {
  let element: HTMLDivElement | null = null
  let items: SlashItem[] = []
  let selectedIndex = 0
  // 最新 suggestion props（command 回调）；键盘事件回调不含 editor，经此转发。
  let currentProps: SuggestionProps<SlashItem> | null = null

  const ensureElement = (): HTMLDivElement => {
    if (element) return element
    element = document.createElement('div')
    element.className = 'rich-text-slash-menu'
    return element
  }

  const renderItems = () => {
    const menu = ensureElement()
    const isZh = callbacksRef.current.isZh
    menu.innerHTML = ''
    if (items.length === 0) {
      const empty = document.createElement('div')
      empty.className = 'rich-text-slash-empty'
      empty.textContent = isZh ? '没有匹配项' : 'No results'
      menu.appendChild(empty)
      return
    }
    items.forEach((item, index) => {
      const row = document.createElement('button')
      row.type = 'button'
      row.className = `rich-text-slash-item${index === selectedIndex ? ' is-selected' : ''}`
      const icon = document.createElement('span')
      icon.className = 'rich-text-slash-icon'
      icon.textContent = item.icon
      const label = document.createElement('span')
      label.textContent = isZh ? item.zh : item.en
      row.appendChild(icon)
      row.appendChild(label)
      // mousedown 先于失焦，避免编辑器先丢选区。
      row.addEventListener('mousedown', (e) => {
        e.preventDefault()
        selectedIndex = index
        currentProps?.command(item)
        close()
      })
      menu.appendChild(row)
    })
  }

  const position = (props: SuggestionProps<SlashItem>) => {
    const menu = ensureElement()
    const rect = props.clientRect?.()
    if (!rect) {
      menu.style.display = 'none'
      return
    }
    menu.style.display = 'block'
    // 视口内翻转：下方空间不足时改为向上展开。
    const below = window.innerHeight - rect.bottom
    const openUp = below < 260 && rect.top > 260
    menu.style.top = openUp ? '' : `${rect.bottom + 6}px`
    menu.style.bottom = openUp ? `${window.innerHeight - rect.top + 6}px` : ''
    menu.style.left = `${Math.max(8, Math.min(rect.left, window.innerWidth - 280))}px`
  }

  const close = () => {
    if (element) {
      element.remove()
      element = null
    }
    items = []
    selectedIndex = 0
  }

  return {
    onStart(props: SuggestionProps<SlashItem>) {
      currentProps = props
      items = filterItems(getItems(callbacksRef), props.query)
      selectedIndex = 0
      renderItems()
      position(props)
      document.body.appendChild(ensureElement())
    },
    onUpdate(props: SuggestionProps<SlashItem>) {
      currentProps = props
      items = filterItems(getItems(callbacksRef), props.query)
      selectedIndex = Math.min(selectedIndex, Math.max(0, items.length - 1))
      renderItems()
      position(props)
    },
    onKeyDown(props: SuggestionKeyDownProps) {
      if (props.event.key === 'ArrowUp') {
        props.event.preventDefault()
        if (items.length > 0) selectedIndex = (selectedIndex - 1 + items.length) % items.length
        renderItems()
        return true
      }
      if (props.event.key === 'ArrowDown') {
        props.event.preventDefault()
        if (items.length > 0) selectedIndex = (selectedIndex + 1) % items.length
        renderItems()
        return true
      }
      if (props.event.key === 'Enter' || props.event.key === 'Tab') {
        props.event.preventDefault()
        const item = items[selectedIndex]
        if (item) {
          currentProps?.command(item)
          close()
        }
        return true
      }
      return false
    },
    onExit() {
      close()
    },
  }
}

/** slash 菜单扩展（命令绑定 callbacksRef，语言/回调实时读取）。 */
export function createSlashMenuExtension(callbacksRef: SlashMenuCallbacksRef) {
  return Extension.create({
    name: 'docflowSlashMenu',
    addProseMirrorPlugins() {
      return [
        Suggestion<SlashItem>({
          editor: this.editor,
          char: '/',
          startOfLine: false,
          // 仅在 '/' 前只有空白时触发，避免 URL/普通文本中间的 '/'。
          allow: ({ state, range }) => {
            const $from = state.doc.resolve(range.from)
            const before = $from.parent.textBetween(0, $from.parentOffset, undefined, '\uFFFC')
            return before.replace(/^[ \t]*/, '').length === 0
          },
          command: ({ editor, range, props }) => {
            props.command(editor, range)
          },
          items: ({ query }) => filterItems(getItems(callbacksRef), query),
          render: () => createMenuRenderer(callbacksRef),
        }),
      ]
    },
  })
}
