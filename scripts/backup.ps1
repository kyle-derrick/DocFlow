$ErrorActionPreference = 'Stop'
# DocFlow backup script v2 (same semantics as scripts/backup.sh).
#
# Output: BACKUP_DIR/docflow-backup-<ts>/ directory containing
#   - postgres.sql       PostgreSQL logical dump (pg_dump)
#   - objects.zip        object storage dir archive (STORAGE_DRIVER=local only;
#                        skipped for s3: the bucket is backed up/versioned at the
#                        ops layer; manifest marks object_store=external)
#   - env.sanitized      sanitized .env export (secret keys excluded: key names
#                        containing SECRET/PASS, case-insensitive)
#   - manifest.json      timestamp / components / object_store / per-file
#                        sha256 and size
#
# Usage:
#   powershell -File scripts/backup.ps1            run a backup and prune old
#                                                  backup dirs beyond RETENTION days
#   powershell -File scripts/backup.ps1 -verify    recompute sha256 of the most
#                                                  recent backup and write the
#                                                  verify.json marker (read by
#                                                  GET /admin/backups/status);
#                                                  non-zero exit on failure
#                                                  (-verify is the PowerShell-style
#                                                  alias of --verify)
#
# Encryption note: this script does NOT encrypt backup artifacts (the service and
# script layer hold no encryption keys). Encrypt the BACKUP_DIR storage at the
# ops layer instead (BitLocker/LUKS full-disk encryption, cloud KMS managed
# volumes or encrypted buckets).
$dir = if ($env:BACKUP_DIR) { $env:BACKUP_DIR } else { './backups' }
$driver = if ($env:STORAGE_DRIVER) { $env:STORAGE_DRIVER } else { 'local' }
$root = if ($env:STORAGE_ROOT) { $env:STORAGE_ROOT } else { './storage' }
$envFile = if ($env:ENV_FILE) { $env:ENV_FILE } else { './.env' }
$retention = if ($env:RETENTION) { [int]$env:RETENTION } else { 30 }
$verifyMode = ($args -contains '--verify') -or ($args -contains '-verify')

# ---------- --verify: recompute sha256 of the most recent backup ----------
if ($verifyMode) {
  $latest = Get-ChildItem $dir -Directory -Filter 'docflow-backup-*' -ErrorAction SilentlyContinue | Sort-Object Name | Select-Object -Last 1
  if (-not $latest) { throw "no docflow-backup-* directory found under $dir" }
  $manifestPath = Join-Path $latest.FullName 'manifest.json'
  if (-not (Test-Path $manifestPath -PathType Leaf)) { throw "manifest.json not found in $($latest.FullName)" }
  $m = Get-Content $manifestPath -Raw | ConvertFrom-Json
  $errors = @(); $count = 0
  foreach ($e in $m.entries) {
    $count++
    $p = Join-Path $latest.FullName $e.path
    if (-not (Test-Path $p -PathType Leaf)) { $errors += "$($e.path): missing file"; continue }
    $hash = (Get-FileHash $p -Algorithm SHA256).Hash.ToLower()
    if ($hash -ne ([string]$e.sha256).ToLower()) { $errors += "$($e.path): sha256 mismatch" }
    elseif ($null -ne $e.size -and (Get-Item $p).Length -ne [int64]$e.size) { $errors += "$($e.path): size mismatch" }
  }
  $verified = $errors.Count -eq 0
  [ordered]@{
    verified = $verified
    verified_at = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    manifest = $manifestPath
    files = $count
    errors = $errors
  } | ConvertTo-Json -Depth 4 | Set-Content (Join-Path $latest.FullName 'verify.json')
  foreach ($e in $errors) { [Console]::Error.WriteLine("FAIL $e") }
  $state = if ($verified) { 'passed' } else { 'failed' }
  Write-Output "verify ${state}: $($latest.FullName) ($count files, $($errors.Count) error(s))"
  if (-not $verified) { exit 1 }
  exit 0
}

if ($args.Count -gt 0) { throw 'usage: backup.ps1 [-verify]' }
if ($retention -lt 1) { throw 'RETENTION must be >= 1' }
if (-not (Get-Command pg_dump -ErrorAction SilentlyContinue)) { throw 'pg_dump is required' }
if ($driver -ne 'local' -and $driver -ne 's3') { throw 'STORAGE_DRIVER must be local or s3' }
New-Item -ItemType Directory -Force -LiteralPath $dir | Out-Null

$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ')
$iso = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$out = Join-Path $dir "docflow-backup-$stamp"
New-Item -ItemType Directory -Path $out | Out-Null

if (-not $env:DATABASE_URL) { throw 'DATABASE_URL is required' }
& pg_dump $env:DATABASE_URL | Set-Content -Encoding utf8 (Join-Path $out 'postgres.sql')

$components = @('postgres'); $objectStore = 'local'
if ($driver -eq 'local') {
  if (-not (Test-Path $root -PathType Container)) { throw "STORAGE_ROOT directory not found: $root" }
  Compress-Archive -Path $root -DestinationPath (Join-Path $out 'objects.zip') -Force
  $components += 'object-store'
} else {
  # STORAGE_DRIVER=s3: the bucket is hosted by an external object store
  # (versioning / cross-region replication configured at the ops layer); the
  # script does not export it and the manifest marks object_store=external.
  $objectStore = 'external'
}

if (Test-Path $envFile -PathType Leaf) {
  # Sanitized export: keep only non-secret keys (key names containing
  # SECRET or PASS, case-insensitive, are excluded).
  Get-Content $envFile | Where-Object {
    $_ -match '^[A-Za-z_][A-Za-z0-9_]*\s*=' -and
    (($_ -split '=', 2)[0].ToUpper() -notmatch 'SECRET') -and
    (($_ -split '=', 2)[0].ToUpper() -notmatch 'PASS')
  } | Set-Content (Join-Path $out 'env.sanitized')
  $components += 'env'
} else {
  Write-Warning "ENV_FILE not found ($envFile), skipping sanitized env export"
}

$entries = @()
Get-ChildItem $out -File | ForEach-Object {
  $type = if ($_.Name -eq 'postgres.sql') { 'postgres_dump' } elseif ($_.Name -eq 'env.sanitized') { 'env' } else { 'object' }
  $entries += [ordered]@{
    path = $_.Name
    type = $type
    sha256 = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower()
    size = $_.Length
  }
}
[ordered]@{
  version = 2
  timestamp = $iso
  components = $components
  object_store = $objectStore
  encryption = 'ops-layer'
  entries = $entries
} | ConvertTo-Json -Depth 5 | Set-Content (Join-Path $out 'manifest.json')

# retention: prune backup dirs (and legacy v1 loose artifacts) older than RETENTION days.
Get-ChildItem $dir -Directory -Filter 'docflow-backup-*' -ErrorAction SilentlyContinue |
  Where-Object { $_.LastWriteTimeUtc -lt (Get-Date).ToUniversalTime().AddDays(-$retention) } |
  Remove-Item -Recurse -Force
Get-ChildItem $dir -File -Filter 'docflow-*' -ErrorAction SilentlyContinue |
  Where-Object { $_.LastWriteTimeUtc -lt (Get-Date).ToUniversalTime().AddDays(-$retention) } |
  Remove-Item -Force

Write-Output "backup complete: $out (object-store=$objectStore)"
Write-Output "manifest: $(Join-Path $out 'manifest.json')"
