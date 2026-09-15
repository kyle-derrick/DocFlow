#!/bin/bash
# compose 栈验证循环：重建 backend 镜像 → recreate → TRUNCATE → reseed → 冒烟
set -e
cd /mnt/d/data/code/git/own/DocFlow
docker compose --profile minimal build backend 2>&1 | tail -2
docker compose --profile minimal --profile search up -d 2>&1 | tail -2
# backend recreate 会级联重启 caddy（depends_on healthy），轮询等入口可用
for i in $(seq 1 90); do
  curl -sf -o /dev/null http://127.0.0.1/ready && break
  sleep 2
done
curl -sf -o /dev/null http://127.0.0.1/ready || { echo 'FATAL: entry not ready'; exit 1; }
docker compose exec -T postgres psql -U docflow -d docflow -t -A -c \
  "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" \
  | docker compose exec -T postgres psql -U docflow -d docflow
docker compose run --rm seed 2>&1 | tail -1
tr -d '\r' < scripts/compose-smoke.sh > /tmp/compose-smoke.sh && bash /tmp/compose-smoke.sh
