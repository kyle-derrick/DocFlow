$ErrorActionPreference = 'Stop'
$dir = if ($env:BACKUP_DIR) { $env:BACKUP_DIR } else { './backups' }
New-Item -ItemType Directory -Force -LiteralPath $dir | Out-Null
$retention = if ($env:RETENTION) { $env:RETENTION } else { '30' }
if ($retention -notmatch '^[1-9][0-9]*$') { throw 'RETENTION must be a positive integer' }
$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'); $out = Join-Path $dir "docflow-$stamp.sql"
if (-not $env:DATABASE_URL) { throw 'DATABASE_URL is required' }
& pg_dump $env:DATABASE_URL | Out-File -Encoding utf8 $out
$hash = (Get-FileHash $out -Algorithm SHA256).Hash.ToLower(); "$hash  $out" | Set-Content ("$out.sha256")
Copy-Item "$out.sha256" (Join-Path $dir 'latest.manifest') -Force
Get-ChildItem -LiteralPath $dir -Filter 'docflow-*.sql' -File | Where-Object { $_.LastWriteTimeUtc -lt (Get-Date).ToUniversalTime().AddDays(-[int]$retention) } | Remove-Item -LiteralPath { $_.FullName } -Force
if ((Get-FileHash $out -Algorithm SHA256).Hash.ToLower() -ne $hash) { throw 'backup verification failed' }
