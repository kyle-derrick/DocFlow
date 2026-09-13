// Playwright E2E 配置。
//
// 双 webServer：
//   1) backend：node e2e/setup-backend.mjs —— 校验 E2E_DATABASE_URL、执行
//      migrations、seed 管理员，然后启动后端（:8080，COOKIE_SECURE=false、
//      限流与扫描关闭）；缺 E2E_DATABASE_URL 时该脚本 fail fast 并打印指引。
//   2) frontend：vite dev server（:5173，/api 已代理到 :8080）。
//
// 运行前置与说明见仓库 Makefile 的 e2e 目标注释。
import { defineConfig, devices } from '@playwright/test'
import { cfg } from './e2e/config.mjs'

// E2E_LIST_ONLY=1：仅枚举用例（跳过 webServer 与 globalSetup，不要求
// PostgreSQL）——供无环境的机器上执行
// `E2E_LIST_ONLY=1 npx playwright test --list` 验证用例可被发现；
// 实际运行必须走完整 webServer 流程。
const listOnly = process.env.E2E_LIST_ONLY === '1'

export default defineConfig({
  testDir: './e2e',
  globalSetup: listOnly ? undefined : './e2e/global-setup.ts',
  // E2E 共用同一个种子账号与后端状态，串行执行避免交叉干扰。
  fullyParallel: false,
  workers: 1,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: [['list'], ['html', { open: 'never' }]],
  outputDir: './test-results',
  use: {
    baseURL: cfg.frontendBaseURL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    locale: 'zh-CN',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  ...(listOnly
    ? {}
    : {
        webServer: [
          {
            command: 'node e2e/setup-backend.mjs',
            url: `${cfg.backendURL}/metrics`,
            // 首次运行包含 go build + 迁移 + seed，放宽就绪等待。
            timeout: 240_000,
            // 不复用已有 8080 服务：每次都要保证「迁移 + seed + 本次代码构建」执行。
            reuseExistingServer: false,
            gracefulShutdown: 'on',
          },
          {
            command: 'npm run dev -- --port 5173 --strictPort',
            url: `${cfg.frontendBaseURL}/`,
            timeout: 60_000,
            // 已有 dev server 时复用（/api 代理同样指向 8080）。
            reuseExistingServer: true,
          },
        ],
      }),
})
