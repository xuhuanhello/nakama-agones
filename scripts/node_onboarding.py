#!/usr/bin/env python3
"""Internal fixed-purpose SSH installer used by the Kubernetes controller."""
from __future__ import annotations

import base64
import hashlib
import ipaddress
import json
import os
import pwd
from pathlib import Path
import re
import secrets
import socket
import ssl
import stat
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

ID = re.compile(r'^[a-f0-9]{32}$')
LABEL = re.compile(r'^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$')
FINAL = {'ready', 'failed'}
TTL = 600
MAX_BODY = 8192


class Failure(Exception):
    def __init__(self, code, status=400):
        self.code, self.status = code, status
        super().__init__(code)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # The cluster bearer token must never follow even a same-host redirect.
        return None


def lock_process(config):
    import fcntl
    directory = Path(config['state_dir'])
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = directory.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
        raise Failure('unsafe_broker_directory', 503)
    fd = os.open(directory / '.broker.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    lock = os.fdopen(fd, 'a')
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise Failure('unsafe_broker_lock', 503)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return lock
    except (OSError, Failure):
        lock.close()
        raise Failure('another_onboarding_broker_is_running', 503) from None


def private_read(path, maximum=65536, owner=None):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            expected_owner = os.geteuid() if owner is None else owner
            if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != expected_owner:
                raise Failure('private_file_permissions', 503)
            value = stream.read(maximum + 1)
            if not value or len(value) > maximum:
                raise Failure('private_file_size', 503)
            return value
    except OSError:
        raise Failure('private_file_unavailable', 503) from None


def atomic_json(path, value):
    path = Path(path)
    tmp = path.parent / ('.write-' + secrets.token_hex(8))
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, ensure_ascii=True, allow_nan=False)
            stream.flush()
            os.fsync(stream.fileno())
        if path.is_symlink():
            raise Failure('unsafe_state_path', 503)
        os.replace(tmp, path)
    finally:
        tmp.unlink(missing_ok=True)


def object_fields(value, fields, required=()):
    if not isinstance(value, dict) or set(value) - set(fields) or set(required) - set(value):
        raise Failure('invalid_request')


def literal_ip(value):
    try:
        address = ipaddress.IPv4Address(value)
    except (ValueError, TypeError):
        raise Failure('literal_ipv4_required') from None
    if address.is_loopback or address.is_multicast or address.is_unspecified or address.is_link_local or address.is_reserved:
        raise Failure('invalid_target_address')
    return str(address)


def bounded_int(value, low, high):
    if type(value) is not int or not low <= value <= high:
        raise Failure('invalid_request')
    return value


def load_config(path):
    try:
        config = json.loads(private_read(path))
        object_fields(config, ('socket_path', 'state_dir', 'runtime_dir', 'node_script', 'regions'),
                      ('socket_path', 'state_dir', 'runtime_dir', 'node_script', 'regions'))
        for key in ('socket_path', 'state_dir', 'runtime_dir', 'node_script'):
            p = Path(config[key])
            if not p.is_absolute() or '..' in p.parts or p.is_symlink():
                raise Failure('invalid_configuration', 503)
        if not isinstance(config['regions'], list) or not 1 <= len(config['regions']) <= 16:
            raise Failure('invalid_configuration', 503)
        names = set()
        for region in config['regions']:
            object_fields(region, ('name', 'cluster_id', 'server', 'private_cidrs', 'protected_hosts',
                          'join_token_file', 'registry_config_file', 'registry_ca_file', 'api_token_file',
                          'api_ca_file', 'api_token_owner', 'game_port_min', 'game_port_max'),
                          ('name', 'cluster_id', 'server', 'private_cidrs', 'protected_hosts',
                           'join_token_file', 'api_token_file', 'api_ca_file'))
            if not LABEL.fullmatch(region['name']) or not LABEL.fullmatch(region['cluster_id']) or region['name'] in names:
                raise Failure('invalid_configuration', 503)
            names.add(region['name'])
            url = urllib.parse.urlsplit(region['server'])
            ip = ipaddress.IPv4Address(url.hostname or '')
            if url.scheme != 'https' or url.port != 6443 or url.path not in ('', '/') or url.username or url.query or url.fragment or not ip.is_private:
                raise Failure('invalid_configuration', 503)
            if not region['private_cidrs'] or len(region['private_cidrs']) > 8:
                raise Failure('invalid_configuration', 503)
            for cidr in region['private_cidrs']:
                network = ipaddress.IPv4Network(cidr, strict=True)
                if not network.is_private or network.prefixlen < 16:
                    raise Failure('invalid_configuration', 503)
            for host in region['protected_hosts']:
                literal_ip(host)
            for key in ('join_token_file', 'registry_config_file', 'registry_ca_file', 'api_token_file', 'api_ca_file'):
                if region.get(key) and (not Path(region[key]).is_absolute() or '..' in Path(region[key]).parts):
                    raise Failure('invalid_configuration', 503)
            if bool(region.get('registry_config_file')) != bool(region.get('registry_ca_file')):
                raise Failure('invalid_configuration', 503)
            if region.get('api_token_owner', 'root') not in ('root', 'fleet-console'):
                raise Failure('invalid_configuration', 503)
        return config
    except (ValueError, TypeError, KeyError):
        raise Failure('invalid_configuration', 503) from None


# Only fixed probe code is executed remotely. It reads system metadata and
# attempts a TCP connection to the explicitly configured private API endpoint.
PROBE = r'''import json,os,pathlib,platform,socket,subprocess,sys
v=json.load(sys.stdin)
def command(args):
 p=subprocess.run(args,capture_output=True,text=True,timeout=4)
 return p.stdout if p.returncode==0 else ''
osr={}
for line in pathlib.Path('/etc/os-release').read_text().splitlines():
 if '=' in line:
  k,x=line.split('=',1);osr[k]=x.strip('"')
mem=next(int(x.split()[1])//1024 for x in pathlib.Path('/proc/meminfo').read_text().splitlines() if x.startswith('MemTotal:'))
disk=os.statvfs('/'); free=disk.f_bavail*disk.f_frsize/(1024**3)
addresses=[]
for link in json.loads(command(['ip','-j','address','show']) or '[]'):
 for a in link.get('addr_info',[]):
  if a.get('family')=='inet': addresses.append({'interface':link['ifname'],'ip':a['local']})
api=False
try:
 with socket.create_connection((v['api_host'],6443),timeout=3):api=True
except OSError:pass
print(json.dumps({'uid':os.geteuid(),'system':platform.system(),'arch':platform.machine(),
 'os':osr.get('ID',''),'cpu_cores':os.cpu_count(),'memory_mib':mem,'disk_free_gib':round(free,1),
 'systemd':pathlib.Path('/run/systemd/system').is_dir(),'swap':len(pathlib.Path('/proc/swaps').read_text().splitlines())>1,
 'addresses':addresses,'api_reachable':api,'python_version':list(sys.version_info[:2]),
 'existing_installation':any(pathlib.Path(p).exists() for p in ('/etc/rancher/k3s','/var/lib/rancher/k3s','/etc/kubernetes','/var/lib/kubelet','/etc/rancher/rke2'))}))
'''

INSTALL = r'''import base64,json,os,pathlib,subprocess,sys,tempfile
v=json.load(sys.stdin);os.umask(0o077)
try:
 with tempfile.TemporaryDirectory(prefix='fleet-join-',dir='/run') as d:
  d=pathlib.Path(d)
  for name,data in v['files'].items():
   if name not in ('k3s_node.py','join.token','registry.json','registry-ca.crt'):raise ValueError()
   p=d/name;p.write_bytes(base64.b64decode(data,validate=True));p.chmod(0o600)
  for cmd in (['apt-get','update','-qq'],['apt-get','install','-y','--no-install-recommends','python3','ca-certificates','curl','iproute2','logrotate']):
   subprocess.run(cmd,check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=180,env=dict(os.environ,DEBIAN_FRONTEND='noninteractive'))
  cmd=['python3',str(d/'k3s_node.py'),'worker','--cluster-id',v['cluster_id'],'--node-name',v['node_name'],
   '--private-ip',v['private_ip'],'--external-ip',v['external_ip'],'--interface',v['interface'],
   '--server',v['server'],'--token-file',str(d/'join.token')]
  if 'registry.json' in v['files']:cmd+=['--registry-config-file',str(d/'registry.json'),'--registry-ca-file',str(d/'registry-ca.crt')]
  subprocess.run(cmd,check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=420)
 print(json.dumps({'installed':True}))
except Exception:
 print(json.dumps({'installed':False}));sys.exit(1)
'''


class Broker:
    def __init__(self, config):
        self.config = config
        self.regions = {r['name']: r for r in config['regions']}
        self.lock = threading.RLock()
        self.slots = threading.BoundedSemaphore(2)
        self.scans, self.preflights, self.jobs = {}, {}, {}
        self.state = Path(config['state_dir'])
        self.runtime = Path(config['runtime_dir'])
        for directory in (self.state, self.runtime):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)
            if directory.is_symlink() or directory.stat().st_uid != os.geteuid() or directory.stat().st_mode & 0o077:
                raise Failure('unsafe_broker_directory', 503)
        for p in self.state.glob('job-*.json'):
            try:
                job = json.loads(private_read(p))
                if not ID.fullmatch(job['id']): continue
                if job['state'] not in FINAL:
                    job.update(state='failed', stage='manual_verification_required', error='broker_restarted_check_node_before_retry', updated_at=int(time.time()))
                    atomic_json(p, job)
                self.jobs[job['id']] = job
            except (Failure, ValueError, KeyError):
                raise Failure('invalid_job_state', 503) from None
        self.prune()

    def prune(self):
        now = time.time()
        with self.lock:
            for values in (self.scans, self.preflights):
                for key, value in list(values.items()):
                    active_job = value.get('job_id') and self.jobs.get(value['job_id'], {}).get('state') not in FINAL
                    if value['expires_at'] < now and not active_job:
                        values.pop(key, None)
            ordered = sorted(self.jobs.values(), key=lambda x:x['created_at'], reverse=True)
            for i, job in enumerate(ordered):
                if job['state'] in FINAL and (i >= 100 or job['updated_at'] < now - 7*86400):
                    self.jobs.pop(job['id'], None)
                    (self.state / ('job-' + job['id'] + '.json')).unlink(missing_ok=True)

    def capabilities(self):
        self.prune()
        return {'enabled':True, 'username_mode':'root_only',
                'regions':[{k:r.get(k,20000 if k=='game_port_min' else 20999) for k in ('name','cluster_id','server','game_port_min','game_port_max')} for r in self.regions.values()],
                'jobs':sorted(self.jobs.values(),key=lambda x:x['created_at'],reverse=True)[:100]}

    def region(self, name):
        if not isinstance(name, str) or name not in self.regions:
            raise Failure('unknown_region')
        return self.regions[name]

    def scan(self, data):
        object_fields(data, ('region','host','port'), ('region','host'))
        region = self.region(data['region'])
        host, port = literal_ip(data['host']), bounded_int(data.get('port',22),1,65535)
        if host in region['protected_hosts'] or host == urllib.parse.urlsplit(region['server']).hostname:
            raise Failure('protected_control_host')
        self.prune()
        with self.lock:
            if len(self.scans) >= 20: raise Failure('too_many_pending_requests',429)
        try:
            result = subprocess.run(['/usr/bin/ssh-keyscan','-T','5','-p',str(port),'-t','ed25519',host],capture_output=True,timeout=7)
        except (OSError,subprocess.TimeoutExpired):
            raise Failure('ssh_scan_failed',502) from None
        candidates=[]
        for line in result.stdout.decode('ascii',errors='ignore').splitlines():
            parts=line.split()
            if len(parts)!=3 or parts[1]!='ssh-ed25519':continue
            try: raw=base64.b64decode(parts[2],validate=True)
            except ValueError:continue
            if not 32 <= len(raw) <= 256:continue
            expected=host if port==22 else '['+host+']:'+str(port)
            if parts[0]!=expected:continue
            fingerprint='SHA256:'+base64.b64encode(hashlib.sha256(raw).digest()).decode().rstrip('=')
            candidates.append((line,fingerprint))
        if not candidates or len(set(x[1] for x in candidates))!=1:
            raise Failure('ssh_host_identity_unavailable',502)
        scan_id=secrets.token_hex(16)
        scan={'scan_id':scan_id,'host':host,'port':port,'region':region['name'],
              'expires_at':int(time.time())+TTL,'known_host':candidates[0][0],
              'fingerprints':[{'algorithm':'ssh-ed25519','fingerprint':candidates[0][1]}]}
        with self.lock:self.scans[scan_id]=scan
        return {k:v for k,v in scan.items() if k!='known_host'}

    def ssh(self, session, script, payload, timeout):
        # Only a one-call 0600 file on /run (tmpfs in the service deployment)
        # carries the password to SSH_ASKPASS. It is removed even on exceptions.
        import tempfile
        with tempfile.TemporaryDirectory(prefix='ssh-',dir=self.runtime) as directory:
            d=Path(directory)
            for name,value in (('password',session['password']),('known_hosts',session['known_host']+'\n')):
                p=d/name;p.write_text(value);p.chmod(0o600)
            ask=d/'askpass'
            ask.write_text('#!/bin/sh\nexec /bin/cat "$FLEET_SSH_PASSWORD_FILE"\n')
            ask.chmod(0o700)
            env=dict(os.environ,SSH_ASKPASS=str(ask),SSH_ASKPASS_REQUIRE='force',DISPLAY='fleet-enrollment',FLEET_SSH_PASSWORD_FILE=str(d/'password'))
            encoded=base64.b64encode(script.encode()).decode()
            # Base64 contains no shell metacharacters; neither host nor password
            # nor any user text is interpolated into the remote command.
            command="python3 -c 'import base64;exec(base64.b64decode(\""+encoded+"\"))'"
            args=['/usr/bin/ssh','-F','/dev/null','-T','-o','ConnectTimeout=5','-o','ConnectionAttempts=1',
                  '-o','StrictHostKeyChecking=yes','-o','UserKnownHostsFile='+str(d/'known_hosts'),
                  '-o','GlobalKnownHostsFile=/dev/null','-o','HostKeyAlgorithms=ssh-ed25519',
                  '-o','PubkeyAuthentication=no','-o','PreferredAuthentications=password','-o','NumberOfPasswordPrompts=1',
                  '-o','ServerAliveInterval=10','-o','ServerAliveCountMax=2','-p',str(session['port']),
                  'root@'+session['host'],command]
            try:
                result=subprocess.run(args,input=json.dumps(payload).encode(),capture_output=True,env=env,timeout=timeout)
            except (OSError,subprocess.TimeoutExpired):
                raise Failure('ssh_operation_timeout_check_node',502) from None
            if result.returncode or len(result.stdout)>65536:
                raise Failure('ssh_operation_failed',502)
            try:return json.loads(result.stdout)
            except ValueError:raise Failure('ssh_invalid_response',502) from None

    def node(self, region, name):
        # Auth files rotate independently; load them for every query.
        owner=pwd.getpwnam(region.get('api_token_owner','root')).pw_uid
        token=private_read(region['api_token_file'],owner=owner).decode().strip()
        try:
            ca=Path(region['api_ca_file']).read_text()
            context=ssl.create_default_context(cadata=ca)
            opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPSHandler(context=context),NoRedirect())
            request=urllib.request.Request(region['server'].rstrip('/')+'/api/v1/nodes/'+name,headers={'Authorization':'Bearer '+token})
            with opener.open(request,timeout=4) as response:
                raw=response.read(1<<20)
                return json.loads(raw)
        except urllib.error.HTTPError as error:
            if error.code==404:return None
            raise Failure('cluster_read_access_unavailable',502) from None
        except (OSError,ValueError):
            raise Failure('cluster_unreachable',502) from None

    def preflight(self, data):
        object_fields(data,('scan_id','username','password','fingerprint'),('scan_id','username','password','fingerprint'))
        if data['username']!='root':raise Failure('root_user_required')
        password=data['password']
        if not isinstance(password,str) or not 1<=len(password.encode())<=1024 or any(c in password for c in '\r\n\x00'):
            raise Failure('invalid_ssh_password')
        with self.lock:
            scan=self.scans.get(data['scan_id']) if isinstance(data['scan_id'],str) else None
            if not scan or scan['expires_at']<time.time():raise Failure('scan_expired',409)
            if data['fingerprint']!=scan['fingerprints'][0]['fingerprint']:raise Failure('host_fingerprint_mismatch',409)
            # Consume once: failed authentication must start a fresh scan.
            scan=self.scans.pop(data['scan_id'])
        session=dict(scan,password=password)
        region=self.region(scan['region'])
        info=self.ssh(session,PROBE,{'api_host':urllib.parse.urlsplit(region['server']).hostname},12)
        addresses=[a for a in info.get('addresses',[]) if isinstance(a,dict) and
                   any(ipaddress.IPv4Address(a.get('ip','0.0.0.0')) in ipaddress.IPv4Network(c) for c in region['private_cidrs']) and
                   re.fullmatch(r'[a-zA-Z0-9_.:-]{1,32}',a.get('interface',''))]
        node_name='game-'+scan['host'].replace('.','-')
        existing=self.node(region,node_name)
        checks=[]
        def check(name,ok,detail):checks.append({'name':name,'ok':bool(ok),'detail':detail})
        check('platform',info.get('system')=='Linux' and info.get('arch') in ('x86_64','amd64') and info.get('os') in ('debian','ubuntu'),'Debian/Ubuntu Linux amd64')
        check('root_and_systemd',info.get('uid')==0 and info.get('systemd'),'root + systemd')
        check('python',info.get('python_version',[0,0])>=[3,9],'Python 3.9+')
        check('resources',info.get('cpu_cores',0)>=2 and info.get('memory_mib',0)>=1700 and info.get('disk_free_gib',0)>=10,'至少 2 vCPU、约 2 GiB RAM、10 GiB 可用磁盘')
        check('swap_disabled',info.get('swap') is False,'需要先停用 swap')
        check('private_network',len(addresses)==1,'必须唯一匹配所选区域的私网地址')
        check('api_reachable',info.get('api_reachable'),'TCP 6443 到管理节点')
        check('fresh_node',not info.get('existing_installation',True) and existing is None,'保留已有 K3s/Kubernetes 安装，拒绝覆盖')
        external=ipaddress.IPv4Address(scan['host'])
        check('public_game_address',external.is_global,'SSH 目标须为该 VPS 的公网游戏 IPv4')
        preflight_id=secrets.token_hex(16)
        view={'preflight_id':preflight_id,'expires_at':int(time.time())+TTL,'host':scan['host'],'region':scan['region'],
              'node_name':node_name,'private_ip':addresses[0]['ip'] if len(addresses)==1 else '',
              'external_ip':scan['host'],'interface':addresses[0]['interface'] if len(addresses)==1 else '',
              'cpu_cores':info.get('cpu_cores'),'memory_mib':info.get('memory_mib'),'disk_free_gib':info.get('disk_free_gib'),
              'existing_installation':info.get('existing_installation',True),'checks':checks,'can_join':all(c['ok'] for c in checks),
              'plan':['安装系统依赖及固定版本 K3s agent；保留已有 Docker。',
                      '使用仅限 worker 的入群凭证和只读镜像仓库凭证。',
                      '预留 300m CPU / 512 MiB 给系统与 K3s，加入游戏节点标签。',
                      '检查 Kubernetes Ready 与私网/公网地址；不会重启 Nakama。',
                      '云防火墙需预先允许集群 UDP 8472、TCP 10250/6443 和游戏 UDP 20000–20999。',
                      'Ready 只证明节点接入；跨节点 Pod 网络与公网游戏 UDP 还需实际对局验收。']}
        if view['can_join']:
            with self.lock:
                if len(self.preflights)>=20:raise Failure('too_many_pending_requests',429)
                self.preflights[preflight_id]={'expires_at':view['expires_at'],'session':session,'view':view}
        return view

    def save_job(self,job,**updates):
        with self.lock:
            job.update(updates,updated_at=int(time.time()))
            atomic_json(self.state/('job-'+job['id']+'.json'),job)

    def join(self,data):
        object_fields(data,('preflight_id',),('preflight_id',))
        with self.lock:
            item=self.preflights.get(data['preflight_id']) if isinstance(data['preflight_id'],str) else None
            if item and item.get('job_id'):return dict(self.jobs[item['job_id']])
            if not item or item['expires_at']<time.time():raise Failure('preflight_expired',409)
            if any(j['state'] not in FINAL for j in self.jobs.values()):raise Failure('another_node_join_in_progress',409)
            view=item['view'];job_id=secrets.token_hex(16)
            job={'id':job_id,'host':view['host'],'region':view['region'],'node_name':view['node_name'],
                 'state':'queued','stage':'queued','created_at':int(time.time()),'updated_at':int(time.time())}
            self.jobs[job_id]=job;item['job_id']=job_id
            self.save_job(job)
            threading.Thread(target=self.install,args=(data['preflight_id'],item,job),daemon=True).start()
            return dict(job)

    def install(self,preflight_id,item,job):
        try:
            region=self.region(job['region']);view=item['view']
            if self.node(region,view['node_name']) is not None:raise Failure('node_name_already_registered',409)
            # Recheck immediately before installation to catch changes after the plan.
            info=self.ssh(item['session'],PROBE,{'api_host':urllib.parse.urlsplit(region['server']).hostname},12)
            address_ok=any(a.get('ip')==view['private_ip'] and a.get('interface')==view['interface'] for a in info.get('addresses',[]))
            if info.get('existing_installation') or not info.get('api_reachable') or info.get('swap') is not False or not address_ok:
                raise Failure('host_changed_repeat_preflight',409)
            token=private_read(region['join_token_file'])
            if not re.fullmatch(rb'K10[a-f0-9]{64}::node:[A-Za-z0-9._~-]{16,512}\s*',token):
                raise Failure('agent_only_join_token_required',503)
            script=Path(self.config['node_script'])
            if script.is_symlink() or not script.is_file() or script.stat().st_mode & 0o022 or script.stat().st_uid!=os.geteuid():
                raise Failure('unsafe_node_installer',503)
            files={'k3s_node.py':base64.b64encode(script.read_bytes()).decode(),'join.token':base64.b64encode(token).decode()}
            if region.get('registry_config_file'):
                files['registry.json']=base64.b64encode(private_read(region['registry_config_file'])).decode()
                files['registry-ca.crt']=base64.b64encode(private_read(region['registry_ca_file'])).decode()
            payload={k:view[k] for k in ('node_name','private_ip','external_ip','interface')}
            payload.update(cluster_id=region['cluster_id'],server=region['server'],files=files)
            self.save_job(job,state='installing',stage='installing_k3s_agent')
            result=self.ssh(item['session'],INSTALL,payload,660)
            item.pop('session',None) # Password no longer required.
            if result.get('installed') is not True:raise Failure('installation_failed_check_host',502)
            self.save_job(job,state='verifying',stage='waiting_for_node_ready')
            for _ in range(36):
                node=self.node(region,view['node_name'])
                if node:
                    ready=any(c.get('type')=='Ready' and c.get('status')=='True' for c in node.get('status',{}).get('conditions',[]))
                    labels=node.get('metadata',{}).get('labels',{})
                    addresses=node.get('status',{}).get('addresses',[])
                    correct=labels.get('nakama-agones.io/game-node')=='true' and labels.get('nakama-agones.io/cluster')==region['cluster_id']
                    correct=correct and any(a.get('type')=='InternalIP' and a.get('address')==view['private_ip'] for a in addresses)
                    correct=correct and any(a.get('type')=='ExternalIP' and a.get('address')==view['external_ip'] for a in addresses)
                    if ready and correct:
                        self.save_job(job,state='ready',stage='node_ready_game_udp_verification_required',checks=[{'name':'kubernetes_ready','ok':True,'detail':'节点身份、标签、地址及 Ready 已核验；公网游戏 UDP 待实测'}])
                        return
                time.sleep(5)
            raise Failure('node_ready_timeout_check_cluster',504)
        except Failure as error:
            self.save_job(job,state='failed',stage='manual_verification_required',error=error.code)
        except Exception:
            self.save_job(job,state='failed',stage='manual_verification_required',error='node_join_failed_check_host')
        finally:
            with self.lock:
                item.pop('session',None)
                # Retain only the idempotent lookup for repeated confirmation.
                item['expires_at']=int(time.time())+TTL

    def handle(self,method,path,data):
        if method=='GET' and path=='/v1/capabilities':return self.capabilities()
        if method=='GET' and path.startswith('/v1/jobs?id='):
            job_id=path.removeprefix('/v1/jobs?id=')
            if not ID.fullmatch(job_id):raise Failure('invalid_job_id')
            with self.lock:
                if job_id not in self.jobs:raise Failure('job_not_found',404)
                return dict(self.jobs[job_id])
        action={'/v1/scan':self.scan,'/v1/preflight':self.preflight,'/v1/join':self.join}.get(path)
        if method!='POST' or action is None:raise Failure('not_found',404)
        if not self.slots.acquire(blocking=False):raise Failure('onboarding_busy',429)
        try:return action(data)
        finally:self.slots.release()


if __name__ == '__main__':
    raise SystemExit('node_onboarding is an internal installer module; use the Kubernetes NodeEnrollment controller')
