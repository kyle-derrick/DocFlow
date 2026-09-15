$ErrorActionPreference = 'Stop'
# Dry-run by default: stop writers externally and set CONFIRM_RESTORE=YES before destructive restore.
if ($env:CONFIRM_RESTORE -ne 'YES') { throw 'dry-run only: set CONFIRM_RESTORE=YES after stopping writes externally' }
if (-not $env:BACKUP_FILE -or -not $env:DATABASE_URL) { throw 'BACKUP_FILE and DATABASE_URL are required' }
$work = if ($env:RESTORE_DIR) { $env:RESTORE_DIR } else { './restore-work' }; New-Item -ItemType Directory -Force $work | Out-Null
$archive = Join-Path $work 'archive.zip'
if ($env:BACKUP_FILE.EndsWith('.age')) { if (-not (Get-Command age -ErrorAction SilentlyContinue)) { throw 'age is required' }; & age -d -o $archive $env:BACKUP_FILE } else { Copy-Item $env:BACKUP_FILE $archive -Force }
Expand-Archive $archive $work -Force
& psql $env:DATABASE_URL -f (Join-Path $work 'postgres.sql')
if (($env:STORAGE_DRIVER ?? 'local') -eq 'local') { Expand-Archive (Join-Path $work 'objects.zip') (Split-Path ($env:STORAGE_ROOT ?? './storage')) -Force } else { if (-not (Get-Command aws -ErrorAction SilentlyContinue)) { throw 'aws-cli is required' }; & aws s3 sync (Join-Path $work 'objects') "s3://$($env:S3_BUCKET)" }
& go run ./cmd/backup-verify -dir $work
