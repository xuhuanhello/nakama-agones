#!/usr/bin/env python3
"""Real production SSH forwarding in disposable Docker resources; never contacts a VPS."""
import argparse,json,secrets,subprocess,tempfile,time
from pathlib import Path


def main():
 p=argparse.ArgumentParser(description=__doc__)
 p.add_argument('--tunnel-source',type=Path,default=Path(__file__).resolve().parents[1]/'deploy/tunnel')
 p.add_argument('--alpine-image',default='alpine:3.21')
 p.add_argument('--client-image',default='python:3.12-slim')
 a=p.parse_args();name='gf-ssh-'+secrets.token_hex(5);containers=[];vol=False;net=False;images=[]
 def docker(*args,data=None,check=True,timeout=120):
  r=subprocess.run(['docker',*args],input=data,capture_output=True,timeout=timeout)
  if check and r.returncode:raise RuntimeError('isolated Docker '+args[0]+' failed: '+r.stderr.decode(errors='replace')[-1500:])
  return r.stdout
 with tempfile.TemporaryDirectory(prefix=name) as temp:
  temp=Path(temp)
  try:
   for image in (a.alpine_image,a.client_image):docker('image','inspect',image)
   tunnel_image=name+'-tunnel';fixture_image=name+'-fixture';images+=[tunnel_image,fixture_image]
   docker('build','--build-arg','ALPINE_IMAGE='+a.alpine_image,'-t',tunnel_image,str(a.tunnel_source),timeout=180)
   (temp/'Dockerfile').write_text('ARG ALPINE_IMAGE\nFROM ${ALPINE_IMAGE}\nRUN apk add --no-cache openssh-server python3 && adduser -D -h /home/tunnel tunnel && echo "tunnel:fixture-password-disabled-by-sshd" | chpasswd && mkdir -p /run/sshd\n')
   docker('build','--build-arg','ALPINE_IMAGE='+a.alpine_image,'-t',fixture_image,str(temp),timeout=180)
   docker('volume','create',name);vol=True
   init="""set -eu
umask 077
ssh-keygen -q -t ed25519 -N '' -f /fixture/platform-ssh-key
ssh-keygen -q -t ed25519 -N '' -f /fixture/ssh_host_key
cp /fixture/platform-ssh-key.pub /fixture/authorized_keys
printf 'platform ' > /fixture/platform-known-hosts
cat /fixture/ssh_host_key.pub >> /fixture/platform-known-hosts
chmod 0400 /fixture/platform-ssh-key /fixture/platform-known-hosts
chmod 0644 /fixture/authorized_keys
cat > /fixture/sshd_config <<'EOF'
Port 22
ListenAddress 0.0.0.0
HostKey /fixture/ssh_host_key
AuthorizedKeysFile /fixture/authorized_keys
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers tunnel
AllowTcpForwarding local
PermitOpen 127.0.0.1:17682
PermitTTY no
AllowAgentForwarding no
X11Forwarding no
GatewayPorts no
StrictModes yes
UsePAM no
LogLevel ERROR
EOF
cat > /fixture/server.py <<'EOF'
from http.server import BaseHTTPRequestHandler,HTTPServer
class H(BaseHTTPRequestHandler):
 def do_GET(self):
  data=b'{"forwarded":true}';self.send_response(200);self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
 def log_message(self,*args):pass
HTTPServer(('127.0.0.1',17682),H).serve_forever()
EOF
chmod 0644 /fixture/server.py /fixture/sshd_config
"""
   docker('run','--rm','--network','none','-v',name+':/fixture',fixture_image,'sh','-c',init)
   docker('network','create','--internal',name);net=True
   server=name+'-sshd';containers.append(server)
   docker('run','-d','--name',server,'--network',name,'--network-alias','platform','-v',name+':/fixture:ro',fixture_image,'sh','-c','/usr/sbin/sshd -D -e -f /fixture/sshd_config & exec python3 /fixture/server.py')
   owner=name+'-owner';containers.append(owner)
   docker('run','-d','--name',owner,'--network',name,a.client_image,'python','-c','import time; time.sleep(900)')
   sidecar=name+'-tunnel';containers.append(sidecar)
   docker('run','-d','--name',sidecar,'--restart','unless-stopped','--network','container:'+owner,'--cap-drop','ALL','--security-opt','no-new-privileges:true','--read-only','-v',name+':/run/secrets:ro','-e','GAMEFLEET_SSH_TARGET=tunnel@platform','-e','GAMEFLEET_SSH_PORT=22',tunnel_image)
   probe="import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:17682',timeout=2).read().decode())"
   def forwarded(seconds):
    deadline=time.monotonic()+seconds
    while time.monotonic()<deadline:
     response=docker('exec',owner,'python','-c',probe,check=False,timeout=5)
     if response.strip()==b'{"forwarded":true}':return True
     time.sleep(.25)
    return False
   if not forwarded(40):
    raise RuntimeError('forwarding failed: '+docker('logs',sidecar,check=False).decode(errors='replace')[-500:])
   inspect=json.loads(docker('inspect',sidecar))[0]
   if inspect['HostConfig']['CapDrop']!=['ALL'] or inspect['HostConfig']['PortBindings']:raise RuntimeError('sidecar isolation mismatch')
   modes=docker('exec',sidecar,'stat','-c','%u:%a','/run/secrets/platform-ssh-key','/run/secrets/platform-known-hosts').decode().splitlines()
   if modes!=['0:400','0:400']:raise RuntimeError('credential owner/modes mismatch')
   docker('restart','--time','1',owner)
   owner_restart=forwarded(8)
   automatic_recovered=owner_restart or forwarded(75)
   recovered=automatic_recovered
   if not recovered:
    docker('restart','--time','1',sidecar)
    recovered=forwarded(40)
   if not recovered:raise RuntimeError('forwarding did not recover after namespace-owner and sidecar restart')
   print(json.dumps({'passed':True,'production_tunnel_entrypoint':True,'real_ssh_forwarded_http':True,'cap_drop_all':True,'root_0400_credentials':True,'owner_restart_alone_preserved_forward':owner_restart,'automatic_restart_policy_recovered':automatic_recovered,'owner_and_sidecar_restart_recovered':recovered,'public_ports':False,'runtime_external_network':False,'vps_contacted':False,'build_may_fetch_alpine_packages':True}))
  finally:
   for container in reversed(containers):docker('rm','-f',container,check=False)
   if net:docker('network','rm',name,check=False)
   if vol:docker('volume','rm',name,check=False)
   for image in images:docker('image','rm',image,check=False)

if __name__=='__main__':main()
