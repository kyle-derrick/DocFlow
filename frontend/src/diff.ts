// 行级 LCS diff（文本版本对比）：自研实现，无第三方依赖。
// 算法：先剥离公共前缀/后缀（典型版本间绝大多数行相同），仅对差异中段
// 做经典 LCS 动态规划（Uint32Array 一维化）；中段过大时退化为整段
// 「旧全删 + 新全增」，保证任意输入都有界耗时。

export type DiffRowType = 'add' | 'del' | 'same'

export interface DiffRow {
  type: DiffRowType
  text: string
  /** 旧版本行号（1 起）；add 行无。 */
  aLine?: number
  /** 新版本行号（1 起）；del 行无。 */
  bLine?: number
}

export interface DiffResult {
  rows: DiffRow[]
  added: number
  removed: number
}

/** 差异中段做 DP 的行数上限（双方任一超限即走退化路径）。 */
const DP_MAX_LINES = 2500

function splitLines(text: string): string[] {
  if (text === '') return []
  return text.split('\n')
}

/** 对差异中段做经典 LCS 回溯，产出增/删/不变行序列。 */
function lcsRows(a: string[], b: string[], aBase: number, bBase: number): DiffRow[] {
  const n = a.length
  const m = b.length
  const width = m + 1
  // dp[i][j] = a[i:] 与 b[j:] 的 LCS 长度（一维化，行主序）。
  const dp = new Uint32Array((n + 1) * width)
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i * width + j] =
        a[i] === b[j]
          ? dp[(i + 1) * width + (j + 1)] + 1
          : Math.max(dp[(i + 1) * width + j], dp[i * width + (j + 1)])
    }
  }
  const rows: DiffRow[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      rows.push({ type: 'same', text: a[i], aLine: aBase + i + 1, bLine: bBase + j + 1 })
      i++
      j++
    } else if (dp[(i + 1) * width + j] >= dp[i * width + (j + 1)]) {
      rows.push({ type: 'del', text: a[i], aLine: aBase + i + 1 })
      i++
    } else {
      rows.push({ type: 'add', text: b[j], bLine: bBase + j + 1 })
      j++
    }
  }
  for (; i < n; i++) rows.push({ type: 'del', text: a[i], aLine: aBase + i + 1 })
  for (; j < m; j++) rows.push({ type: 'add', text: b[j], bLine: bBase + j + 1 })
  return rows
}

/** 退化路径：中段过大时旧段全删、新段全增（不做逐行对齐）。 */
function fallbackRows(a: string[], b: string[], aBase: number, bBase: number): DiffRow[] {
  const rows: DiffRow[] = []
  for (let i = 0; i < a.length; i++) rows.push({ type: 'del', text: a[i], aLine: aBase + i + 1 })
  for (let j = 0; j < b.length; j++) rows.push({ type: 'add', text: b[j], bLine: bBase + j + 1 })
  return rows
}

/** 行级 diff：aText 为旧版本（A）、bText 为新版本（B）。 */
export function lineDiff(aText: string, bText: string): DiffResult {
  const a = splitLines(aText)
  const b = splitLines(bText)
  // 剥离公共前缀与后缀，仅对差异中段做 DP。
  let start = 0
  while (start < a.length && start < b.length && a[start] === b[start]) start++
  let endA = a.length
  let endB = b.length
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
    endA--
    endB--
  }
  const midA = a.slice(start, endA)
  const midB = b.slice(start, endB)
  const rows: DiffRow[] = []
  for (let i = 0; i < start; i++) {
    rows.push({ type: 'same', text: a[i], aLine: i + 1, bLine: i + 1 })
  }
  const mid =
    midA.length <= DP_MAX_LINES && midB.length <= DP_MAX_LINES
      ? lcsRows(midA, midB, start, start)
      : fallbackRows(midA, midB, start, start)
  rows.push(...mid)
  for (let k = 0; k < a.length - endA; k++) {
    rows.push({ type: 'same', text: a[endA + k], aLine: endA + k + 1, bLine: endB + k + 1 })
  }
  let added = 0
  let removed = 0
  for (const row of rows) {
    if (row.type === 'add') added++
    else if (row.type === 'del') removed++
  }
  return { rows, added, removed }
}
