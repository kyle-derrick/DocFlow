import type { NavigateFunction } from 'react-router-dom'

/**
 * 校验编辑器「返回」目标：仅接受站内路径（以单个斜杠开头且非 //，防止
 * 开放重定向与协议相对跳转）；缺省或非法返回 null（交给关闭回退链处理）。
 */
export function safeReturnTo(value: string | null): string | null {
  if (!value?.startsWith('/') || value.startsWith('//')) return null
  return value
}

/**
 * 编辑器统一关闭：优先 window.close()（window.open 打开的窗口可直接关闭；
 * noopener 新窗口里 history 不可靠，不依赖它）；短延迟后窗口仍在则确定性
 * 导航——有已校验的 returnTo 用之（replace，避免残留编辑器历史），否则
 * 有历史可退时退一页（navigate(-1)），最终兜底回 '/'。
 */
export function closeEditorWithFallback(navigate: NavigateFunction, returnTo: string | null): void {
  window.close()
  window.setTimeout(() => {
    if (window.closed) return
    if (returnTo) navigate(returnTo, { replace: true })
    else if (window.history.length > 1) navigate(-1)
    else navigate('/', { replace: true })
  }, 50)
}
