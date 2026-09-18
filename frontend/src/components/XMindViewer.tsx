// XMind 思维导图纯前端离线查看（不依赖任何外部网络服务）：
// - fflate 解 zip 容器取 content.json（XMind Zen/2020+ 新格式），本地解析
//   sheet/topic 树并生成 Markdown 大纲（规则与后端 internal/xmind 一致：
//   每 sheet 一个 `# 标题`、根 topic 首层 `- ` 列表、children 逐层缩进、
//   notes 转缩进 `> ` 引用），交给已有 MarkmapDiagram 渲染（d3 缩放/平移）；
// - 老版 content.xml（XMind 8 及以前）与损坏内容给出明确错误提示，不再
//   停留在永久加载态；
// - >10MB 大文件提示下载后查看（解压 + 渲染开销大）；
// - 顶部「转为 Markdown」：POST convert-markdown 生成 .md 落同目录，
//   成功后新窗口打开 /markdown/:newId 编辑。
import { useEffect, useState } from 'react'
import { Button } from 'antd'
import { convertMarkdown, fetchFileArrayBuffer } from '../api'
import { useLocale } from '../i18n'
import MarkmapDiagram from './MarkmapDiagram'

interface XMindViewerProps {
  fileId: string
  title: string
}

/** 单次解析的 .xmind 文件大小上限（超过提示下载查看）。 */
const MAX_XMIND_BYTES = 10 * 1024 * 1024

/** content.json 的 topic 节点：标题、备注（plain.content 优先）与挂靠子主题。 */
interface XTopic {
  title?: string
  notes?: { plain?: { content?: string } | null; content?: string } | null
  children?: { attached?: XTopic[] } | null
}

/** content.json 数组元素：画布标题 + 根 topic。 */
interface XSheet {
  title?: string
  rootTopic?: XTopic
}

function notesText(t: XTopic): string {
  const n = t.notes
  if (!n) return ''
  return (n.plain?.content ?? n.content ?? '').trim()
}

/** topic 树 → Markdown 列表（与后端 internal/xmind.writeTopic 同规则）。 */
function writeTopic(lines: string[], t: XTopic, level: number): void {
  const indent = '  '.repeat(level)
  const title = (t.title ?? '').trim()
  const note = notesText(t)
  // 标题非空（或无备注）时输出列表项；空标题仅有备注时只输出引用块。
  if (title !== '' || note === '') lines.push(`${indent}- ${title}`)
  if (note !== '') {
    for (const line of note.split('\n')) lines.push(`${indent}  > ${line.trim()}`)
  }
  for (const child of t.children?.attached ?? []) writeTopic(lines, child, level + 1)
}

/** sheets → Markdown 大纲：每 sheet `# 标题` + 根 topic 列表（同后端 ToMarkdown）。 */
function sheetsToMarkdown(sheets: XSheet[]): string {
  const out: string[] = []
  sheets.forEach((sh, i) => {
    if (i > 0) out.push('')
    let title = (sh.title ?? '').trim()
    if (title === '') title = (sh.rootTopic?.title ?? '').trim()
    if (title === '') title = `Sheet ${i + 1}`
    out.push(`# ${title}`, '')
    const topicLines: string[] = []
    if (sh.rootTopic) writeTopic(topicLines, sh.rootTopic, 0)
    out.push(...topicLines)
  })
  return `${out.join('\n').replace(/\n+$/, '')}\n`
}

/** 解析 .xmind 字节流 → Markdown；失败抛出带明确文案的 Error。 */
async function xmindToMarkdown(buffer: ArrayBuffer): Promise<string> {
  const { unzipSync } = await import('fflate')
  let entries: Record<string, Uint8Array>
  try {
    entries = unzipSync(new Uint8Array(buffer))
  } catch {
    throw new Error('文件不是有效的 .xmind（zip 容器解析失败）')
  }
  const decoder = new TextDecoder()
  const content = entries['content.json']
  if (!content) {
    if (entries['content.xml']) {
      throw new Error('老版 XMind 格式（content.xml，XMind 8 及以前）暂不支持在线查看，请下载后用 XMind 打开，或点击「转为 Markdown」由服务端转换')
    }
    throw new Error('无效的 .xmind 文件（缺少 content.json）')
  }
  let sheets: XSheet[]
  try {
    sheets = JSON.parse(decoder.decode(content)) as XSheet[]
  } catch {
    throw new Error('content.json 解析失败（文件可能已损坏）')
  }
  if (!Array.isArray(sheets) || sheets.length === 0) {
    throw new Error('无效的 .xmind 文件（content.json 无画布数据）')
  }
  return sheetsToMarkdown(sheets)
}

export default function XMindViewer({ fileId, title }: XMindViewerProps) {
  const [markdown, setMarkdown] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [converting, setConverting] = useState(false)
  const [convertNotice, setConvertNotice] = useState('')
  const [convertError, setConvertError] = useState('')
  const locale = useLocale()
  const zh = locale === 'zh-CN'

  useEffect(() => {
    let alive = true
    const load = async () => {
      setError('')
      setLoading(true)
      try {
        const buffer = await fetchFileArrayBuffer(fileId)
        if (!alive) return
        if (buffer.byteLength > MAX_XMIND_BYTES) {
          throw new Error(zh
            ? `文件过大（${(buffer.byteLength / 1024 / 1024).toFixed(1)} MB，上限 10 MB），请下载后使用 XMind 客户端查看`
            : `File too large (${(buffer.byteLength / 1024 / 1024).toFixed(1)} MB, limit 10 MB). Please download and open it in XMind.`)
        }
        const md = await xmindToMarkdown(buffer)
        if (!alive) return
        setMarkdown(md)
      } catch (err) {
        if (alive) setError(err instanceof Error ? err.message : '思维导图加载失败')
      } finally {
        if (alive) setLoading(false)
      }
    }
    void load()
    return () => { alive = false }
  }, [fileId, zh])

  /** 转为 Markdown：产物落源文件所在目录（<源名>.md），成功后新窗口打开编辑页。 */
  const handleConvert = async () => {
    if (converting) return
    setConverting(true)
    setConvertNotice('')
    setConvertError('')
    try {
      const created = await convertMarkdown(fileId)
      setConvertNotice(zh ? `已生成「${created.name}」，正在打开编辑…` : `Created “${created.name}”, opening editor…`)
      window.open(`/markdown/${created.file_id}`, '_blank', 'noopener')
    } catch (err) {
      setConvertError(err instanceof Error ? err.message : (zh ? '转换失败' : 'Conversion failed'))
    } finally {
      setConverting(false)
    }
  }

  return (
    <div className="xmind-viewer" title={title}>
      <div className="xmind-toolbar">
        <Button size="small" disabled={converting} loading={converting} onClick={() => void handleConvert()}>
          {converting ? (zh ? '转换中…' : 'Converting…') : (zh ? '转为 Markdown' : 'Convert to Markdown')}
        </Button>
        {convertNotice && <span className="muted xmind-toolbar-note">{convertNotice}</span>}
        {convertError && <span className="error-text">{convertError}</span>}
      </div>
      {error ? (
        <div className="banner error xmind-error">{error}</div>
      ) : loading ? (
        <div className="viewer-loading-hint">{zh ? '正在加载思维导图…' : 'Loading mind map…'}</div>
      ) : (
        <div className="xmind-viewer-host"><MarkmapDiagram source={markdown} /></div>
      )}
    </div>
  )
}
