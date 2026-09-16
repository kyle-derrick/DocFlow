// 文件全流程 E2E（serial：同一测试账号下按业务顺序串联）：
// 新建文件夹 → 重命名 → 进入并创建二级目录 → 上传文本文件（轮询 available）
// → 预览 → 下载 → 删除 → 回收站出现 → 恢复 → 彻底删除。
import { test, expect, type Page } from '@playwright/test'
import { autoAcceptDialogs, fileRow, loginViaUI } from './helpers'

// 名称互不为前缀，避免行过滤 hasText 的子串误匹配；stamp 保证跨运行唯一。
const stamp = Date.now().toString(36)
const folder = `e2eA_${stamp}`
const folderRenamed = `e2eB_${stamp}`
const subFolder = `e2eC_${stamp}`
const fileName = `e2eD_${stamp}.txt`
const fileBody = `DocFlow E2E ${stamp}\nhello playwright\n`
const createdTextPrefix = 'text-'
const createdMarkdownPrefix = 'note-'

/** 登录后进入二级目录（根 → folderRenamed → subFolder）。 */
async function openSubFolder(page: Page): Promise<void> {
  await page.goto('/')
  await fileRow(page, folderRenamed).locator('.name-btn').click()
  await expect(page.locator('.breadcrumb')).toContainText(folderRenamed)
  await fileRow(page, subFolder).locator('.name-btn').click()
  await expect(page.locator('.breadcrumb')).toContainText(subFolder)
}

test.describe.serial('文件全流程', () => {
  test('新建文件夹后出现在列表', async ({ page }) => {
    await loginViaUI(page)
    await page.getByRole('button', { name: '＋ 新建' }).click()
    await page.getByRole('button', { name: '文件夹', exact: true }).click()
    await page.getByLabel('名称').fill(folder)
    await page.getByRole('button', { name: '创建', exact: true }).click()
    await expect(fileRow(page, folder)).toBeVisible()
  })

  test('重命名文件夹', async ({ page }) => {
    await loginViaUI(page)
    await fileRow(page, folder).getByRole('button', { name: '重命名', exact: true }).click()
    await page.getByLabel('新名称').fill(folderRenamed)
    await page.getByRole('button', { name: '保存', exact: true }).click()
    await expect(fileRow(page, folderRenamed)).toBeVisible()
  })

  test('进入目录并创建二级子目录', async ({ page }) => {
    await loginViaUI(page)
    await fileRow(page, folderRenamed).locator('.name-btn').click()
    await expect(page.locator('.breadcrumb')).toContainText(folderRenamed)

    await page.getByRole('button', { name: '＋ 新建' }).click()
    await page.getByRole('button', { name: '文件夹', exact: true }).click()
    await page.getByLabel('名称').fill(subFolder)
    await page.getByRole('button', { name: '创建', exact: true }).click()
    await expect(fileRow(page, subFolder)).toBeVisible()
  })

  test('上传文本文件并轮询至 available', async ({ page }) => {
    await loginViaUI(page)
    await openSubFolder(page)

    await page.locator('input[type="file"]').setInputFiles({
      name: fileName,
      mimeType: 'text/plain',
      buffer: Buffer.from(fileBody, 'utf8'),
    })
    // 上传面板状态徽标：created→uploading→…→available（已完成）。
    await expect
      .poll(
        async () =>
          page
            .locator('.upload-row')
            .filter({ hasText: fileName })
            .locator('.badge')
            .textContent(),
        { timeout: 60_000 },
      )
      .toBe('已完成')
    // 上传完成后列表自动刷新，文件出现在当前目录。
    await expect(fileRow(page, fileName)).toBeVisible()
  })

  test('新建文本和 Markdown，并通过菜单在独立编辑页打开', async ({ page, context }) => {
    await loginViaUI(page)
    await openSubFolder(page)

    for (const name of ['文本文件', 'Markdown 笔记']) {
      await page.getByRole('button', { name: '＋ 新建' }).click()
      const [editor] = await Promise.all([
        context.waitForEvent('page'),
        page.getByRole('button', { name, exact: true }).click(),
      ])
      await editor.waitForLoadState()
      await expect(editor.locator('textarea')).toBeVisible()
      await editor.close()
    }

    await expect(fileRow(page, createdTextPrefix)).toBeVisible()
    await expect(fileRow(page, createdMarkdownPrefix)).toBeVisible()

    const row = fileRow(page, fileName)
    await row.getByRole('button', { name: '操作' }).click()
    const [editor] = await Promise.all([
      context.waitForEvent('page'),
      page.locator('.ctx-menu').getByRole('button', { name: '打开', exact: true }).click(),
    ])
    await expect(editor).toHaveURL(/\/text\//)
    await expect(editor.locator('textarea')).toHaveValue(fileBody)
    await editor.close()
  })

  test('下载文件', async ({ page }) => {
    await loginViaUI(page)
    await openSubFolder(page)
    const [download] = await Promise.all([
      page.waitForEvent('download'),
      fileRow(page, fileName).getByRole('button', { name: '下载', exact: true }).click(),
    ])
    expect(download.suggestedFilename()).toBe(fileName)
  })

  test('删除后进入回收站可见', async ({ page }) => {
    autoAcceptDialogs(page)
    await loginViaUI(page)
    await openSubFolder(page)
    await fileRow(page, fileName).getByRole('button', { name: '删除', exact: true }).click()
    await expect(fileRow(page, fileName)).toHaveCount(0)

    await page.goto('/trash')
    await expect(fileRow(page, fileName)).toBeVisible()
  })

  test('从回收站恢复文件', async ({ page }) => {
    await loginViaUI(page)
    await page.goto('/trash')
    await fileRow(page, fileName).getByRole('button', { name: '恢复', exact: true }).click()
    await expect(page.locator('.banner.ok')).toContainText('已恢复')
    await expect(fileRow(page, fileName)).toHaveCount(0)

    // 恢复后回到原二级目录可见。
    await openSubFolder(page)
    await expect(fileRow(page, fileName)).toBeVisible()
  })

  test('彻底删除文件', async ({ page }) => {
    autoAcceptDialogs(page)
    await loginViaUI(page)
    await openSubFolder(page)
    await fileRow(page, fileName).getByRole('button', { name: '删除', exact: true }).click()
    await expect(fileRow(page, fileName)).toHaveCount(0)

    await page.goto('/trash')
    await fileRow(page, fileName).getByRole('button', { name: '彻底删除', exact: true }).click()
    await expect(page.locator('.banner.ok')).toContainText('已彻底删除')
    await expect(fileRow(page, fileName)).toHaveCount(0)
  })
})
