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
    await page.getByRole('button', { name: '登录', exact: true }).click()

    await expect(page).toHaveURL(/\/$/)
    await expect(page.getByRole('button', { name: '退出登录' })).toBeVisible()
    await expect(page.getByRole('heading', { name: '文件' })).toBeVisible()
  })

  test('错误密码提示 401 错误信息', async ({ page }) => {
    const { email } = e2eSeed()
    await page.goto('/login')
    await page.getByLabel('邮箱').fill(email)
    await page.getByLabel('密码').fill('WrongPassword123!')
    await page.getByRole('button', { name: '登录', exact: true }).click()

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
    await expect(page.getByRole('button', { name: '退出登录' })).toBeVisible()
    await expect(page.getByRole('heading', { name: '文件' })).toBeVisible()
  })
})
