#!/bin/bash
# TLS 全链路验证（一条龙）：重置库 + seed → 登录 → GET/PUT /admin/tls →
# internal(127.0.0.1) 断言 https → 切回 http 断言恢复。
set -e
PD="$(cd "$(dirname "$0")/.." && pwd)"
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin
C="docker compose --env-file $PD/.env -f $PD/docker-compose.yml -f $PD/scripts/compose-scale.yml -p docflow"
PG="docker exec docflow-postgres-1 psql -U docflow -d docflow -t -A"
PASS=$(grep '^SEED_ADMIN_PASSWORD=' $PD/.env | cut -d= -f2-)
# 当前生效协议自适应：上次运行可能停在 internal/acme（80 → 308），登录与
# 切换请求须与现状同协议；tls_state 为后端持久化表（无记录时按 http）。
MODE=$($PG -c "SELECT mode FROM tls_state ORDER BY id DESC LIMIT 1" 2>/dev/null | tr -d '[:space:]')
PROTO=http; [ "$MODE" = internal ] || [ "$MODE" = acme ] && PROTO=https
echo "current tls mode: ${MODE:-http} -> $PROTO"

echo '=== [0] reset db + seed ==='
$PG -c "SELECT 'TRUNCATE TABLE ' || string_agg(format('%I.%I', schemaname, tablename), ', ') || ' RESTART IDENTITY CASCADE' FROM pg_tables WHERE schemaname='public' AND tablename <> 'schema_migrations';" | $PG
$C run --rm seed 2>&1 | tail -1

echo '=== [1] login（login 接口按 email 认证） ==='
RESP=$(curl -sk -m 5 -X POST $PROTO://127.0.0.1/api/v1/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"admin@example.com\",\"password\":\"$PASS\"}")
TOK=$(echo "$RESP" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
[ -n "$TOK" ] || { echo "LOGIN_FAIL: $(echo "$RESP" | head -c 150)"; exit 1; }

echo '=== [2] GET /admin/tls ==='
curl -sk $PROTO://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK"; echo

echo '=== [3] switch to internal (127.0.0.1) ==='
curl -sk -X PUT $PROTO://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"mode":"internal","domain":"127.0.0.1"}'; echo
sleep 2
code=$(curl -sk -m 5 -o /dev/null -w '%{http_code}' https://127.0.0.1/)
[ "$code" = 200 ] && echo 'PASS https internal front 200' || { echo "FAIL https front $code"; exit 1; }
curl -sk -m 5 https://127.0.0.1/ready; echo

echo '=== [4] switch back to http ==='
# 此刻 caddy 已 https 化（80 → 308），须走 https 下发切回，明文 PUT 会被重定向吞掉
curl -sk -X PUT https://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"mode":"http"}'; echo
sleep 2
code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1/)
[ "$code" = 200 ] && echo 'PASS http restored front 200' || { echo "FAIL http front $code"; exit 1; }
curl -s http://127.0.0.1/api/v1/admin/tls -H "Authorization: Bearer $TOK"; echo
echo TLS_VERIFY_DONE
