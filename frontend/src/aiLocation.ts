// AI 助手「位置跟随」：文件页把当前空间/目录广播给全局 AI 助手，
// 供其工作目录「跟随当前位置」态使用（用户未手动固定工作目录时生效）。
// 内存模块级状态 + 'docflow:location' 事件；离开文件页不清空（保留最后位置）。
import { useEffect, useState } from 'react'

/** 当前位置（path = 「空间名/目录/…」；folderId null = 空间根）。 */
export interface AILocation {
  spaceId: string
  folderId: string | null
  path: string
}

const AI_LOCATION_EVENT = 'docflow:location'

let current: AILocation | null = null

/** 更新当前位置并广播（null = 清空；各页面自行决定何时调用）。 */
export function setAILocation(loc: AILocation | null): void {
  current = loc
  window.dispatchEvent(new Event(AI_LOCATION_EVENT))
}

/** 读取当前位置（同步；不入 React 状态）。 */
export function getAILocation(): AILocation | null {
  return current
}

/** 订阅当前位置（监听 docflow:location 事件；null = 尚无位置）。 */
export function useAILocation(): AILocation | null {
  const [loc, setLoc] = useState<AILocation | null>(current)
  useEffect(() => {
    const onLoc = () => setLoc(current)
    window.addEventListener(AI_LOCATION_EVENT, onLoc)
    return () => window.removeEventListener(AI_LOCATION_EVENT, onLoc)
  }, [])
  return loc
}
