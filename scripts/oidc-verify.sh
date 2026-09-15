#!/bin/bash
# Keycloak OIDC 端到端：起 Keycloak → kcadm 配 realm/client/user → backend 启用
# OIDC → curl 走 authorization code（登录页表单提交）→ 断言自动开户 + 会话。
set -e
cd /mnt/d/data/code/git/own/DocFlow
C="docker compose -f docker-compose.yml -f scripts/compose-scale.yml --profile minimal --profile search --profile full --profile storage --profile antivirus"

echo '=== [1] up keycloak ==='
$C up -d keycloak 2>&1 | tail -1
echo '=== [2] wait keycloak ready (首次较慢) ==='
ok=0
for i in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18090/realms/master 2>/dev/null || echo 000)
  [ "$code" = "200" ] && { ok=1; echo "keycloak ready (${i}x5s)"; break; }
  sleep 5
done
[ "$ok" = 1 ] || { echo KC_NOT_READY; $C logs keycloak --tail 5; exit 1; }

echo '=== [3] kcadm: realm + client + user ==='
KCC="$C exec -T keycloak /opt/keycloak/bin/kcadm.sh"
$C exec -T keycloak /opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin >/dev/null
$C exec -T keycloak /opt/keycloak/bin/kcadm.sh create realms -s realm=docflow -s enabled=true 2>/dev/null || echo 'realm exists'
CLIENT_SECRET=$($C exec -T keycloak /opt/keycloak/bin/kcadm.sh create clients -r docflow \
  -s clientId=docflow -s enabled=true -s 'redirectUris=["http://127.0.0.1/api/v1/auth/oidc/callback"]' \
  -s secret=verify-oidc-client-secret -s publicClient=false -i 2>/dev/null && echo created || true)
$C exec -T keycloak /opt/keycloak/bin/kcadm.sh create clients -r docflow \
  -s clientId=docflow -s enabled=true -s 'redirectUris=["http://127.0.0.1/api/v1/auth/oidc/callback"]' \
  -s secret=verify-oidc-client-secret -s publicClient=false 2>/dev/null || echo 'client exists'
$C exec -T keycloak /opt/keycloak/bin/kcadm.sh create users -r docflow \
  -s username=oidcuser -s enabled=true -s email=oidcuser@example.com \
  -s firstName=Oidc -s lastName=User -s emailVerified=true 2>/dev/null || echo 'user exists'
$C exec -T keycloak /opt/keycloak/bin/kcadm.sh set-password -r docflow \
  --username oidcuser --new-password OidcPass123 2>/dev/null || true
echo 'keycloak configured'

echo '=== [4] backend OIDC on ==='
grep -q '^OIDC_ENABLED=' .env || cat >> .env <<'EOF'
OIDC_ENABLED=true
OIDC_ISSUER=http://172.18.0.1:18090/realms/docflow
OIDC_CLIENT_ID=docflow
OIDC_CLIENT_SECRET=verify-oidc-client-secret
PUBLIC_BASE_URL=http://127.0.0.1
EOF
$C up -d --force-recreate backend 2>&1 | tail -1
for i in $(seq 1 45); do curl -sf -o /dev/null http://127.0.0.1:18081/ready && break; sleep 2; done
curl -sf http://127.0.0.1/ready; echo ' (oidc backend ready)'
$C up -d caddy 2>&1 | tail -1

echo '=== [5] discovery check (backend -> issuer) ==='
$C exec -T backend wget -qO- http://172.18.0.1:18090/realms/docflow/.well-known/openid-configuration | head -c 120; echo

echo '=== [6] authorization code flow ==='
python3 scripts/oidc-flow.py
