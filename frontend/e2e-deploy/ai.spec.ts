// AI 基座（L1 平台原生 AI）部署验收（临时测试，跑完即删）。
// 部署环境 http://localhost（admin@example.com）。数据准备走 API（Bearer），
// 行为断言走浏览器 UI；AI 能力用内置 Mock Provider 全链路验证流式 UI。
import { test, expect, request, type APIRequestContext, type Page } from '@playwright/test'

const EMAIL = 'admin@example.com'
const PASSWORD = 'AdminPassword123'
const R = () => Math.random().toString(36).slice(2, 8)

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

async function loginUI(page: Page) {
  await page.goto('/login')
  await page.getByLabel(/邮箱/).first().fill(EMAIL)
  await page.getByLabel('密码').fill(PASSWORD)
  await page.getByRole('button', { name: /^登\s*录$/ }).click()
  await expect(page).toHaveURL(/\/(files)?$/)
}

let savedCookies: Array<{ name: string; value: string; domain: string; path: string; expires: number; httpOnly: boolean; secure: boolean; sameSite: 'Strict' | 'Lax' | 'None' }> | null = null

async function openAuthed(page: Page, path: string) {
  const tryOpen = async () => {
    await page.context().addCookies(savedCookies!)
    await page.goto(path)
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
    await loginUI(page)
    savedCookies = await page.context().cookies()
    await page.goto(path)
  }
  await page.waitForLoadState('domcontentloaded')
  savedCookies = await page.context().cookies()
}

/** 设置 AI 配置（整体覆盖；api_key 留空 = 保持现值）。 */
async function putAI(body: unknown) {
  const res = await api.put('/api/v1/admin/settings/ai', { headers: H(), data: body })
  expect(res.ok(), `putAI: ${res.status()} ${await res.text()}`).toBeTruthy()
  return await res.json()
}

const mockCfg = () => ({
  enabled: true,
  providers: [{ id: 'mockdemo', name: 'Mock 演示', kind: 'mock', model: 'mock-echo', enabled: true }],
  default_provider: 'mockdemo',
  temperature: 0.3,
  max_tokens: 2048,
  per_user_per_min: 100,
})

// ---------------------------------------------------------------- 1 未配置态
test.describe.serial('AI 基座验收', () => {
  test('1 未配置 Provider：全站入口隐藏 + 网关 404 + MCP 工具不注册', async ({ page }) => {
    // 清空全部 Provider（幂等基线；enabled 缺省 = 保持现值无碍）。
    await putAI({ providers: [], default_provider: '', temperature: 0.3, max_tokens: 2048, per_user_per_min: 100 })

    const status = await api.get('/api/v1/ai/status', { headers: H() })
    expect(status.ok()).toBeTruthy()
    expect((await status.json()).enabled).toBe(false)

    const chat = await api.post('/api/v1/ai/chat', { headers: H(), data: { messages: [{ role: 'user', content: 'hi' }] } })
    expect(chat.status()).toBe(404)

    const summarize = await api.post('/api/v1/ai/summarize', { headers: H(), data: { fileId: '00000000-0000-0000-0000-000000000000' } })
    expect(summarize.status()).toBe(404)

    const tools = await api.post('/mcp', { headers: H(), data: { jsonrpc: '2.0', id: 1, method: 'tools/list' } })
    const list = ((await tools.json()).result.tools) as Array<{ name: string; description: string }>
    const ask = list.find((t) => t.name === 'ask_docs')
    expect(ask, 'ask_docs 应在工具清单（描述标注未启用）').toBeTruthy()
    expect(ask!.description).toContain('未启用')
    const sum = list.find((t) => t.name === 'summarize_file')
    expect(sum!.description).toContain('未启用')

    // UI：顶栏无 AI 助手入口、搜索框旁无「问 AI」。
    await openAuthed(page, '/')
    await expect(page.getByLabel('AI 助手')).toHaveCount(0)
    await expect(page.getByRole('button', { name: /问 AI/ })).toHaveCount(0)
  })

  // ------------------------------------------- 2 管理面板 + mock + 测试连接
  test('2 管理后台 AI 设置：mock Provider 保存 + 测试连接成功', async ({ page }) => {
    await putAI(mockCfg())
    const status = await api.get('/api/v1/ai/status', { headers: H() })
    expect((await status.json()).enabled).toBe(true)

    await openAuthed(page, '/admin/ai')
    // Provider 卡片出现（mockdemo）。
    const card = page.locator('.ai-provider-card').filter({ hasText: 'Mock 演示' })
    await expect(card).toBeVisible()
    // 测试连接：mock → ok（Zap 图标按钮 + .ai-test-result 文案）。
    await card.locator('.ai-provider-actions button').first().click()
    await expect(card.locator('.ai-test-result').filter({ hasText: /连接成功/ })).toBeVisible({ timeout: 15000 })
  })

  // ------------------------------------------- 3 假 Provider 测试连接失败提示
  test('3 假 Provider：测试连接失败提示正确（不可达上游）', async ({ page }) => {
    // 追加一个指向不可达地址的 openai_compatible（保留 mock 为默认）。
    await putAI({
      ...mockCfg(),
      providers: [
        ...mockCfg().providers,
        { id: 'fake1', name: '假网关', kind: 'openai_compatible', base_url: 'http://127.0.0.1:9/v1', model: 'gpt-x', enabled: true },
      ],
    })
    await openAuthed(page, '/admin/ai')
    const card = page.locator('.ai-provider-card').filter({ hasText: '假网关' })
    await expect(card).toBeVisible()
    await card.locator('.ai-provider-actions button').first().click()
    // 失败提示：.ai-test-result 含「连接失败」与上游错误详情。
    await expect(card.locator('.ai-test-result').filter({ hasText: /连接失败/ })).toBeVisible({ timeout: 25000 })
    // 清理假网关（保留 mock）。
    await putAI(mockCfg())
  })

  // ------------------------------------------- 4 搜索「问 AI」抽屉（RAG+流式+来源）
  test('4 全局搜索「问 AI」：抽屉流式回答 + 引用来源', async ({ page }) => {
    await openAuthed(page, '/')
    // 上传一个含独特关键词的 md 文件（UI 上传链路）。
    const kw = `量子协同加速器${R()}`
    const name = `AI-RAG-${R()}.md`
    await page.locator('input[type="file"]').first().setInputFiles({
      name,
      mimeType: 'text/markdown',
      buffer: Buffer.from(`# ${kw} 运维手册\n\n该设备用于束流诊断，季度检修包含真空腔清洁。\n`, 'utf8'),
    })
    // 等上传完成（列表行出现 = available）。
    await expect(page.locator('tbody tr').filter({ hasText: name }).first()).toBeVisible({ timeout: 30000 })
    // 等全文索引（meili 异步；轮询搜索 API 直至命中）。
    for (let i = 0; i < 20; i++) {
      const res = await api.get(`/api/v1/search?q=${encodeURIComponent(kw)}`, { headers: H() })
      if (res.ok() && ((await res.json()).results ?? []).some((r: { name: string }) => r.name === name)) break
      await page.waitForTimeout(1000)
    }
    // 入口出现：顶栏 Sparkles + 搜索框旁「问 AI」。
    await expect(page.getByLabel('AI 助手')).toBeVisible()
    // 输入问题 → 点「问 AI」。
    await page.locator('[data-hotkey="search"]').fill(`${kw} 是做什么的？`)
    await page.getByRole('button', { name: /问 AI/ }).click()
    // 抽屉打开 + 流式回答（Mock echo 逐块输出）+ 来源列表（检索命中）。
    await expect(page.locator('.ai-drawer')).toBeVisible()
    await expect(page.locator('.ai-turn-assistant').filter({ hasText: /Mock|量子/ }).first()).toBeVisible({ timeout: 20000 })
    await expect(page.locator('.ai-source-link').first()).toBeVisible({ timeout: 10000 })
    await expect(page.locator('.ai-source-link').filter({ hasText: name })).toBeVisible()
  })

  // ------------------------------------------- 5 编辑器 AI 菜单（摘要/流式/插入）
  test('5 md 编辑页：AI 菜单出现 + 摘要流式 + 插入', async ({ page }) => {
    // 找 4 中上传的文件 id（按名称搜索）。
    const res = await api.get(`/api/v1/search?q=${encodeURIComponent('AI-RAG-')}`, { headers: H() })
    const hits = (await res.json()).results as Array<{ id: string; name: string }>
    const file = hits.find((f) => f.name.startsWith('AI-RAG-'))
    expect(file, 'RAG 测试文件应可检索').toBeTruthy()

    await openAuthed(page, `/markdown/${file!.id}`)
    // 工具栏 AI 按钮 + 菜单。
    await page.getByRole('button', { name: /^AI$/ }).click()
    await page.getByRole('menuitem', { name: '摘要' }).click()
    // 流式预览：Mock 输出（编辑器摘要走通用对话提示，echo 回复）。
    await expect(page.locator('.ai-edit-output').filter({ hasText: /Mock/ })).toBeVisible({ timeout: 20000 })
    // 插入（antd6 对两字按钮自动插入空格「插 入」；等流式完成按钮可点）。
    await page.getByRole('button', { name: /插\s*入/ }).click()
    await expect(page.locator('.ai-edit-output')).toHaveCount(0)
  })

  // ------------------------------------------- 6 右键菜单「AI 摘要」弹窗
  test('6 文件页右键「AI 摘要」：弹窗流式摘要', async ({ page }) => {
    await openAuthed(page, '/')
    const res = await api.get(`/api/v1/search?q=${encodeURIComponent('AI-RAG-')}`, { headers: H() })
    const hits = (await res.json()).results as Array<{ id: string; name: string }>
    const name = hits[0].name
    const row = page.locator('tbody tr').filter({ hasText: name }).first()
    await expect(row).toBeVisible()
    await row.click({ button: 'right' })
    await page.getByRole('menuitem', { name: 'AI 摘要' }).click()
    // 弹窗出现并流式输出 Mock 摘要。
    await expect(page.locator('.ai-summary-dialog').filter({ hasText: /Mock 摘要/ })).toBeVisible({ timeout: 20000 })
    await page.keyboard.press('Escape')
  })

  // ------------------------------------------- 5b 顶栏 AI 助手：多轮流式对话
  test('5b 顶栏 AI 助手：Drawer 多轮对话流式渲染', async ({ page }) => {
    await openAuthed(page, '/')
    // 顶栏 Sparkles 入口打开助手 Drawer。
    await page.getByLabel('AI 助手').click()
    await expect(page.locator('.ai-drawer')).toBeVisible()
    const composer = page.locator('.ai-input-row textarea')
    // 第一轮。
    await composer.fill('第一轮：介绍你自己');
    await composer.press('Enter')
    await expect(page.locator('.ai-turn-assistant').filter({ hasText: /Mock/ }).first()).toBeVisible({ timeout: 20000 })
    // 第二轮（多轮）：mock echo 复述第二轮提问。
    await composer.fill('第二轮：多轮验证')
    await page.locator('.ai-input-row button[aria-label]').last().click()
    await expect(page.locator('.ai-turn-assistant').filter({ hasText: '第二轮：多轮验证' })).toBeVisible({ timeout: 20000 })
    await expect(page.locator('.ai-turn-user')).toHaveCount(2)
  })

  // ------------------------------------------- 5c md 编辑页：选区润色 + 替换 + 重新生成
  test('5c md 编辑页：选中文本 → AI 润色 → 流式 → 重新生成 → 替换选中', async ({ page }) => {
    const res = await api.get(`/api/v1/search?q=${encodeURIComponent('AI-RAG-')}`, { headers: H() })
    const hits = (await res.json()).results as Array<{ id: string; name: string }>
    const file = hits.find((f) => f.name.startsWith('AI-RAG-'))
    expect(file, 'RAG 测试文件应可检索').toBeTruthy()

    await openAuthed(page, `/markdown/${file!.id}`)
    // Monaco 选中文本（点击可见编辑区聚焦 → 全选形成选区）。
    await page.locator('.monaco-editor .view-lines').first().click()
    await page.keyboard.press('Control+a')
    await page.getByRole('button', { name: /^AI$/ }).click()
    await page.getByRole('menuitem', { name: '润色' }).click()
    // 弹窗标题标注作用于选区。
    await expect(page.locator('.modal-card, .ant-modal').filter({ hasText: /作用于选区/ }).first()).toBeVisible({ timeout: 10000 })
    // 流式输出出现。
    await expect(page.locator('.ai-edit-output').filter({ hasText: /Mock/ })).toBeVisible({ timeout: 20000 })
    // 重新生成：清空后重新流式输出。
    await page.getByRole('button', { name: '重新生成' }).click()
    await expect(page.locator('.ai-edit-output').filter({ hasText: /Mock/ })).toBeVisible({ timeout: 20000 })
    // 替换选中：全文被替换为 mock 回复（Monaco 内容变化）。
    await page.getByRole('button', { name: /替\s*换/ }).click()
    await expect(page.locator('.ai-edit-output')).toHaveCount(0)
    await expect(page.locator('.monaco-editor').first()).toContainText('Mock AI 回复', { timeout: 10000 })
  })

  // ------------------------------------------- 5d dfrt（Tiptap）编辑页：AI 插入/替换
  test('5d dfrt 富文本编辑页：AI 摘要插入 + 选区替换（Tiptap insertContentAt）', async ({ page }) => {
    // 上传一个最小 Tiptap JSON 的 .dfrt 文档。
    const name = `AI-RT-${R()}.dfrt`
    const doc = JSON.stringify({ type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: '富文本 AI 验证原始段落。' }] }] })
    await openAuthed(page, '/')
    await page.locator('input[type="file"]').first().setInputFiles({
      name, mimeType: 'application/json', buffer: Buffer.from(doc, 'utf8'),
    })
    await expect(page.locator('tbody tr').filter({ hasText: name }).first()).toBeVisible({ timeout: 30000 })
    // 等全文索引（meili 异步；轮询搜索 API 直至命中）。
    let file: { id: string; name: string } | undefined
    for (let i = 0; i < 20; i++) {
      const res = await api.get(`/api/v1/search?q=${encodeURIComponent('AI-RT-')}`, { headers: H() })
      if (res.ok()) {
        const hits = (await res.json()).results as Array<{ id: string; name: string }>
        file = hits.find((f) => f.name === name)
        if (file) break
      }
      await page.waitForTimeout(1000)
    }
    expect(file, '.dfrt 文件应可检索').toBeTruthy()

    await openAuthed(page, `/dfdoc/${file!.id}`)
    // 工具栏 AI 按钮（无选区 → 全文摘要 → 插入）。
    await page.getByRole('button', { name: /^AI$/ }).click()
    await page.getByRole('menuitem', { name: '摘要' }).click()
    await expect(page.locator('.ai-edit-output').filter({ hasText: /Mock/ })).toBeVisible({ timeout: 20000 })
    // 富文本工具栏自带「插入链接/插入」按钮，故限定弹窗内的操作按钮。
    const modalActions = page.locator('.docflow-modal .modal-actions')
    await modalActions.getByRole('button', { name: /插\s*入/ }).click()
    await expect(page.locator('.ai-edit-output')).toHaveCount(0)
    await expect(page.locator('.ProseMirror').first()).toContainText('Mock', { timeout: 10000 })

    // 选区替换：点击段落 → 全选 → 润色 → 替换。
    await page.locator('.ProseMirror p').first().click()
    await page.keyboard.press('Control+a')
    await page.getByRole('button', { name: /^AI$/ }).click()
    await page.getByRole('menuitem', { name: '润色' }).click()
    await expect(page.locator('.modal-card, .ant-modal').filter({ hasText: /作用于选区/ }).first()).toBeVisible({ timeout: 10000 })
    await expect(page.locator('.ai-edit-output').filter({ hasText: /Mock/ })).toBeVisible({ timeout: 20000 })
    await modalActions.getByRole('button', { name: /替\s*换/ }).click()
    await expect(page.locator('.ai-edit-output')).toHaveCount(0)
    // 原段落被替换（不再含「原始段落」字样）。
    await expect(page.locator('.ProseMirror').first()).not.toContainText('原始段落', { timeout: 10000 })
  })

  // ------------------------------------------- 7 MCP ask_docs / summarize_file
  test('7 MCP：AI 工具注册（ask_docs/summarize_file/ai_chat）+ ask_docs 调用', async ({ page }) => {
    const tools = await api.post('/mcp', { headers: H(), data: { jsonrpc: '2.0', id: 1, method: 'tools/list' } })
    const list = ((await tools.json()).result.tools) as Array<{ name: string; description: string }>
    for (const want of ['ask_docs', 'summarize_file', 'ai_chat']) {
      const tool = list.find((t) => t.name === want)
      expect(tool, `MCP 应注册 ${want}`).toBeTruthy()
      expect(tool!.description, `${want} 启用态不应标注未启用`).not.toContain('未启用')
    }
    const call = await api.post('/mcp', {
      headers: H(),
      data: { jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: 'ask_docs', arguments: { query: '量子协同加速器 运维' } } },
    })
    const body = await call.json()
    expect(call.ok()).toBeTruthy()
    expect(body.error).toBeUndefined()
    expect(body.result.isError).toBeFalsy()
    const text = (body.result.content as Array<{ text: string }>).map((c) => c.text).join('\n')
    expect(text).toContain('Mock')
    expect(text).toContain('来源')
  })

  // ------------------------------------------- 7b PAT 鉴权调 ask_docs（space_id/top_k 参数）
  test('7b PAT 调 ask_docs：答案 + 引用；space_id 过滤生效', async () => {
    // 创建仅含 ai:chat scope 的 PAT。
    const created = await api.post('/api/v1/tokens', { headers: H(), data: { name: `ai-e2e-${R()}`, scopes: ['ai:chat'] } })
    expect(created.ok(), `create PAT: ${created.status()}`).toBeTruthy()
    const pat = ((await created.json()).token) as string
    expect(pat).toMatch(/^dfpat_/)

    const call = (args: Record<string, unknown>) =>
      api.post('/mcp', { headers: { Authorization: `Bearer ${pat}` }, data: { jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'ask_docs', arguments: args } } })
    // 默认：答案 + 来源（命中 4 中上传的运维手册）。
    const r1 = await (await call({ query: '量子协同加速器 运维', top_k: 5 })).json()
    expect(r1.error).toBeUndefined()
    expect(r1.result.isError).toBeFalsy()
    const text1 = (r1.result.content as Array<{ text: string }>).map((c) => c.text).join('\n')
    expect(text1).toContain('Mock')
    expect(text1).toContain('来源')
    // space_id 指向不存在空间：无来源（检索被空间过滤清空）。
    const r2 = await (await call({ query: '量子协同加速器 运维', space_id: '00000000-0000-0000-0000-000000000001' })).json()
    expect(r2.error).toBeUndefined()
    const payload2 = JSON.parse((r2.result.content as Array<{ text: string }>)[0].text)
    expect(payload2.sources).toHaveLength(0)
    // 非法 space_id：参数错误。
    const r3 = await (await call({ query: 'x', space_id: 'not-a-uuid' })).json()
    expect(r3.error?.message ?? '').toContain('space_id')
  })

  // ------------------------------------------- 8 用量统计（ai_usage 落库）
  test('8 用量统计：对话后 ai_usage 有记录', async ({ page }) => {
    await api.post('/api/v1/ai/chat', {
      headers: H(),
      data: { stream: false, messages: [{ role: 'user', content: '用量统计探针' }] },
    })
    const usage = await api.get('/api/v1/admin/ai/usage', { headers: H() })
    expect(usage.ok()).toBeTruthy()
    const data = await usage.json()
    expect(data.total.calls).toBeGreaterThanOrEqual(1)
    expect(data.total.tokens).toBeGreaterThanOrEqual(1)
    // UI：管理面板用量表有行。
    await openAuthed(page, '/admin/ai')
    await expect(page.locator('.ai-admin-panel table tbody tr').first()).toBeVisible({ timeout: 15000 })
  })

  // ------------------------------------------- 9 总开关关闭 → 全站隐藏 + 404
  test('9 总开关 ai.enabled 关闭：入口隐藏 + 网关 404（配置保留）', async ({ page }) => {
    await openAuthed(page, '/admin/ai')
    // 面板总开关当前 on；切到 off（触发保存）。
    const toggle = page.locator('.ai-master-toggle .ant-switch')
    await expect(toggle).toHaveClass(/ant-switch-checked/)
    await toggle.click()
    await expect(page.getByText('AI 设置已保存')).toBeVisible({ timeout: 10000 })
    // 网关 404 + status false。
    const status = await api.get('/api/v1/ai/status', { headers: H() })
    expect((await status.json()).enabled).toBe(false)
    const chat = await api.post('/api/v1/ai/chat', { headers: H(), data: { messages: [{ role: 'user', content: 'x' }] } })
    expect(chat.status()).toBe(404)
    // MCP AI 工具随之标注未启用。
    const tools = await api.post('/mcp', { headers: H(), data: { jsonrpc: '2.0', id: 1, method: 'tools/list' } })
    const list = ((await tools.json()).result.tools) as Array<{ name: string; description: string }>
    expect(list.find((t) => t.name === 'ask_docs')!.description).toContain('未启用')
    // UI 入口隐藏。
    await openAuthed(page, '/')
    await expect(page.getByLabel('AI 助手')).toHaveCount(0)
    await expect(page.getByRole('button', { name: /问 AI/ })).toHaveCount(0)
  })

  // ------------------------------------------- 10 恢复：重新开启（交付态 = mock 可演示）
  test('10 重新开启 AI：入口恢复', async ({ page }) => {
    await putAI(mockCfg())
    await openAuthed(page, '/')
    await expect(page.getByLabel('AI 助手')).toBeVisible()
    await expect(page.getByRole('button', { name: /问 AI/ })).toBeVisible()
  })
})
