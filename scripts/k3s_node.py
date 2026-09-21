#!/usr/bin/env python3
"""Install one explicitly identified K3s node. No cloud API or ambient cluster."""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import ssl
import stat
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request

VERSION = 'v1.35.8+k3s1'
CONFIG = Path('/etc/rancher/k3s/config.yaml')
MARKER = Path('/etc/nakama-agones/node-install.json')
TOKEN = Path('/etc/rancher/k3s/nakama-agones-agent.token')
BIN = Path('/usr/local/bin/k3s')
AGENT_SECRET = Path('/etc/rancher/k3s/nakama-agones-agent-secret')
LOGDIR = Path('/var/log/nakama-agones')
LOGROTATE = Path('/etc/logrotate.d/nakama-agones-k3s')
REGISTRY_CONFIG = Path('/etc/rancher/k3s/registries.yaml')
REGISTRY_CA = Path('/etc/rancher/k3s/private-registry-ca.crt')
CACHE = Path('/var/cache/nakama-agones')
PRIVATE_NETWORKS = tuple(ipaddress.ip_network(s) for s in ('10.0.0.0/8','172.16.0.0/12','192.168.0.0/16'))
LABEL = re.compile(r'^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$')

class SetupError(Exception):
    pass

def private_ip(value):
    try:
        ip = ipaddress.ip_address(value)
    except ValueError:
        raise argparse.ArgumentTypeError('A private IPv4 address is required.')
    if not any(ip in n for n in PRIVATE_NETWORKS):
        raise argparse.ArgumentTypeError('Use an RFC1918 private/VPN IPv4 address.')
    return str(ip)

def public_ip(value):
    try:
        ip = ipaddress.ip_address(value)
    except ValueError:
        raise argparse.ArgumentTypeError('A public IPv4 address is required.')
    if ip.version != 4 or not ip.is_global:
        raise argparse.ArgumentTypeError('Game node external IP must be globally routable IPv4.')
    return str(ip)

def absolute_path(value):
    path=Path(value)
    if not path.is_absolute() or '..' in path.parts:
        raise argparse.ArgumentTypeError('Use an absolute path without parent traversal.')
    return path

def label(value):
    if not LABEL.fullmatch(value):
        raise argparse.ArgumentTypeError('Use a lowercase DNS label, at most 63 characters.')
    return value

def server_url(value):
    try:
        parsed = urllib.parse.urlsplit(value)
        address = private_ip(parsed.hostname or '')
        if parsed.scheme != 'https' or parsed.port != 6443 or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ('','/'):
            raise ValueError()
    except (ValueError, argparse.ArgumentTypeError):
        raise argparse.ArgumentTypeError('Server must be https://PRIVATE_IPV4:6443.')
    return 'https://' + address + ':6443'

def run(args, *, capture=False, env=None, timeout=300):
    # Child output is never included in exceptions; upstream tools may echo tokens.
    result = subprocess.run(list(map(str,args)), stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, env=env, timeout=timeout)
    if result.returncode:
        raise SetupError('Command failed: ' + Path(str(args[0])).name + ' (exit ' + str(result.returncode) + '). Inspect its local service logs.')
    return result.stdout if capture else None

def secure_read(path, *, expected_uid=0):
    path=Path(path)
    with os.fdopen(os.open(path,os.O_RDONLY|os.O_NOFOLLOW),'rb') as stream:
        info=os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600 or info.st_uid!=expected_uid:
            raise SetupError('Credential input must be a regular owner-only 0600 file owned by root.')
        if info.st_size<1 or info.st_size>65536:
            raise SetupError('Credential input has an invalid size.')
        return stream.read(65537).strip()

def join_token(path):
    data=secure_read(path)
    # An agent-only secure token carries the pinned cluster CA. Do not accept the
    # server-admin token, a short password, shell input, or a token in argv.
    if not re.fullmatch(rb'K10[a-f0-9]{64}::node:[A-Za-z0-9._~-]{16,512}',data):
        raise SetupError('Use the secure agent-only token exported by this manager, not its server token.')
    return data

def registry_inputs(args):
    if not args.registry_config_file:return None
    raw=secure_read(args.registry_config_file)
    ca=secure_read(args.registry_ca_file)
    try:
        value=json.loads(raw)
        ssl.create_default_context(cadata=ca.decode('ascii'))
    except (ValueError,ssl.SSLError):
        raise SetupError('Registry input must be valid JSON and a valid PEM CA certificate bundle.')
    if not isinstance(value,dict) or set(value)-{'configs','mirrors'} or not isinstance(value.get('configs'),dict) or not value['configs']:
        raise SetupError('Registry JSON must contain explicit configs and optional mirrors.')
    for host,config in value['configs'].items():
        parsed=urllib.parse.urlsplit('https://'+host)
        if not parsed.hostname or parsed.username or parsed.password or parsed.path or parsed.query or parsed.fragment or host=='*':
            raise SetupError('Registry names must be explicit host[:port] values.')
        if not isinstance(config,dict) or set(config)-{'auth','tls'}:
            raise SetupError('Registry configs may contain only auth and tls.')
        if config.get('tls')!={'ca_file':str(REGISTRY_CA)}:
            raise SetupError('Registry TLS must use the managed CA destination; insecure TLS and client key paths are unsupported.')
        auth=config.get('auth',{})
        if not isinstance(auth,dict) or set(auth)-{'username','password'} or not all(isinstance(v,str) and '\n' not in v and '\r' not in v for v in auth.values()):
            raise SetupError('Registry auth must contain only single-line username/password strings.')
        if auth and (not auth.get('username') or not auth.get('password')):
            raise SetupError('Registry username and password must be supplied together.')
    mirrors=value.get('mirrors',{})
    if not isinstance(mirrors,dict):raise SetupError('Registry mirrors must be an object.')
    for host,mirror in mirrors.items():
        if host not in value['configs'] or not isinstance(mirror,dict) or set(mirror)!={'endpoint'}:
            raise SetupError('Registry mirrors must name a configured registry and contain only endpoint.')
        if mirror['endpoint']!=['https://'+host]:
            raise SetupError('Registry mirror must use the same explicit HTTPS registry endpoint.')
    return encode(value),ca+b'\n'

def atomic_write(path,data,mode=0o600):
    path=Path(path)
    path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    if path.is_symlink():
        raise SetupError('Refusing a symlink at a managed file path.')
    handle,temp=tempfile.mkstemp(prefix='.'+path.name+'.',dir=path.parent)
    try:
        os.fchmod(handle,mode)
        with os.fdopen(handle,'wb') as f:
            f.write(data);f.flush();os.fsync(f.fileno())
        os.replace(temp,path)
    finally:
        if os.path.exists(temp):os.unlink(temp)

def encode(value):
    return (json.dumps(value,sort_keys=True,indent=2)+'\n').encode()

def configuration(args):
    manager=args.role=='manager'
    config={
        'node-name':args.node_name, 'node-ip':args.private_ip,
        'node-label':[
            'nakama-agones.io/cluster='+args.cluster_id,
            'nakama-agones.io/role='+('control' if manager else 'game'),
            'nakama-agones.io/game-node='+('false' if manager else 'true'),
            'agones.dev/agones-system='+('true' if manager else 'false'),
        ],
        'kubelet-arg':[
            'system-reserved=cpu=150m,memory='+('384Mi' if manager else '256Mi')+',ephemeral-storage=1Gi',
            'kube-reserved=cpu='+('350m,memory=768Mi' if manager else '150m,memory=256Mi')+',ephemeral-storage=1Gi',
            'enforce-node-allocatable=pods',
            'eviction-hard=memory.available<100Mi,nodefs.available<10%,imagefs.available<15%,nodefs.inodesFree<5%,imagefs.inodesFree<5%',
            'container-log-max-size=10Mi','container-log-max-files=3',
            'image-gc-high-threshold=80','image-gc-low-threshold=70',
            'read-only-port=0','anonymous-auth=false',
        ],
        'log':str(LOGDIR/'k3s.log'),
        'flannel-iface':args.interface,
        'prefer-bundled-bin':True,
    }
    if args.external_ip:config['node-external-ip']=args.external_ip
    if args.registry_config_file:config['private-registry']=str(REGISTRY_CONFIG)
    if manager:
        config.update({
            'bind-address':args.private_ip,'advertise-address':args.private_ip,
            'tls-san':[args.private_ip], 'write-kubeconfig-mode':'0600',
            'node-taint':['CriticalAddonsOnly=true:NoExecute'],
            'disable':['traefik','servicelb'],
            'secrets-encryption':True,'secrets-encryption-provider':'secretbox',
            'agent-token-file':str(AGENT_SECRET),
            'flannel-backend':'vxlan',
        })
    else:
        config.update({'server':args.server,'token-file':str(TOKEN)})
    return config

def identity(args, config):
    return {'schema':1,'cluster_id':args.cluster_id,'role':args.role,'version':VERSION,
            'config_sha256':hashlib.sha256(encode(config)).hexdigest()}

def nonempty(path):
    path=Path(path)
    return path.exists() and (not path.is_dir() or any(path.iterdir()))

def check_ownership(wanted,config):
    if MARKER.exists():
        try:record=json.loads(secure_read(MARKER))
        except (OSError,ValueError):raise SetupError('Invalid managed-node ownership record.')
        if any(record.get(k)!=v for k,v in wanted.items()):
            raise SetupError('This node belongs to another cluster/profile or has different immutable settings. No files were changed.')
        if CONFIG.exists() and CONFIG.read_bytes()!=encode(config):
            raise SetupError('Managed K3s config was edited outside this script. Refusing to overwrite it.')
        if nonempty('/etc/rancher/k3s/config.yaml.d'):
            raise SetupError('Additional K3s configuration overrides require operator review.')
        return record
    conflicts=[p for p in (BIN,'/etc/rancher/k3s','/var/lib/rancher/k3s','/etc/rancher/rke2',
        '/etc/kubernetes','/var/lib/kubelet','/etc/systemd/system/k3s.service','/etc/systemd/system/k3s-agent.service',
        LOGROTATE) if nonempty(p)]
    if conflicts or shutil.which('k3s') or shutil.which('rke2') or shutil.which('kubelet'):
        raise SetupError('Existing Kubernetes/K3s installation is not owned by this script. Nothing will be overwritten.')
    return None

def validate_host(args):
    if platform.system()!='Linux' or platform.machine() not in ('x86_64','amd64') or os.geteuid()!=0:
        raise SetupError('Apply requires root on a Linux amd64 host.')
    if not Path('/run/systemd/system').is_dir():raise SetupError('A running systemd host is required.')
    for program in ('systemctl','ip','sh','curl','logrotate'):
        if not shutil.which(program):raise SetupError('Install prerequisite: '+program)
    if not re.fullmatch(r'[A-Za-z0-9_.:-]{1,64}',args.interface):raise SetupError('Invalid private network interface.')
    addresses=json.loads(run(['ip','-j','address','show','dev',args.interface],capture=True))
    assigned={v.get('local') for iface in addresses for v in iface.get('addr_info',[])}
    if args.private_ip not in assigned:raise SetupError('Private node IP is not assigned to the specified interface.')
    if len(Path('/proc/swaps').read_text().splitlines())>1:
        raise SetupError('Active swap is unsupported by this baseline; disable it explicitly before installing.')
    total=int(re.search(r'^MemTotal:\s+(\d+)',Path('/proc/meminfo').read_text(),re.M)[1])
    minimum=3*1024*1024 if args.role=='manager' else 1700*1024
    if total<minimum or (os.cpu_count() or 0)<2:
        raise SetupError('Baseline needs at least 2 CPUs and 3 GiB manager / about 2 GiB game-node RAM.')

class HTTPSOnlyRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,request,fp,code,msg,headers,newurl):
        if urllib.parse.urlsplit(newurl).scheme!='https':raise SetupError('Download redirected outside HTTPS.')
        return super().redirect_request(request,fp,code,msg,headers,newurl)

def download(url,target,max_bytes):
    opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),HTTPSOnlyRedirect())
    try:
        with opener.open(url,timeout=60) as response:
            if urllib.parse.urlsplit(response.url).scheme!='https':raise SetupError('Download redirected outside HTTPS.')
            total=0
            with open(target,'wb') as stream:
                while block:=response.read(1024*1024):
                    total+=len(block)
                    if total>max_bytes:raise SetupError('Release artifact exceeded its size limit.')
                    stream.write(block)
    except (OSError,ValueError):raise SetupError('Could not download the pinned official K3s release.')

def install_binary_and_service(args,record):
    if BIN.exists():
        version=run([BIN,'--version'],capture=True,timeout=15).splitlines()[0]
        if (' '+VERSION+' ') not in (' '+version+' '):raise SetupError('Installed K3s version differs from the pinned baseline. Upgrade separately.')
        if record and record.get('installed'):return
    CACHE.mkdir(parents=True,exist_ok=True,mode=0o700)
    with tempfile.TemporaryDirectory(prefix='k3s-',dir=CACHE) as temp:
        temp=Path(temp);tag=urllib.parse.quote(VERSION,safe='')
        base='https://github.com/k3s-io/k3s/releases/download/'+tag+'/'
        download(base+'sha256sum-amd64.txt',temp/'sha256',1<<20)
        expected=None
        for line in (temp/'sha256').read_text().splitlines():
            parts=line.split()
            if len(parts)==2 and parts[1].lstrip('*')=='k3s' and re.fullmatch('[a-f0-9]{64}',parts[0]):expected=parts[0]
        if not expected:raise SetupError('Pinned release does not contain an amd64 k3s checksum.')
        download(base+'k3s',temp/'k3s',256<<20)
        if hashlib.sha256((temp/'k3s').read_bytes()).hexdigest()!=expected:raise SetupError('K3s release checksum mismatch.')
        download('https://raw.githubusercontent.com/k3s-io/k3s/'+tag+'/install.sh',temp/'install.sh',2<<20)
        atomic_write(BIN,(temp/'k3s').read_bytes(),0o755)
        # Do not forward ambient K3S_* variables, proxy credentials or shell state.
        environment={'PATH':'/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
            'INSTALL_K3S_SKIP_DOWNLOAD':'true','INSTALL_K3S_SKIP_START':'true','INSTALL_K3S_SKIP_ENABLE':'true',
            'INSTALL_K3S_EXEC':('server' if args.role=='manager' else 'agent')+' --config '+str(CONFIG)}
        run(['sh',temp/'install.sh'],env=environment)

def apply(args):
    if bool(args.registry_config_file)!=bool(args.registry_ca_file):
        raise SetupError('Supply registry-config-file and registry-ca-file together.')
    config=configuration(args);wanted=identity(args,config)
    if args.plan:
        print(json.dumps({'version':VERSION,'role':args.role,'configuration':config,
            'private_registry':{'enabled':bool(args.registry_config_file),'config_destination':str(REGISTRY_CONFIG),'ca_destination':str(REGISTRY_CA)}},indent=2));return
    validate_host(args)
    record=check_ownership(wanted,config)
    credential=join_token(args.token_file) if args.role=='worker' else None
    registry=registry_inputs(args)
    if registry:
        for destination,data in zip((REGISTRY_CONFIG,REGISTRY_CA),registry):
            if destination.exists() and secure_read(destination)!=data.strip():
                raise SetupError('Managed registry credentials or CA differ. Rotate them through a planned maintenance procedure.')
            if record and record.get('installed') and not destination.exists():
                raise SetupError('Managed registry credentials or CA are missing. Restore them before retrying.')
    if credential and TOKEN.exists() and secure_read(TOKEN)!=credential:
        raise SetupError('A different join token was supplied to an existing node; rotate it through a planned maintenance procedure.')
    if record and record.get('installed'):
        secret=AGENT_SECRET if args.role=='manager' else TOKEN
        if not secret.exists():raise SetupError('Managed credential is missing. Restore it; do not implicitly replace a live cluster identity.')
        secure_read(secret)
    if not record:atomic_write(MARKER,encode(wanted))
    if not CONFIG.exists():atomic_write(CONFIG,encode(config))
    if args.role=='manager' and not AGENT_SECRET.exists():atomic_write(AGENT_SECRET,secrets.token_urlsafe(48).encode()+b'\n')
    if credential and not TOKEN.exists():atomic_write(TOKEN,credential+b'\n')
    if registry:
        for destination,data in zip((REGISTRY_CONFIG,REGISTRY_CA),registry):
            if not destination.exists():atomic_write(destination,data)
    LOGDIR.mkdir(parents=True,exist_ok=True,mode=0o700)
    atomic_write(LOGROTATE,(str(LOGDIR/'k3s.log')+' {\n size 20M\n rotate 5\n compress\n delaycompress\n missingok\n notifempty\n copytruncate\n su root root\n}\n').encode(),0o644)
    install_binary_and_service(args,record)
    service='k3s' if args.role=='manager' else 'k3s-agent'
    run(['systemctl','enable','--now',service])
    wanted['installed']=True;atomic_write(MARKER,encode(wanted))
    print('Installed/verified '+VERSION+' '+args.role+' node '+args.node_name+'. Existing matching service was not restarted.')
    if args.role=='manager':print('Export only the agent token with export-join-token. Validate node Ready and source-restricted firewall rules before joining workers.')
    else:print('Joining the existing cluster. Check Ready and public UDP externally; Nakama does not need restarting to discover eligible capacity.')

def export_join(args):
    if os.geteuid()!=0:raise SetupError('Token export requires root on the manager.')
    record=json.loads(secure_read(MARKER))
    if record.get('role')!='manager':raise SetupError('Only this managed control node can export an agent token.')
    if not any(args.output.is_relative_to(root) for root in (Path('/root'),Path('/run/nakama-agones'),Path('/etc/nakama-agones'))):
        raise SetupError('Export join credentials only under /root, /run/nakama-agones or /etc/nakama-agones, outside a source checkout.')
    data=join_token('/var/lib/rancher/k3s/server/agent-token')
    if Path(args.output).exists():raise SetupError('Token output already exists; choose a new private output path.')
    atomic_write(args.output,data+b'\n')
    print('Agent-only secure join token written to the requested 0600 file. Its value was not printed.')

def parser():
    cli=argparse.ArgumentParser(description=__doc__)
    commands=cli.add_subparsers(dest='role',required=True)
    for role in ('manager','worker'):
        p=commands.add_parser(role)
        p.add_argument('--cluster-id',type=label,required=True)
        p.add_argument('--node-name',type=label,required=True)
        p.add_argument('--private-ip',type=private_ip,required=True)
        p.add_argument('--external-ip',type=public_ip,required=role=='worker')
        p.add_argument('--interface',required=True)
        p.add_argument('--registry-config-file',type=absolute_path,help='Root-owned 0600 JSON registries config; never printed.')
        p.add_argument('--registry-ca-file',type=absolute_path,help='Root-owned 0600 PEM CA for the private registry.')
        p.add_argument('--plan',action='store_true',help='Print only non-secret config; do not install or inspect credentials.')
        if role=='worker':
            p.add_argument('--server',type=server_url,required=True)
            p.add_argument('--token-file',type=absolute_path,required=True)
        p.set_defaults(action=apply)
    p=commands.add_parser('export-join-token')
    p.add_argument('--output',type=absolute_path,required=True)
    p.set_defaults(action=export_join)
    return cli

def main():
    os.umask(0o077)
    try:args=parser().parse_args();args.action(args)
    except (SetupError,OSError,ValueError,subprocess.TimeoutExpired) as error:
        message=str(error) if isinstance(error,SetupError) else 'Node setup failed; inspect local configuration and service logs. No credentials are printed.'
        print(message,file=sys.stderr);return 1
    return 0

if __name__=='__main__':sys.exit(main())
