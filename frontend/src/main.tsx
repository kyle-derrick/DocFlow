import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { initTheme } from './theme'
import './styles.css'

// 主题先行：渲染前把 data-theme/data-mode 应用到 <html>（localStorage 读取，
// 默认 indigo + dark），system 模式监听系统明暗变化。
initTheme()

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
