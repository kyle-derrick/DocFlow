// 轻量快捷键 hook（v1.1，不引入第三方库）。
// - 支持单键（'/'、'?'、'n'、'u'、'Delete'、'Escape'）与两键序列（'g f' 等，
//   空格分隔；首键按下后 1s 内按次键生效）；
// - 跳过条件：Ctrl/Meta/Alt 组合键、焦点在可输入元素（input/textarea/
//   select/contentEditable）、弹窗打开（.modal-backdrop 存在）；
// - Ctrl/Cmd 组合白名单：map 中显式注册为 'mod+<key>'（如 'mod+1'）的
//   组合键才分发（mod = Ctrl 或 Meta；Alt 组合一律不分发），未注册的
//   修饰键组合仍让位给浏览器/系统快捷键，不影响既有跳过逻辑；
// - Escape 例外：恒分发（关闭弹窗/清空选择正是它的职责，输入框内也允许）；
// - 键语义的单一清单见 components/HotkeysHelp 的 hotkeyDocs
//   （'/' 聚焦顶栏全文搜索等，新增快捷键时在该处登记）。
import { useEffect, useRef } from 'react'

export type HotkeyHandler = (e: KeyboardEvent) => void
export type HotkeyMap = Record<string, HotkeyHandler>

/** 序列按键的等待窗口（毫秒）。 */
const SEQUENCE_TIMEOUT_MS = 1000

/** 焦点是否在可输入元素上（快捷键应让位给正常输入）。 */
function focusInEditable(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null
  if (!el) return false
  const tag = el.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable
}

/** 当前是否有弹窗打开（modal open 时页面快捷键跳过）。 */
export function modalOpen(): boolean {
  return document.querySelector('.modal-backdrop') !== null
}

export function useHotkeys(map: HotkeyMap, enabled = true): void {
  // map 每次渲染重建（闭包捕获最新 state），ref 跟随更新，监听器只挂一次。
  const mapRef = useRef(map)
  useEffect(() => {
    mapRef.current = map
  })

  useEffect(() => {
    if (!enabled) return
    let pending: { key: string; timer: number } | null = null
    const clearPending = () => {
      if (pending) {
        clearTimeout(pending.timer)
        pending = null
      }
    }
    const onKeyDown = (e: KeyboardEvent) => {
      const current = mapRef.current
      if (e.ctrlKey || e.metaKey || e.altKey) {
        clearPending()
        // 修饰键组合白名单：仅分发显式注册的 'mod+<key>'（设计 6.16.1 的
        // Ctrl/Cmd+1、Ctrl/Cmd+2 视图切换等），其余组合维持原有跳过行为。
        // 焦点在输入框或弹窗打开时同样跳过（与单键规则一致，Esc 除外）。
        if (!e.altKey) {
          const modHandler = current[`mod+${e.key}`]
          if (modHandler && !focusInEditable(e.target) && !modalOpen()) {
            e.preventDefault()
            modHandler(e)
          }
        }
        return
      }
      if (e.key === 'Escape') {
        clearPending()
        current['Escape']?.(e)
        return
      }
      if (focusInEditable(e.target) || modalOpen()) {
        clearPending()
        return
      }
      if (pending) {
        const combo = `${pending.key} ${e.key}`
        const seqHandler = current[combo]
        clearPending()
        if (seqHandler) {
          e.preventDefault()
          seqHandler(e)
          return
        }
        // 非预期次键：不视为序列，继续按单键匹配。
      }
      const handler = current[e.key]
      if (handler) {
        e.preventDefault()
        handler(e)
        return
      }
      // 可能是某序列的首键：开窗等待次键。
      if (Object.keys(current).some((k) => k.startsWith(`${e.key} `))) {
        pending = {
          key: e.key,
          timer: window.setTimeout(() => {
            pending = null
          }, SEQUENCE_TIMEOUT_MS),
        }
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      clearPending()
    }
  }, [enabled])
}
