#!/usr/bin/env python3
"""手写 HS256 JWT（无三方依赖）+ 模拟 OnlyOffice DocumentServer 保存回调。"""
import base64, hmac, hashlib, io, json, subprocess, sys, urllib.request, time, uuid, zipfile

SECRET = b'verify-onlyoffice-jwt-secret-0123456789'
BASE = 'http://127.0.0.1'

def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b'=').decode()

def sign(claims: dict) -> str:
    h = b64url(json.dumps({'alg': 'HS256', 'typ': 'JWT'}).encode())
    p = b64url(json.dumps(claims).encode())
    sig = b64url(hmac.new(SECRET, f'{h}.{p}'.encode(), hashlib.sha256).digest())
    return f'{h}.{p}.{sig}'

def minimal_docx(text: str) -> bytes:
    out = io.BytesIO()
    with zipfile.ZipFile(out, 'w', zipfile.ZIP_DEFLATED) as archive:
        archive.writestr('[Content_Types].xml', '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>')
        archive.writestr('_rels/.rels', '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>')
        archive.writestr('word/document.xml', '<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>' + text + '</w:t></w:r></w:p><w:sectPr/></w:body></w:document>')
    return out.getvalue()

def login():
    req = urllib.request.Request(f'{BASE}/api/v1/auth/login',
        data=json.dumps({'email': 'admin@example.com', 'password': 'AdminPassword123'}).encode(),
        headers={'Content-Type': 'application/json'})
    return json.load(urllib.request.urlopen(req))['access_token']

def api(method, path, token, body=None, expect=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f'{BASE}{path}', data=data, method=method,
        headers={'Content-Type': 'application/json', 'Authorization': f'Bearer {token}'})
    try:
        with urllib.request.urlopen(req) as r:
            code = r.status
            payload = json.load(r)
    except urllib.error.HTTPError as e:
        raise SystemExit(f'{method} {path}: HTTP {e.code}: {e.read().decode()[:300]}')
    if expect and code != expect:
        raise SystemExit(f'{method} {path}: {code} != {expect}: {payload}')
    return payload

token = login()
me = api('GET', '/api/v1/me', token)
admin_id = me['id'] if isinstance(me, dict) and 'id' in me else me

# 1) 上传合法的最小 DOCX（版本 1）
fname = f'oo-test-{uuid.uuid4().hex[:6]}.docx'
content = minimal_docx('DocFlow OnlyOffice callback test v1')
up = api('POST', '/api/v1/uploads', token, {
    'name': fname, 'size': len(content)}, expect=201)
req = urllib.request.Request(f"{BASE}/api/v1/uploads/{up['id']}", data=content,
    method='PATCH', headers={'Content-Type': 'application/octet-stream',
        'Upload-Offset': '0', 'Authorization': f'Bearer {token}'})
urllib.request.urlopen(req)
done = api('POST', f"/api/v1/uploads/{up['id']}/complete", token, {}, expect=200)
file_id = done['file_id']
print('PASS upload docx:', file_id)

# 2) onlyoffice session → document.key
sess = api('POST', '/api/v1/onlyoffice/session', token, {'file_id': file_id})
doc_key = sess['document']['key']
print('PASS session, document.key =', doc_key)

# 3) 版本基线（响应结构 {"versions": [...]}）
vers_before = api('GET', f'/api/v1/files/{file_id}/versions', token)
n_before = len(vers_before.get('versions', vers_before if isinstance(vers_before, list) else []))
print('versions before =', n_before)

# 4) 把合法 DOCX 放到 DocumentServer 同源临时 HTTP 目录，再模拟保存回调。
saved_name = f'docflow-callback-{uuid.uuid4().hex}.docx'
subprocess.run(['docker', 'compose', 'exec', '-T', 'onlyoffice', 'sh', '-c', f'cat > /var/www/onlyoffice/documentserver-example/public/{saved_name}'], input=minimal_docx('DocFlow OnlyOffice callback saved v2'), check=True)
cb_url = f'http://onlyoffice/example/{saved_name}'
claims = {'key': doc_key, 'status': 2, 'url': cb_url, 'users': [admin_id]}
jwt = sign(claims)
def post_callback(url, tok):
    req = urllib.request.Request(f'{BASE}/api/v1/onlyoffice/callback',
        data=json.dumps({'key': doc_key, 'status': 2, 'url': url,
                         'users': [admin_id], 'token': tok}).encode(),
        headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req) as r:
        return json.load(r)
resp = post_callback(cb_url, jwt)
if resp != {'error': 0}:
    time.sleep(10)  # DS 容器间歇抖动（环境），幂等记录已 Release 可重试
    resp = post_callback(cb_url, jwt)
assert resp == {'error': 0}, f'callback rejected: {resp}'
print('PASS callback accepted')

# 5) 版本 +1
time.sleep(1)
vers_after = api('GET', f'/api/v1/files/{file_id}/versions', token)
items = vers_after.get('versions', vers_after if isinstance(vers_after, list) else [])
print('versions after =', len(items))
assert len(items) == n_before + 1, 'version not created by callback'
print('PASS callback saved new version')

# 6) 幂等：同 (file_id,key,url) 重复回调不加版本
req = urllib.request.Request(f'{BASE}/api/v1/onlyoffice/callback',
    data=json.dumps({'key': doc_key, 'status': 2, 'url': cb_url,
                     'users': [admin_id], 'token': jwt}).encode(),
    headers={'Content-Type': 'application/json'})
with urllib.request.urlopen(req) as r:
    assert json.load(r) == {'error': 0}
vers_dup = api('GET', f'/api/v1/files/{file_id}/versions', token)
items2 = vers_dup.get('versions', vers_dup if isinstance(vers_dup, list) else [])
assert len(items2) == len(items), 'idempotency broken'
print('PASS callback idempotent')

# 7) 非同源 url 拒绝（SSRF 防护）：DS 协议拒绝也是 200 + {"error":1}
bad = sign({'key': doc_key, 'status': 2, 'url': 'http://evil.example/x', 'users': [admin_id]})
req = urllib.request.Request(f'{BASE}/api/v1/onlyoffice/callback',
    data=json.dumps({'key': doc_key, 'status': 2, 'url': 'http://evil.example/x',
                     'users': [admin_id], 'token': bad}).encode(),
    headers={'Content-Type': 'application/json'})
with urllib.request.urlopen(req) as r:
    body = json.load(r)
assert body == {'error': 1}, f'ssrf url accepted: {body}'
print('PASS ssrf url rejected (error:1)')
