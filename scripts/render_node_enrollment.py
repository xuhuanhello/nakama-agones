#!/usr/bin/env python3
"""Render fixed regional controller manifests; never read or generate credentials."""
import argparse
import json
import os
from pathlib import Path
import re
import sys
sys.path.insert(0, str(Path(__file__).resolve().parent))
import node_enrollment_controller as c


def render(config_file, image, output, enable_retirement=False):
    cfg = c.load_config(config_file)
    if not re.fullmatch(r'[a-zA-Z0-9./:_-]+@sha256:[a-f0-9]{64}', image):
        raise c.Failure('controller_image_digest_required')
    if cfg['retirement_enabled'] != enable_retirement:
        raise c.Failure('explicit_retirement_permission_required')
    expected = {'requests_namespace':'fleet-enrollment-requests','system_namespace':'fleet-enrollment-system','game_namespace':'agones-games',
                'node_script':'/opt/enrollment/k3s_node.py','runtime_dir':'/run/enrollment/private','bootstrap_dir':'/run/enrollment-bootstrap',
                'api_ca_file':'/var/run/secrets/enrollment-api/ca.crt','api_token_file':'/var/run/secrets/enrollment-api/token'}
    if any(cfg[k] != v for k,v in expected.items()):
        raise c.Failure('standard_manifest_paths_required')
    if cfg['retirement_system_namespaces'] != ['kube-system','agones-system','agones-observability']:
        raise c.Failure('standard_system_namespace_allowlist_required')
    src = Path(__file__).resolve().parents[1] / 'deploy' / 'enrollment'
    files = {p.name:p.read_text().replace("== 'example-us'", "== '"+cfg['region']['cluster_id']+"'") for p in src.glob('*.yaml')}
    if not enable_retirement:
        files.pop('30-retirement-admission.yaml', None)
        files.pop('31-retirement-rbac.yaml', None)
    system=cfg['system_namespace']
    cm={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'node-enrollment-config','namespace':system},'data':{'config.json':json.dumps(cfg,indent=2)}}
    pod={'serviceAccountName':'fleet-node-enrollment','automountServiceAccountToken':False,
         'nodeSelector':{'kubernetes.io/arch':'amd64','nakama-agones.io/role':'control','nakama-agones.io/cluster':cfg['region']['cluster_id']},
         'tolerations':[{'key':'CriticalAddonsOnly','operator':'Equal','value':'true','effect':'NoExecute'},{'key':'node-role.kubernetes.io/control-plane','operator':'Exists','effect':'NoSchedule'}],
         'securityContext':{'runAsNonRoot':True,'runAsUser':10001,'runAsGroup':10001,'fsGroup':10001,'seccompProfile':{'type':'RuntimeDefault'}},
         'terminationGracePeriodSeconds':15,
         'containers':[{'name':'controller','image':image,'imagePullPolicy':'IfNotPresent',
             'securityContext':{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}},
             'env':[{'name':'CONTROLLER_NODE_NAME','valueFrom':{'fieldRef':{'fieldPath':'spec.nodeName'}}}],
             'resources':{'requests':{'cpu':'50m','memory':'128Mi'},'limits':{'cpu':'500m','memory':'256Mi'}},
             'volumeMounts':[{'name':'config','mountPath':'/etc/enrollment','readOnly':True},{'name':'api','mountPath':'/var/run/secrets/enrollment-api','readOnly':True},
                             {'name':'bootstrap','mountPath':'/run/enrollment-bootstrap','readOnly':True},{'name':'runtime','mountPath':'/run/enrollment'}],
             'readinessProbe':{'exec':{'command':['python3','-I','-c',"import pathlib,time; p=pathlib.Path('/run/enrollment/private/healthy'); assert p.exists() and time.time()-int(p.read_text())<180"]},'periodSeconds':10,'initialDelaySeconds':10},
             'livenessProbe':{'exec':{'command':['python3','-I','-c',"import pathlib,time; p=pathlib.Path('/run/enrollment/private/healthy'); assert p.exists() and time.time()-int(p.read_text())<300"]},'periodSeconds':30,'initialDelaySeconds':120}}],
         'volumes':[{'name':'config','configMap':{'name':'node-enrollment-config'}},
                    {'name':'api','projected':{'defaultMode':0o440,'sources':[{'serviceAccountToken':{'path':'token','expirationSeconds':3600}},
                            {'configMap':{'name':'kube-root-ca.crt','items':[{'key':'ca.crt','path':'ca.crt'}]}}]}},
                    {'name':'bootstrap','secret':{'secretName':'enrollment-bootstrap','defaultMode':0o440}},
                    {'name':'runtime','emptyDir':{'medium':'Memory','sizeLimit':'64Mi'}}]}
    deployment={'apiVersion':'apps/v1','kind':'Deployment','metadata':{'name':'node-enrollment-controller','namespace':system},'spec':{
       'replicas':1,'strategy':{'type':'Recreate'},'selector':{'matchLabels':{'app':'node-enrollment-controller'}},
       'template':{'metadata':{'labels':{'app':'node-enrollment-controller'}},'spec':pod}}}
    files['40-controller.yaml']=json.dumps(cm,indent=2)+'\n---\n'+json.dumps(deployment,indent=2)+'\n'
    # API, DNS and target SSH only. Installer package downloads run on the target.
    policy={'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':{'name':'node-enrollment-controller','namespace':system},'spec':{
       'podSelector':{'matchLabels':{'app':'node-enrollment-controller'}},'policyTypes':['Ingress','Egress'],'ingress':[],
       'egress':[{'ports':[{'protocol':'TCP','port':443},{'protocol':'TCP','port':6443}]},
                 {'ports':[{'protocol':'UDP','port':53},{'protocol':'TCP','port':53}]},
                 {'to':[{'ipBlock':{'cidr':'0.0.0.0/0','except':['10.0.0.0/8','127.0.0.0/8','169.254.0.0/16','172.16.0.0/12','192.168.0.0/16','224.0.0.0/4']}}],
                  'ports':[{'protocol':'TCP','port':1,'endPort':65535}]}]}}
    files['50-network-policy.yaml']=json.dumps(policy,indent=2)+'\n'
    target=Path(output)
    if target.exists() and (target.is_symlink() or not target.is_dir() or target.stat().st_uid != os.geteuid() or target.stat().st_mode&0o077):
        raise c.Failure('private_output_directory_required')
    for name, value in files.items():
        p=target/name
        if p.exists() and (p.is_symlink() or p.read_text()!=value):
            raise c.Failure('output_profile_drift')
    target.mkdir(mode=0o700,parents=True,exist_ok=True)
    for name,value in files.items():
        p=target/name
        if not p.exists():
            fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
            with os.fdopen(fd,'w') as f:f.write(value)
    return len(files)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config',required=True);p.add_argument('--image',required=True);p.add_argument('--output',required=True)
    p.add_argument('--enable-retirement',action='store_true')
    a=p.parse_args()
    count=render(a.config,a.image,a.output,a.enable_retirement)
    print('Rendered '+str(count)+' fixed public manifests; no credentials written.')

if __name__=='__main__':
    try:main()
    except c.Failure as e:raise SystemExit(e.code)
