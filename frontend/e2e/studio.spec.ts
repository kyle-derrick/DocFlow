// AI 创作空间（/studio）冒烟 E2E（serial）：
// 经管理 API 注入 Mock Provider（capabilities.reasoning=true）→ 重载启用
// 全站 AI 入口 → 新建项目（platform 引擎）→ 右栏与 AI 对话一轮（mock 回复
// + 思考过程折叠区出现）→ 顶栏新建 Markdown（中栏 Tab 打开 Monaco 编辑）→
// 查看|编辑切换 → 关闭 Tab 后中栏收起、AI 面板延展。
import { test, expect, type Page } from '@playwright/test'
import { e2eSeed, loginViaUI } from './helpers'

const stamp = Date.now().toString(36)
const projectName = `e2eStudio_${stamp}`

/** 经管理 API 注入 Mock Provider（幂等）：模型勾选 reasoning 以驱动思考链路。
 *  API 鉴权走 Bearer access token（内存态，与前端 api() 同口径；cookie 只有
 *  refresh/csrf，故先经登录端点换 token 再 PUT）。 */
async function seedMockAI(page: Page): Promise<void> {
  const login = await page.request.post('/api/v1/auth/login', {
    data: { identifier: e2eSeed().email, password: e2eSeed().password },
  })
  const { access_token: token } = (await login.json()) as { access_token?: string }
  const res = await page.request.put('/api/v1/admin/settings/ai', {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    data: {
      providers: [
        {
          id: 'e2e-mock',
          name: 'E2E Mock',
          kind: 'mock',
          enabled: true,
          models: [{ id: 'e2e-m', capabilities: { kind: 'chat', reasoning: true } }],
        },
      ],
      default_provider: 'e2e-mock',
      default_models: { chat: { provider_id: 'e2e-mock', model_id: 'e2e-m' } },
      temperature: 0.3,
      max_tokens: 256,
      per_user_per_min: 60,
      // rag 块整体覆盖：top_k/chunk_size/chunk_overlap 有硬校验（1-10 /
      // 100-10000 / 0<chunk_size），须带完整合法值（vector 关闭时无需
      // qdrant/embedding）。
      rag: { mode: 'keyword', top_k: 8, chunk_size: 1000, chunk_overlap: 100 },
    },
  })
  if (!res.ok()) {
    const body = await res.text()
    throw new Error(`seedMockAI PUT 失败：HTTP ${res.status()} ${body.slice(0, 300)}`)
  }
}

/** 登录 + 注入 AI 后重载（前端 /ai/status 模块缓存随页面重载重置）。 */
async function loginWithAI(page: Page): Promise<void> {
  await loginViaUI(page)
  await seedMockAI(page)
  await page.reload()
  await expect(page.locator('.user-menu-trigger')).toBeVisible()
}

/** 新建项目（项目清单为浏览器 localStorage 态——`docflow.studio.projects.<uid>`，
 *  不跨用例上下文共享，故每个用例独立创建；服务端产物为空间目录与文件）。 */
async function createProject(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: '新建项目', exact: true }).click()
  await page.getByPlaceholder('如：产品官网').fill(name)
  await page.getByRole('button', { name: /创\s*建/ }).click()
  await expect(page.locator('.studio-topbar .studio-proj-dd .name')).toHaveText(name)
  await expect(page.locator('.studio-left .ftree')).toBeVisible()
}

test.describe.serial('AI 创作空间冒烟', () => {
  test('启用 AI 后可进入创作空间并新建项目', async ({ page }) => {
    await loginWithAI(page)
    await page.goto('/studio')
    // 无项目：右栏引导新建。
    await page.getByRole('button', { name: '新建项目', exact: true }).click()
    await page.getByPlaceholder('如：产品官网').fill(projectName)
    await page.getByRole('button', { name: /创\s*建/ }).click()
    // 顶栏出现项目名；左栏目录树挂载（项目根）。
    await expect(page.locator('.studio-topbar .studio-proj-dd .name')).toHaveText(projectName)
    await expect(page.locator('.studio-left .ftree')).toBeVisible()
  })

  test('AI 对话一轮：mock 回复与思考过程折叠区', async ({ page }) => {
    await loginWithAI(page)
    await page.goto('/studio')
    await createProject(page, `${projectName}-chat`)
    // 输入框在；Enter 发送（IME 语义由组件处理，测试环境直接回车）。
    const box = page.getByPlaceholder(/描述任务|Describe the task/)
    await box.fill(`e2e 打招呼 ${stamp}`)
    await box.press('Enter')
    // mock 回复气泡出现（思考开启 + reasoning 模型 → 思考折叠区先行出现）。
    await expect(page.locator('.aic-bubble-ai').first()).toContainText('Mock', { timeout: 30_000 })
    await expect(page.locator('.aic-thinking').first()).toBeVisible()
  })

  test('新建 Markdown 在中栏 Tab 打开编辑并可切换查看', async ({ page }) => {
    await loginWithAI(page)
    await page.goto('/studio')
    await createProject(page, `${projectName}-md`)
    // 顶栏快捷「新建 Markdown」。
    await page.getByRole('button', { name: '新建 Markdown', exact: true }).click()
    // 中栏出现编辑 Tab（Monaco 编辑器挂载）。
    await expect(page.locator('.stab').first()).toBeVisible({ timeout: 20_000 })
    await expect(page.locator('.monaco-editor').first()).toBeVisible({ timeout: 20_000 })
    // 查看|编辑切换：切到查看后编辑器被查看器替换（Tab 仍在）。antd Segmented
    // 的原生 radio input 是隐藏元素，点击可见的分段项本体。
    await page.locator('.stab .ant-segmented-item', { hasText: /查看|View/ }).click()
    await expect(page.locator('.stab .ant-segmented-item-selected')).toContainText(/查看|View/)
    // 关闭 Tab：中栏收起（.stab 消失，AI 面板延展占满）。
    await page.locator('.stab-close').first().click()
    await expect(page.locator('.stab')).toHaveCount(0)
    await expect(page.locator('.studio-right')).toBeVisible()
  })
})
