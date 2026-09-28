// 文件名 → 类型图标（VSCode 式视觉区分；目录树/编辑 Tab 等共用）。
import { FileCode2, FileImage, FileJson, FileText, FileType2 } from 'lucide-react'

export function fileIcon(name: string, size = 14) {
  const lower = name.toLowerCase()
  const p = { size, strokeWidth: 2, 'aria-hidden': true } as const
  if (/\.(md|markdown)$/.test(lower)) return <FileType2 {...p} />
  if (/\.(json|dfrt|dfdoc)$/.test(lower)) return <FileJson {...p} />
  if (/\.(png|jpe?g|gif|svg|webp|bmp|ico)$/.test(lower)) return <FileImage {...p} />
  if (/\.(html?|css|scss|less|js|mjs|cjs|jsx|ts|tsx|go|py|java|c|cpp|h|sh|yml|yaml|toml|xml|sql)$/.test(lower)) return <FileCode2 {...p} />
  return <FileText {...p} />
}
