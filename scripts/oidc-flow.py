#!/usr/bin/env python3
"""OIDC authorization code 流程模拟（Keycloak 登录页表单提交 → DocFlow 会话）。"""
import http.cookiejar, json, re, sys, urllib.parse, urllib.request

BASE = 'http://127.0.0.1'
# Keycloak issuer 主机名（与 .env OIDC_ISSUER 逐字一致；Ubuntu hosts 指向
# 127.0.0.1，Docker Desktop 对集成发行区做 localhost 端口转发）。
ISSUER_HOST = 'http://docflow-idp.local:18090'

jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(
    urllib.request.HTTPCookieProcessor(jar),
    urllib.request.HTTPRedirectHandler())

# 不自动跟随重定向：逐步检查 Location
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None
step = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), NoRedirect())

def get(url, headers=None):
    req = urllib.request.Request(url, headers=headers or {})
    try:
        with step.open(req) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()

# 1) DocFlow login 端点 -> 302 Keycloak authorize
code, hdrs, body = get(f'{BASE}/api/v1/auth/oidc/login')
assert code in (302, 303), f'login not redirect: {code} {body[:200]}'
loc = hdrs.get('Location')
assert loc.startswith(ISSUER_HOST), f'unexpected idp host: {loc[:120]}'
print('PASS login ->', loc[:100], '...')

# 2) authorize -> 200 登录表单（cookie: AUTH_SESSION_ID）
code, hdrs, body = get(loc)
assert code == 200, f'authorize page: {code}'
html = body.decode()
m = re.search(r'action="([^"]+)"', html)
assert m, 'no form action in login page'
action = m.group(1).replace('&amp;', '&')
print('PASS got keycloak login form')

# 3) 提交用户名密码 -> 302 callback?code&state
data = urllib.parse.urlencode({'username': 'oidcuser', 'password': 'OidcPass123',
    'credentialId': ''}).encode()
req = urllib.request.Request(action, data=data)
try:
    with step.open(req) as r:
        code, hdrs, body = r.status, dict(r.headers), r.read()
except urllib.error.HTTPError as e:
    code, hdrs, body = e.code, dict(e.headers), e.read()
assert code in (302, 303), f'kc submit not redirect: {code} {body[:300]}'
cb = hdrs.get('Location')
# Keycloak 26 首登 VERIFY_PROFILE 必办动作：再提交一次预填表单
if 'login-actions/required-action' in cb:
    code, hdrs, body = get(cb)
    assert code == 200, f'required-action page: {code}'
    html = body.decode()
    m = re.search(r'action="([^"]+)"', html)
    assert m, 'no form action in required-action'
    action = m.group(1).replace('&amp;', '&')
    fields = dict(re.findall(r'name="([^"]+)" value="([^"]*)"', html))
    # VERIFY_PROFILE 表单的可见字段（username/email/firstName/lastName）须
    # 全部提供合法值：空值会导致校验失败回到同一页（200）。
    defaults = {'firstName': 'Oidc', 'lastName': 'User',
                'username': 'oidcuser', 'email': 'oidcuser@example.com'}
    for k, v in defaults.items():
        if not fields.get(k):
            fields[k] = v
    data = urllib.parse.urlencode(fields).encode()
    req = urllib.request.Request(action, data=data)
    try:
        with step.open(req) as r:
            code, hdrs, body = r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        code, hdrs, body = e.code, dict(e.headers), e.read()
    cb = None
    for k, v in hdrs.items():
        if k.lower() == 'location':
            cb = v
    if cb is None:
        raise SystemExit(f'required-action submit: status={code}, headers={ {k: v for k, v in hdrs.items() if k.lower() in ("location", "content-type")} }, body={body[:200]}')
    print('PASS required-action (VERIFY_PROFILE) completed')
assert '/api/v1/auth/oidc/callback' in cb and 'code=' in cb, f'no callback code: {cb[:160]}'
print('PASS keycloak authenticated -> callback with code')

# 4) callback -> 302 前端 /sso#access_token=...（DocFlow 下发 refresh cookie）
code, hdrs, body = get(cb.replace('http://127.0.0.1', BASE))
assert code in (302, 303), f'callback: {code} {body[:300]}'
names = [c.name for c in jar]
assert any('refresh' in n for n in names), f'no session cookie: {names}'
fwd = None
for k, v in hdrs.items():
    if k.lower() == 'location':
        fwd = v
print('PASS callback ok, cookies:', names, '->', fwd)
assert '#access_token=' in fwd, 'no access token in redirect'
access = fwd.split('#access_token=')[1].split('&')[0]

# 5) 会话生效：Bearer access -> /me 返回自动开户的 oidc 用户
req = urllib.request.Request(f'{BASE}/api/v1/me',
    headers={'Authorization': f'Bearer {access}'})
with opener.open(req) as r:
    me = json.load(r)
print('PASS /me ->', json.dumps(me)[:160])
assert me.get('email') == 'oidcuser@example.com', f'unexpected user: {me}'
print('ALL OIDC PASS')
