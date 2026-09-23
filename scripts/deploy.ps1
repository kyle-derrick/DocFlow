[CmdletBinding()]
param(
    [ValidateSet('minimal', 'full')]
    [string]$Profile = 'minimal'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Invoke-Checked([string]$File, [string[]]$Arguments) {
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$File $($Arguments -join ' ') failed with exit code $LASTEXITCODE" }
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw '未检测到 Docker，请先安装并启动 Docker Desktop。'
}
Invoke-Checked 'docker' @('compose', 'version')

if (-not (Test-Path '.env')) {
    Copy-Item '.env.example' '.env'
    Write-Warning '已从 .env.example 创建 .env。请先设置 JWT_SECRET、SEED_ADMIN_PASSWORD 和 POSTGRES_PASSWORD 等密钥后重新运行。'
    exit 1
}

$envText = Get-Content '.env' -Raw
$placeholders = @('replace-with-at-least-32-random-bytes', 'change-this-password', 'CHANGE_ME', 'CHANGE_ME_')
$missing = $placeholders | Where-Object { $envText -match [regex]::Escape($_) }
if ($missing) {
    throw '检测到 .env 仍含模板密钥占位符，请设置 JWT_SECRET、SEED_ADMIN_PASSWORD、POSTGRES_PASSWORD 等密钥后重试。已有 .env 不会被覆盖。'
}

$profileArgs = @('--profile', $Profile)
if ($Profile -eq 'full') { $env:ONLYOFFICE_UPSTREAM = 'onlyoffice:80' }
Invoke-Checked 'docker' @('compose', 'config', '--quiet')
# Agent 创作舱默认镜像（Dockerfile.agent：node:20-alpine + git + agent-runner；
# 即 agent.DefaultImage 的 docflow/agent:1.0.0）随部署构建，跟随 deploy 流程。
Invoke-Checked 'docker' @('build', '-f', 'Dockerfile.agent', '-t', 'docflow/agent:1.0.0', '.')
Invoke-Checked 'docker' @('compose', 'build', 'backend', 'caddy', 'migrate', 'seed')
Invoke-Checked 'docker' (@('compose') + $profileArgs + @('up', '-d'))

Write-Host "部署完成（$Profile profile）。migrate/seed 已由 compose 按依赖顺序执行。"
Write-Host '访问地址：http://localhost/（配置 APP_DOMAIN 后使用对应域名/HTTPS）'
Write-Host '默认账号：SEED_ADMIN_EMAIL / SEED_ADMIN_USERNAME 与 .env 中的 SEED_ADMIN_PASSWORD；首次登录后请立即修改密码。'
