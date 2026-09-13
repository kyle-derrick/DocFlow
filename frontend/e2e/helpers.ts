// E2E 共享工具：环境变量解析（与 setup-backend.mjs 的默认值同源于 config.json）、
// UI 登录、window.confirm 自动接受、文件表格行定位。
import { expect, type Page } from '@playwright/test'
import { cfg } from './config.mjs'

export { cfg }

export function e2eSeed(): { email: string; password: string } {
  return {
    email: process.env.E2E_SEED_EMAIL || cfg.defaultSeedEmail,
    password: process.env.E2E_SEED_PASSWORD || cfg.defaultSeedPassword,
  }
}

/** 通过登录页 UI 登录种子管理员，等待跳转文件页。 */
export async function loginViaUI(page: Page): Promise<void> {
  const { email, password } = e2eSeed()
  await page.goto('/login')
  await page.getByLabel('邮箱').fill(email)
  await page.getByLabel('密码').fill(password)
  await page.getByRole('button', { name: '登录', exact: true }).click()
  await expect(page).toHaveURL(`${cfg.frontendBaseURL}/`)
  await expect(page.getByRole('button', { name: '退出登录' })).toBeVisible()
}

/** 退出登录（顶栏按钮），等待回到登录页。 */
export async function logoutViaUI(page: Page): Promise<void> {
  await page.getByRole('button', { name: '退出登录' }).click()
  await expect(page).toHaveURL(/\/login$/)
}

/** 自动接受 window.confirm（删除/撤销等确认框）。 */
export function autoAcceptDialogs(page: Page): void {
  page.on('dialog', (dialog) => void dialog.accept())
}

/** 文件表格（文件页/回收站）中名称包含 name 的行。 */
export function fileRow(page: Page, name: string) {
  return page.locator('.file-table tbody tr').filter({ hasText: name })
}
