// AI 对话共享 Markdown 渲染组件（Cherry Studio 同款技术栈）：
// - react-markdown + remark-gfm（表格/任务列表/删除线）+ remark-math +
//   rehype-katex（$…$ / $$…$$ 公式）+ rehype-highlight（代码高亮）；
// - 代码块自定义渲染：头部条 = 语言标签（无 language- 前缀显示「文本」）+
//   复制按钮（Copy/Check 切换）+「保存为文件」（Save 图标）；
// - 长代码块默认折叠（超 12 行或 2KB，头部「展开 N 行」切换，复制/保存
//   按钮保留）——AI 回复内嵌的大段文件内容/生成 payload（白板 JSON、
//   drawio XML 等）不再整屏裸输出；流式期间不折叠（避免布局跳动）；
// - 「保存为文件」：Modal 内 空间 Select（listSpaces）+ 目录懒加载树
//   （listSpaceFiles，根目录 + 逐级展开）+ 文件名 Input（按语言推导默认值，
//   可改）→ uploadFile(new File([code], name), folderId) 落盘，成功
//   message「已保存：<空间/目录/文件名>」+「新窗口打开」链接（/view/{id}）；
// - AIAssistant / AIEditChat / StudioPage 对话气泡共用（assistant 消息；
//   错误分支保持各自原样式）；memo 降低流式期间重渲染开销。
import { createContext, memo, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { App as AntdApp, Button, Input, Select, Tooltip, Tree } from 'antd'
import type { DataNode } from 'antd/es/tree'
import { Check, ChevronDown, ChevronRight, Copy, Save } from 'lucide-react'
import ReactMarkdown from 'react-markdown'
import type { Components } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import remarkMath from 'remark-math'
import rehypeKatex from 'rehype-katex'
import rehypeHighlight from 'rehype-highlight'
import 'katex/dist/katex.min.css'
import { listSpaceFiles, listSpaces, uploadFile } from '../api'
import type { Space } from '../api'
import { Modal } from './FileBrowser'

/** 语言 → 默认文件名（「保存为文件」初始值，可改；其余 → snippet.txt）。 */
const LANG_DEFAULT_FILENAME: Record<string, string> = {
  js: 'index.js', javascript: 'index.js',
  html: 'index.html', css: 'style.css',
  json: 'data.json',
  python: 'script.py', py: 'script.py',
  sql: 'query.sql',
  markdown: 'document.md', md: 'document.md',
}

/** 代码块语言显示名（空 = 「文本」）。 */
function langLabel(lang: string, zh: boolean): string {
  return lang || (zh ? '文本' : 'text')
}

/** 递归提取 React 子树的纯文本（复制/保存用；rehype-highlight 后为 span 树）。 */
function extractText(node: ReactNode): string {
  if (node == null || typeof node === 'boolean') return ''
  if (typeof node === 'string' || typeof node === 'number') return String(node)
  if (Array.isArray(node)) return node.map(extractText).join('')
  if (typeof node === 'object' && 'props' in node) {
    return extractText((node as React.ReactElement<{ children?: ReactNode }>).props?.children)
  }
  return ''
}

/** 目录树合成根节点 key（= 空间根目录，parentId null）。 */
const TREE_ROOT_KEY = '__root__'

/** 在树中按 key 追加子节点（懒加载结果合并；无匹配原样返回）。 */
function attachChildren(nodes: DataNode[], key: React.Key, children: DataNode[]): DataNode[] {
  return nodes.map((n) => (n.key === key
    ? { ...n, children }
    : n.children
      ? { ...n, children: attachChildren(n.children, key, children) }
      : n))
}

/** 在树中查 key 的路径（根 → 节点的 title 链；未命中返回 null）。 */
function findTitlePath(nodes: DataNode[], key: React.Key): string[] | null {
  for (const n of nodes) {
    if (n.key === key) return [String(n.title ?? '')]
    if (n.children) {
      const sub = findTitlePath(n.children, key)
      if (sub) return [String(n.title ?? ''), ...sub]
    }
  }
  return null
}

/**
 * 「保存为文件」弹窗：空间 Select + 目录懒加载树（antd Tree，根目录合成
 * 节点）+ 文件名 Input；确认经 uploadFile 落盘到所选目录（根目录 =
 * parentId null）。成功 message 带「新窗口打开」链接后关闭。
 */
function SaveCodeModal({ code, lang, zh, onClose }: { code: string; lang: string; zh: boolean; onClose: () => void }) {
  const { message } = AntdApp.useApp()
  const [spaces, setSpaces] = useState<Space[]>([])
  const [spaceId, setSpaceId] = useState('')
  const [treeData, setTreeData] = useState<DataNode[]>([])
  const [folderKey, setFolderKey] = useState<string>(TREE_ROOT_KEY)
  const [name, setName] = useState(LANG_DEFAULT_FILENAME[lang] ?? 'snippet.txt')
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState('')

  // 打开时拉空间列表：默认空间优先，其次第一个；重置目录选择。
  useEffect(() => {
    let alive = true
    listSpaces()
      .then((list) => {
        if (!alive) return
        setSpaces(list)
        const def = list.find((s) => s.is_default) ?? list[0]
        if (def) setSpaceId(def.id)
      })
      .catch((e) => {
        if (alive) setErr(e instanceof Error ? e.message : (zh ? '空间列表加载失败' : 'Failed to load spaces'))
      })
    return () => {
      alive = false
    }
  }, [zh])

  // 空间就绪/切换：重置树（仅合成根节点，展开时懒加载一级目录）。
  useEffect(() => {
    if (!spaceId) return
    setFolderKey(TREE_ROOT_KEY)
    setTreeData([{ key: TREE_ROOT_KEY, title: zh ? '根目录' : 'Root folder' }])
  }, [spaceId, zh])

  /** 懒加载某节点的子目录（仅目录；文件不可作为保存位置）。 */
  const loadChildren = async (key: React.Key): Promise<void> => {
    if (!spaceId) return
    try {
      const parentId = key === TREE_ROOT_KEY ? null : String(key)
      const { files } = await listSpaceFiles(spaceId, parentId)
      const children: DataNode[] = files
        .filter((f) => f.type === 'folder')
        .map((f) => ({ key: f.id, title: f.name }))
      setTreeData((prev) => attachChildren(prev, key, children.length > 0 ? children : [{ key: `${String(key)}:empty`, title: zh ? '（空）' : '(empty)', disabled: true, isLeaf: true }]))
    } catch (e) {
      setErr(e instanceof Error ? e.message : (zh ? '目录加载失败' : 'Failed to load folders'))
    }
  }

  /** 确认保存：uploadFile(new File([code])) 到所选目录；成功 message + 关闭。 */
  const save = async () => {
    const fname = name.trim()
    if (!fname || saving || !spaceId) return
    setSaving(true)
    setErr('')
    try {
      const session = await uploadFile(new File([code], fname, { type: 'text/plain' }), folderKey === TREE_ROOT_KEY ? null : folderKey, () => {})
      const space = spaces.find((s) => s.id === spaceId)
      const dirPath = folderKey === TREE_ROOT_KEY ? [] : (findTitlePath(treeData, folderKey) ?? [])
      const path = [space?.name ?? '', ...dirPath].filter(Boolean).join('/')
      void message.success(
        <span>
          {zh ? `已保存：${path}/${fname} ` : `Saved: ${path}/${fname} `}
          {session.file_id && (
            <a href={`/view/${session.file_id}`} target="_blank" rel="noopener noreferrer">
              {zh ? '新窗口打开' : 'Open'}
            </a>
          )}
        </span>,
      )
      onClose()
    } catch (e) {
      setErr(e instanceof Error ? e.message : (zh ? '保存失败' : 'Failed to save'))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal title={zh ? '保存为文件' : 'Save as file'} onClose={onClose}>
      <div className="ai-md-save-form">
        <label className="ai-md-save-row">
          <span className="ai-md-save-label">{zh ? '空间' : 'Space'}</span>
          <Select
            size="small"
            value={spaceId || undefined}
            placeholder={zh ? '选择空间' : 'Select a space'}
            onChange={setSpaceId}
            options={spaces.map((s) => ({ value: s.id, label: s.name }))}
            style={{ flex: 1 }}
          />
        </label>
        <label className="ai-md-save-row">
          <span className="ai-md-save-label">{zh ? '目录' : 'Folder'}</span>
          <Tree
            className="ai-md-save-tree"
            treeData={treeData}
            defaultExpandedKeys={[TREE_ROOT_KEY]}
            selectedKeys={[folderKey]}
            loadData={(node) => loadChildren(node.key)}
            onSelect={(keys) => {
              const k = keys[0]
              if (k !== undefined && k !== null) setFolderKey(String(k))
            }}
          />
        </label>
        <label className="ai-md-save-row">
          <span className="ai-md-save-label">{zh ? '文件名' : 'File name'}</span>
          <Input
            size="small"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={LANG_DEFAULT_FILENAME[lang] ?? 'snippet.txt'}
            style={{ flex: 1 }}
          />
        </label>
        {err && <div className="ai-md-save-error error-text">{err}</div>}
        <div className="modal-actions">
          <Button type="primary" loading={saving} disabled={!spaceId || !name.trim()} onClick={() => void save()}>
            {zh ? '保存' : 'Save'}
          </Button>
          <Button onClick={onClose}>{zh ? '取消' : 'Cancel'}</Button>
        </div>
      </div>
    </Modal>
  )
}

/** 长代码块折叠阈值：行数或字节数任一超限即默认折叠。 */
const COLLAPSE_LINES = 12
const COLLAPSE_CHARS = 2048

/**
 * 单个代码块：头部条（语言标签 + 复制 + 保存为文件）+ 原始 <pre> 内容
 * （rehype-highlight 的 span 树原样渲染，仅外层包壳）。
 * 长代码块（超阈值且非流式）默认折叠：头部显示「展开 N 行」切换按钮，
 * 复制/保存按钮始终可用；展开后可再收起。
 */
function AICodeBlock({ children, zh, streaming }: { children?: ReactNode; zh: boolean; streaming?: boolean }) {
  const [copied, setCopied] = useState(false)
  const [saveOpen, setSaveOpen] = useState(false)
  const [expanded, setExpanded] = useState(false)
  // children = <code class="hljs language-x">…</code>（或数组取首个）。
  const el = (Array.isArray(children) ? children[0] : children) as React.ReactElement<{ className?: unknown; children?: ReactNode }> | undefined
  const cls = typeof el?.props?.className === 'string' ? el.props.className : ''
  const lang = /language-([\w+-]+)/.exec(cls)?.[1] ?? ''
  const raw = extractText(el?.props?.children)
  const lineCount = raw ? raw.replace(/\n+$/, '').split('\n').length : 0
  const collapsible = !streaming && (lineCount > COLLAPSE_LINES || raw.length > COLLAPSE_CHARS)
  const showCode = !collapsible || expanded

  const copy = () => {
    void navigator.clipboard.writeText(raw)
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1200)
  }

  return (
    <div className="ai-md-code">
      <div className="ai-md-code-head">
        <span className="ai-md-code-lang">{langLabel(lang, zh)}</span>
        {collapsible && (
          <Button
            size="small"
            type="text"
            className="ai-md-code-expand"
            onClick={() => setExpanded((v) => !v)}
            aria-label={expanded ? (zh ? '收起代码' : 'Collapse code') : (zh ? `展开 ${lineCount} 行代码` : `Expand ${lineCount} lines`)}
          >
            {expanded ? <ChevronDown size={12} strokeWidth={2} aria-hidden="true" /> : <ChevronRight size={12} strokeWidth={2} aria-hidden="true" />}
            <span className="muted">{expanded ? (zh ? '收起' : 'Collapse') : (zh ? `展开 ${lineCount} 行` : `Expand ${lineCount} lines`)}</span>
          </Button>
        )}
        <span className="ai-md-code-actions">
          <Tooltip title={copied ? (zh ? '已复制' : 'Copied') : (zh ? '复制代码' : 'Copy code')}>
            <Button size="small" type="text" className="ai-md-code-btn" aria-label={zh ? '复制代码' : 'Copy code'} onClick={copy}>
              {copied ? <Check size={12} strokeWidth={2} aria-hidden="true" /> : <Copy size={12} strokeWidth={2} aria-hidden="true" />}
            </Button>
          </Tooltip>
          <Tooltip title={zh ? '保存为文件（可选空间与目录）' : 'Save as file (pick a space and folder)'}>
            <Button size="small" type="text" className="ai-md-code-btn" aria-label={zh ? '保存为文件' : 'Save as file'} onClick={() => setSaveOpen(true)}>
              <Save size={12} strokeWidth={2} aria-hidden="true" />
            </Button>
          </Tooltip>
        </span>
      </div>
      {showCode ? (
        <pre>{children}</pre>
      ) : (
        <button type="button" className="ai-md-code-folded muted" onClick={() => setExpanded(true)}>
          {zh ? `已折叠 ${lineCount} 行代码，点击展开` : `${lineCount} lines folded — click to expand`}
        </button>
      )}
      {saveOpen && <SaveCodeModal code={raw} lang={lang} zh={zh} onClose={() => setSaveOpen(false)} />}
    </div>
  )
}

/** 代码块渲染上下文（Components 表在模块级，zh/streaming 经 Context 注入子树）。 */
const AILangContext = createContext({ zh: true, streaming: false })

/** react-markdown 自定义渲染表（模块级常量，流式期间引用稳定）。 */
const mdComponents: Components = {
  pre: ({ children }) => (
    <AILangContext.Consumer>
      {(ctx) => <AICodeBlock zh={ctx.zh} streaming={ctx.streaming}>{children}</AICodeBlock>}
    </AILangContext.Consumer>
  ),
}

/**
 * AI 对话 Markdown 渲染（共享组件；AIAssistant / AIEditChat / StudioPage 气泡
 * 使用）。streaming 时同样渲染（调用方已有的行内光标负责动画；流式期间
 * 代码块不折叠，避免边生成边展开/收起跳动）。
 */
function AIMarkdown({ text, zh, streaming, className = '' }: { text: string; zh?: boolean; streaming?: boolean; className?: string }) {
  return (
    <div className={`markdown-preview ai-markdown ai-md${streaming ? ' ai-md-streaming' : ''}${className ? ` ${className}` : ''}`}>
      <AILangContext.Provider value={{ zh: zh ?? true, streaming: !!streaming }}>
        <ReactMarkdown
          remarkPlugins={[remarkGfm, remarkMath]}
          rehypePlugins={[rehypeKatex, [rehypeHighlight, { detect: false }]]}
          components={mdComponents}
        >
          {text}
        </ReactMarkdown>
      </AILangContext.Provider>
    </div>
  )
}

export default memo(AIMarkdown)
