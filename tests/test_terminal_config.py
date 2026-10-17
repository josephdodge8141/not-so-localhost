import json,subprocess,tempfile,pathlib
r=pathlib.Path(__file__).resolve().parents[1]
c=json.loads(subprocess.check_output(['docker','compose','config','--format','json'],cwd=r))['services']['config-gen']
image='not-so-localhost-config-gen'
subprocess.run(['docker','compose','build','config-gen'],cwd=r,check=True,stdout=subprocess.DEVNULL)
with tempfile.TemporaryDirectory() as d:
 def run(url,auth):
  args=['docker','run','--rm','--user',str(__import__('os').getuid())+':'+str(__import__('os').getgid()),'-v',str(r/'traefik/dynamic/infra.yml.tmpl')+':/templates/infra.yml.tmpl:ro','-v',d+':/output']
  for k,v in {'DOMAIN':'test.invalid','NODE_NAME':'windows-pc','REGISTRY_PROXY_TOKEN':'dummy','NSL_TERMINAL_URL':url,'NSL_TERMINAL_AUTHORIZATION':auth}.items(): args+=['-e',k+'='+v]
  return subprocess.run(args+[image]+[v.replace('$$', '$') for v in c['command']],capture_output=True)
 for url,auth in [('http://host.docker.internal:7681',''),('http://172.22.21.38:7681','Basic dGVzdDp0ZXN0')]:
  p=run(url,auth);assert p.returncode==0,p.stderr.decode();s=(pathlib.Path(d)/'infra.yml').read_text();assert url in s;assert '        - auth\n        - authz-check\n        - terminal-backend\n      service: ttyd' in s;assert '${' not in s
 for url,auth in [('http://user:pass@host:7681',''),('http://host:7681','bad\nheader')]:
  assert run(url,auth).returncode!=0
print('Real config-gen default/WSL rendering, middleware order and invalid-input rejection passed')