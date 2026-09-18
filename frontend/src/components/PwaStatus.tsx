/// <reference types="vite-plugin-pwa/vanillajs" />
import { useEffect, useState } from 'react'
import { Button } from 'antd'
import { registerSW } from 'virtual:pwa-register'

// PWA 状态组件（v1.1）：
// - 模块加载即注册 SW（registerType=autoUpdate：新版本安装后自动激活接管，
//   页面内存中仍是旧 Shell，此时回调 onNeedReload —— autoUpdate 模式下
//   onNeedRefresh 不会触发，onNeedReload 即「新版本已就绪」通知，未消费
//   则插件会直接强制刷新页面）；
// - OfflineBadge：顶栏离线徽标（navigator.onLine + online/offline 事件）；
// - UpdateToast：新版本 toast，点击「刷新」重载拿到新 Shell。
// dev 模式下虚拟模块为空实现，仅生产构建生效。

/** 订阅 SW 新版本就绪的监听器集合。 */
const needReloadListeners = new Set<(need: boolean) => void>()

registerSW({
  onNeedReload() {
    needReloadListeners.forEach((fn) => fn(true))
  },
})

/** 顶栏离线徽标：断网时提示仅可查看缓存页面（应用 Shell，无离线数据）。 */
export function OfflineBadge() {
  const [online, setOnline] = useState(() => navigator.onLine)
  useEffect(() => {
    const goOnline = () => setOnline(true)
    const goOffline = () => setOnline(false)
    window.addEventListener('online', goOnline)
    window.addEventListener('offline', goOffline)
    return () => {
      window.removeEventListener('online', goOnline)
      window.removeEventListener('offline', goOffline)
    }
  }, [])
  if (online) return null
  return (
    <span className="offline-badge" role="status">
      离线模式——仅可查看缓存页面
    </span>
  )
}

/** 新版本 toast：新 SW 已激活接管，点击「刷新」加载新版本。 */
export function UpdateToast() {
  const [needReload, setNeedReload] = useState(false)
  useEffect(() => {
    needReloadListeners.add(setNeedReload)
    return () => {
      needReloadListeners.delete(setNeedReload)
    }
  }, [])
  if (!needReload) return null
  return (
    <div className="update-toast" role="status">
      <span>新版本可用，点击刷新</span>
      <Button size="small" type="primary" onClick={() => window.location.reload()}>
        刷新
      </Button>
      <Button size="small" type="text" onClick={() => setNeedReload(false)} aria-label="关闭">
        ×
      </Button>
    </div>
  )
}
