// E2E global setup：webServer（后端 + vite）就绪后的最终环境自检。
// 迁移与种子管理员由 e2e/setup-backend.mjs 在启动后端前完成（见
// playwright.config.ts 的 webServer.backend.command）；此处只做 fail fast：
//   1) E2E_DATABASE_URL 缺失给出明确指引（后端 webServer 通常已先行失败）；
//   2) 等待后端 /metrics 可达；
//   3) 用种子管理员做一次真实登录，验证迁移 + seed 与后端整体可用；
//   4) 预热前端 dev server——用无头浏览器完整加载一次登录页，触发 vite
//      按需编译全部首屏 chunk（此前首个用例吃冷编译延迟，auth 前两例
//      在本机间歇性超时）。
import type { FullConfig } from '@playwright/test'
import { chromium } from '@playwright/test'
import { cfg } from './config.mjs'

async function waitForURL(url: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs
  let lastError: unknown = null
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url)
      if (res.ok) return
      lastError = new Error(`HTTP ${res.status}`)
    } catch (err) {
      lastError = err
    }
    await new Promise((resolve) => setTimeout(resolve, 1000))
  }
  throw new Error(`[e2e] 等待 ${url} 超时：${String(lastError)}`)
}

export default async function globalSetup(_config: FullConfig): Promise<void> {
  if (!process.env.E2E_DATABASE_URL) {
    throw new Error(
      '[e2e] 缺少 E2E_DATABASE_URL：E2E 需要真实 PostgreSQL，' +
        '准备方式见 e2e/setup-backend.mjs 的提示或 Makefile 的 e2e 目标说明。',
    )
  }
  await waitForURL(`${cfg.backendURL}/metrics`, 60_000)

  const seedEmail = process.env.E2E_SEED_EMAIL || cfg.defaultSeedEmail
  const seedPassword = process.env.E2E_SEED_PASSWORD || cfg.defaultSeedPassword
  const res = await fetch(`${cfg.backendURL}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: seedEmail, password: seedPassword }),
  })
  if (res.status !== 200) {
    throw new Error(
      `[e2e] 种子管理员登录自检失败（HTTP ${res.status}）：` +
        '请确认 E2E_SEED_EMAIL / E2E_SEED_PASSWORD 与被 seed 的账号一致' +
        '（setup-backend 每次 E2E 都会以环境变量为准幂等 upsert）。',
    )
  }
  // 预热 vite dev server：完整加载登录页（含按需编译的依赖 chunk）。
  // 失败不阻断（预热是性能优化，非正确性依赖）。
  try {
    const browser = await chromium.launch()
    const page = await browser.newPage()
    await page.goto(`${cfg.frontendBaseURL}/login`, { waitUntil: 'networkidle', timeout: 60_000 })
    await page.waitForSelector('input', { timeout: 30_000 })
    await browser.close()
    console.log('[e2e] 前端预热完成（登录页 chunk 已编译）')
  } catch (err) {
    console.warn(`[e2e] 前端预热跳过（不影响用例正确性）：${String(err)}`)
  }
  console.log(`[e2e] 环境就绪：backend=${cfg.backendURL} frontend=${cfg.frontendBaseURL} seed=${seedEmail}`)
}
