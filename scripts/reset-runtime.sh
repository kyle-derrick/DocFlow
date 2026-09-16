#!/bin/bash
# DocFlow 运行时重置：停服 -> TRUNCATE -> 重建二进制 -> 启动 -> seed 管理员
# 用法（Windows 侧）：
#   wsl -e bash -c "tr -d '\r' < <repo-root>/scripts/reset-runtime.sh > /tmp/reset-runtime.sh && bash /tmp/reset-runtime.sh"
set -e
export PATH=/opt/go/bin:$PATH
cd "$(dirname "$0")/.."

echo "=== [1/6] stop old server ==="
pkill -f docflow-runtime-server 2>/dev/null || true
for i in $(seq 1 30); do
  if ! ss -ltn 2>/dev/null | grep -q ':18080 '; then break; fi
  sleep 0.5
done
if ss -ltn 2>/dev/null | grep -q ':18080 '; then echo "FATAL: port 18080 busy"; exit 1; fi

echo "=== [2/6] truncate database ==="
docker exec docflow-pg-runtime psql -U docflow -d docflow -t -A -c \
  "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" \
  | docker exec -i docflow-pg-runtime psql -U docflow -d docflow

echo "=== [3/6] build ==="
go build -o /tmp/docflow-runtime-server ./cmd/server
go build -o /tmp/docflow-runtime-seed ./cmd/seed
go build -o /tmp/docflow-migrate ./cmd/migrate

echo "=== [3b/6] apply migrations ==="
env DATABASE_URL='postgres://docflow:docflow-test-password@127.0.0.1:55432/docflow?sslmode=disable' \
  /tmp/docflow-migrate -dir migrations

echo "=== [4/6] start server ==="
env DATABASE_URL='postgres://docflow:docflow-test-password@127.0.0.1:55432/docflow?sslmode=disable' \
  JWT_SECRET='runtime-test-jwt-secret-012345678901234567890' \
  PORT=18080 STORAGE_ROOT=/tmp/docflow-runtime-storage STORAGE_DRIVER=local \
  SCAN_ENABLED=false COOKIE_SECURE=false JANITOR_ENABLED=false \
  PUBLIC_BASE_URL='http://127.0.0.1:18080' \
  nohup /tmp/docflow-runtime-server > /tmp/docflow-runtime.log 2>&1 &

echo "=== [5/6] wait ready ==="
ready=0
for i in $(seq 1 40); do
  if curl -sf http://127.0.0.1:18080/ready > /dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != "1" ]; then echo "FATAL: server not ready"; tail -50 /tmp/docflow-runtime.log; exit 1; fi
echo "SERVER READY"

echo "=== [6/6] seed admin ==="
env DATABASE_URL='postgres://docflow:docflow-test-password@127.0.0.1:55432/docflow?sslmode=disable' \
  SEED_ADMIN_EMAIL=admin@example.com SEED_ADMIN_PASSWORD=AdminPassword123 \
  SEED_ADMIN_USERNAME=admin SEED_ADMIN_ROLE=admin /tmp/docflow-runtime-seed
echo "RESET DONE"
