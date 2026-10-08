// 认证 E2E：登录跳转、错误密码提示、登出、刷新页面会话保持。
import { test, expect } from '@playwright/test'
import { e2eSeed, loginViaUI, logoutViaUI } from './helpers'

test.describe('认证', () => {
  test('登录成功后跳转到文件页', async ({ page }) => {
    const { email, password } = e2eSeed()
    // 未登录访问受保护页 → 被导回登录页。
    await page.goto('/')
    await expect(page).toHaveURL(/\/login$/)

    await page.getByLabel('邮箱').fill(email)
    await page.getByLabel('密码').fill(password)
    await page.getByRole('button', { name: /登\s*录/ }).click()

    await expect(page).toHaveURL(/\/$/)
    await expect(page.locator('.user-menu-trigger')).toBeVisible()
    // 文件页主体就绪：文件表（有内容）或空态提示（全新库默认空间为空）
    // 二选一——断言不得依赖库内既有数据（CI 每次全新库）。
    await expect(page.locator('.file-table, .file-browser .empty')).toBeVisible()
  })

  test('错误密码提示 401 错误信息', async ({ page }) => {
    const { email } = e2eSeed()
    await page.goto('/login')
    await page.getByLabel('邮箱').fill(email)
    await page.getByLabel('密码').fill('WrongPassword123!')
    await page.getByRole('button', { name: /登\s*录/ }).click()

    // 后端 401 契约：{"error":"invalid credentials"}，登录页原样展示。
    await expect(page.locator('.error-text')).toHaveText('invalid credentials')
    await expect(page).toHaveURL(/\/login$/)
  })

  test('登出后回到登录页', async ({ page }) => {
    await loginViaUI(page)
    await logoutViaUI(page)
    await expect(page.getByLabel('邮箱')).toBeVisible()
  })

  test('刷新页面会话保持（refresh cookie 静默续期）', async ({ page }) => {
    await loginViaUI(page)
    // access_token 仅存内存：刷新后 RequireAuth 用 refresh cookie 换新
    // access token 静默恢复会话，不应被踢回登录页。
    await page.reload()
    await expect(page).toHaveURL(/\/$/)
    await expect(page.locator('.user-menu-trigger')).toBeVisible()
    // 同上：文件表或空态二选一（全新库默认空间为空）。
    await expect(page.locator('.file-table, .file-browser .empty')).toBeVisible()
  })

  test('两个标签页并发恢复会话不会触发 refresh token 重放', async ({ page, context }) => {
    await loginViaUI(page)
    const second = await context.newPage()
    await Promise.all([page.reload(), second.goto('/')])
    await expect(page).toHaveURL(/\/$/)
    await expect(second).toHaveURL(/\/$/)
    await expect(page.locator('.user-menu-trigger')).toBeVisible()
    await expect(second.locator('.user-menu-trigger')).toBeVisible()
  })
})
