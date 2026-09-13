import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

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
  plugins: [react()],
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
