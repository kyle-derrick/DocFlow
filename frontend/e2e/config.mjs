// E2E 共享配置：setup-backend.mjs（Node ESM）与 playwright.config.ts /
// global-setup.ts / helpers.ts（Playwright 转译的 TS）同源引用。
// 默认值仅用于本地 E2E，请勿用于生产环境。
export const cfg = {
  backendURL: 'http://localhost:8080',
  backendPort: 8080,
  frontendBaseURL: 'http://localhost:5173',
  defaultJWTSecret: 'e2e-insecure-jwt-secret-please-change-32b',
  defaultSeedEmail: 'e2e-admin@docflow.test',
  defaultSeedPassword: 'E2eAdminPassw0rd!',
}
