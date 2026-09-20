// 临时部署验证配置：直连 docker compose 部署环境 http://localhost
// （caddy + backend + postgres 全套），不启动本地 webServer。
// 仅用于本次 12 项整改的验收实测，跑完即删。
import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e-deploy',
  fullyParallel: false,
  workers: 1,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: './test-results-deploy',
  use: {
    baseURL: 'http://127.0.0.1',
    locale: 'zh-CN',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    // PWA service worker 会劫持加载导致 load 事件异常（ERR_ABORTED），验收期间禁用。
    serviceWorkers: 'block',
    // 单个动作 15s 未完成即失败（防被遮挡元素无限等待吃满用例超时）。
    actionTimeout: 15_000,
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
})
