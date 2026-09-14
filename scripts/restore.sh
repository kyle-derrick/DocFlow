#!/usr/bin/env sh
# Restore is destructive: stop writers externally and inspect the archive first.
set -eu
: "${BACKUP_FILE:?BACKUP_FILE is required}"
: "${RESTORE_DIR:=./restore-work}"
[ "${CONFIRM_RESTORE:-}" = YES ] || { echo 'dry-run only: set CONFIRM_RESTORE=YES after stopping writes externally' >&2; exit 2; }
command -v pg_restore >/dev/null || command -v psql >/dev/null || { echo 'pg_restore or psql is required' >&2; exit 1; }
mkdir -p "$RESTORE_DIR"
case "$BACKUP_FILE" in *.age) command -v age >/dev/null || { echo 'age is required' >&2; exit 1; }; age -d -o "$RESTORE_DIR/archive.tar" "$BACKUP_FILE"; tar -xf "$RESTORE_DIR/archive.tar" -C "$RESTORE_DIR";; *) tar -xf "$BACKUP_FILE" -C "$RESTORE_DIR";; esac
psql "${DATABASE_URL:?DATABASE_URL is required}" < "$RESTORE_DIR/postgres.sql"
if [ "${STORAGE_DRIVER:-local}" = local ]; then tar -xf "$RESTORE_DIR/objects.tar" -C "$(dirname "${STORAGE_ROOT:?STORAGE_ROOT is required}")"; else command -v aws >/dev/null || { echo 'aws-cli is required' >&2; exit 1; }; aws s3 sync "$RESTORE_DIR/objects" "s3://${S3_BUCKET:?S3_BUCKET is required}"; fi
go run ./cmd/backup-verify -dir "$RESTORE_DIR" -database-url "$DATABASE_URL"
