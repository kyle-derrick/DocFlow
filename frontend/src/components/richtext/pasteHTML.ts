// 粘贴富文本（Word/网页）的清理与图片重上传：
// - sanitizePastedHTML：DOMParser 解析后按标签白名单/属性白名单清洗——
//   去掉脚本/样式/元信息/Office 私有标签（含命名空间的 o:p、w:* 等）与
//   style/class/id 等表现属性，保留结构（标题/段落/列表/表格/链接/图片，
//   结构标签由 Tiptap schema 再过滤一层）；
// - rewriteRemoteImages：对 <img> 的 http(s)/data: 来源逐个尝试抓取并经
//   uploader 上传，成功后改写为 docflowImage 节点标记（div[data-docflow-image]，
//   由 DocflowImage.parseHTML 消费）；CORS 抓取失败的远程图保留原 URL，
//   file:// 等本地引用（Word 本地路径）移除。

/** 允许保留的结构标签（其余整棵移除；不在名单内的容器标签做「去壳保留子节点」）。 */
const KEEP_TAGS = new Set([
  'P', 'BR', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6',
  'UL', 'OL', 'LI', 'BLOCKQUOTE', 'PRE', 'HR',
  'TABLE', 'THEAD', 'TBODY', 'TFOOT', 'TR', 'TD', 'TH', 'CAPTION',
  'A', 'IMG', 'CODE', 'STRONG', 'B', 'EM', 'I', 'U', 'S', 'DEL', 'MARK', 'SPAN',
  'DIV', 'SECTION', 'ARTICLE', 'FIGURE', 'FIGCAPTION',
])

/** 按标签保留的属性白名单（其余属性一律剔除）。 */
const KEEP_ATTRS: Record<string, string[]> = {
  A: ['href'],
  IMG: ['src', 'alt'],
  TD: ['colspan', 'rowspan'],
  TH: ['colspan', 'rowspan'],
}

function sanitizeElement(el: Element): void {
  const tag = el.tagName
  // Office 私有标签（含冒号）与脚本/样式/元信息整体移除。
  if (tag.includes(':') || ['SCRIPT', 'STYLE', 'META', 'LINK', 'TITLE', 'HEAD', 'NOSCRIPT', 'IFRAME', 'OBJECT', 'EMBED', 'VIDEO', 'AUDIO', 'FORM', 'INPUT', 'BUTTON', 'SELECT', 'TEXTAREA', 'SVG', 'BASE'].includes(tag)) {
    el.remove()
    return
  }
  // 白名单外容器：去壳保留子节点（如 font/center/aside 等语义弱包装）。
  if (!KEEP_TAGS.has(tag)) {
    const parent = el.parentNode
    if (!parent) return
    while (el.firstChild) parent.insertBefore(el.firstChild, el)
    parent.removeChild(el)
    return
  }
  const allowed = new Set(KEEP_ATTRS[tag] ?? [])
  for (const attr of Array.from(el.attributes)) {
    if (!allowed.has(attr.name.toLowerCase())) el.removeAttribute(attr.name)
  }
  if (tag === 'A') {
    const href = el.getAttribute('href') ?? ''
    if (!/^(https?:|mailto:|tel:)/i.test(href)) el.removeAttribute('href')
  }
}

/** 清理粘贴的富文本 HTML（保留结构，去除表现层样式与危险内容）。 */
export function sanitizePastedHTML(html: string): string {
  const doc = new DOMParser().parseFromString(html, 'text/html')
  // 深度优先自底向上处理（子节点先于父节点判定，避免去壳后丢失引用）。
  // 注意 body/html 自身不可清洗（去壳逻辑会把子节点搬进 Document 并移除
  // body，随后 doc.body 变 null）。
  const walk = (node: Node) => {
    for (const child of Array.from(node.childNodes)) walk(child)
    if (node instanceof Element && node !== doc.body) sanitizeElement(node)
  }
  walk(doc.body)
  // Word 条件注释等残留在 head/顶层：只取 body。
  return doc.body.innerHTML
}

/** 重写 HTML 内图片：可抓取的（http(s)/data:）上传后换 docflowImage 标记。
 * clipboardFiles 为剪贴板随 HTML 一并提供的图片文件（网页复制图片时
 * <img src="blob:..."> 指向源页上下文的 blob URL，跨页不可 fetch）：blob:
 * 来源优先按顺序消费剪贴板文件重上传，取不到才保留/移除。 */
export async function rewriteRemoteImages(
  html: string,
  upload: (file: File) => Promise<{ id: string; name: string; size: number }>,
  clipboardFiles: File[] = [],
): Promise<string> {
  const doc = new DOMParser().parseFromString(html, 'text/html')
  const imgs = Array.from(doc.body.querySelectorAll('img[src]'))
  // 剪贴板图片文件队列（每次粘贴按顺序对应 HTML 中的 blob: 图片）。
  const pool = clipboardFiles.filter((f) => f.type.startsWith('image/'))
  let poolIndex = 0
  for (const img of imgs) {
    const src = img.getAttribute('src') ?? ''
    if (!src) continue
    if (/^file:/i.test(src) || (!/^(https?:|data:|blob:)/i.test(src) && !src.startsWith('/'))) {
      img.remove()
      continue
    }
    // blob: URL 属源页上下文，本页不可 fetch：优先用剪贴板文件重上传。
    if (/^blob:/i.test(src)) {
      const file = pool[poolIndex++]
      if (!file) {
        img.remove()
        continue
      }
      try {
        const uploaded = await upload(file)
        const holder = doc.createElement('div')
        holder.setAttribute('data-docflow-image', '1')
        holder.setAttribute('data-file-id', uploaded.id)
        holder.setAttribute('data-title', uploaded.name)
        holder.setAttribute('data-width', '100')
        img.replaceWith(holder)
      } catch {
        img.remove()
      }
      continue
    }
    try {
      const res = await fetch(src)
      if (!res.ok) throw new Error(`fetch ${res.status}`)
      const blob = await res.blob()
      if (!blob.type.startsWith('image/')) throw new Error('not an image')
      const nameFromURL = (() => {
        try {
          const u = new URL(src, window.location.origin)
          const last = u.pathname.split('/').filter(Boolean).pop() ?? 'image'
          return /[.][A-Za-z0-9]{1,6}$/.test(last) ? last : `image.${(blob.type.split('/')[1] || 'png').replace('jpeg', 'jpg')}`
        } catch {
          return 'image.png'
        }
      })()
      const uploaded = await upload(new File([blob], nameFromURL, { type: blob.type }))
      const holder = doc.createElement('div')
      holder.setAttribute('data-docflow-image', '1')
      holder.setAttribute('data-file-id', uploaded.id)
      holder.setAttribute('data-title', uploaded.name)
      holder.setAttribute('data-width', '100')
      img.replaceWith(holder)
    } catch {
      // 抓取失败（常见为 CORS）：远程 URL 保留原样（在线仍可显示）。
    }
  }
  return doc.body.innerHTML
}
