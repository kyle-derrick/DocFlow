#!/usr/bin/env bash
# DocFlow 全量运行时 API 冒烟测试 v2（真实后端 + 真实 PostgreSQL）。
set -u
base=http://127.0.0.1:18080
pass=0; fail=0
ok(){ printf 'PASS %s\n' "$1"; pass=$((pass+1)); }
bad(){ printf 'FAIL %s => %s | body: %s\n' "$1" "${2:-}" "$(head -c 200 /tmp/r 2>/dev/null | tr -d '\n')"; fail=$((fail+1)); }
expect(){ desc=$1; want=$2; got=$3; if [ "$want" = "$got" ]; then ok "$desc"; else bad "$desc (want $want got $got)"; fi; }
json(){ python3 -c "import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1:]:
    if isinstance(d,dict): d=d.get(k)
    elif isinstance(d,list) and k.isdigit() and int(k)<len(d): d=d[int(k)]
    else: d=None
    if d is None: break
print(d if d is not None else '')" "$@"; }
sha(){ sha256sum "$1" | cut -d' ' -f1; }

# ---------- 认证 ----------
login=$(curl -sS -c /tmp/dfcookies -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"AdminPassword123"}' "$base/api/v1/auth/login")
token=$(printf '%s' "$login" | json access_token)
[ -n "$token" ] && ok "auth/login" || bad "auth/login"
csrf=$(awk '$6=="docflow_csrf" {print $7}' /tmp/dfcookies)
A=(-H "Authorization: Bearer $token" -H "X-CSRF-Token: $csrf" -b /tmp/dfcookies)
expect "login wrong password 401" 401 "$(curl -sS -o /tmp/r -w '%{http_code}' -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"WrongPassword123"}' "$base/api/v1/auth/login")"
csrf2=$(awk '$6=="docflow_csrf" {print $7}' /tmp/dfcookies2 2>/dev/null || true)
code=$(curl -sS -o /tmp/r -w '%{http_code}' -c /tmp/dfcookies2 -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"AdminPassword123"}' "$base/api/v1/auth/login")
expect "login repeat 200" 200 "$code"
csrf2=$(awk '$6=="docflow_csrf" {print $7}' /tmp/dfcookies2)
expect "auth/refresh rotation 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -b /tmp/dfcookies2 -c /tmp/dfcookies3 -H "X-CSRF-Token: $csrf2" -H "Origin: http://127.0.0.1:18080" -X POST "$base/api/v1/auth/refresh")"
expect "unauth 401" 401 "$(curl -sS -o /dev/null -w '%{http_code}' "$base/api/v1/files")"

# ---------- 文件与目录 ----------
expect "files list 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/files")"
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"smoke-dir"}' "$base/api/v1/folders")
expect "create folder 201" 201 "$code"; folder=$(cat /tmp/r | json id)
expect "duplicate folder 409" 409 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"smoke-dir"}' "$base/api/v1/folders")"
expect "invalid name 400" 400 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"bad/name"}' "$base/api/v1/folders")"
expect "rename folder 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"smoke-dir-2"}' -X PATCH "$base/api/v1/files/$folder")"

# ---------- 上传 ----------
printf 'hello smoke test runtime!!\n' >/tmp/notes.txt
body=$(python3 -c "import json;print(json.dumps({'parent_id':'$folder','name':'notes.txt','size':27,'expected_sha256':'$(sha /tmp/notes.txt)'}))")
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary "$body" "$base/api/v1/uploads")
expect "upload session 201" 201 "$code"; upload_id=$(cat /tmp/r | json id)
expect "upload patch 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/octet-stream' -H 'Upload-Offset: 0' --data-binary @/tmp/notes.txt -X PATCH "$base/api/v1/uploads/$upload_id")"
expect "upload complete 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{}' "$base/api/v1/uploads/$upload_id/complete")"
file=$(cat /tmp/r | json file_id)
st=""
for i in 1 2 3 4 5 6 7 8; do st=$(curl -sS "${A[@]}" "$base/api/v1/uploads/$upload_id" | json status); [ "$st" = "available" ] && break; sleep 1; done
expect "upload available" available "$st"
sleep 2
expect "search 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/search?q=smoke")"
hits=$(cat /tmp/r | json results 0 id)
[ -n "$hits" ] && ok "search hit" || bad "search no hit"

# ---------- 下载/预览/Range ----------
expect "download 200" 200 "$(curl -sS -o /tmp/dl -w '%{http_code}' "${A[@]}" "$base/api/v1/files/$file/download")"
cmp -s /tmp/dl /tmp/notes.txt && ok "download bytes" || bad "download bytes"
expect "range 206" 206 "$(curl -sS -o /dev/null -w '%{http_code}' -H 'Range: bytes=0-4' "${A[@]}" "$base/api/v1/files/$file/download")"
expect "preview 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/files/$file/preview")"

# ---------- 版本/标签/收藏/复制/批量 ----------
expect "star 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"starred":true}' -X PATCH "$base/api/v1/files/$file/starred")"
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"smoke-tag"}' "$base/api/v1/tags")
tag_code=$code; tag=$(cat /tmp/r | json id)
[ "$tag_code" = 201 ] || [ "$tag_code" = 200 ] && ok "create tag ($tag_code)" || bad "create tag"
expect "tag file 201" 201 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"tag_id":"'"$tag"'"}' "$base/api/v1/files/$file/tags")"
expect "copy 201" 201 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"parent_id":"'"$folder"'"}' "$base/api/v1/files/$file/copy")"
printf 'version2!\n' >/tmp/v2.txt
body=$(python3 -c "import json;print(json.dumps({'name':'notes.txt','size':10,'expected_sha256':'$(sha /tmp/v2.txt)','file_id':'$file'}))")
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary "$body" "$base/api/v1/uploads")
expect "new version session 200/201" OK "$([ "$code" = 200 ] || [ "$code" = 201 ] && echo OK || echo "$code")"; up2=$(cat /tmp/r | json id)
curl -sS -o /dev/null "${A[@]}" -H 'Content-Type: application/octet-stream' -H 'Upload-Offset: 0' --data-binary @/tmp/v2.txt -X PATCH "$base/api/v1/uploads/$up2"
curl -sS -o /dev/null "${A[@]}" -H 'Content-Type: application/json' --data-binary '{}' -X POST "$base/api/v1/uploads/$up2/complete"
sleep 2
expect "versions 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/files/$file/versions")"
vcount=$(python3 -c 'import json;print(len(json.load(open("/tmp/r"))["versions"]))' 2>/dev/null || echo 0)
[ "${vcount:-0}" -ge 1 ] && ok "versions recorded ($vcount)" || bad "no versions"
expect "batch zip 200" 200 "$(curl -sS -o /tmp/zip -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"file_ids":["'"$file"'"]}' "$base/api/v1/files/batch/download")"
python3 -c 'import zipfile;zipfile.ZipFile("/tmp/zip").namelist()' 2>/dev/null && ok "zip valid" || bad "zip invalid"
expect "batch trash 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"file_ids":["'"$file"'"]}' "$base/api/v1/files/batch/trash")"
expect "trash list 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/trash")"
expect "restore 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X POST "$base/api/v1/files/$file/restore")"

# ---------- tus ----------
expect "tus options 204" 204 "$(curl -sS -o /dev/null -w '%{http_code}' "${A[@]}" -X OPTIONS "$base/api/v1/tus/files")"
code=$(curl -sS -o /dev/null -D /tmp/tushdr -w '%{http_code}' "${A[@]}" -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 5' -H 'Upload-Metadata: filename dHVzLnR4dA==' -X POST "$base/api/v1/tus/files")
if [ "$code" = 429 ]; then ok "tus create rate-limited (acceptable)"; else expect "tus create 201" 201 "$code"; fi
tusloc=$(grep -ai '^location:' /tmp/tushdr | tr -d '\r' | awk '{print $2}')
if [ -n "$tusloc" ]; then
  ok "tus location"
  expect "tus patch 204" 204 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Tus-Resumable: 1.0.0' -H 'Content-Type: application/offset+octet-stream' -H 'Upload-Offset: 0' --data-binary 'hello' -X PATCH "$base$tusloc")"
else bad "tus no location"; fi

# ---------- 分享 ----------
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"file_id":"'"$file"'","permission":"download","expires_in":3600}' "$base/api/v1/shares")
expect "public share 201" 201 "$code"; share=$(cat /tmp/r | json id); stoken=$(cat /tmp/r | json token)
expect "public info 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "$base/api/v1/public/shares/$stoken")"
expect "public download 200" 200 "$(curl -sS -o /tmp/pdl -w '%{http_code}' "$base/api/v1/public/shares/$stoken/download")"
expect "share stats 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/shares/$share")"
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"file_id":"'"$file"'","permission":"download","password":"secret-pass-1"}' "$base/api/v1/shares")
expect "password share 201" 201 "$code"; ptoken=$(cat /tmp/r | json token)
expect "password gate 401" 401 "$(curl -sS -o /tmp/r -w '%{http_code}' "$base/api/v1/public/shares/$ptoken")"
expect "password verify 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -c /tmp/sacookie -H 'Content-Type: application/json' --data-binary '{"password":"secret-pass-1"}' "$base/api/v1/public/shares/$ptoken/verify")"
expect "unlock download 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -b /tmp/sacookie "$base/api/v1/public/shares/$ptoken/download")"
expect "wrong password 401" 401 "$(curl -sS -o /tmp/r -w '%{http_code}' -H 'Content-Type: application/json' --data-binary '{"password":"wrong"}' "$base/api/v1/public/shares/$ptoken/verify")"
expect "revoke 204" 204 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X DELETE "$base/api/v1/shares/$share")"
expect "revoked 410" 410 "$(curl -sS -o /tmp/r -w '%{http_code}' "$base/api/v1/public/shares/$stoken")"

# ---------- 团队/角色/ACL ----------
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"smoke-team"}' "$base/api/v1/teams")
expect "create team 201" 201 "$code"; team=$(cat /tmp/r | json id)
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"team-dir"}' "$base/api/v1/teams/$team/folders")
expect "team folder 201" 201 "$code"; tfolder=$(cat /tmp/r | json id)
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"readonly","permissions":{"read":true}}' "$base/api/v1/teams/$team/roles")
expect "custom role 201" 201 "$code"
expect "role list 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/teams/$team/roles")"
expect "members 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/teams/$team/members")"
expect "get ACL 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/folders/$tfolder/acl")"
uid=$(curl -sS "${A[@]}" "$base/api/v1/me" | json id)
expect "set ACL 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"entries":[{"subject_type":"user","subject_id":"'"$uid"'","effect":"deny","permissions":["read"]}]}' -X PUT "$base/api/v1/folders/$tfolder/acl")"
expect "team files 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/teams/$team/files")"

# ---------- 邀请/注册 ----------
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"email":"newuser@example.com","role":"user"}' "$base/api/v1/admin/invitations")
expect "invitation 201" 201 "$code"
invite_token=$(cat /tmp/r | json accept_url | sed 's|.*/||')
[ -n "$invite_token" ] && ok "invitation token extracted" || bad "invitation token missing: $(cat /tmp/r)"
expect "register 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -c /tmp/nucookie -H 'Content-Type: application/json' --data-binary '{"token":"'"$invite_token"'","username":"newuser1","password":"NewUserPass123"}' "$base/api/v1/auth/register")"
nutoken=$(cat /tmp/r | json access_token)
NU=(-H "Authorization: Bearer $nutoken")
expect "new user me 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${NU[@]}" "$base/api/v1/me")"
expect "new user admin 403" 403 "$(curl -sS -o /tmp/r -w '%{http_code}' "${NU[@]}" "$base/api/v1/admin/stats")"

# ---------- 密码重置（管理端闭环） ----------
expect "forgot-password 202" 202 "$(curl -sS -o /tmp/r -w '%{http_code}' -H 'Content-Type: application/json' --data-binary '{"email":"newuser@example.com"}' "$base/api/v1/auth/forgot-password")"
nuid=$(curl -sS "${A[@]}" "$base/api/v1/admin/users" | python3 -c 'import json,sys;d=json.load(sys.stdin);us=d.get("users") or d.get("items") or [];print(next((u["id"] for u in us if u.get("username")=="newuser1"),""),end="")')
[ -n "$nuid" ] && ok "admin found new user" || bad "new user not found in admin list"
expect "admin reset pwd 204" 204 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"new_password":"ResetPass12345"}' "$base/api/v1/admin/users/$nuid/reset-password")"
expect "login reset pwd 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -H 'Content-Type: application/json' --data-binary '{"email":"newuser@example.com","password":"ResetPass12345"}' "$base/api/v1/auth/login")"

# ---------- PAT ----------
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"name":"smoke-pat"}' "$base/api/v1/tokens")
expect "create PAT 201" 201 "$code"; pat=$(cat /tmp/r | json token); patid=$(cat /tmp/r | json id)
expect "PAT auth 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -H "Authorization: Bearer $pat" "$base/api/v1/me")"
expect "PAT list 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/tokens")"
expect "PAT revoke 204" 204 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X DELETE "$base/api/v1/tokens/$patid")"
expect "revoked PAT 401" 401 "$(curl -sS -o /tmp/r -w '%{http_code}' -H "Authorization: Bearer $pat" "$base/api/v1/me")"

# ---------- TOTP ----------
totp(){ python3 -c "
import hmac,hashlib,struct,base64,time,sys
key=base64.b32decode(sys.argv[1]+'='*(-len(sys.argv[1])%8))
t=struct.pack('>Q',int(time.time())//30)
h=hmac.new(key,t,hashlib.sha1).digest()
o=h[-1]&15
print('%06d'%((struct.unpack('>I',h[o:o+4])[0]&0x7fffffff)%1000000))" "$1"; }
expect "totp setup 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X POST "$base/api/v1/auth/totp/setup")"
secret=$(cat /tmp/r | json secret)
expect "totp confirm 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"code":"'"$(totp "$secret")"'"}' "$base/api/v1/auth/totp/confirm")"
expect "login blocked 401" 401 "$(curl -sS -o /tmp/r -w '%{http_code}' -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"AdminPassword123"}' "$base/api/v1/auth/login")"
expect "totp login 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' -H 'Content-Type: application/json' --data-binary '{"email":"admin@example.com","password":"AdminPassword123","code":"'"$(totp "$secret")"'"}' "$base/api/v1/auth/login/totp")"
token=$(cat /tmp/r | json access_token); A[1]="Authorization: Bearer $token"
expect "totp disable 204" 204 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"password":"AdminPassword123"}' -X DELETE "$base/api/v1/auth/totp")"

# ---------- AI 禁用态 / 集成探测 ----------
expect "ai 503" 503 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X POST "$base/api/v1/files/$file/ai/summary")"
expect "onlyoffice config" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/onlyoffice/config")"
expect "drawio config" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/drawio/config")"

# ---------- 管理端 ----------
expect "settings update 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"value":5}' -X PUT "$base/api/v1/admin/settings/upload.max_versions_per_file")"
expect "audit logs 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/admin/audit-logs?limit=20")"
expect "audit csv 200" 200 "$(curl -sS -o /tmp/csv -w '%{http_code}' "${A[@]}" "$base/api/v1/admin/audit-logs/export.csv")"
grep -q 'action' /tmp/csv && ok "csv content" || bad "csv empty"
expect "quarantine 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/admin/quarantine")"
expect "backup run 501" 501 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X POST "$base/api/v1/admin/backups/run")"
expect "admin stats 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/admin/stats")"

# ---------- 通知/webhook/会话 ----------
expect "notifications 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/notifications")"
expect "prefs 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/notification-preferences")"
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{"url":"https://example.com/hook","events":["upload.completed"]}' "$base/api/v1/webhooks")
expect "webhook create 201" 201 "$code"; whid=$(cat /tmp/r | json id)
expect "webhook list 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/webhooks")"
expect "webhook delete 204" 204 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X DELETE "$base/api/v1/webhooks/$whid")"
expect "sessions 200" 200 "$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" "$base/api/v1/auth/sessions")"

# ---------- 网页包 ----------
mkdir -p /tmp/webpkg-src && printf '<html><body>hi</body></html>' >/tmp/webpkg-src/index.html
python3 "$(dirname "$0")/make_webpkg_zip.py" >/dev/null
body=$(python3 -c "import json,os;print(json.dumps({'parent_id':'$folder','name':'site.zip','size':os.path.getsize('/tmp/webpkg.zip'),'expected_sha256':'$(sha /tmp/webpkg.zip)'}))")
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary "$body" "$base/api/v1/uploads")
expect "webpkg session 200/201" OK "$([ "$code" = 200 ] || [ "$code" = 201 ] && echo OK || echo "$code")"; wpup=$(cat /tmp/r | json id)
curl -sS -o /dev/null "${A[@]}" -H 'Content-Type: application/octet-stream' -H 'Upload-Offset: 0' --data-binary @/tmp/webpkg.zip -X PATCH "$base/api/v1/uploads/$wpup"
code=$(curl -sS -o /tmp/r2 -w '%{http_code}' "${A[@]}" -H 'Content-Type: application/json' --data-binary '{}' -X POST "$base/api/v1/uploads/$wpup/complete")
sleep 2; wpfile=$(cat /tmp/r2 | json file_id)
code=$(curl -sS -o /tmp/r -w '%{http_code}' "${A[@]}" -X POST "$base/api/v1/files/$wpfile/webpkg/extract")
expect "webpkg extract 200" 200 "$code"; pid=$(cat /tmp/r | json public_id 2>/dev/null)
[ -n "$pid" ] && ok "webpkg pid" || bad "webpkg pid missing: $(cat /tmp/r)"
if [ -n "$pid" ]; then
  expect "webpkg content 200" 200 "$(curl -sS -o /tmp/wc -w '%{http_code}' "$base/content/$pid/index.html")"
  curl -sS -D /tmp/wch -o /dev/null "$base/content/$pid/index.html"
  grep -qi 'content-security-policy' /tmp/wch && ok "webpkg CSP" || bad "webpkg no CSP"
fi

printf '\nRESULT: pass=%s fail=%s\n' "$pass" "$fail"
[ "$fail" = 0 ]
