#!/bin/bash
# 生产前核对 - 脏库重置（TRUNCATE + seed）后全量冒烟，不 recreate 任何容器
set -e
cd /mnt/d/data/code/git/own/DocFlow
C="docker compose --env-file .env -f docker-compose.yml -f scripts/compose-scale.yml -p docflow"
$C exec -T postgres psql -U docflow -d docflow -t -A -c \
  "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" \
  | $C exec -T postgres psql -U docflow -d docflow
echo '=== reseed ==='
$C run --rm seed 2>&1 | tail -1
echo '=== smoke ==='
tr -d '\r' < scripts/compose-smoke.sh > /tmp/compose-smoke.sh && bash /tmp/compose-smoke.sh
