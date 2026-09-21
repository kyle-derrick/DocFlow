// 10 项修复的部署环境验收（http://localhost，admin@example.com）。
// 数据准备走独立 APIRequestContext（Bearer，无浏览器 cookie → 免 CSRF），
// 行为断言走浏览器 UI。
import { test, expect, request, type APIRequestContext, type Page } from '@playwright/test'

const EMAIL = 'admin@example.com'
const PASSWORD = 'AdminPassword123'

let api: APIRequestContext
let token = ''

test.beforeAll(async () => {
  api = await request.newContext({ baseURL: 'http://127.0.0.1' })
  const res = await api.post('/api/v1/auth/login', { data: { identifier: EMAIL, password: PASSWORD } })
  expect(res.ok(), `login failed: ${res.status()}`).toBeTruthy()
  token = (await res.json()).access_token
})

test.afterAll(async () => {
  await api?.dispose()
})

const H = () => ({ Authorization: `Bearer ${token}` })

/** UI 登录（浏览器侧，等跳转文件页）。 */
async function loginUI(page: Page) {
  await page.goto('/login')
  await page.getByLabel(/邮箱/).first().fill(EMAIL)
  await page.getByLabel('密码').fill(PASSWORD)
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await expect(page).toHaveURL(/\/(files)?$/)
}

/** 已保存的会话 cookie（首个测试 UI 登录后采集；后续测试免登录，防限流）。 */
let savedCookies: Array<{ name: string; value: string; domain: string; path: string; expires: number; httpOnly: boolean; secure: boolean; sameSite: 'Strict' | 'Lax' | 'None' }> | null = null

/** 复用会话打开页面。后端 refresh token 严格旋转（旧 cookie 复用即 401），
 *  故每次成功恢复会话后回写该 context 的最新 cookie，后续测试链式使用；
 *  若会话失效被重定向回登录页，自动回退 UI 登录并刷新 cookie 快照。 */
async function openAuthed(page: Page, path: string) {
  const tryOpen = async () => {
    await page.context().addCookies(savedCookies!)
    await page.goto(path)
    // 等会话恢复完成：顶栏渲染（成功）；超时或被踢回登录页均视为失败。
    try {
      await page.waitForSelector('.topbar', { timeout: 12000 })
      return !page.url().includes('/login')
    } catch {
      return false
    }
  }
  if (!savedCookies) {
    await loginUI(page)
    savedCookies = await page.context().cookies()
    await page.goto(path)
  } else if (!(await tryOpen())) {
    console.log('openAuthed: session expired on', path, '→ re-login')
    await loginUI(page)
    savedCookies = await page.context().cookies()
    await page.goto(path)
  }
  await page.waitForLoadState('domcontentloaded')
  // 链式回写：refresh 旋转后的最新 cookie 供下一个测试使用。
  savedCookies = await page.context().cookies()
}

/** 随机后缀（隔离多次运行的数据）。 */
const R = () => Math.random().toString(36).slice(2, 8)

/** API 建目录（默认空间根 parent=null）。 */
async function mkDir(name: string, parent: string | null): Promise<string> {
  const res = await api.post('/api/v1/folders', { headers: H(), data: { name, parent_id: parent } })
  expect(res.ok(), `mkdir ${name}: ${res.status()} ${await res.text()}`).toBeTruthy()
  return (await res.json()).id
}

// ---------------------------------------------------------------- 修复 1
test('1 树导航三层子目录：仅一次列表请求、无根闪现', async ({ page }) => {
  const r = R()
  const l1 = await mkDir(`V25L1-${r}`, null)
  const l2 = await mkDir(`V25L2-${r}`, l1)
  await mkDir(`V25L3-${r}`, l2)
  await mkDir(`V25Decoy-${r}`, null) // 干扰目录：若闪根列表会包含它

  await openAuthed(page, '/')

  // 列表请求（区别于树懒加载：列表查询带 sort 参数）
  const listUrls: string[] = []
  const listItems: string[][] = []
  page.on('response', async (res) => {
    const url = res.request().url()
    if (/\/api\/v1\/(files|spaces\/[^/]+\/files)\?/.test(url) && url.includes('sort=')) {
      listUrls.push(url)
      try {
        const body = await res.json()
        const names = (body.files ?? body.items ?? []).map((it: { name: string }) => it.name)
        listItems.push(names)
      } catch {
        listItems.push([])
      }
    }
  })

  const treeName = (name: string) =>
    page.locator('.folder-tree-row .folder-tree-name-text').filter({ hasText: name })
  const caretOf = (name: string) =>
    page.locator('.folder-tree-row', { has: page.locator('.folder-tree-name-text', { hasText: name }) })
      .locator('> .folder-tree-caret')

  // 点击 L1：一次列表请求，面包屑直达 L1（不闪根）
  // 先等初始根列表渲染完成（response 事件已 fire），避免迟到响应污染基线。
  await expect(page.locator('.file-browser .file-table, .file-browser .empty').first()).toBeVisible()
  await page.waitForTimeout(300)
  const baseCount = listItems.length
  await treeName(`V25L1-${r}`).first().click()
  await expect(page.locator('.files-toolbar .crumb').last()).toContainText(`V25L1-${r}`)
  await page.waitForTimeout(600)
  const afterL1 = listUrls.length
  console.log('LIST URLS:', JSON.stringify(listUrls, null, 1))
  expect(afterL1 - baseCount, '进入 L1 应只有一次列表请求').toBeLessThanOrEqual(1)
  // 无根闪现：点击后的列表响应里没有 Decoy 目录
  expect(listItems.slice(baseCount).some((names) => names.includes(`V25Decoy-${r}`)), '不应出现根目录内容闪现').toBeFalsy()

  // 展开 L1（树懒加载）→ 点 L2：再一次列表请求
  await caretOf(`V25L1-${r}`).first().click()
  await expect(treeName(`V25L2-${r}`).first()).toBeVisible()
  await treeName(`V25L2-${r}`).first().click()
  await expect(page.locator('.files-toolbar .crumb').last()).toContainText(`V25L2-${r}`)
  await page.waitForTimeout(600)
  expect(listUrls.length - afterL1, '进入 L2 应只有一次列表请求').toBeLessThanOrEqual(1)

  // 展开 L2 → 点 L3（三层）
  await caretOf(`V25L2-${r}`).first().click()
  await expect(treeName(`V25L3-${r}`).first()).toBeVisible()
  const beforeL3 = listItems.length
  await treeName(`V25L3-${r}`).first().click()
  await expect(page.locator('.files-toolbar .crumb').last()).toContainText(`V25L3-${r}`)
  await page.waitForTimeout(600)
  // 全程无根闪现（基线之后的列表响应从未包含根级目录；最后的回根是
  // 用户主动操作，不在本断言范围）
  expect(listItems.slice(baseCount, beforeL3 + 1).some((names) => names.includes(`V25Decoy-${r}`)), '全程不应出现根目录内容闪现').toBeFalsy()

  // 回根（点树根节点名）：一次列表请求 + 面包屑回到根
  const rootClicks = listUrls.length
  await page.locator('.folder-tree-row').first().locator('.folder-tree-name').click()
  await page.waitForTimeout(600)
  expect(listUrls.length - rootClicks, '回根应只有一次列表请求').toBeLessThanOrEqual(1)
})

// ---------------------------------------------------------------- 修复 2
test('2 三栏等高：中间列表空目录也撑满', async ({ page }) => {
  const r = R()
  const l1 = await mkDir(`V25H1-${r}`, null)
  await mkDir(`V25Empty-${r}`, l1)

  await openAuthed(page, '/')
  // 诊断：页面实际状态
  console.log('T2 url:', page.url(), JSON.stringify(await page.evaluate(() => ({
    topbar: Boolean(document.querySelector('.topbar')),
    filesShell: Boolean(document.querySelector('.files-shell')),
    bodyText: document.body.innerText.slice(0, 80),
  }))))
  // 等三栏布局渲染完成再测量。
  await expect(page.locator('.files-tree-main .file-browser')).toBeVisible()
  await expect(page.locator('.folder-tree-nav')).toBeVisible()

  const heights = (sel: string) =>
    page.evaluate((s) => {
      const el = document.querySelector(s)
      return el ? Math.round(el.getBoundingClientRect().height) : -1
    }, sel)

  const treeH = await heights('.folder-tree-nav')
  const mainH = await heights('.files-tree-main')
  expect(Math.abs(treeH - mainH), `左树 ${treeH} vs 中区 ${mainH} 应等高`).toBeLessThanOrEqual(2)

  // 进入空目录：中区高度不变（空态等高）
  const caretOf = (name: string) =>
    page.locator('.folder-tree-row', { has: page.locator('.folder-tree-name-text', { hasText: name }) })
      .locator('> .folder-tree-caret')
  await page.locator('.folder-tree-row .folder-tree-name-text').filter({ hasText: `V25H1-${r}` }).first().click()
  await expect(page.locator('.files-toolbar .crumb').last()).toContainText(`V25H1-${r}`)
  await caretOf(`V25H1-${r}`).first().click()
  await page.locator('.folder-tree-row .folder-tree-name-text').filter({ hasText: `V25Empty-${r}` }).first().click()
  await expect(page.locator('.files-toolbar .crumb').last()).toContainText(`V25Empty-${r}`)
  await page.waitForTimeout(400)
  const mainH2 = await heights('.files-tree-main')
  expect(Math.abs(mainH - mainH2), `空目录中区高度应保持（${mainH} → ${mainH2}）`).toBeLessThanOrEqual(2)
})

// ---------------------------------------------------------------- 修复 4/5
test('4/5 弹窗打开不挤压页面；文件页 1560 限宽居中', async ({ page }) => {
  await openAuthed(page, '/dashboard')
  await page.waitForLoadState('networkidle')

  const contentX = () =>
    page.evaluate(() => Math.round(document.querySelector('.content')?.getBoundingClientRect().x ?? -1))

  // 概览页：打开任意 antd 弹窗（个人信息）前后 .content 左边距不变
  const before = await contentX()
  console.log('T4 diag url:', page.url(), 'trigger:', await page.locator('.user-menu-trigger').count())
  await page.locator('.user-menu-trigger').click()
  await page.waitForTimeout(600)
  console.log('T4 dropdown items:', await page.locator('.ant-dropdown-menu-item').allTextContents())
  await page.locator('.ant-dropdown-menu-item').filter({ hasText: '个人信息' }).click()
  await page.waitForTimeout(600)
  console.log('T4 modal count:', await page.locator('.ant-modal').count())
  // 弹窗打开标志：标题「个人信息」的 modal 已挂载（不依赖动画可见性）。
  await expect(page.locator('.ant-modal-title').filter({ hasText: '个人信息' })).toHaveCount(1)
  await page.waitForTimeout(300)
  const during = await contentX()
  expect(during, `弹窗打开前后 .content 左边距应不变（${before} → ${during}）`).toBe(before)

  // 概览最近文件弹窗（若存在数据）
  await page.keyboard.press('Escape')
  await page.waitForTimeout(200)
  const recentBtn = page.locator('.dash-recent-item').first()
  if (await recentBtn.count()) {
    const x0 = await contentX()
    await recentBtn.click()
    await expect(page.locator('.ant-modal')).toBeVisible()
    await page.waitForTimeout(300)
    expect(await contentX(), '最近文件弹窗打开后 .content 左边距应不变').toBe(x0)
    await page.keyboard.press('Escape')
  }

  // 文件页：.content 恢复 1560 限宽（与其他页一致）
  await page.goto('/')
  await page.waitForLoadState('networkidle')
  const box = await page.evaluate(() => {
    const el = document.querySelector('.content')
    const r = el!.getBoundingClientRect()
    const cs = getComputedStyle(el)
    return { x: Math.round(r.x), width: Math.round(r.width), maxW: cs.maxWidth }
  })
  expect(box.maxW, '文件页 .content 应恢复 max-width 1560px').toBe('1560px')
  expect(box.width, `文件页内容宽度应受 1560 约束（实际 ${box.width}）`).toBeLessThanOrEqual(1560)
  // 居中留白：左右边距 > 0（1600 视口）
  expect(box.x, `文件页应左右留白居中（左缘 ${box.x}）`).toBeGreaterThan(0)

  // 文件页弹窗同样不挤压（新建 → 文件夹）
  await page.getByRole('button', { name: /新\s*建/ }).first().click()
  await page.locator('.ant-dropdown-menu-item').filter({ hasText: '文件夹' }).click()
  await expect(page.locator('.ant-modal:visible').first()).toBeVisible()
  await page.waitForTimeout(300)
  const box2 = await page.evaluate(() => {
    const r = document.querySelector('.content')!.getBoundingClientRect()
    return { x: Math.round(r.x), width: Math.round(r.width) }
  })
  expect(box2.x, `文件页弹窗打开后左边距不变（${box.x} → ${box2.x}）`).toBe(box.x)
})

// ---------------------------------------------------------------- 修复 3
test('3 空间设置危险区：转让与解散两个独立子卡片', async ({ page }) => {
  const r = R()
  const res = await api.post('/api/v1/spaces', { headers: H(), data: { name: `V25S-${r}`, description: '' } })
  expect(res.ok()).toBeTruthy()
  const spaceId = (await res.json()).id

  await openAuthed(page, '/spaces')
  const card = page.locator('.team-card').filter({ hasText: `V25S-${r}` })
  await card.getByRole('button', { name: /^管\s*理$/ }).click()
  // 左导航切「空间设置」
  await page.getByRole('menuitem', { name: '空间设置' }).click()
  const zone = page.locator('.danger-card')
  await expect(zone).toHaveCount(2)
  await expect(zone.first()).toContainText('转让所有权')
  await expect(zone.first()).toContainText('降为管理员')
  await expect(zone.nth(1)).toContainText('解散空间')
  await expect(zone.nth(1)).toHaveClass(/danger-card-destructive/)
  await expect(zone.nth(1)).toContainText('不可恢复')
})

// ---------------------------------------------------------------- 修复 6
test('6 /spaces 卡片 SplitButton：管理 / 解散', async ({ page }) => {
  const r = R()
  const res = await api.post('/api/v1/spaces', { headers: H(), data: { name: `V25S-${r}`, description: '' } })
  expect(res.ok()).toBeTruthy()

  await openAuthed(page, '/spaces')
  const card = page.locator('.team-card').filter({ hasText: `V25S-${r}` })
  // 主按钮文案 = 管理（非「空间管理」）
  await expect(card.locator('.team-card-actions-split .ant-btn-primary').first()).toHaveText(/^管\s*理$/)
  // 下拉危险项 = 解散（非「解散空间」）
  await card.locator('.team-card-actions-split button[aria-label="更多操作"]').click()
  const item = page.locator('.ant-dropdown-menu-item').filter({ hasText: '解散' })
  await expect(item).toHaveText('解散')
  await expect(page.locator('.ant-dropdown-menu-item').filter({ hasText: '解散空间' })).toHaveCount(0)
})

// ---------------------------------------------------------------- 修复 7
test('7 设置页分区直达：邮件配置/TLS/系统设置不再跳外观', async ({ page }) => {
  await openAuthed(page, '/settings/appearance')

  for (const [section, marker] of [
    ['mail', '邮件配置（SMTP）'],
    ['tls', 'HTTPS / TLS'],
    ['system', '过滤设置键'],
  ] as const) {
    await page.goto(`/settings/${section}`)
    await page.waitForLoadState('networkidle')
    await expect(page, `${section} 应停留在本分区`).toHaveURL(new RegExp(`/settings/${section}$`))
    await expect(page.locator('.section-content'), `${section} 应渲染对应面板`).toContainText(marker)
  }

  // 全部 tab 逐个点击断言内容正确
  const tabs: Array<[string, string]> = [
    ['外观', '主题色'],
    ['打开方式', '打开方式'],
    ['账号安全', '两步验证'],
    ['通知', '通知'],
    ['开发者', '开发者'],
    ['邮件配置', '邮件配置（SMTP）'],
    ['TLS', 'HTTPS / TLS'],
    ['系统设置', '过滤设置键'],
  ]
  await page.goto('/settings/appearance')
  for (const [label, marker] of tabs) {
    await page.locator('.section-sidebar a').filter({ hasText: label }).click()
    await expect(page).toHaveURL(new RegExp(`/settings/([a-z]+)$`))
    await expect(page.locator('.section-content')).toContainText(marker, { timeout: 10_000 })
  }
})

// ---------------------------------------------------------------- 修复 8
test('8 主题不漂移：emerald 高频导航/弹窗 30 轮后不变', async ({ page }) => {
  await openAuthed(page, '/')
  await page.evaluate(() => {
    localStorage.setItem('docflow.theme', JSON.stringify({ accent: 'emerald', mode: 'dark' }))
  })
  await page.goto('/')
  await page.waitForLoadState('networkidle')

  const theme = () => page.evaluate(() => document.documentElement.dataset.theme)
  expect(await theme()).toBe('emerald')

  // antd cssVar 主色（primary 按钮背景）
  const primaryColor = () =>
    page.evaluate(() => {
      const el = document.querySelector<HTMLElement>('.ant-btn-primary')
      return el ? getComputedStyle(el).backgroundColor : ''
    })
  const before = await primaryColor()
  expect(before, 'primary 按钮应有 accent 色').not.toBe('')

  for (let i = 0; i < 10; i++) {
    // SPA 内导航（顶栏链接）：内存 access token 保留，无整页重载。
    await page.locator('.nav a[href="/spaces"]').click()
    await page.waitForSelector('.team-grid, .empty', { timeout: 10_000 })
    await page.locator('.nav a[href="/shared"]').click()
    await page.waitForLoadState('domcontentloaded')
    await page.locator('.nav a[href="/"]').click()
    await page.waitForSelector('.files-tree-main', { timeout: 10_000 })
    // 开弹窗（新建 → 文件夹）再关闭
    await page.getByRole('button', { name: /新\s*建/ }).first().click()
    await page.locator('.ant-dropdown-menu-item').filter({ hasText: '文件夹' }).click()
    await expect(page.locator('.ant-modal:visible').first()).toBeVisible()
    await page.keyboard.press('Escape')
    await page.waitForTimeout(200)
  }

  expect(await theme(), '30 轮高频操作后 accent 不应漂移').toBe('emerald')
  expect(await primaryColor(), 'antd cssVar 主色不应漂移').toBe(before)
})

// ---------------------------------------------------------------- 修复 9
test('9 人员与组：antd Menu 左树 + 图标按钮 + 无包裹边框', async ({ page }) => {
  const r = R()
  const g = await api.post('/api/v1/admin/groups', { headers: H(), data: { name: `V25G-${r}`, description: '' } })
  expect(g.ok(), await g.text()).toBeTruthy()

  await openAuthed(page, '/admin/people')
  await page.waitForLoadState('networkidle')

  // 左树 = antd Menu
  const menu = page.locator('.people-group-menu.ant-menu')
  await expect(menu).toBeVisible()
  await expect(menu).toContainText('全部用户')
  await expect(menu).toContainText(`V25G-${r}`)

  // 组行图标按钮（Edit3/Trash2 svg），无「改」「删」文字按钮
  const groupItem = menu.locator('.ant-menu-item').filter({ hasText: `V25G-${r}` })
  await expect(groupItem.locator('.people-group-action svg')).toHaveCount(2)
  expect(await menu.locator('button').filter({ hasText: /^改$/ }).count(), '不应再有「改」文字按钮').toBe(0)
  expect(await menu.locator('button').filter({ hasText: /^删$/ }).count(), '不应再有「删」文字按钮').toBe(0)

  // 无包裹边框（旧卡片边框已去除）
  const borderWidth = await page.evaluate(() => getComputedStyle(document.querySelector('.people-group-tree')!).borderWidth)
  expect(borderWidth, '左树容器不应有包裹边框').toBe('0px')

  // 点选组过滤右表
  await groupItem.click()
  await page.waitForTimeout(400)
  await expect(groupItem).toHaveClass(/ant-menu-item-selected/)
})

// ---------------------------------------------------------------- 修复 10
test('10 成员栏组行展开显示组内成员', async ({ page }) => {
  const r = R()
  // 组 + admin 入组 + 新空间 + 组入空间
  const g = await api.post('/api/v1/admin/groups', { headers: H(), data: { name: `V25G-${r}`, description: '' } })
  expect(g.ok(), await g.text()).toBeTruthy()
  const groupId = (await g.json()).id
  const me = await api.get('/api/v1/me', { headers: H() })
  const meId = (await me.json()).id
  const add = await api.post(`/api/v1/admin/groups/${groupId}/members`, { headers: H(), data: { user_id: meId } })
  expect(add.ok(), await add.text()).toBeTruthy()
  const sp = await api.post('/api/v1/spaces', { headers: H(), data: { name: `V25S-${r}`, description: '' } })
  expect(sp.ok()).toBeTruthy()
  const spaceId = (await sp.json()).id
  const ag = await api.post(`/api/v1/spaces/${spaceId}/groups`, { headers: H(), data: { group_id: groupId, role: 'member' } })
  expect(ag.ok(), await ag.text()).toBeTruthy()

  await openAuthed(page, `/files?space=${spaceId}`)
  await page.waitForLoadState('networkidle')
  // 诊断：页面与成员栏状态
  console.log('T10 page url:', page.url(), JSON.stringify(await page.evaluate(() => ({
    hasAside: Boolean(document.querySelector('.workspace-aside')),
    hasPanel: Boolean(document.querySelector('.member-panel')),
    panelText: document.querySelector('.member-panel')?.textContent?.slice(0, 120) ?? '',
  }))))

  // 右侧成员栏：组行（caret）点击展开 → 显示组内成员（admin）
  const groupRow = page.locator('.member-group-row').filter({ hasText: `V25G-${r}` })
  await expect(groupRow).toBeVisible()
  await expect(groupRow).toContainText('1 人')
  await groupRow.locator('.member-info-btn').click()
  const members = page.locator('.member-group-members')
  await expect(members).toBeVisible()
  await expect(members).toContainText('admin')
  // 组内成员行有头像 + 点击可弹用户信息
  await expect(members.locator('.member-avatar').first()).toBeVisible()
  await members.locator('.member-info-btn').first().click()
  await expect(page.locator('.ant-modal').filter({ hasText: /admin/ })).toBeVisible()
})



