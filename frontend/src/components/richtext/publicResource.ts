// 公开分享态的富文本引用资源解析：
// .dfrt 内的引用节点只存 fileId + title（文件名），公开分享页无认证 API，
// 按文件名在分享树内探测——文档同级 assets/ 子目录优先（编辑器上传图片/
// 拖入文件的既定去向），其次文档同级目录（手动挑选/新建的图表）。
// 探测失败（文件不在分享范围内）由调用方渲染占位提示。
import type { RichTextPublicBase } from './RichTextPublicContext'

/** dfrt 相对路径的目录段（'a/b/x.dfrt' → 'a/b'，根级 → ''）。 */
function docDir(docPath: string): string {
  const i = docPath.lastIndexOf('/')
  return i >= 0 ? docPath.slice(0, i) : ''
}

/** 逐段 encodeURIComponent 拼 '/'（与 SharePage.rawUrlOf 同口径）。 */
function encodeRel(path: string): string {
  return path.split('/').filter(Boolean).map((s) => encodeURIComponent(s)).join('/')
}

/** 引用文件名在分享树内的候选相对路径（同级 assets/ 优先，其次同级）。 */
export function publicCandidates(base: RichTextPublicBase, name: string): string[] {
  if (!name) return []
  const dir = docDir(base.docPath)
  const rels = dir ? [`${dir}/assets/${name}`, `${dir}/${name}`] : [`assets/${name}`, name]
  return rels.map((rel) => `${base.rawBase}/${encodeRel(rel)}`)
}

/** 依次探测候选 URL（GET 命中即返回）：全部失败返回 null。 */
export async function resolvePublicUrl(base: RichTextPublicBase, name: string): Promise<string | null> {
  for (const url of publicCandidates(base, name)) {
    try {
      const res = await fetch(url)
      if (res.ok) return url
    } catch {
      /* 试下一个候选 */
    }
  }
  return null
}

/** 公开 raw URL 拉取文本内容（失败抛错）。 */
export async function fetchPublicText(url: string): Promise<string> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`内容加载失败（${res.status}）`)
  return res.text()
}
