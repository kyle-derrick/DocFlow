// E2E 后端启动器（playwright webServer 使用）：
//   校验 E2E_* 环境 → go run ./cmd/migrate（幂等迁移）→ go run ./cmd/seed
//   （幂等 upsert 管理员）→ go build 出独立二进制并启动（避免 go run 孙进程
//   在 teardown 时成为孤儿）→ 随本进程退出而终止。
//
// 环境变量：
//   E2E_DATABASE_URL    必填，指向已备好的 PostgreSQL（空则 fail fast 退出）
//   E2E_JWT_SECRET      可选，默认 config.json 的 defaultJWTSecret（仅测试用）
//   E2E_SEED_EMAIL      可选，默认 config.json 的 defaultSeedEmail
//   E2E_SEED_PASSWORD   可选，默认 config.json 的 defaultSeedPassword
//   E2E_STORAGE_ROOT    可选，后端本地存储根目录（默认 <repo>/storage/e2e）
import { spawn, spawnSync } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { cfg } from './config.mjs'

const e2eDir = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(e2eDir, '..', '..')

const databaseURL = process.env.E2E_DATABASE_URL || ''
if (!databaseURL) {
  console.error(
    '[e2e] 缺少 E2E_DATABASE_URL：E2E 需要真实 PostgreSQL。\n' +
      '[e2e] 本机准备一个（示例，数据可复用）:\n' +
      '[e2e]   docker run -d --name docflow-e2e-pg -p 5432:5432 \\\n' +
      "[e2e]     -e POSTGRES_DB=docflow -e POSTGRES_USER=docflow -e POSTGRES_PASSWORD=change-me postgres:16-alpine\n" +
      '[e2e]   export E2E_DATABASE_URL="postgres://docflow:change-me@localhost:5432/docflow?sslmode=disable"\n' +
      '[e2e]   # Windows PowerShell: $env:E2E_DATABASE_URL="postgres://docflow:change-me@localhost:5432/docflow?sslmode=disable"',
  )
  process.exit(1)
}
const jwtSecret = process.env.E2E_JWT_SECRET || cfg.defaultJWTSecret
const seedEmail = process.env.E2E_SEED_EMAIL || cfg.defaultSeedEmail
const seedPassword = process.env.E2E_SEED_PASSWORD || cfg.defaultSeedPassword

function runGo(args, extraEnv, label) {
  console.log(`[e2e] ${label}: go ${args.join(' ')}`)
  const result = spawnSync('go', args, {
    cwd: repoRoot,
    env: { ...process.env, ...extraEnv },
    stdio: 'inherit',
  })
  if (result.status !== 0) {
    console.error(`[e2e] ${label} 失败（exit ${result.status}），中止`)
    process.exit(result.status ?? 1)
  }
}

// 1) 迁移（cmd/migrate 幂等，可重复执行）。
runGo(['run', './cmd/migrate'], { DATABASE_URL: databaseURL }, '执行迁移')
// 2) 种子管理员（cmd/seed 对 email 幂等 upsert，密码以本次输入为准）。
runGo(
  ['run', './cmd/seed'],
  { DATABASE_URL: databaseURL, SEED_ADMIN_EMAIL: seedEmail, SEED_ADMIN_PASSWORD: seedPassword },
  '种子管理员',
)

// 3) 构建独立后端二进制（直接 spawn，保证 teardown 时进程可被可靠终止）。
const binary = path.join(
  tmpdir(),
  `docflow-e2e-server-${process.pid}${process.platform === 'win32' ? '.exe' : ''}`,
)
runGo(['build', '-o', binary, './cmd/server'], {}, '构建后端')

const storageRoot = process.env.E2E_STORAGE_ROOT || path.join(repoRoot, 'storage', 'e2e')
mkdirSync(storageRoot, { recursive: true })

// 4) 启动后端：cookie 须为非 Secure（http://localhost:5173）；关闭扫描与
//    各类限流（E2E 高频登录/上传不受固定窗口限流干扰）。
const child = spawn(binary, [], {
  cwd: repoRoot,
  env: {
    ...process.env,
    DATABASE_URL: databaseURL,
    JWT_SECRET: jwtSecret,
    PORT: String(cfg.backendPort),
    COOKIE_SECURE: 'false',
    STORAGE_DRIVER: 'local',
    STORAGE_ROOT: storageRoot,
    SCAN_ENABLED: 'false',
    ONLYOFFICE_ENABLED: 'false',
    RATE_LIMIT_PER_MIN: '0',
    LOGIN_RATE_LIMIT_PER_MIN: '0',
    PUBLIC_RATE_LIMIT_PER_MIN: '0',
    METRICS_ENABLED: 'true',
  },
  stdio: 'inherit',
})
console.log(`[e2e] 后端已启动 pid=${child.pid} port=${cfg.backendPort} storage=${storageRoot}`)

let stopping = false
const stop = () => {
  if (stopping || child.exitCode !== null) return
  stopping = true
  child.kill('SIGTERM')
  setTimeout(() => child.kill('SIGKILL'), 5000).unref()
}
process.on('SIGINT', stop)
process.on('SIGTERM', stop)
process.on('exit', stop)
child.on('exit', (code) => {
  console.log(`[e2e] 后端退出 code=${code}`)
  process.exit(code ?? 1)
})
