#!/bin/bash
# SMTP 真实投递验证：起 mailpit → recreate backend（SMTP_ENABLED）→
# 创建邀请 + 忘记密码 → Mailpit API 断言邮件到达且含令牌链接。
set -e
cd "$(dirname "$0")/.."
C="docker compose -f docker-compose.yml -f scripts/compose-scale.yml --profile minimal --profile search --profile full --profile storage"

echo '=== [1] up mailpit + recreate backend ==='
$C up -d mailpit 2>&1 | tail -1
$C up -d --force-recreate backend 2>&1 | tail -1
sleep 3
$C up -d caddy 2>&1 | tail -1
for i in $(seq 1 45); do curl -sf -o /dev/null http://127.0.0.1/ready && break; sleep 2; done
$C logs backend 2>&1 | grep -m1 'mail transport' || { echo 'FATAL smtp not enabled'; exit 1; }

login=$(curl -sS -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"AdminPassword123"}' http://127.0.0.1/api/v1/auth/login)
token=$(printf '%s' "$login" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
H=(-H "Authorization: Bearer $token" -H 'Content-Type: application/json')

echo '=== [2] invitation mail ==='
curl -sS "${H[@]}" --data-binary '{"email":"invitee@example.com","role":"user"}' -X POST http://127.0.0.1/api/v1/admin/invitations | head -c 120; echo

echo '=== [3] forgot-password mail ==='
curl -sS -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com"}' -X POST http://127.0.0.1/api/v1/auth/forgot-password | head -c 120; echo
sleep 2

echo '=== [4] mailpit inbox ==='
curl -sS 'http://127.0.0.1:18025/api/v1/messages' | python3 -c '
import json,sys
d=json.load(sys.stdin)
msgs=d.get("messages",[])
print("total=", d.get("total"))
for m in msgs[:5]:
    print("- ", m["From"]["Address"], "->", m["To"][0]["Address"], ":", m["Subject"])
assert len(msgs)>=2, "expect >=2 mails"
'
echo '=== [5] body contains links ==='
MID=$(curl -sS 'http://127.0.0.1:18025/api/v1/messages' | python3 -c 'import json,sys;print(json.load(sys.stdin)["messages"][0]["ID"])')
curl -sS "http://127.0.0.1:18025/api/v1/message/$MID" | python3 -c '
import json,sys
b=json.load(sys.stdin)
text=b.get("Text") or b.get("HTML") or ""
links=[l for l in text.split() if l.startswith("http")]
assert links, "no link in body"
print("PASS mail body has link:", links[:1])
'
