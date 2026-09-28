// 文件全流程 E2E（serial：同一测试账号下按业务顺序串联）：
// 新建文件夹 → 重命名 → 进入并创建二级目录 → 上传文本文件（轮询 available）
// → 新建文本/Markdown（v1.1 新建菜单弹框输文件名，创建后新窗口打开编辑器）
// → 预览 → 下载 → 删除 → 回收站出现 → 恢复 → 彻底删除。
// 文本类编辑/查看为 Monaco（.monaco-editor 容器，不再是 textarea）。
import { test, expect, type Page } from '@playwright/test'
import { autoAcceptDialogs, fileRow, loginViaUI } from './helpers'

// 名称互不为前缀，避免行过滤 hasText 的子串误匹配；stamp 保证跨运行唯一。
const stamp = Date.now().toString(36)
const folder = `e2eA_${stamp}`
const folderRenamed = `e2eB_${stamp}`
const subFolder = `e2eC_${stamp}`
const fileName = `e2eD_${stamp}.txt`
const fileBody = `DocFlow E2E ${stamp}\nhello playwright\n`
const createdTxt = `e2eE_${stamp}.txt`
const createdMd = `e2eF_${stamp}.md`

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
    await page.getByRole('button', { name: /新\s*建/ }).click()
    // v2.x 新建菜单为 antd Dropdown（menuitem 角色，非旧版 .ctx-menu button）。
    await page.getByRole('menuitem', { name: '文件夹' }).click()
    await page.getByLabel('名称').fill(folder)
    await page.getByRole('button', { name: /创\s*建/ }).click()
    await expect(fileRow(page, folder)).toBeVisible()
  })

  test('重命名文件夹', async ({ page }) => {
    await loginViaUI(page)
    await fileRow(page, folder).getByRole('button', { name: '操作' }).click()
    await page.locator('.ctx-menu').getByRole('menuitem', { name: /重命名/ }).click()
    await page.getByLabel('新名称').fill(folderRenamed)
    await page.getByRole('button', { name: /保\s*存/ }).click()
    await expect(fileRow(page, folderRenamed)).toBeVisible()
  })

  test('进入目录并创建二级子目录', async ({ page }) => {
    await loginViaUI(page)
    await fileRow(page, folderRenamed).locator('.name-btn').click()
    await expect(page.locator('.breadcrumb')).toContainText(folderRenamed)

    await page.getByRole('button', { name: /新\s*建/ }).click()
    await page.getByRole('menuitem', { name: '文件夹' }).click()
    await page.getByLabel('名称').fill(subFolder)
    await page.getByRole('button', { name: /创\s*建/ }).click()
    await expect(fileRow(page, subFolder)).toBeVisible()
  })

  test('上传文本文件并轮询至 available', async ({ page }) => {
    await loginViaUI(page)
    await openSubFolder(page)

    // 普通文件上传 input（工具栏另有「上传目录」的 webkitdirectory input，取第一个）。
    await page.locator('input[type="file"]').first().setInputFiles({
      name: fileName,
      mimeType: 'text/plain',
      buffer: Buffer.from(fileBody, 'utf8'),
    })
    // v2.6 上传进度移入顶栏「传输任务」弹窗（.upload-row 不再常驻 DOM）；
    // 以列表行出现为上传完成信号（完成后列表自动刷新）。
    await expect(fileRow(page, fileName)).toBeVisible({ timeout: 60_000 })
  })

  test('新建文本和 Markdown，并通过菜单在独立页打开', async ({ page, context }) => {
    await loginViaUI(page)
    await openSubFolder(page)

    // 新建文本文件：新建菜单「文本文件」弹框输文件名 → 创建并打开（新窗口）。
    // v2.x 菜单项为 antd menuitem（label 带扩展名提示，用子串匹配）。
    await page.getByRole('button', { name: /新\s*建/ }).click()
    await page.getByRole('menuitem', { name: /文本文件/ }).click()
    await page.getByLabel('文件名').fill(createdTxt)
    const [txtEditor] = await Promise.all([
      context.waitForEvent('page'),
      page.getByRole('button', { name: '创建并打开', exact: true }).click(),
    ])
    await expect(txtEditor).toHaveURL(/\/text\//)
    // Monaco 编辑器（.monaco-editor 容器；懒加载 chunk 需要等待）。
    await expect(txtEditor.locator('.monaco-editor')).toBeVisible({ timeout: 30_000 })
    await txtEditor.close()

    // 新建 Markdown：默认名弹框确认 → 编辑器（新窗口）。
    await page.getByRole('button', { name: /新\s*建/ }).click()
    await page.getByRole('menuitem', { name: /Markdown 文档/ }).click()
    await page.getByLabel('文件名').fill(createdMd)
    const [mdEditor] = await Promise.all([
      context.waitForEvent('page'),
      page.getByRole('button', { name: '创建并打开', exact: true }).click(),
    ])
    await expect(mdEditor).toHaveURL(/\/markdown\//)
    // v2.x md 编辑器与文本同为 Monaco（.rich-text-content 已是 .dfrt 富文本专属）。
    await expect(mdEditor.locator('.monaco-editor')).toBeVisible({ timeout: 30_000 })
    await mdEditor.close()

    await expect(fileRow(page, createdTxt)).toBeVisible()
    await expect(fileRow(page, createdMd)).toBeVisible()

    // 上传的文件经行菜单「查看（方式名）」在独立查看页打开（Monaco 只读）；
    // v2.x 行菜单无「新窗口查看」项——默认查看即新窗口分发（openWithMethod）。
    const row = fileRow(page, fileName)
    await row.getByRole('button', { name: '操作' }).click()
    const [viewer] = await Promise.all([
      context.waitForEvent('page'),
      page.locator('.ctx-menu').getByRole('menuitem', { name: /查看（/ }).click(),
    ])
    await expect(viewer).toHaveURL(/\/view\//)
    // 独立查看页按类型分发：txt 走只读文本（.preview-text），md/代码走 Monaco。
    await expect(viewer.locator('.monaco-editor, .preview-text').first()).toBeVisible({ timeout: 30_000 })
    await viewer.close()
  })

  test('下载文件', async ({ page }) => {
    await loginViaUI(page)
    await openSubFolder(page)
    const [download] = await Promise.all([
      page.waitForEvent('download'),
      (async () => {
        await fileRow(page, fileName).getByRole('button', { name: '操作' }).click()
        await page.locator('.ctx-menu').getByRole('menuitem', { name: /下\s*载/ }).click()
      })(),
    ])
    expect(download.suggestedFilename()).toBe(fileName)
  })

  test('删除后进入回收站可见', async ({ page }) => {
    autoAcceptDialogs(page)
    await loginViaUI(page)
    await openSubFolder(page)
    await fileRow(page, fileName).getByRole('button', { name: '操作' }).click()
    await page.locator('.ctx-menu').getByRole('menuitem', { name: /删\s*除/ }).click()
    // 删除确认走 antd Modal.confirm（okText=删除），在确认弹层内再点删除。
    await page.locator('.ant-modal-confirm').getByRole('button', { name: /删\s*除/ }).click()
    await expect(fileRow(page, fileName)).toHaveCount(0)

    // v2.2 起回收站为文件页内弹窗（旧 /trash 路由已并入文件页）。
    await page.getByRole('button', { name: '回收站' }).click()
    await expect(
      page.locator('.ant-modal .file-table tbody tr').filter({ hasText: fileName }),
    ).toBeVisible()
  })

  test('从回收站恢复文件', async ({ page }) => {
    autoAcceptDialogs(page)
    await loginViaUI(page)
    await page.getByRole('button', { name: '回收站' }).click()
    const trashRow = page.locator('.ant-modal .file-table tbody tr').filter({ hasText: fileName })
    await expect(trashRow).toBeVisible()
    await trashRow.getByRole('button', { name: /恢\s*复/ }).click()
    await expect(page.locator('.ant-modal .banner.ok')).toContainText('已恢复')
    await expect(trashRow).toHaveCount(0)
    await page.keyboard.press('Escape')

    // 恢复后回到原二级目录可见。
    await openSubFolder(page)
    await expect(fileRow(page, fileName)).toBeVisible()
  })

  test('彻底删除文件', async ({ page }) => {
    autoAcceptDialogs(page)
    await loginViaUI(page)
    await openSubFolder(page)
    await fileRow(page, fileName).getByRole('button', { name: '操作' }).click()
    await page.locator('.ctx-menu').getByRole('menuitem', { name: /删\s*除/ }).click()
    // 同前：antd Modal.confirm 确认删除。
    await page.locator('.ant-modal-confirm').getByRole('button', { name: /删\s*除/ }).click()
    await expect(fileRow(page, fileName)).toHaveCount(0)

    await page.getByRole('button', { name: '回收站' }).click()
    const trashRow = page.locator('.ant-modal .file-table tbody tr').filter({ hasText: fileName })
    await expect(trashRow).toBeVisible()
    await trashRow.getByRole('button', { name: '彻底删除' }).click()
    // 彻底删除二次确认（antd Modal.confirm，okText=彻底删除）。
    await page.locator('.ant-modal-confirm').getByRole('button', { name: '彻底删除' }).click()
    await expect(page.locator('.ant-modal .banner.ok')).toContainText('已彻底删除')
    await expect(trashRow).toHaveCount(0)
  })
})
