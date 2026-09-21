#!/usr/bin/env python3
"""Manage only the isolated agones-nakama local cluster. Never ambient kubeconfig."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import time
import urllib.request

ROOT=Path(__file__).resolve().parents[1]
LOCAL=ROOT/'.local'
CLUSTER='agones-nakama'
CONTEXT='k3d-'+CLUSTER
CONTROL='agones-control'
RUNTIME='nakama-agones-local:dev'
TOOLS='nakama-agones-tools:dev'
FIXTURE='nakama-agones-fixture:dev'
os.umask(0o077)

def run(*args, data=None, capture=False):
    return subprocess.run(list(map(str,args)),cwd=ROOT,input=data,text=True,check=True,
                          stdout=subprocess.PIPE if capture else None).stdout

def tool(name):
    override=os.environ.get(name.upper())
    candidate=Path(override) if override else LOCAL/'tools'/name
    if not candidate.is_file():
        raise SystemExit('Run ./scripts/bootstrap-tools.sh first (or set '+name.upper()+').')
    return str(candidate)

def kube(*args, **kwargs):
    return run('kubectl','--kubeconfig',LOCAL/'kubeconfig','--context',CONTEXT,*args,**kwargs)

def obj(kind,name,spec=None,**fields):
    value={'apiVersion':'v1','kind':kind,'metadata':{'name':name,'namespace':CONTROL},**fields}
    if spec is not None:value['spec']=spec
    return value

def deployment(name,containers,**spec):
    value=obj('Deployment',name,{'replicas':1,'selector':{'matchLabels':{'app':name}},'strategy':{'type':'Recreate'},
        'template':{'metadata':{'labels':{'app':name}},'spec':{'containers':containers,**spec}}})
    value['apiVersion']='apps/v1'
    return value

def manifest():
    client_file=LOCAL/'client.json'
    if client_file.exists():
        client=json.loads(client_file.read_text())
    else:
        client={'base_url':'http://127.0.0.1:17850','server_key':'local-server-key',
                'admin_token':secrets.token_urlsafe(32),'signing_key':secrets.token_urlsafe(32)}
        client_file.write_text(json.dumps(client));client_file.chmod(0o600)
    env={'AGONES_FLEET_MODE':'local','AGONES_FLEET_ALLOW_HTTP':'true','AGONES_FLEET_DEPLOYMENT_ID':'local-agones-v1',
        'AGONES_FLEET_DATABASE_URL':'postgres://postgres:local-postgres-password@postgres:5432/fleet?sslmode=disable',
        'AGONES_FLEET_CONTROL_URL':'http://nakama.agones-control.svc.cluster.local:7350',
        'AGONES_FLEET_BUILD_HASH':'local-build','AGONES_FLEET_REGION':'local','AGONES_FLEET_MAX_ROOMS':'2',
        'AGONES_FLEET_MIN_INSTANCES':'0','AGONES_FLEET_MAX_INSTANCES':'3','AGONES_FLEET_IDLE_SECONDS':'300',
        'AGONES_FLEET_LAUNCH_TIMEOUT':'180','AGONES_FLEET_ALLOCATION_TIMEOUT':'240','AGONES_FLEET_PROVIDER_POLL_SECONDS':'2',
        'AGONES_FLEET_ADMIN_TOKEN':client['admin_token'],'AGONES_FLEET_SIGNING_KEY':client['signing_key'],
        'AGONES_NAMESPACE':'agones-games','AGONES_POOL':'local','AGONES_GAME_IMAGE':'docker.io/library/'+FIXTURE,
        'AGONES_NODE_SELECTOR_JSON':'{"nakama-agones.io/game-node":"true"}',
        'AGONES_GAME_PORT':'7770','AGONES_CPU_REQUEST':'100m','AGONES_MEMORY_REQUEST':'64Mi','AGONES_MEMORY_LIMIT':'256Mi'}
    dbvol=[{'name':'data','persistentVolumeClaim':{'claimName':'postgres'}},
           {'name':'init','configMap':{'name':'postgres-init'}}]
    probe={'exec':{'command':['pg_isready','-U','postgres']},'periodSeconds':2,'failureThreshold':90}
    objects=[obj('Secret','fleet-config',type='Opaque',stringData=env),
        obj('ConfigMap','nakama-config',data={'local.yml':(ROOT/'deploy/nakama.local.yml').read_text()}),
        obj('ConfigMap','postgres-init',data={'init.sql':(ROOT/'deploy/postgres/001-databases.sql').read_text()}),
        obj('PersistentVolumeClaim','postgres',{'accessModes':['ReadWriteOnce'],'resources':{'requests':{'storage':'1Gi'}}}),
        deployment('postgres',[{'name':'postgres','image':'postgres:16-alpine','env':[{'name':'POSTGRES_PASSWORD','value':'local-postgres-password'}],
            'volumeMounts':[{'name':'data','mountPath':'/var/lib/postgresql/data'},{'name':'init','mountPath':'/docker-entrypoint-initdb.d'}],
            'readinessProbe':probe,'resources':{'requests':{'cpu':'100m','memory':'128Mi'},'limits':{'memory':'512Mi'}}}],volumes=dbvol),
        obj('Service','postgres',{'selector':{'app':'postgres'},'ports':[{'port':5432}]}),
        deployment('nakama',[{'name':'nakama','image':RUNTIME,'imagePullPolicy':'IfNotPresent',
            'args':['--config','/nakama/data/local.yml','--database.address','postgres:local-postgres-password@postgres:5432/nakama?sslmode=disable'],
            'envFrom':[{'secretRef':{'name':'fleet-config'}}],
            'volumeMounts':[{'name':'config','mountPath':'/nakama/data/local.yml','subPath':'local.yml','readOnly':True}],
            'readinessProbe':{'exec':{'command':['/nakama/nakama','healthcheck']},'periodSeconds':3,'failureThreshold':60},
            'resources':{'requests':{'cpu':'200m','memory':'256Mi'},'limits':{'memory':'768Mi'}}}],
            serviceAccountName='nakama-agones',terminationGracePeriodSeconds=20,
            volumes=[{'name':'config','configMap':{'name':'nakama-config'}}],
            initContainers=[{'name':'nakama-migrate','image':RUNTIME,'imagePullPolicy':'IfNotPresent',
                'args':['migrate','up','--database.address','postgres:local-postgres-password@postgres:5432/nakama?sslmode=disable']},
                {'name':'fleet-migrate','image':TOOLS,'imagePullPolicy':'IfNotPresent','envFrom':[{'secretRef':{'name':'fleet-config'}}]}]),
        obj('Service','nakama',{'selector':{'app':'nakama'},'ports':[{'name':'api','port':7350},{'name':'console','port':7351}]})]
    image_ids={image:run('docker','image','inspect','--format','{{.Id}}',image,capture=True).strip() for image in (RUNTIME,TOOLS,FIXTURE)}
    (LOCAL/'images.json').write_text(json.dumps(image_ids))
    for item in objects:
        if item['kind']=='Deployment' and item['metadata']['name']=='nakama':
            item['spec']['template']['metadata']['annotations']={'nakama-agones.io/local-image-set':hashlib.sha256(json.dumps(image_ids,sort_keys=True).encode()).hexdigest()}
    return {'apiVersion':'v1','kind':'List','items':objects}

def forward():
    # Do not kill arbitrary processes based on an unverified stale PID file.
    pidfile=LOCAL/'forward.pid'
    if pidfile.exists():
        try:
            pid=int(pidfile.read_text());cmd=run('ps','-p',pid,'-o','command=',capture=True)
            if 'kubectl' in cmd and str(LOCAL/'kubeconfig') in cmd and 'port-forward' in cmd:
                os.kill(pid,signal.SIGTERM)
        except (ValueError,ProcessLookupError,subprocess.CalledProcessError):pass
    with (LOCAL/'port-forward.log').open('a') as log:
        proc=subprocess.Popen(['kubectl','--kubeconfig',str(LOCAL/'kubeconfig'),'--context',CONTEXT,'-n',CONTROL,
            'port-forward','--address=127.0.0.1','service/nakama','17850:7350','17851:7351'],stdout=log,stderr=log,start_new_session=True)
    pidfile.write_text(str(proc.pid))
    for _ in range(60):
        if proc.poll() is not None:raise SystemExit('Port forwarding failed; see .local/port-forward.log')
        try:
            with urllib.request.urlopen('http://127.0.0.1:17850/healthcheck',timeout=1) as response:
                if response.status==200:return
        except OSError:time.sleep(1)
    raise SystemExit('Local Nakama health timeout')

def up(skip_build=False):
    LOCAL.mkdir(exist_ok=True)
    k3d=tool('k3d');helm=tool('helm')
    clusters=json.loads(run(k3d,'cluster','list','-o','json',capture=True))
    if not any(c['name']==CLUSTER for c in clusters):
        run(k3d,'cluster','create',CLUSTER,'--image','rancher/k3s:v1.35.8-k3s1','--servers','1','--agents','1',
            '--api-port','127.0.0.1:17443','--port','127.0.0.1:17770-17789:17770-17789/udp@agent:0',
            '--k3s-arg','--disable=traefik,servicelb@server:0','--k3s-arg','--node-external-ip=127.0.0.1@agent:0',
            '--k3s-node-label','nakama-agones.io/game-node=true@agent:0',
            '--kubeconfig-update-default=false','--kubeconfig-switch-context=false','--timeout','240s')
    else:run(k3d,'cluster','start',CLUSTER)
    (LOCAL/'kubeconfig').write_text(run(k3d,'kubeconfig','get',CLUSTER,capture=True));(LOCAL/'kubeconfig').chmod(0o600)
    kube('apply','-f','deploy/kubernetes/rbac.yaml')
    run(helm,'upgrade','--install','agones','https://agones.dev/chart/stable/agones-1.60.0.tgz',
        '--kubeconfig',LOCAL/'kubeconfig','--kube-context',CONTEXT,'--namespace','agones-system','--create-namespace',
        '--values','deploy/agones.local.values.yaml','--wait','--timeout','300s')
    if not skip_build:
        for target,name in [('runtime',RUNTIME),('tools',TOOLS)]:
            run('docker','build','--platform','linux/amd64','-f','deploy/Dockerfile','--target',target,'-t',name,'.')
        run('docker','build','-t',FIXTURE,'examples/room-worker')
    run('docker','pull','postgres:16-alpine')
    # Docker's default multi-platform export may reference absent manifests, and
    # k3d can report success despite ctr import errors. Export each concrete
    # platform explicitly and require every node import to return zero.
    for image in (RUNTIME,TOOLS,FIXTURE,'postgres:16-alpine'):
        platform=run('docker','image','inspect','--format','{{.Os}}/{{.Architecture}}',image,capture=True).strip()
        archive=LOCAL/'image-import.tar'
        run('docker','image','save','--platform',platform,'-o',archive,image)
        try:
            for node in ('k3d-'+CLUSTER+'-server-0','k3d-'+CLUSTER+'-agent-0'):
                with archive.open('rb') as source:
                    subprocess.run(['docker','exec','-i',node,'ctr','-n','k8s.io','images','import',
                        '--platform',platform,'-'],stdin=source,check=True)
        finally:archive.unlink(missing_ok=True)
    # Apply local-only credentials on stdin, never print manifests or command arguments with secrets.
    kube('apply','-f','-',data=json.dumps(manifest()))
    kube('-n',CONTROL,'rollout','status','deployment/postgres','--timeout=180s')
    kube('-n',CONTROL,'rollout','status','deployment/nakama','--timeout=240s')
    forward()
    print('Local Agones stack ready: http://127.0.0.1:17850 (fixture, not DM gameplay)')

def stop(delete=False):
    pidfile=LOCAL/'forward.pid'
    if pidfile.exists():
        try:
            pid=int(pidfile.read_text());cmd=run('ps','-p',pid,'-o','command=',capture=True)
            if 'kubectl' in cmd and str(LOCAL/'kubeconfig') in cmd and 'port-forward' in cmd:os.kill(pid,signal.SIGTERM)
        except (ValueError,ProcessLookupError,subprocess.CalledProcessError):pass
        pidfile.unlink()
    run(tool('k3d'),'cluster','delete' if delete else 'stop',CLUSTER)

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action',choices=['up','forward','stop','reset'])
    parser.add_argument('--skip-build',action='store_true',help='Reuse already built local images')
    args=parser.parse_args()
    if args.action=='up':up(args.skip_build)
    elif args.action=='forward':forward()
    else:stop(args.action=='reset')
