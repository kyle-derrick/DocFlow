#!/bin/bash
# compose 栈验证循环：重建 backend 镜像 → recreate → TRUNCATE → reseed → 冒烟
# 注：与真实验证栈同参数（含 compose-scale.yml 叠加，backend 依赖其
# extra_hosts 解析 OIDC issuer；profiles 与全栈一致避免配置漂移触发重建）。
set -e
cd "$(dirname "$0")/.."
C="docker compose -f docker-compose.yml -f scripts/compose-scale.yml --profile minimal --profile full --profile search --profile storage"
$C build backend 2>&1 | tail -2
$C up -d 2>&1 | tail -2
# backend recreate 会级联重启 caddy（depends_on healthy），轮询等入口可用
for i in $(seq 1 90); do
  curl -sf -o /dev/null http://127.0.0.1/ready && break
  sleep 2
done
curl -sf -o /dev/null http://127.0.0.1/ready || { echo 'FATAL: entry not ready'; exit 1; }
$C exec -T postgres psql -U docflow -d docflow -t -A -c \
  "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" \
  | $C exec -T postgres psql -U docflow -d docflow
$C run --rm seed 2>&1 | tail -1
tr -d '\r' < scripts/compose-smoke.sh > /tmp/compose-smoke.sh && bash /tmp/compose-smoke.sh
