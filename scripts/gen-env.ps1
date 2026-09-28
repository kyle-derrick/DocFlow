# .env 初始化器：从模板生成 .env，密钥按需「随机生成 / 手动输入」。
# 用法：
#   powershell -File scripts/gen-env.ps1                     # 交互式（每项可选生成/手动）
#   powershell -File scripts/gen-env.ps1 -Auto               # 全自动（全部随机生成，无人值守）
#   powershell -File scripts/gen-env.ps1 -Template deploy/env/.env.single-node.example
#   powershell -File scripts/gen-env.ps1 -Patch              # 现有 .env 仅替换占位符/缺失密钥
#   常用参数：-Force（允许覆盖已存在的 .env）
# 生成规则：
#   - POSTGRES_PASSWORD：20 位字母数字（同步重写 DATABASE_URL 内嵌密码）；
#   - JWT_SECRET / ONLYOFFICE_JWT_SECRET：32 字节 URL-safe 随机（≥32 字节要求）；
#   - SEED_ADMIN_PASSWORD：20 位字母数字；SEED_ADMIN_EMAIL/USERNAME 给默认值可改。
# 安全：.env 已被 gitignore；生成值仅在结束时汇总展示一次，请自行妥善备份。
[CmdletBinding()]
param(
    [string]$Template = '.env.example',
    [switch]$Auto,
    [switch]$Patch,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# ---------- 随机生成 ----------
$alnum = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789' # 去易混字符（l/1/I/0/O）

function New-Password([int]$Length = 20) {
    $bytes = New-Object byte[] $Length
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    -join ($bytes | ForEach-Object { $alnum[$_ % $alnum.Length] })
}

function New-Secret32() {
    # 32 字节 → base64url（43 字符，无 +/ 与 shell 特殊字符）
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

# ---------- 交互提问（-Auto 时直接走生成/默认） ----------
# 返回用户选择的值；$Generate 脚本块给出随机值；-Auto 或直接回车 = 生成/默认。
function Ask-Secret([string]$Label, [scriptblock]$Generate, [string]$Hint = '', [switch]$Optional, [string]$Default = '') {
    if ($Auto) { return & $Generate }
    if ($Default -ne '' ) {
        Write-Host ''
        Write-Host "$Label（回车 = 使用默认）" -ForegroundColor Cyan
        if ($Hint) { Write-Host "  $Hint" -ForegroundColor DarkGray }
        $v = Read-Host "  输入值（默认 $Default）"
        if ([string]::IsNullOrWhiteSpace($v)) { return $Default }
        return $v
    }
    while ($true) {
        Write-Host ''
        Write-Host "$Label" -ForegroundColor Cyan
        if ($Hint) { Write-Host "  $Hint" -ForegroundColor DarkGray }
        if ($Optional) {
            $v = Read-Host '  回车 = 生成 / s = 跳过（留空）/ m = 手动输入'
            if ($v -eq 's') { return '' }
        } else {
            $v = Read-Host '  回车 = 生成随机值 / m = 手动输入'
        }
        if ([string]::IsNullOrWhiteSpace($v)) { return & $Generate }
        if ($v -ne 'm') { return $v }
        $manual = Read-Host '  请输入（输入内容明文显示）'
        if (-not [string]::IsNullOrWhiteSpace($manual)) { return $manual }
        Write-Host '  输入为空，重新选择。' -ForegroundColor Yellow
    }
}

# ---------- 主流程 ----------
if (-not (Test-Path $Template)) { throw "模板不存在：$Template" }
$exists = Test-Path '.env'
if ($exists -and -not $Patch -and -not $Force) {
    throw '.env 已存在（不覆盖）。如需在现有 .env 上替换占位符/补密钥请用 -Patch；确要重建请用 -Force。'
}

if ($Patch) {
    if (-not $exists) { throw '-Patch 需要已存在的 .env。' }
    # PS 5.1 Get-Content 缺省按 ANSI/GBK 解码：UTF-8 中文注释尾字节会与换行符
    # 配对成双字节字符吞掉换行（行粘连），必须显式 UTF-8。
    $text = Get-Content '.env' -Raw -Encoding UTF8
} else {
    $text = Get-Content $Template -Raw -Encoding UTF8
}

$made = [System.Collections.Generic.List[string]]::new()

# 1) PostgreSQL 密码 + DATABASE_URL 联动重写
$pgPass = Ask-Secret 'PostgreSQL 密码（POSTGRES_PASSWORD，同步重写 DATABASE_URL）' { New-Password } '自用建议直接生成；需接入既有数据库时选手动输入'
if ($pgPass) {
    $text = $text -replace '(?m)^POSTGRES_PASSWORD=.*$', "POSTGRES_PASSWORD=$pgPass"
    # DATABASE_URL：存在则替换内嵌密码并归一 host 为 compose 内 postgres；不存在则追加
    $dbUrl = "postgres://docflow:$pgPass@postgres:5432/docflow?sslmode=disable"
    if ($text -match '(?m)^DATABASE_URL=.*$') {
        $text = $text -replace '(?m)^DATABASE_URL=.*$', "DATABASE_URL=$dbUrl"
    } else {
        $text += "`r`nDATABASE_URL=$dbUrl`r`n"
    }
    $made.Add('POSTGRES_PASSWORD + DATABASE_URL')
}

# 2) JWT_SECRET（≥32 字节）
$jwt = Ask-Secret 'JWT_SECRET（会话签发密钥，≥32 字节）' { New-Secret32 } ''
if ($jwt) {
    if ($jwt.Length -lt 32) { throw 'JWT_SECRET 至少 32 字节。' }
    $text = $text -replace '(?m)^JWT_SECRET=.*$', "JWT_SECRET=$jwt"
    $made.Add('JWT_SECRET')
}

# 3) 初始管理员
$email = Ask-Secret '初始管理员邮箱（SEED_ADMIN_EMAIL）' { 'admin@docflow.local' } '' -Default 'admin@docflow.local'
$user = Ask-Secret '初始管理员用户名（SEED_ADMIN_USERNAME）' { 'admin' } '' -Default 'admin'
$adminPass = Ask-Secret '初始管理员密码（SEED_ADMIN_PASSWORD，首次登录后可改）' { New-Password } ''
foreach ($pair in @(@('SEED_ADMIN_EMAIL', $email), @('SEED_ADMIN_USERNAME', $user), @('SEED_ADMIN_PASSWORD', $adminPass))) {
    $k, $v = $pair
    if (-not $v) { continue }
    if ($text -match "(?m)^$k=.*$") { $text = $text -replace "(?m)^$k=.*$", "$k=$v" }
    else { $text += "`r`n$k=$v`r`n" }
    if ($k -eq 'SEED_ADMIN_PASSWORD') { $made.Add($k) } else { $made.Add($k) }
}

# 4) ONLYOFFICE_JWT_SECRET（可选：full profile 才需要）
$oo = Ask-Secret 'ONLYOFFICE_JWT_SECRET（启用 OnlyOffice（full profile）时需要；minimal 可跳过）' { New-Secret32 } '' -Optional
if ($oo) {
    $text = $text -replace '(?m)^ONLYOFFICE_JWT_SECRET=.*$', "ONLYOFFICE_JWT_SECRET=$oo"
    $made.Add('ONLYOFFICE_JWT_SECRET')
}

Set-Content -Path '.env' -Value $text -NoNewline -Encoding UTF8

Write-Host ''
Write-Host '==== .env 已生成 ====' -ForegroundColor Green
Write-Host "文件：$root\.env（已 gitignore，请妥善备份）"
if ($made.Count) { Write-Host ("本次写入：{0}" -f ($made -join '、')) }
Write-Host ''
Write-Host '初始管理员：' -ForegroundColor Yellow
Write-Host "  邮箱/用户名：$email / $user"
Write-Host "  密码：$adminPass"
Write-Host '  （首次登录后建议立即在「设置 → 账号安全」修改。）'
if (-not $oo) { Write-Host '  ONLYOFFICE_JWT_SECRET 未设置：使用 minimal profile 不受影响；启用 full 时重跑 -Patch 补齐。' }
