#!/bin/bash
# TLS 全链路验证（一条龙）：重置库 + seed → 登录 → GET/PUT /admin/tls →
# internal(127.0.0.1) 断言 https → 切回 http 断言恢复。
set -e
PD=/mnt/d/data/code/git/own/DocFlow
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin
C="docker compose --env-file $PD/.env -f $PD/docker-compose.yml -f $PD/scripts/compose-scale.yml -p docflow"
PG="docker exec docflow-postgres-1 psql -U docflow -d docflow -t -A"
PASS=$(grep '^SEED_ADMIN_PASSWORD=' $PD/.env | cut -d= -f2-)

echo '=== [0] reset db + seed ==='
$PG -c "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" | $PG
$C run --rm seed 2>&1 | tail -1

echo '=== [1] login（login 接口按 email 认证） ==='
RESP=$(curl -s -m 5 -X POST http://127.0.0.1/api/v1/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"admin@example.com\",\"password\":\"$PASS\"}")
TOK=$(echo "$RESP" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
[ -n "$TOK" ] || { echo "LOGIN_FAIL: $(echo "$RESP" | head -c 150)"; exit 1; }

echo '=== [2] GET /admin/tls ==='
curl -s http://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK"; echo

echo '=== [3] switch to internal (127.0.0.1) ==='
curl -s -X PUT http://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"mode":"internal","domain":"127.0.0.1"}'; echo
sleep 2
curl -sk -m 5 -o /dev/null -w 'https front: %{http_code}\n' https://127.0.0.1/ || echo HTTPS_FAIL
curl -sk -m 5 https://127.0.0.1/ready; echo

echo '=== [4] switch back to http ==='
curl -s -X PUT http://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"mode":"http"}'; echo
sleep 2
curl -s -m 5 -o /dev/null -w 'http front: %{http_code}\n' http://127.0.0.1/
curl -s http://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK"; echo
echo TLS_VERIFY_DONE
