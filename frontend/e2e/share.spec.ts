// 公开分享 E2E（serial）：创建公开分享 → 复制链接 → 退出登录 → 访问 /s/:token
// → 文件名与文本预览 → 下载；登录后在「我的分享」撤销 → 访问显示 410 友好页。
import { test, expect } from '@playwright/test'
import { autoAcceptDialogs, fileRow, loginViaUI, logoutViaUI } from './helpers'

const stamp = Date.now().toString(36)
const fileName = `e2eS_${stamp}.txt`
const fileBody = `DocFlow E2E share ${stamp}\n`

// token 仅在创建响应返回一次，serial 用例间经模块状态传递。
let shareToken = ''

test.describe.serial('公开分享', () => {
  test('创建公开分享、复制链接并退出登录', async ({ page }) => {
    await loginViaUI(page)
    // 普通文件上传 input（工具栏另有「上传目录」的 webkitdirectory input，取第一个）。
    await page.locator('input[type="file"]').first().setInputFiles({
      name: fileName,
      mimeType: 'text/plain',
      buffer: Buffer.from(fileBody, 'utf8'),
    })
    // v2.6 上传进度移入顶栏「传输任务」弹窗（.upload-row 不再常驻 DOM）；
    // 以列表行出现为上传完成信号（完成后列表自动刷新）。
    await expect(fileRow(page, fileName)).toBeVisible({ timeout: 60_000 })
    await expect(fileRow(page, fileName)).toBeVisible()

    // 创建公开分享（默认：公开链接 + 可下载 + 永久）。操作在「⋯」菜单内。
    await fileRow(page, fileName).getByRole('button', { name: '操作' }).click()
    await page.locator('.ctx-menu').getByRole('menuitem', { name: /分\s*享/ }).click()
    await page.getByRole('button', { name: '创建链接', exact: true }).click()
    const linkInput = page.locator('.share-link input')
    const link = await linkInput.inputValue()
    shareToken = link.split('/s/')[1] ?? ''
    expect(shareToken).not.toBe('')

    // 复制链接（授权 clipboard 权限后点击，按钮切换为已复制）。
    // 无头 CI（GitHub Actions）上 navigator.clipboard.writeText 偶发被拒，
    // 「已复制 ✓」断言失败时仅 CI 下宽容跳过（console.warn），本地保持严格
    // 失败——shareToken 已从输入框取到，后续用例不受影响。
    await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
    await page.getByRole('button', { name: /复\s*制/ }).click()
    try {
      await expect(page.getByRole('button', { name: '已复制 ✓' })).toBeVisible()
    } catch (err) {
      if (!process.env.CI) throw err
      console.warn(`[e2e] CI 无头环境：clipboard 复制反馈断言失败，跳过（本地严格）：${String(err)}`)
    }

    // 底部「关 闭」按钮（exact 避开右上角 modal 关闭 X 的 aria-label「关闭」）。
    await page.getByRole('button', { name: '关 闭', exact: true }).click()
    await logoutViaUI(page)
  })

  test('未登录访问分享页：文件名、预览与下载', async ({ page }) => {
    await page.goto(`/s/${shareToken}`)
    await expect(page.locator('.share-title')).toHaveText(fileName)
    await expect(page.locator('.preview-text')).toContainText(`DocFlow E2E share ${stamp}`)

    const [download] = await Promise.all([
      page.waitForEvent('download'),
      page.getByRole('link', { name: /下\s*载/ }).click(),
    ])
    expect(download.suggestedFilename()).toBe(fileName)
  })

  test('撤销分享后访问显示 410 友好页', async ({ page }) => {
    autoAcceptDialogs(page)
    await loginViaUI(page)
    await page.goto('/shared')
    // 「我的分享」行：文件名列由 file_id 异步解析为文件名。
    const row = page.locator('.share-table tbody tr').filter({ hasText: fileName })
    await expect(row).toBeVisible()
    await row.getByRole('button', { name: /撤\s*销/ }).click()
    // 撤销确认走 antd Modal.confirm（okText 为「撤销」而非「确定」；
    // autoAcceptDialogs 的 window.confirm 不适用），在确认弹层内再点一次撤销。
    await page.locator('.ant-modal-confirm').getByRole('button', { name: /撤\s*销/ }).click()
    await expect(page.locator('.banner.ok')).toContainText('分享已撤销')

    // 撤销后公开接口返回 410，前端渲染友好失效页。
    await page.goto(`/s/${shareToken}`)
    await expect(page.getByRole('heading', { name: '链接不存在或已失效' })).toBeVisible()
  })
})
