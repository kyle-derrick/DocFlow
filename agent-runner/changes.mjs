// agent-runner/changes.mjs：git 变更集解析——git status --porcelain 的
// XY 状态码 → {changes:[{path,status:A|M|D}]} 写入 .docflow-changes.json，
// 供平台侧 git 式产物同步消费（.gitignore 过滤天然生效）。与 runner.mjs
// 内置收尾逻辑等价（entrypoint 统一在最后调用本脚本）。
import { execFileSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'

const WS = process.env.DOCFLOW_WORKSPACE || '/workspace'

function git(args) {
  try {
    return execFileSync('git', ['-C', WS, ...args], { encoding: 'utf8', maxBuffer: 8 << 20 })
  } catch {
    return ''
  }
}

// porcelain 行：XY <path> 或 XY <old> -> <new>（R/C 重命名）。带引号路径
//（含空格/非 ASCII）形如 "a b"——按 git 规则去引号并反转义。
function unquote(p) {
  if (p.startsWith('"') && p.endsWith('"')) {
    try { return JSON.parse(p) } catch { return p.slice(1, -1) }
  }
  return p
}

git(['add', '-A'])
const out = git(['status', '--porcelain'])
const changes = []
for (const line of out.split('\n')) {
  if (!line) continue
  const xy = line.slice(0, 2)
  let rest = line.slice(3)
  // 重命名/复制：取新路径，标记 M（平台 diff 语义按内容变更处理）
  if (rest.includes(' -> ')) rest = rest.split(' -> ').pop()
  const path = unquote(rest.trim())
  if (!path || path === '.docflow-changes.json') continue
  const y = xy[1] !== ' ' ? xy[1] : xy[0]
  let status
  if (y === 'D') status = 'D'
  else if (xy[0] === 'D' && xy[1] === 'D') status = 'D'
  else if (y === 'A' || y === '?') status = 'A'
  else status = 'M' // M/R/C/U 均按内容变更
  changes.push({ path, status })
}
if (changes.length > 500) changes.length = 500
try {
  writeFileSync(join(WS, '.docflow-changes.json'), JSON.stringify({ changes }, null, 2))
} catch (e) {
  console.error('changes.mjs: write failed:', e && e.message)
}
