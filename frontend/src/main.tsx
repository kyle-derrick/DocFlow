import React, { ReactNode, useEffect, useState } from 'react'
import ReactDOM from 'react-dom/client'
import { App as AntdApp, ConfigProvider, theme as antdTheme } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import enUS from 'antd/locale/en_US'
import App from './App'
import { THEME_ACCENTS, initTheme, useColorMode } from './theme'
import './styles.css'
import { LocaleContext, loadLocale, Locale } from './i18n'

/** antd 字体与 body 字体栈保持一致（styles.css body font-family；中文字形
 * 依次回退 PingFang SC → 微软雅黑 → Noto Sans SC）。 */
const FONT_FAMILY = "'Segoe UI', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Noto Sans SC', sans-serif"

/** 字号层级：13 正文 / 12 辅助（styles.css body font-size 同值对齐）。 */
const FONT_SIZE = 13

/**
 * antd 基础面板色对齐存量 styles.css 变量体系（--bg/--surface/--border/
 * --text/--muted）：antd 弹层（Modal/Dropdown/Select 等）挂 body，若用
 * antd 默认底色会出现「半黑半白」割裂，故按明暗模式映射同源色值。
 */
const PALETTE = {
  dark: {
    colorBgBase: '#0f1115',
    colorBgContainer: '#161a22',
    colorBgElevated: '#1d222d',
    colorBorder: '#2a303c',
    colorText: '#e6e8ee',
    colorTextSecondary: '#8b93a3',
  },
  light: {
    colorBgBase: '#f4f5f7',
    colorBgContainer: '#ffffff',
    colorBgElevated: '#ffffff',
    colorBorder: '#d7dbe3',
    colorText: '#1d2433',
    colorTextSecondary: '#626a7a',
  },
} as const

/** accent 主色实时读取（data-theme 变化时更新）：设置页切换主题即同步 antd。 */
function useAccentColor(): string {
  const read = () =>
    THEME_ACCENTS.find((a) => a.value === document.documentElement.dataset.theme)?.color ?? THEME_ACCENTS[0].color
  const [color, setColor] = useState(read)
  useEffect(() => {
    const observer = new MutationObserver(() => setColor(read()))
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
    return () => observer.disconnect()
  }, [])
  return color
}

/**
 * antd ConfigProvider 接入层（v6）：
 * - cssVar 开启（v6 CSS 变量模式）：明暗/accent 切换仅更新变量，零运行时开销；
 * - 明暗算法跟随 data-mode（useColorMode）、colorPrimary 跟随 accent 主题、
 *   borderRadius 对齐 --radius（6px）、locale 跟随 i18n（zh_CN / en_US）；
 * - 面板色 token 对齐 styles.css 变量体系（见 PALETTE），弹层与存量页面同源。
 *
 * 控件尺寸统一（v1.7 正解：ConfigProvider componentSize="small"）：
 * - componentSize="small" 全局生效：Input/Select/Button/Segmented/InputNumber/
 *   Table 密度/Pagination 等未显式声明 size 的组件一律 small（全站统一口径；
 *   散写的 size="small" 与之等价可留，管理后台 Table 的 size="middle" 已删
 *   交给全局）；
 * - controlHeight=24 且 controlHeightSM=24 兜底对齐：显式 size="middle" 与
 *   small 同高（24px），漏网的中号声明不再产生 32px 高差；
 * - large 派生为 32px：登录页主输入/按钮显式 size="large" 作为视觉主体；
 * - Menu 条目高度由 styles.css 全局紧凑规格固定（30px）；Modal 页脚按钮走
 *   Button 默认尺寸，同样随 componentSize=small；站内 Switch 均已显式 small。
 */
function AntdConfig({ locale, children }: { locale: Locale; children: ReactNode }) {
  const mode = useColorMode()
  const accent = useAccentColor()
  return (
    <ConfigProvider
      componentSize="small"
      locale={locale === 'zh-CN' ? zhCN : enUS}
      theme={{
        cssVar: { key: 'docflow' },
        algorithm: mode === 'light' ? antdTheme.defaultAlgorithm : antdTheme.darkAlgorithm,
        token: {
          colorPrimary: accent,
          colorInfo: accent,
          borderRadius: 6,
          fontFamily: FONT_FAMILY,
          fontSize: FONT_SIZE,
          controlHeight: 24,
          controlHeightSM: 24,
          ...PALETTE[mode],
        },
      }}
    >
      {children}
    </ConfigProvider>
  )
}

function Root() {
  const [locale, setLocale] = useState<Locale>(loadLocale)
  React.useEffect(() => {
    const onLocale = () => setLocale(loadLocale())
    window.addEventListener('docflow:locale', onLocale)
    return () => window.removeEventListener('docflow:locale', onLocale)
  }, [])
  return (
    <LocaleContext.Provider value={locale}>
      <AntdConfig locale={locale}>
        {/* antd App 壳：App.useApp() 提供走 ConfigProvider 上下文的
            modal/message（替代 window.confirm/alert 与静态方法），弹窗
            自动跟随明暗/accent 主题。 */}
        <AntdApp>
          <App />
        </AntdApp>
      </AntdConfig>
    </LocaleContext.Provider>
  )
}

// 主题先行：渲染前把 data-theme/data-mode 应用到 <html>（localStorage 读取，
// 默认 indigo + dark），system 模式监听系统明暗变化。
initTheme()

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
)
