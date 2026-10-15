#!/usr/bin/env python3
"""Validate the running auth-owner stack. Requires requests and local .env files.

Creates named validation fixtures, then disables them without deleting history.
Sessions stay in memory. Secure cookies are relayed only to the local test chain.
"""
import requests, re, html
from urllib.parse import urlsplit, urljoin
from pathlib import Path
ROOT = Path(__file__).resolve().parent.parent

def env(path):
    return dict((line.strip().split('=', 1) for line in path.read_text().splitlines() if line.strip() and (not line.startswith('#')) and ('=' in line)))
DOMAIN = env(ROOT / '.env')['DOMAIN']
NODE = env(ROOT / '.env')['NODE_NAME']

def login(user='joe', password=None):
    s = requests.Session()

    def req(url, method='GET', **kwargs):
        p = urlsplit(url)
        cookies = '; '.join((f'{c.name}={c.value}' for c in s.cookies if p.hostname.endswith(c.domain.lstrip('.')) and p.path.startswith(c.path)))
        return s.request(method, 'http://127.0.0.1' + p.path + ('?' + p.query if p.query else ''), headers={'Host': p.netloc, 'X-Forwarded-Proto': 'https', 'Cookie': cookies}, allow_redirects=False, timeout=25, **kwargs)
    url = 'https://apps.' + DOMAIN + '/oauth2/start?rd=https%3A%2F%2Fapps.' + DOMAIN + '%2Fapi%2Fv2%2Fme'
    for i in range(12):
        r = req(url)
        if r.is_redirect:
            url = urljoin(url, r.headers['Location'])
            continue
        if 'name="password"' in r.text:
            action = html.unescape(re.search('<form[^>]*action="([^"]+)"', r.text).group(1))
            data = {'username': user, 'password': password or env(ROOT / '.env')['KEYCLOAK_USER_PASSWORD'], 'credentialId': ''}
            r = req(action, 'POST', data=data)
            if r.is_redirect:
                url = urljoin(action, r.headers['Location'])
                continue
        break

    def call(host, path, method='GET', **kw):
        return req('https://' + host + path, method, **kw)
    identity = call('apps.' + DOMAIN, '/api/v2/me')
    assert identity.status_code == 200 and identity.json()['user'] == user, 'fresh login failed'
    return call
import secrets, threading, time
from http.server import HTTPServer, BaseHTTPRequestHandler
import requests

class Backend(BaseHTTPRequestHandler):

    def do_GET(self):
        status = 200 if not self.path.startswith('/v1') or self.headers.get('Authorization') == 'Bearer validation-key' else 401
        self.send_response(status)
        self.end_headers()
        self.wfile.write(b'validation backend')

    def log_message(self, *args):
        pass
for attempt in range(30):
    try:
        if requests.get('http://127.0.0.1:7272/healthz', timeout=2).status_code == 200:
            break
    except requests.RequestException:
        pass
    time.sleep(1)
server = HTTPServer(('0.0.0.0', 19001), Backend)
threading.Thread(target=server.serve_forever, daemon=True).start()
admin = login()
r = admin('apps.' + DOMAIN, '/api/v2/users?q=joe')
print('directory', r.status_code)
assert r.status_code == 200
settings = env(ROOT / '.env')
s = requests.Session()
s.headers.update({'Host': 'auth.' + DOMAIN, 'X-Forwarded-Proto': 'https'})
r = s.post('http://127.0.0.1/realms/master/protocol/openid-connect/token', data={'client_id': 'admin-cli', 'grant_type': 'password', 'username': 'admin', 'password': settings['KEYCLOAK_ADMIN_PASSWORD']})
r.raise_for_status()
s.headers['Authorization'] = 'Bearer ' + r.json()['access_token']
base = 'http://127.0.0.1/admin/realms/local'
username = 'nsl-validation-member'
password = secrets.token_urlsafe(24)
users = s.get(base + '/users', params={'username': username, 'exact': 'true'}).json()
if users:
    memberID = users[0]['id']
    s.put(base + '/users/' + memberID, json={'enabled': True, 'firstName': 'Validation', 'lastName': 'Member', 'requiredActions': []}).raise_for_status()
    s.put(base + '/users/' + memberID + '/reset-password', json={'type': 'password', 'value': password, 'temporary': False}).raise_for_status()
else:
    r = s.post(base + '/users', json={'username': username, 'firstName': 'Validation', 'lastName': 'Member', 'email': username + '@' + DOMAIN, 'enabled': True, 'emailVerified': True, 'credentials': [{'type': 'password', 'value': password, 'temporary': False}]})
    r.raise_for_status()
    memberID = r.headers['Location'].rsplit('/', 1)[1]
local = requests.Session()
local.headers['Authorization'] = 'Bearer ' + settings['REGISTRY_API_TOKEN']
app = None
try:
    member = login(username, password)
    name = 'authz-validation'
    host = name + '--' + NODE + '.' + DOMAIN
    body = {'name': name, 'description': 'NSL authorization integration fixture', 'target_url': 'http://host.docker.internal:19001', 'grant': {'users': [memberID]}, 'routes': [{'rule': f'Host(`{host}`) && !(Path(`/v1`) || PathPrefix(`/v1/`))', 'priority': 100, 'auth': 'browser'}, {'rule': f'Host(`{host}`) && (Path(`/v1`) || PathPrefix(`/v1/`))', 'priority': 110, 'auth': 'upstream'}]}
    existing = [a for a in local.get('http://127.0.0.1:7272/api/v2/apps').json() if a['name'] == name]
    if existing:
        assert existing[0]['target_url'] == 'http://host.docker.internal:19001', 'fixture name belongs to another app'
        app = existing[0]
        r = local.put('http://127.0.0.1:7272/api/v2/apps/' + app['id'], json=body, headers={'If-Match': f'''"app:{app['id']}:{app['generation']}"'''})
    else:
        r = local.post('http://127.0.0.1:7272/api/v2/apps', json=body)
    r.raise_for_status()
    app = r.json()
    for _ in range(30):
        if admin(host, '/').status_code == 200:
            break
        time.sleep(1)

    def check(label, response, want):
        print(label, response.status_code)
        assert response.status_code == want, (label, response.status_code)
    check('admin browser app', admin(host, '/'), 200)
    check('assigned member browser app', member(host, '/'), 200)
    check('member terminal', member('t--' + NODE + '.' + DOMAIN, '/'), 403)
    tr = admin('t--' + NODE + '.' + DOMAIN, '/')
    print('admin terminal', tr.status_code)
    assert tr.status_code not in [401, 403]
    check('member mutation', member('apps.' + DOMAIN, '/api/v2/apps', 'POST', json=body), 403)
    check('member directory', member('apps.' + DOMAIN, '/api/v2/users'), 403)
    headers = {'Host': host, 'X-Auth-Request-User': memberID, 'X-Auth-Request-Groups': 'admin', 'X-Forwarded-User': 'joe', 'X-Forwarded-Groups': 'admin'}
    check('anonymous forged identity', requests.get('http://127.0.0.1/', headers=headers, allow_redirects=False), 401)
    check('API without app key', requests.get('http://127.0.0.1/v1/models', headers={'Host': host}, allow_redirects=False), 401)
    check('API with app key', requests.get('http://127.0.0.1/v1/models', headers={'Host': host, 'Authorization': 'Bearer validation-key'}, allow_redirects=False), 200)
    body['grant'] = {'users': []}
    r = local.put('http://127.0.0.1:7272/api/v2/apps/' + app['id'], json=body, headers={'If-Match': f'''"app:{app['id']}:{app['generation']}"'''})
    r.raise_for_status()
    app = r.json()
    check('revoked member browser app', member(host, '/'), 403)
    apps = member('apps.' + DOMAIN, '/api/v2/apps').json()
    assert not any((a['id'] == app['id'] for a in apps))
    print('revoked app hidden: pass')
finally:
    if app:
        body['enabled'] = False
        body['grant'] = {'users': []}
        r = local.put('http://127.0.0.1:7272/api/v2/apps/' + app['id'], json=body, headers={'If-Match': f'''"app:{app['id']}:{app['generation']}"'''})
        print('test app disabled', r.status_code)
    r = s.put(base + '/users/' + memberID, json={'enabled': False})
    print('test account disabled', r.status_code)
    server.shutdown()
