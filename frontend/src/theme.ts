// 主题（accent 色系）与明暗模式管理（v1.1）。
// - 偏好持久化在 localStorage（key: docflow.theme，JSON {accent, mode}）；
// - 应用方式：document.documentElement 的 data-theme（accent）与
//   data-mode（dark|light，system 模式按 prefers-color-scheme 实时解析）；
// - 默认 indigo + dark，与既有视觉保持一致；非法存储值回退默认。

/** 五种 accent 主题（与 styles.css :root[data-theme] 选择器一致）。 */
export type ThemeAccent = 'indigo' | 'violet' | 'emerald' | 'rose' | 'amber'

/** 明暗模式：system 跟随 prefers-color-scheme。 */
export type ThemeMode = 'dark' | 'light' | 'system'

export const THEME_STORAGE_KEY = 'docflow.theme'

/** 各 accent 的展示名与色块颜色（设置页外观卡片与帮助展示用）。 */
export const THEME_ACCENTS: Array<{ value: ThemeAccent; label: string; color: string }> = [
  { value: 'indigo', label: '靛蓝', color: '#4f7cff' },
  { value: 'violet', label: '紫罗兰', color: '#8b5cf6' },
  { value: 'emerald', label: '翡翠绿', color: '#10b981' },
  { value: 'rose', label: '玫瑰红', color: '#f43f5e' },
  { value: 'amber', label: '琥珀', color: '#f59e0b' },
]

export interface ThemePreference {
  accent: ThemeAccent
  mode: ThemeMode
}

const DEFAULT_THEME: ThemePreference = { accent: 'indigo', mode: 'dark' }

const ACCENTS: ThemeAccent[] = ['indigo', 'violet', 'emerald', 'rose', 'amber']
const MODES: ThemeMode[] = ['dark', 'light', 'system']

function normalize(raw: unknown): ThemePreference {
  let parsed: Partial<ThemePreference> = {}
  if (typeof raw === 'string') {
    try {
      parsed = JSON.parse(raw) as Partial<ThemePreference>
    } catch {
      parsed = {}
    }
  }
  return {
    accent: ACCENTS.includes(parsed.accent as ThemeAccent) ? (parsed.accent as ThemeAccent) : DEFAULT_THEME.accent,
    mode: MODES.includes(parsed.mode as ThemeMode) ? (parsed.mode as ThemeMode) : DEFAULT_THEME.mode,
  }
}

/** 读取并归一化 localStorage 中的主题偏好（缺失/非法回退默认）。 */
export function loadTheme(): ThemePreference {
  try {
    return normalize(window.localStorage.getItem(THEME_STORAGE_KEY))
  } catch {
    return { ...DEFAULT_THEME }
  }
}

/** system 模式下解析当前生效的明暗（其余模式原样返回）。 */
function resolvedMode(mode: ThemeMode): 'dark' | 'light' {
  if (mode !== 'system') return mode
  return typeof window.matchMedia === 'function' && window.matchMedia('(prefers-color-scheme: light)').matches
    ? 'light'
    : 'dark'
}

/** 把偏好应用到 <html>：data-theme=accent、data-mode=解析后的明暗。 */
export function applyTheme(theme: ThemePreference): void {
  const el = document.documentElement
  const mode = resolvedMode(theme.mode)
  el.dataset.theme = theme.accent
  el.dataset.mode = mode
  // 同步 <meta name="theme-color">（PWA 地址栏/标题栏跟随明暗；色值同 styles.css --bg）。
  document
    .querySelector('meta[name="theme-color"]')
    ?.setAttribute('content', mode === 'light' ? '#f4f5f7' : '#0f1115')
}

/** 保存偏好（localStorage + 立即应用）。 */
export function saveTheme(theme: ThemePreference): void {
  applyTheme(theme)
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, JSON.stringify(theme))
  } catch {
    // 隐私模式等写入失败：仅本次会话生效（已 apply）。
  }
}

/**
 * 应用启动时初始化主题：读取偏好并应用到 <html>；mode=system 时监听系统
 * 明暗变化实时跟随（不落库，仍是 system 偏好）。返回清理函数。
 */
export function initTheme(): () => void {
  const theme = loadTheme()
  applyTheme(theme)
  if (theme.mode !== 'system' || typeof window.matchMedia !== 'function') return () => {}
  const mql = window.matchMedia('(prefers-color-scheme: light)')
  const onChange = () => applyTheme(loadTheme())
  mql.addEventListener('change', onChange)
  return () => mql.removeEventListener('change', onChange)
}
