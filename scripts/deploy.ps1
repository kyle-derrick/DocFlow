[CmdletBinding()]
param(
    [ValidateSet('minimal', 'full')]
    [string]$Profile = 'minimal',
    # -Auto：无人值守部署——.env 缺失/含占位符时全部密钥随机生成（交互提示关闭）。
    [switch]$Auto
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

$genEnv = Join-Path $PSScriptRoot 'gen-env.ps1'
if (-not (Test-Path '.env')) {
    if ($Auto) {
        # -Auto：无人值守——.env 缺失时全部密钥随机生成后继续部署。
        Invoke-Checked 'powershell' @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $genEnv, '-Auto')
    } elseif (Test-Path $genEnv) {
        # 交互式初始化（每项可选生成/手动输入）；完成後继续本次部署。
        Invoke-Checked 'powershell' @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $genEnv)
    } else {
        Copy-Item '.env.example' '.env'
        Write-Warning '已从 .env.example 创建 .env。请先设置 JWT_SECRET、SEED_ADMIN_PASSWORD 和 POSTGRES_PASSWORD 等密钥后重新运行。'
        exit 1
    }
}

$envText = Get-Content '.env' -Raw -Encoding UTF8
$placeholders = @('replace-with-at-least-32-random-bytes', 'change-this-password', 'CHANGE_ME', 'CHANGE_ME_')
$missing = $placeholders | Where-Object { $envText -match [regex]::Escape($_) }
if ($missing) {
    # 占位符残留：交互式下提供「自动生成替换」选项（-Auto 直接替换），避免手动编辑。
    $patch = $true
    if (-not $Auto) {
        Write-Warning '检测到 .env 仍含模板密钥占位符。'
        $ans = Read-Host '回车 = 自动生成并替换全部占位密钥 / m = 我自己改完后重试'
        $patch = ($ans -ne 'm')
    }
    if ($patch -and (Test-Path $genEnv)) {
        $autoArgs = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $genEnv, '-Patch')
        if ($Auto) { $autoArgs += '-Auto' }
        Invoke-Checked 'powershell' $autoArgs
        $envText = Get-Content '.env' -Raw -Encoding UTF8
        $missing = $placeholders | Where-Object { $envText -match [regex]::Escape($_) }
        if ($missing) { throw "替换后仍有占位符残留：$($missing -join ', ')。请手动检查 .env。" }
    } else {
        throw '请设置 JWT_SECRET、SEED_ADMIN_PASSWORD、POSTGRES_PASSWORD 等密钥后重试。已有 .env 不会被覆盖。'
    }
}

$profileArgs = @('--profile', $Profile)
if ($Profile -eq 'full') { $env:ONLYOFFICE_UPSTREAM = 'onlyoffice:80' }
Invoke-Checked 'docker' @('compose', 'config', '--quiet')
Invoke-Checked 'docker' @('compose', 'build', 'backend', 'caddy', 'migrate', 'seed')
Invoke-Checked 'docker' (@('compose') + $profileArgs + @('up', '-d'))

Write-Host "部署完成（$Profile profile）。migrate/seed 已由 compose 按依赖顺序执行。"
Write-Host '访问地址：http://localhost/（配置 APP_DOMAIN 后使用对应域名/HTTPS）'
Write-Host '默认账号：SEED_ADMIN_EMAIL / SEED_ADMIN_USERNAME 与 .env 中的 SEED_ADMIN_PASSWORD；首次登录后请立即修改密码。'
