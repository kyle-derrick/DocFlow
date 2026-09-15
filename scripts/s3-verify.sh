#!/bin/bash
set -e
cd /mnt/d/data/code/git/own/DocFlow
C="docker compose -f docker-compose.yml -f scripts/compose-scale.yml --profile minimal --profile search --profile full --profile storage"
$C up -d caddy 2>&1 | tail -1
for i in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1/ready && break; sleep 2; done
curl -sf http://127.0.0.1/ready; echo ' ENTRY_OK'
$C exec -T postgres psql -U docflow -d docflow -t -A -c \
  "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" \
  | $C exec -T postgres psql -U docflow -d docflow
$C run --rm seed 2>&1 | tail -1
tr -d '\r' < scripts/compose-smoke.sh > /tmp/compose-smoke.sh && bash /tmp/compose-smoke.sh > /tmp/r.txt 2>&1 || true
tail -1 /tmp/r.txt
grep '^FAIL' /tmp/r.txt || echo ALL_PASS
