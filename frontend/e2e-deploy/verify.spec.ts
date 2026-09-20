// 12 项整改部署验收（直连 http://localhost，账号 admin@example.com）。
// 重点：项5（树点击弹窗串扰）与项9（弹窗高频开关卡死）反复复现验证。
import { test, expect, type Page } from '@playwright/test'

const EMAIL = 'admin@example.com'
const PASSWORD = 'AdminPassword123'
const ts = Date.now().toString().slice(-6)
const SPACE_NAME = `e2e-验收-${ts}`

test.describe.configure({ mode: 'serial' })

let page: Page
/** 网络失败（≥400）与控制台错误收集（失败时输出诊断）。 */
const netErrors: string[] = []

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage()
  page.on('pageerror', (e) => { throw new Error(`页面未捕获异常：${e.message}`) })
  page.on('console', (m) => { if (m.type() === 'error') netErrors.push(`console: ${m.text().slice(0, 200)}`) })
  page.on('response', (r) => { if (r.status() >= 400) netErrors.push(`${r.status()} ${r.request().method()} ${r.url().slice(0, 120)}`) })
  await page.goto('/login', { waitUntil: 'domcontentloaded', timeout: 30_000 })
  await page.waitForSelector('.login-card', { timeout: 30_000 })
  await page.getByPlaceholder('you@example.com 或 username').fill(EMAIL)
  await page.getByPlaceholder('••••••••').fill(PASSWORD)
  await page.locator('.login-submit').click()
  await expect(page).not.toHaveURL(/\/login/, { timeout: 20_000 })
})

test.afterAll(async () => {
  if (netErrors.length > 0) console.log(`[diag] 网络/控制台错误（${netErrors.length}）：\n` + netErrors.slice(-40).join('\n'))
  await page?.close()
})

/** 等待弹层动画与 sweepModalLayer（60ms）完成，再断言无可见弹窗 + body 滚动锁已释放。 */
async function assertNoModalAndUnlocked(p: Page) {
  await p.waitForTimeout(450)
  await expect(p.locator('.ant-modal-wrap:visible')).toHaveCount(0)
  const overflow = await p.evaluate(() => document.body.style.overflow)
  expect(overflow, 'body 滚动锁应已释放（项9）').not.toBe('hidden')
}

/** 关闭当前可见 antd 弹窗（点右上角 ×）；无可见弹窗时立即返回（防挂起）。 */
async function closeModal(p: Page) {
  const btn = p.locator('.ant-modal-wrap:visible .ant-modal-close').first()
  if (await btn.isVisible().catch(() => false)) await btn.click()
  await p.waitForTimeout(150)
}

// ---------- 项6：/spaces 卡片操作收进右上角 ⋯ ----------
test('项6 空间卡片：⋯ 菜单 + 主点击直跳文件页，无底部按钮行', async () => {
  await page.goto('/spaces')
  await page.waitForSelector('.team-card', { timeout: 15_000 })
  // 卡片底部不再有按钮行（team-card-actions 已删除，高度不再被拉高）
  await expect(page.locator('.team-card-actions')).toHaveCount(0)
  // admin 对默认空间：⋯ 菜单含「空间管理」（默认空间不可解散）
  await page.locator('.team-card-more').first().click()
  await expect(page.locator('.ant-dropdown-menu-item').filter({ hasText: '空间管理' })).toBeVisible()
  await page.keyboard.press('Escape')
  await page.waitForTimeout(250)
  // 主点击 = 进入文件页
  await page.locator('.team-card .team-card-main').first().click()
  await expect(page).toHaveURL(/\/files/, { timeout: 15_000 })
})

// ---------- 项1/2/3：空间管理弹窗（成员一行式 + 配额单位 + 危险区） ----------
test('项1/2/3 空间管理弹窗：一行布局 / 配额单位 / 危险区（默认空间禁用）', async () => {
  await page.goto('/spaces')
  await page.waitForSelector('.team-card', { timeout: 15_000 })
  await page.locator('.team-card-more').first().click()
  await page.locator('.ant-dropdown-menu-item').filter({ hasText: '空间管理' }).click()
  const modal = page.locator('.ant-modal:visible', { hasText: '空间管理' }).first()
  await expect(modal).toBeVisible()

  // 项1：邀请 tab 的「添加已有用户」一行式（多选 Select flex 撑满）
  await modal.locator('.ant-menu-item').filter({ hasText: '邀请' }).click()
  const rows = modal.locator('.member-add-row')
  await expect(rows).toHaveCount(2, { timeout: 10_000 })
  // 第一行 = 添加已有用户（多选 Select），第二行 = 邮箱邀请
  const mainInput = rows.nth(0).locator('.member-add-main').first()
  const width = await mainInput.evaluate((el) => el.getBoundingClientRect().width)
  expect(width, '添加已有用户 Select 宽度应 ≥ 200px（修复异常窄）').toBeGreaterThanOrEqual(200)
  await expect(rows.nth(0).locator('.member-add-role')).toBeVisible()
  await expect(rows.nth(0).locator('.member-add-submit')).toBeVisible()
  const emailWidth = await rows.nth(1).locator('.member-add-main').first().evaluate((el) => el.getBoundingClientRect().width)
  expect(emailWidth, '邮箱输入框也应同行撑满').toBeGreaterThanOrEqual(200)

  // 项2：设置 tab 配额带单位（KiB/MiB/GiB/TiB/B 或 不限）
  await modal.locator('.ant-menu-item').filter({ hasText: '空间设置' }).click()
  await expect(modal.getByText('存储用量')).toBeVisible()
  const quotaText = await modal.locator('.hint').filter({ hasText: '存储用量' }).first().textContent()
  expect(quotaText ?? '', '配额显示应带单位或“不限”').toMatch(/(\d+(\.\d+)?\s?(B|KiB|MiB|GiB|TiB))|不限/)

  // 项3：危险区（owner 可见；默认空间禁用解散）
  await expect(modal.getByText('危险区')).toBeVisible()
  await expect(modal.getByText('默认空间不可删除（可改名）')).toBeVisible()
  await expect(modal.getByRole('button', { name: '解散空间' })).toHaveCount(0)
  await closeModal(page)
  await assertNoModalAndUnlocked(page)
})

// ---------- 项7/8：设置页重分配 ----------
test('项7/8 设置页：无资料 tab；admin 含邮件/TLS/系统设置；键名直显', async () => {
  await page.goto('/settings/profile')
  // /settings/profile（资料已删）重定向到 appearance
  await expect(page).toHaveURL(/\/settings\/appearance/, { timeout: 15_000 })
  const sidebarText = await page.locator('.section-sidebar').innerText()
  expect(sidebarText, '设置页不应再有「资料」入口（项7）').not.toContain('资料')
  // admin 可见：邮件配置 / TLS / 系统设置（项8）
  for (const label of ['邮件配置', 'TLS', '系统设置']) {
    expect(sidebarText, `admin 设置页应含「${label}」`).toContain(label)
  }
  // 系统设置面板：键名直显 + 值渲染
  await page.locator('.section-sidebar a').filter({ hasText: '系统设置' }).click()
  await expect(page.locator('.setting-key-code').first()).toBeVisible({ timeout: 15_000 })
  const count = await page.locator('.setting-key-code').count()
  expect(count, '系统设置应有大量设置项直显').toBeGreaterThan(10)
  await expect(page.locator('.setting-key-code', { hasText: 'audit.retention_days' })).toBeVisible()
  await assertNoModalAndUnlocked(page)
})

// ---------- 项10/11：管理页人员与组 + 邀请记录 ----------
test('项10/11 管理页：人员与组合并页 + 邀请记录弹窗（含重发）', async () => {
  await page.goto('/admin/people')
  // 左组树 + 右成员表
  await expect(page.locator('.people-groups-layout')).toBeVisible({ timeout: 15_000 })
  await expect(page.locator('.people-group-tree .people-group-node').first()).toContainText('全部用户')
  await expect(page.locator('.people-group-main .ant-table')).toBeVisible()
  await expect(page.getByRole('button', { name: '新建用户组' })).toBeVisible()
  // 邀请记录弹窗（项11）
  await page.getByRole('button', { name: '邀请记录' }).click()
  const invModal = page.locator('.ant-modal:visible', { hasText: '邀请记录' }).first()
  await expect(invModal).toBeVisible()
  await expect(invModal.getByRole('columnheader', { name: '邮箱' })).toBeVisible()
  // 造一条待接受邀请（重发/撤销仅对 pending 行显示）
  const INVITE_EMAIL = `e2e-invite-${ts}@example.com`
  await invModal.getByRole('button', { name: '创建邀请' }).click()
  const createInv = page.locator('.ant-modal:visible', { hasText: '创建注册邀请' }).last()
  await createInv.locator('input[type="email"], input').first().fill(INVITE_EMAIL)
  await createInv.getByRole('button', { name: /创\s*建/ }).click()
  const invRow = invModal.locator('.ant-table-row', { hasText: INVITE_EMAIL })
  await expect(invRow).toBeVisible({ timeout: 15_000 })
  // 重发（项11：撤销旧 token + 生成新一次性链接）
  await invRow.getByRole('button', { name: /重\s*发/ }).click()
  await expect(page.locator('.banner.ok', { hasText: '已重发' })).toBeVisible({ timeout: 15_000 })
  await expect(page.locator('.share-link input').first()).toHaveValue(/\/register\//, { timeout: 10_000 })
  // 清理：撤销该邀请（后端删除该条，行消失）
  await invRow.getByRole('button', { name: /撤\s*销/ }).click()
  await expect(page.locator('.banner.ok', { hasText: '已撤销邀请' })).toBeVisible({ timeout: 15_000 })
  await expect(invRow).toHaveCount(0, { timeout: 15_000 })
  await closeModal(page)
  await assertNoModalAndUnlocked(page)
  // 管理导航收敛（项8）：人员与组 / 威胁防护；无「用户组」「安全」
  const nav = await page.locator('.section-sidebar').innerText()
  expect(nav).toContain('人员与组')
  expect(nav, '「用户组」已合并').not.toContain('用户组')
  expect(nav).toContain('威胁防护')
  expect(nav, '原「安全」应改名「威胁防护」').not.toContain('安全')
})

// ---------- 项12a：空间软删 → 已解散筛选 → 彻底删除 ----------
test('项12a 空间：解散（输入名确认）→ 已解散视图 → 彻底删除（二次确认）', async () => {
  // 1. 创建空间
  await page.goto('/spaces')
  await page.waitForSelector('.team-card', { timeout: 15_000 })
  await page.locator('.page-head button', { hasText: '创建空间' }).click()
  const createModal = page.locator('.ant-modal:visible', { hasText: '创建空间' }).first()
  await createModal.locator('input').first().fill(SPACE_NAME)
  await createModal.getByRole('button', { name: /创\s*建/ }).click()
  const card = page.locator('.team-card', { hasText: SPACE_NAME })
  await expect(card).toBeVisible({ timeout: 15_000 })

  // 2. ⋯ → 解散 → 直开管理弹窗「空间设置」tab 危险区（项3/6 联动）
  await card.locator('.team-card-more').click()
  await page.locator('.ant-dropdown-menu-item').filter({ hasText: '解散空间' }).click()
  const manage = page.locator('.ant-modal:visible', { hasText: '空间管理' }).first()
  await expect(manage).toBeVisible()
  await expect(manage.getByText('危险区')).toBeVisible()
  await manage.getByRole('button', { name: '解散空间' }).click()
  const confirmModal = page.locator('.ant-modal:visible').filter({ hasText: '以确认解散' }).first()
  await confirmModal.locator('input').fill(SPACE_NAME)
  await confirmModal.getByRole('button', { name: /解\s*散/ }).click()
  // 解散成功：弹窗关闭 + 回 /spaces + 卡片消失
  await expect(page.locator('.ant-modal-wrap:visible')).toHaveCount(0, { timeout: 15_000 })
  await expect(page).toHaveURL(/\/spaces/, { timeout: 10_000 })
  await expect(page.locator('.team-card', { hasText: SPACE_NAME })).toHaveCount(0, { timeout: 15_000 })
  await assertNoModalAndUnlocked(page)

  // 3. admin 空间页：已解散视图（解散时间列 + 彻底删除）
  await page.goto('/admin/spaces')
  await expect(page.locator('.ant-segmented')).toBeVisible({ timeout: 15_000 })
  await page.locator('.ant-segmented-item').filter({ hasText: '已解散' }).click()
  await expect(page.locator('.ant-table')).toBeVisible()
  const row = page.locator('.ant-table-row', { hasText: SPACE_NAME })
  await expect(row).toBeVisible({ timeout: 15_000 })
  await expect(row.locator('.badge', { hasText: '已解散' })).toBeVisible()
  await expect(page.locator('.ant-table-thead').getByText('解散时间')).toBeVisible()
  await expect(row.getByRole('button', { name: '彻底删除' })).toBeVisible()

  // 4. 彻底删除：名字不匹配 → 拦截；匹配 → 物理删除
  await row.getByRole('button', { name: '彻底删除' }).click()
  let prompt = page.locator('.ant-modal:visible').filter({ hasText: '彻底删除空间' }).first()
  await prompt.locator('input').fill('错误的名字')
  await prompt.getByRole('button', { name: /彻底删除/ }).click()
  await expect(page.locator('.banner.error').filter({ hasText: '不匹配' })).toBeVisible({ timeout: 10_000 })
  await expect(page.locator('.ant-table-row', { hasText: SPACE_NAME })).toBeVisible()
  await row.getByRole('button', { name: '彻底删除' }).click()
  prompt = page.locator('.ant-modal:visible').filter({ hasText: '彻底删除空间' }).first()
  await prompt.locator('input').fill(SPACE_NAME)
  await prompt.getByRole('button', { name: /彻底删除/ }).click()
  await expect(page.locator('.banner.ok').filter({ hasText: '已彻底删除' })).toBeVisible({ timeout: 20_000 })
  await expect(page.locator('.ant-table-row', { hasText: SPACE_NAME })).toHaveCount(0, { timeout: 10_000 })
  // 切回正常视图仍正常
  await page.locator('.ant-segmented-item').filter({ hasText: '正常' }).click()
  await expect(page.locator('.ant-table-row').first()).toBeVisible({ timeout: 15_000 })
})

// ---------- 项12b：审计保留期设置 ----------
test('项12b 审计页：保留期设置项（0=永久，保存生效并持久化）', async () => {
  await page.goto('/admin/audit')
  const retentionRow = page.locator('.setting-row', { hasText: '审计日志保留期' })
  await expect(retentionRow).toBeVisible({ timeout: 15_000 })
  await expect(retentionRow.locator('code')).toContainText('audit.retention_days')
  // 改为 30 天保存
  await retentionRow.locator('input').fill('30')
  await retentionRow.getByRole('button', { name: /保\s*存/ }).click()
  await expect(page.locator('.banner.ok').filter({ hasText: '30 天' })).toBeVisible({ timeout: 15_000 })
  // 改回 0（永久）
  await retentionRow.locator('input').fill('0')
  await retentionRow.getByRole('button', { name: /保\s*存/ }).click()
  await expect(page.locator('.banner.ok').filter({ hasText: '永久保留' })).toBeVisible({ timeout: 15_000 })
  // 刷新后仍是 0（服务端持久化）
  await page.reload()
  const row2 = page.locator('.setting-row', { hasText: '审计日志保留期' })
  await expect(row2).toBeVisible({ timeout: 15_000 })
  await expect(row2.locator('input')).toHaveValue('0')
})

// ---------- 项4（数据面）：用户组挂进空间 → 成员表组来源徽标 ----------
test('项4 成员 tab 显示组员（「组」徽标，组员只读）', async () => {
  // 建组（含 admin 本人）
  await page.goto('/admin/people')
  await page.getByRole('button', { name: '新建用户组' }).click()
  const gModal = page.locator('.ant-modal:visible', { hasText: '新建用户组' }).first()
  await gModal.locator('input').first().fill(`e2e组${ts}`)
  await gModal.getByRole('button', { name: /创\s*建/ }).click()
  await page.locator('.people-group-node', { hasText: `e2e组${ts}` }).waitFor({ timeout: 15_000 })
  // 选中组 → 右侧「加入该组」：远程搜索 admin 并加入（UI 全流程，不绕 API）
  await page.locator('.people-group-node', { hasText: `e2e组${ts}` }).click()
  await page.getByText('用户（昵称 / 用户名 / 邮箱，至少 2 字）').waitFor({ timeout: 10_000 })
  await page.locator('.people-group-main .ant-select').first().click()
  await page.keyboard.type('admin')
  await page.locator('.ant-select-dropdown .ant-select-item-option').first().waitFor({ timeout: 10_000 })
  await page.locator('.ant-select-dropdown .ant-select-item-option').first().click()
  await page.getByRole('button', { name: `加入「e2e组${ts}」` }).click()
  // 加入成功：左树该组节点成员计数变为 1（admin 本人）
  const groupNode = page.locator('.people-group-node', { hasText: `e2e组${ts}` })
  await expect(groupNode.locator('.people-group-count')).toHaveText('1', { timeout: 15_000 })
  // 把组挂到 admin 的默认空间（第一个卡片 = 默认空间）
  await page.goto('/spaces')
  await page.waitForSelector('.team-card', { timeout: 15_000 })
  await page.locator('.team-card').first().locator('.team-card-more').click()
  await page.locator('.ant-dropdown-menu-item').filter({ hasText: '空间管理' }).click()
  const modal = page.locator('.ant-modal:visible', { hasText: '空间管理' }).first()
  await expect(modal).toBeVisible()
  await modal.locator('.ant-menu-item').filter({ hasText: '用户组' }).click()
  await modal.locator('.member-add-row .ant-select').first().click()
  await page.locator('.ant-select-dropdown .ant-select-item-option', { hasText: `e2e组${ts}` }).first().click()
  await modal.locator('.member-add-row .member-add-submit').click()
  await expect(modal.locator('.ant-table-row', { hasText: `e2e组${ts}` })).toBeVisible({ timeout: 15_000 })
  // 成员 tab：admin 行出现「组」徽标（合并显示直接成员 + 组内用户）
  await modal.locator('.ant-menu-item').filter({ hasText: '成员' }).click()
  await expect(modal.locator('.badge-group-src').first()).toBeVisible({ timeout: 15_000 })
  // 清理：移除空间组挂载（「移除」→ 确认「删除」）
  await modal.locator('.ant-menu-item').filter({ hasText: '用户组' }).click()
  const gRow = modal.locator('.ant-table-row', { hasText: `e2e组${ts}` })
  await gRow.getByRole('button', { name: /移\s*除/ }).click()
  const confirmWrap = page.locator('.ant-modal-wrap:visible').filter({ hasText: '的授权' })
  await expect(confirmWrap).toHaveCount(1, { timeout: 10_000 })
  await confirmWrap.getByRole('button', { name: /删\s*除/ }).click()
  // onOk 完成后确认层关闭
  await expect(confirmWrap).toHaveCount(0, { timeout: 15_000 })
  await expect(gRow).toHaveCount(0, { timeout: 15_000 })
  await closeModal(page)
  await assertNoModalAndUnlocked(page)
  // 删组
  await page.goto('/admin/people')
  const node = page.locator('.people-group-node', { hasText: `e2e组${ts}` })
  await node.waitFor({ timeout: 15_000 })
  await node.getByRole('button', { name: '删' }).click()
  const delConfirm = page.locator('.ant-modal-wrap:visible').filter({ hasText: '确定删除用户组' })
  await expect(delConfirm).toHaveCount(1, { timeout: 10_000 })
  await delConfirm.getByRole('button', { name: /删\s*除/ }).click()
  // 组已删：左树节点消失（业务结果断言）
  await expect(node).toHaveCount(0, { timeout: 15_000 })
})

// ---------- 项5（高优）：树点击弹窗串扰 ----------
test('项5（高优）回收站→关闭→点目录树：正序 10 轮 + 乱序 5 轮零串扰', async () => {
  await page.goto('/files')
  // 造一个目录，保证树有可点的目录节点
  const newBtn = page.locator('.files-topbar button', { hasText: /新\s*建/ }).first()
  if (await newBtn.isVisible().catch(() => false)) {
    await newBtn.click()
    await page.locator('.ant-dropdown-menu-item').filter({ hasText: '文件夹' }).click()
    const fModal = page.locator('.ant-modal:visible', { hasText: '新建文件夹' }).first()
    await fModal.locator('input').first().fill(`e2e目录${ts}`)
    await fModal.getByRole('button', { name: /创\s*建/ }).click()
    await page.waitForTimeout(900)
    await closeModal(page).catch(() => {})
  }
  await page.waitForSelector('.folder-tree-row', { timeout: 20_000 })
  const treeRows = page.locator('.folder-tree-row:not(.file-leaf)')
  await expect(treeRows.first()).toBeVisible()

  for (let i = 0; i < 10; i++) {
    // 打开回收站
    await page.locator('.files-topbar button', { hasText: '回收站' }).first().click()
    await expect(page.locator('.ant-modal:visible .ant-modal-title', { hasText: '回收站' })).toBeVisible()
    // 关闭（交替 × / Esc）
    if (i % 2 === 0) await closeModal(page)
    else await page.keyboard.press('Escape')
    await expect(page.locator('.ant-modal-wrap:visible')).toHaveCount(0)
    // 立即点目录树（偶数轮点根，奇数轮点业务目录）
    await treeRows.nth(i % 2 === 0 ? 0 : Math.min(1, (await treeRows.count()) - 1)).click()
    await page.waitForTimeout(350)
    // 断言：无任何弹窗重放（串扰）
    const leaked = await page.locator('.ant-modal-wrap:visible').count()
    expect(leaked, `第 ${i + 1} 轮：点目录后不应自动弹窗（串扰），实际可见弹窗 ${leaked} 个`).toBe(0)
    const overflow = await page.evaluate(() => document.body.style.overflow)
    expect(overflow, `第 ${i + 1} 轮：body 不应残留滚动锁`).not.toBe('hidden')
  }
  // 乱序变体：点树 → 开回收站 → 关 → 立即点树
  for (let i = 0; i < 5; i++) {
    await treeRows.first().click()
    await page.locator('.files-topbar button', { hasText: '回收站' }).first().click()
    await expect(page.locator('.ant-modal:visible .ant-modal-title', { hasText: '回收站' })).toBeVisible()
    await closeModal(page)
    await treeRows.last().click()
    await page.waitForTimeout(300)
    expect(await page.locator('.ant-modal-wrap:visible').count(), `乱序第 ${i + 1} 轮不应串扰`).toBe(0)
  }
})

// ---------- 项9（高优）：弹窗高频开关 50 次零卡死 ----------
test('项9（高优）弹窗高频开关 50 次：零 mask 残留 / 滚动锁 / 点不动', async () => {
  await page.goto('/files')
  await page.waitForSelector('.files-topbar', { timeout: 20_000 })
  // ① 回收站 × 25（交替 × / Esc / mask 点击）
  for (let i = 0; i < 25; i++) {
    await page.locator('.files-topbar button', { hasText: '回收站' }).first().click()
    await expect(page.locator('.ant-modal:visible .ant-modal-title', { hasText: '回收站' })).toBeVisible()
    if (i % 3 === 0) await closeModal(page)
    else if (i % 3 === 1) await page.keyboard.press('Escape')
    else await page.locator('.ant-modal-wrap:visible', { hasText: '回收站' }).first().click({ position: { x: 8, y: 8 } })
    await page.waitForTimeout(100)
  }
  await assertNoModalAndUnlocked(page)
  // ② 新建文件夹弹窗 × 12
  for (let i = 0; i < 12; i++) {
    await page.locator('.files-topbar button', { hasText: /新\s*建/ }).first().click()
    await page.locator('.ant-dropdown-menu-item').filter({ hasText: '文件夹' }).click()
    await expect(page.locator('.ant-modal:visible', { hasText: '新建文件夹' }).first()).toBeVisible()
    await closeModal(page)
  }
  await assertNoModalAndUnlocked(page)
  // ③ /spaces 空间管理弹窗 × 13（交替 × / Esc）
  await page.goto('/spaces')
  await page.waitForSelector('.team-card', { timeout: 15_000 })
  for (let i = 0; i < 13; i++) {
    await page.locator('.team-card-more').first().click()
    await page.locator('.ant-dropdown-menu-item').filter({ hasText: '空间管理' }).click()
    await expect(page.locator('.ant-modal:visible', { hasText: '空间管理' }).first()).toBeVisible()
    if (i % 2 === 0) await closeModal(page)
    else await page.keyboard.press('Escape')
    // 关闭动画（~300ms）结束后再进入下一轮，避免动画中重开的竞态。
    await page.waitForTimeout(400)
    // 每轮复核关闭成功（失败即早暴露并带轮次号）。
    const leaked = await page.locator('.ant-modal-wrap:visible').count()
    expect(leaked, `第 ${i} 轮关闭后仍有 ${leaked} 个可见弹窗`).toBe(0)
  }
  await assertNoModalAndUnlocked(page)
  // ④ 终检：页面仍可交互（真实点击打开/关闭弹窗成功 = 无 pointer-events 卡死），
  //    且无残留空弹层根节点
  await page.locator('.page-head button', { hasText: '创建空间' }).click()
  await expect(page.locator('.ant-modal:visible', { hasText: '创建空间' }).first()).toBeVisible()
  await closeModal(page)
  await assertNoModalAndUnlocked(page)
  const strayLayers = await page.evaluate(() => {
    const roots = document.querySelectorAll('body > .ant-modal-root')
    let stray = 0
    roots.forEach((r) => {
      const wrap = r.querySelector('.ant-modal-wrap')
      if (!wrap || wrap.style.display === 'none' || wrap.childElementCount === 0) stray++
    })
    return { stray, total: roots.length }
  })
  expect(strayLayers.stray, '不应残留空弹层根节点').toBe(0)
})
