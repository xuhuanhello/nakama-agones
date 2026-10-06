#!/usr/bin/env python3
"""Exercise the actual production entrypoint locally; no SSH, external ports or email."""
import argparse, ast, base64, hashlib, json, os, secrets, subprocess, tempfile, time
from pathlib import Path


def main():
 p=argparse.ArgumentParser(description=__doc__)
 p.add_argument('--entrypoint',type=Path,default=Path(__file__).resolve().parents[1]/'deploy/nakama-entrypoint.sh')
 p.add_argument('--fixed-source',type=Path,required=True,help='Fixed source checkout containing deploy/account/smoke.py')
 p.add_argument('--image',default='fixed-nakama:bootstrap-check')
 p.add_argument('--postgres-image',default='postgres:16-alpine')
 p.add_argument('--client-image',default='python:3.12-slim')
 a=p.parse_args(); prefix='gf-entrypoint-'+secrets.token_hex(5); names=[];network=False
 def docker(*args,data=None,check=True,timeout=90):
  r=subprocess.run(['docker',*args],input=data,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=timeout)
  if check and r.returncode:raise RuntimeError('isolated Docker operation failed: '+args[0])
  return r.stdout
 with tempfile.TemporaryDirectory(prefix=prefix) as temp:
  folder=Path(temp)
  def write(name,data):
   f=folder/name;f.write_text(data);f.chmod(0o600);return f
  try:
   for image in (a.image,a.postgres_image,a.client_image):docker('image','inspect',image)
   docker('network','create','--internal',prefix);network=True
   secret_names=['postgres_password','nakama_socket_server_key','nakama_session_encryption_key','nakama_refresh_encryption_key','nakama_runtime_http_key','nakama_console_password','nakama_console_signing_key']
   values={name:secrets.token_urlsafe(32) for name in secret_names}
   for name,value in values.items():write(name,value)
   write('account-credentials.json',json.dumps({'secret_id':'fixture-no-api-identity','secret_key':'fixture-no-api-secret','code_hmac_key':secrets.token_hex(32)}))
   db_env=write('postgres.env','POSTGRES_USER=nakama\nPOSTGRES_DB=nakama\nPOSTGRES_PASSWORD='+values['postgres_password']+'\n')
   db=prefix+'-db';names.append(db)
   docker('run','-d','--pull=never','--name',db,'--network',prefix,'--network-alias','postgres','--env-file',str(db_env),a.postgres_image)
   for _ in range(45):
    if subprocess.run(['docker','exec',db,'pg_isready','-U','nakama'],capture_output=True).returncode==0:break
    time.sleep(1)
   else:raise RuntimeError('database readiness failed')
   scope={'key':'gfsvc_'+secrets.token_urlsafe(32),'service_id':prefix+'-service','application_id':prefix+'-app','identity_issuer':'fixture-issuer','region':'fixture-region','compatibility':'fixture-compatibility'}
   write('service-scope.json',json.dumps(scope));write('service-key',scope['key'])
   source=ast.parse((a.fixed_source/'deploy/account/smoke.py').read_text())
   mocks=[n.value for n in ast.walk(source) if isinstance(n,ast.Constant) and isinstance(n.value,str) and "HTTPServer(('127.0.0.1', 17682), Handler).serve_forever()" in n.value]
   if len(mocks)!=1:raise RuntimeError('account service fixture source ambiguous')
   write('mock.py',mocks[0])
   env={'NAKAMA_FLEET_BACKEND':'gamefleet-service','GAMEFLEET_SERVICE_URL':'http://127.0.0.1:17682','GAMEFLEET_SERVICE_KEY_FILE':'/fixture/service-key','GAMEFLEET_SERVICE_ID':scope['service_id'],'GAMEFLEET_SERVICE_APPLICATION_ID':scope['application_id'],'GAMEFLEET_SERVICE_IDENTITY_ISSUER':scope['identity_issuer'],'GAMEFLEET_SERVICE_REGION':scope['region'],'GAMEFLEET_SERVICE_COMPATIBILITY':scope['compatibility'],'SES_REGION':'ap-hongkong','SES_FROM_EMAIL':'noreply@example.test','SES_FROM_NAME':'Fixture','SES_REGISTER_TEMPLATE_ID':'1','SES_RESET_TEMPLATE_ID':'2','SES_REGISTER_SUBJECT':'Fixture register','SES_RESET_SUBJECT':'Fixture reset','DM_ACCOUNT_SES_CREDENTIALS_FILE':'/fixture/account-credentials.json'}
   environment=write('nakama.env',''.join(k+'='+v+'\n' for k,v in env.items()))
   # Keep a stable namespace owner so restarting Nakama does not invalidate mock sockets.
   client=prefix+'-namespace';names.append(client)
   docker('run','-d','--pull=never','--name',client,'--network',prefix,'-v',str(folder)+':/fixture:ro',a.client_image,'python','/fixture/mock.py')
   server=prefix+'-nakama';names.append(server)
   args=['run','-d','--pull=never','--platform','linux/amd64','--name',server,'--network','container:'+client,'--env-file',str(environment),'-v',str(folder)+':/fixture:ro','-v',str(a.entrypoint.resolve())+':/production-entrypoint.sh:ro','--tmpfs','/run/nakama-config:rw,noexec,nosuid,mode=0700']
   for name in secret_names:args+=['-v',str(folder/name)+':/run/secrets/'+name+':ro']
   args+=['--entrypoint','/bin/sh',a.image,'/production-entrypoint.sh']
   docker(*args)
   probe='''import sys,json,urllib.request,urllib.error
v=json.load(sys.stdin);r=urllib.request.Request('http://127.0.0.1:7350'+v['path'],headers=v['headers'],data=json.dumps(v['payload']).encode() if v['payload'] is not None else None)
try:
 with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(r,timeout=3) as s: print(json.dumps([s.status,json.loads(s.read(65536))]))
except urllib.error.HTTPError as e: print(json.dumps([e.code,json.loads(e.read(65536))]))
except OSError: print(json.dumps([0,{}]))
'''
   def api(path,payload=None,bearer=None):
    auth='Bearer '+bearer if bearer else 'Basic '+base64.b64encode((values['nakama_socket_server_key']+':').encode()).decode()
    return json.loads(docker('exec','-i',client,'python','-c',probe,data=json.dumps({'path':path,'payload':payload,'headers':{'Authorization':auth,'Content-Type':'application/json'}}).encode()))
   def ready():
    for _ in range(60):
     status,body=api('/account/v1/config')
     if status==200 and body.get('enabled') is True:return
     time.sleep(1)
    logs=docker('logs',server,check=False).decode(errors='replace')
    # Fixture secrets are never echoed; diagnostics expose only initialization signals.
    raise RuntimeError('entrypoint initialization failed; migration='+str('migrat' in logs.lower())+' plugin='+str('plugin' in logs.lower()))
   ready()
   status,session=api('/v2/account/authenticate/device?create=true',{'id':prefix+'-device'})
   if status!=200 or not session.get('token'):raise RuntimeError('device registration failed')
   status,account=api('/v2/account',bearer=session['token'])
   if status!=200:raise RuntimeError('account read failed')
   user_id=account['user']['id']
   def snapshot():
    # Hash only: generated configuration and secrets never enter test output.
    config_hash=docker('exec',server,'sha256sum','/run/nakama-config/config.yml').decode().split()[0]
    hashes={name:hashlib.sha256((folder/name).read_bytes()).hexdigest() for name in secret_names+['account-credentials.json','service-key']}
    return config_hash,hashes
   before=snapshot()
   status,_=api('/v2/account/authenticate/email?create=true',{'email':'unverified@example.test','password':'Fixture-Not-A-Secret'})
   if status not in (400,401,403,404):raise RuntimeError('native signup bypass not blocked')
   docker('restart','--time','35',server,timeout=60);ready()
   status,preserved=api('/v2/account',bearer=session['token'])
   if status!=200 or preserved.get('user',{}).get('id')!=user_id:raise RuntimeError('existing session/account not preserved')
   status,again=api('/v2/account/authenticate/device?create=false',{'id':prefix+'-device'})
   if status!=200:raise RuntimeError('existing device login after restart failed')
   if snapshot()!=before:raise RuntimeError('restart changed configuration or secret source')
   count=docker('logs',client).decode().count('account_smoke_service_scope_ok')
   if count<2:raise RuntimeError('service plugin was not loaded on both starts')
   rows=docker('exec',db,'psql','-U','nakama','-d','nakama','-tAc',"SELECT count(*) FROM users WHERE email='unverified@example.test';").decode().strip()
   if rows!='0':raise RuntimeError('unverified account persisted')
   print(json.dumps({'passed':True,'actual_production_entrypoint':True,'migration_on_both_starts':True,'both_plugins_loaded_on_both_starts':True,'existing_session_and_user_preserved':True,'generated_config_and_secrets_unchanged':True,'unverified_signup_blocked':True,'external_ports':False,'external_network':False,'mail_sent':False,'ssh_tested':False}))
  finally:
   for name in reversed(names):docker('rm','-f','-v',name,check=False)
   if network:docker('network','rm',prefix,check=False)

if __name__=='__main__':main()
