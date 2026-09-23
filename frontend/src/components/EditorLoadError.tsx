// 编辑页加载失败统一错误卡（v2.7 用户反馈 8：OnlyOffice / draw.io /
// Excalidraw 编辑页在服务地址不可达（如后端只回环 127.0.0.1 的 public
// URL）或静态资源加载失败时整页白屏无提示）。三个编辑页共用：
// - EditorLoadError：页面级错误卡——错误说明 + 资源地址（Document Server
//   / 图表服务 / 静态资源）+ 排查提示 + 可选重试；文案由调用页按 locale
//   组织（本组件不依赖词条）。
// - EditorLoadErrorBoundary：class 错误边界，捕获 React.lazy chunk 加载
//   失败（Excalidraw 1MB+ 独立 chunk 拉取失败会从 Suspense 向上抛异常，
//   无边界即白屏），fail 时渲染调用页给定的错误卡。
import { Component } from 'react'
import type { ReactNode } from 'react'
import { Button } from 'antd'

/** 页面级加载失败错误卡（非白屏）：说明 + 资源地址 + 排查提示 + 重试。 */
export function EditorLoadError({
  message,
  resourceLabel,
  resourceUrl,
  hints,
  onRetry,
  retryText,
}: {
  /** 错误说明（如「编辑服务脚本加载失败」），调用页按 locale 传入。 */
  message: string
  /** 资源地址标签（如「Document Server 地址」）。 */
  resourceLabel: string
  /** 当前使用的资源地址（config 探测到的 public url / iframe src）。 */
  resourceUrl: string
  /** 排查提示（每条一行）。 */
  hints: string[]
  /** 可选重试回调（重新探测/重载编辑器）。 */
  onRetry?: () => void
  /** 重试按钮文案（调用页按 locale 传入）。 */
  retryText?: string
}) {
  return (
    <div className="editor-load-error" role="alert">
      <h3>{message}</h3>
      <div className="editor-load-error-row">
        <span className="muted">{resourceLabel}：</span>
        <code className="editor-load-error-url">{resourceUrl || '—'}</code>
      </div>
      {hints.length > 0 && (
        <ul className="editor-load-error-hints">
          {hints.map((hint) => <li key={hint}>{hint}</li>)}
        </ul>
      )}
      {onRetry && <Button size="small" onClick={onRetry}>{retryText ?? '重试'}</Button>}
    </div>
  )
}

/** 懒加载编辑器 chunk 拉取失败的错误边界：fail 时渲染调用页给定错误卡
 *（renderError 闭包可引用页面 locale 文案），恢复期由 key 重挂。 */
export class EditorLoadErrorBoundary extends Component<
  { renderError: () => ReactNode; children: ReactNode },
  { failed: boolean }
> {
  state = { failed: false }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  componentDidCatch(error: unknown) {
    console.error('[docflow] editor chunk load failed:', error)
  }

  render() {
    return this.state.failed ? this.props.renderError() : this.props.children
  }
}
