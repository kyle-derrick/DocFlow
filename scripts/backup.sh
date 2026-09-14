#!/usr/bin/env sh
set -eu
: "${BACKUP_DIR:=./backups}"
mkdir -p "$BACKUP_DIR"
case "${RETENTION:-30}" in
  ''|*[!0-9]*) echo 'RETENTION must be a positive integer' >&2; exit 1 ;;
 esac
[ "${RETENTION:-30}" -ge 1 ] || { echo 'RETENTION must be >= 1' >&2; exit 1; }
ts=$(date -u +%Y%m%dT%H%M%SZ); out="$BACKUP_DIR/docflow-$ts.sql"
pg_dump "${DATABASE_URL:?DATABASE_URL is required}" > "$out"
sha256sum "$out" > "$out.sha256"
cp "$out.sha256" "$BACKUP_DIR/latest.manifest"
find "$BACKUP_DIR" -type f -name 'docflow-*.sql' -mtime +"${RETENTION:=30}" -delete
sha256sum -c "$out.sha256"
