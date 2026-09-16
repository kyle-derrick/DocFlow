#!/bin/bash
# HTTPS 运行时切换验证：等 backend 自愈（VM 重启后 OIDC 时序）→ 登录 →
# GET /admin/tls → 切 internal(127.0.0.1) 断言 https TLS → 切回 http 断言恢复。
# 注意：admin 密码须与 .env SEED_ADMIN_PASSWORD 一致（密码重置类验证会改密）。
set -e
PD=/mnt/d/data/code/git/own/DocFlow
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin
C="docker compose --env-file $PD/.env -f $PD/docker-compose.yml -f $PD/scripts/compose-scale.yml -p docflow --profile minimal --profile full --profile antivirus --profile search --profile storage"
PASS="${SEED_ADMIN_PASSWORD:-AdminPassword123}"

echo '=== [0] wait backend healthy ==='
for i in $(seq 1 60); do
  st=$(docker inspect docflow-backend-1 --format '{{.State.Health.Status}}' 2>/dev/null || echo none)
  [ "$st" = "healthy" ] && { echo "healthy (${i}x3s)"; break; }
  sleep 3
done
[ "$st" = "healthy" ] || { echo BACKEND_NOT_HEALTHY; exit 1; }

echo '=== [1] login ==='
RESP=$(curl -s -m 5 -X POST http://127.0.0.1/api/v1/auth/login -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$PASS\"}")
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
