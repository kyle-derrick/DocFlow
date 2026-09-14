#!/usr/bin/env sh
# DocFlow 备份脚本 v2（与 scripts/backup.ps1 同语义）。
#
# 产物：BACKUP_DIR/docflow-backup-<ts>/ 目录，内含
#   - postgres.sql       PostgreSQL 逻辑备份（pg_dump）
#   - objects.tar        对象存储目录打包（仅 STORAGE_DRIVER=local；s3 时跳过，
#                        对象桶由运维层负责备份/版本化，manifest 标注
#                        object_store=external）
#   - env.sanitized      脱敏 .env 导出（仅非密钥键：键名含 SECRET/PASS 一律排除）
#   - manifest.json      timestamp / components / object_store / 每文件 sha256 与 size
#
# 用法：
#   scripts/backup.sh            执行备份，并按 RETENTION（天）清理旧备份目录
#   scripts/backup.sh --verify   对最近一次备份重算每文件 sha256 校验，
#                                结果写 verify.json 标记（GET /admin/backups/status
#                                读取该标记展示「是否验证」），失败退出码非 0
#
# 加密说明：本脚本不加密备份产物（服务与脚本层不持有加密密钥）。
# 请由运维层对 BACKUP_DIR 所在存储加密（如 LUKS 全盘加密、云 KMS 托管
# 加密卷或加密对象桶），密钥管理不进入本脚本环境。
set -eu

: "${BACKUP_DIR:=./backups}"
: "${STORAGE_DRIVER:=local}"
: "${STORAGE_ROOT:=./storage}"
: "${ENV_FILE:=./.env}"
: "${RETENTION:=30}"

retention_is_valid() {
  case "$RETENTION" in ''|*[!0-9]*) return 1;; esac
  [ "$RETENTION" -ge 1 ]
}

# ---------- --verify：对最近备份重算 sha256 ----------
if [ "${1:-}" = "--verify" ]; then
  command -v python3 >/dev/null || { echo 'python3 is required for --verify' >&2; exit 1; }
  latest=$(find "$BACKUP_DIR" -maxdepth 1 -type d -name 'docflow-backup-*' 2>/dev/null | sort | tail -n 1)
  [ -n "$latest" ] || { echo "no docflow-backup-* directory found under $BACKUP_DIR" >&2; exit 1; }
  python3 - "$latest" <<'PY'
import datetime, hashlib, json, os, sys
root = sys.argv[1]
manifest_path = os.path.join(root, 'manifest.json')
if not os.path.isfile(manifest_path):
    print('manifest.json not found in %s' % root, file=sys.stderr)
    sys.exit(2)
with open(manifest_path) as f:
    m = json.load(f)
errors, count = [], 0
for e in m.get('entries', []):
    count += 1
    p = os.path.join(root, e['path'])
    if not os.path.isfile(p):
        errors.append('%s: missing file' % e['path'])
        continue
    h = hashlib.sha256()
    with open(p, 'rb') as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b''):
            h.update(chunk)
    if h.hexdigest().lower() != str(e.get('sha256', '')).lower():
        errors.append('%s: sha256 mismatch' % e['path'])
    elif 'size' in e and os.path.getsize(p) != e['size']:
        errors.append('%s: size mismatch' % e['path'])
verified = not errors
now = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
marker = {'verified': verified, 'verified_at': now, 'manifest': manifest_path,
          'files': count, 'errors': errors}
with open(os.path.join(root, 'verify.json'), 'w') as f:
    json.dump(marker, f, indent=2)
for e in errors:
    print('FAIL %s' % e, file=sys.stderr)
print('verify %s: %s (%d files, %d error(s))' % ('passed' if verified else 'failed', root, count, len(errors)))
sys.exit(0 if verified else 1)
PY
  exit $?
fi

if [ "$#" -gt 0 ]; then
  echo "usage: $0 [--verify]" >&2
  exit 1
fi

mkdir -p "$BACKUP_DIR"
retention_is_valid || { echo 'RETENTION must be a positive integer' >&2; exit 1; }
command -v pg_dump >/dev/null || { echo 'pg_dump is required' >&2; exit 1; }
command -v python3 >/dev/null || { echo 'python3 is required' >&2; exit 1; }
case "$STORAGE_DRIVER" in
  local|s3) ;;
  *) echo 'STORAGE_DRIVER must be local or s3' >&2; exit 1;;
esac

stamp=$(date -u +%Y%m%dT%H%M%SZ)
iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
out="$BACKUP_DIR/docflow-backup-$stamp"
mkdir "$out"

pg_dump "${DATABASE_URL:?DATABASE_URL is required}" > "$out/postgres.sql"

components='["postgres"'
object_store='local'
if [ "$STORAGE_DRIVER" = local ]; then
  command -v tar >/dev/null || { echo 'tar is required for local object backup' >&2; exit 1; }
  [ -d "$STORAGE_ROOT" ] || { echo "STORAGE_ROOT directory not found: $STORAGE_ROOT" >&2; exit 1; }
  tar -cf "$out/objects.tar" -C "$(dirname "$STORAGE_ROOT")" "$(basename "$STORAGE_ROOT")"
  components="$components,\"object-store\""
else
  # STORAGE_DRIVER=s3：对象桶由外部对象存储托管（版本化/跨区复制由运维层
  # 配置），本脚本不导出，manifest 以 object_store=external 标注。
  object_store='external'
fi

if [ -f "$ENV_FILE" ]; then
  # 脱敏导出：仅保留非密钥键（键名含 SECRET/PASS，大小写不敏感，一律排除）。
  awk -F= '/^[A-Za-z_][A-Za-z0-9_]*[[:space:]]*=/ { k=$1; gsub(/[[:space:]]/, "", k); if (toupper(k) !~ /SECRET/ && toupper(k) !~ /PASS/) print }' "$ENV_FILE" > "$out/env.sanitized"
  components="$components,\"env\""
else
  echo "note: ENV_FILE not found ($ENV_FILE), skipping sanitized env export" >&2
fi
components="$components]"

python3 - "$out" "$iso" "$components" "$object_store" <<'PY'
import hashlib, json, os, sys
root, ts, components, object_store = sys.argv[1:5]
entries = []
for name in sorted(os.listdir(root)):
    p = os.path.join(root, name)
    if not os.path.isfile(p):
        continue
    h = hashlib.sha256()
    with open(p, 'rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    typ = 'postgres_dump' if name == 'postgres.sql' else ('env' if name == 'env.sanitized' else 'object')
    entries.append({'path': name, 'type': typ, 'sha256': h.hexdigest(), 'size': os.path.getsize(p)})
manifest = {'version': 2, 'timestamp': ts, 'components': json.loads(components),
            'object_store': object_store, 'encryption': 'ops-layer', 'entries': entries}
with open(os.path.join(root, 'manifest.json'), 'w') as f:
    json.dump(manifest, f, indent=2)
PY

# retention：清理超过 RETENTION 天的备份目录与旧版（v1）散落产物。
find "$BACKUP_DIR" -maxdepth 1 -type d -name 'docflow-backup-*' -mtime +"$RETENTION" -exec rm -rf {} +
find "$BACKUP_DIR" -maxdepth 1 -type f -name 'docflow-*' -mtime +"$RETENTION" -delete

printf 'backup complete: %s (object-store=%s)\n' "$out" "$object_store"
printf 'manifest: %s\n' "$out/manifest.json"
