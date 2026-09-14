import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// 开发服务器将 /api 代理到本地 Go 后端，避免跨域；refresh cookie 也保持同源。
//
// ONLYOFFICE 说明：DocEditor 脚本按绝对地址直连 DocumentServer
// （{server_url}/web-apps/apps/api/documents/api.js，见 src/pages/EditorPage.tsx），
// 因此 /onlyoffice 无需 dev proxy。但 DocumentServer 运行在 docker 网络内，
// 浏览器（docker 网络外）须本地可达才能加载脚本：
//   - 开发时后端 ONLYOFFICE_SERVER_URL 应指向 http://localhost:8081
//     （docker-compose.yml 将 onlyoffice 容器 80 映射为宿主机 8081）；
//   - 或直接禁用集成（ONLYOFFICE_ENABLED=false，此时前端不显示「编辑」入口）。
export default defineConfig({
  plugins: [
    react(),
    // PWA 基础（v1.1）：可安装（manifest）+ 应用 Shell 预缓存，不做离线数据。
    // - registerType autoUpdate：新 SW 安装即激活（skipWaiting+clientsClaim），
    //   运行中的页面内存里仍是旧 Shell，由前端 onNeedReload 提示「点击刷新」；
    // - navigateFallback 仅兜底应用内 SPA 导航；API/文件内容、ONLYOFFICE/draw.io
    //   iframe 资源与公开分享页（/s）都需要网络，一律排除不落 SW。
    VitePWA({
      registerType: 'autoUpdate',
      manifest: {
        name: 'DocFlow',
        short_name: 'DocFlow',
        description: 'DocFlow 团队云文档',
        theme_color: '#0f1115',
        background_color: '#0f1115',
        display: 'standalone',
        icons: [
          { src: '/icon.svg', sizes: 'any', type: 'image/svg+xml', purpose: 'any' },
          { src: '/icon-192.png', sizes: '192x192', type: 'image/png' },
          { src: '/icon-512.png', sizes: '512x512', type: 'image/png' },
        ],
      },
      workbox: {
        navigateFallback: 'index.html',
        navigateFallbackDenylist: [/^\/api\//, /^\/content\//, /^\/onlyoffice\//, /^\/drawio\//, /^\/s\//],
        globPatterns: ['**/*.{js,css,html,svg,png,webmanifest}'],
      },
    }),
  ],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
