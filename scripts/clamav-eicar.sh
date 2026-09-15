#!/bin/bash
# EICAR 上传隔离链路（backend 已连宿主 clamd）
set -e
cd /mnt/d/data/code/git/own/DocFlow
login=$(curl -sS -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"AdminPassword123"}' http://127.0.0.1/api/v1/auth/login)
token=$(printf '%s' "$login" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
H=(-H "Authorization: Bearer $token")
echo '=== EICAR upload (expect rejected + quarantine) ==='
printf 'X5O!P%%@AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*' > /tmp/eicar.txt
body=$(python3 -c "import json,os;print(json.dumps({'name':'eicar.txt','size':os.path.getsize('/tmp/eicar.txt')}))")
up=$(curl -sS "${H[@]}" -H 'Content-Type: application/json' --data-binary "$body" http://127.0.0.1/api/v1/uploads | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
curl -sS -o /dev/null "${H[@]}" -H 'Content-Type: application/octet-stream' -H 'Upload-Offset: 0' --data-binary @/tmp/eicar.txt -X PATCH "http://127.0.0.1/api/v1/uploads/$up"
curl -sS "${H[@]}" -H 'Content-Type: application/json' --data-binary '{}' -X POST "http://127.0.0.1/api/v1/uploads/$up/complete" | head -c 300
echo
echo '=== quarantine records ==='
curl -sS "${H[@]}" 'http://127.0.0.1/api/v1/admin/quarantine' | head -c 400
echo
echo '=== clean file still passes ==='
printf 'clean content for clamav test' > /tmp/clean.txt
body=$(python3 -c "import json,os;print(json.dumps({'name':'clean.txt','size':os.path.getsize('/tmp/clean.txt')}))")
up2=$(curl -sS "${H[@]}" -H 'Content-Type: application/json' --data-binary "$body" http://127.0.0.1/api/v1/uploads | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
curl -sS -o /dev/null "${H[@]}" -H 'Content-Type: application/octet-stream' -H 'Upload-Offset: 0' --data-binary @/tmp/clean.txt -X PATCH "http://127.0.0.1/api/v1/uploads/$up2"
curl -sS "${H[@]}" -H 'Content-Type: application/json' --data-binary '{}' -X POST "http://127.0.0.1/api/v1/uploads/$up2/complete" | head -c 250
echo
