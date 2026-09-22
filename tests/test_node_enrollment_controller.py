import base64
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import node_enrollment_controller as c
import render_node_enrollment as render


class MemoryAPI:
    def __init__(self):
        self.objects = {}; self.calls = []; self.conflict = False; self.before_delete = None
    def get(self, path):return copy.deepcopy(self.objects.get(path))
    def put(self, path, obj):self.objects[path]=copy.deepcopy(obj);return self.get(path)
    def status(self,path,obj,status):
        key=path+'/'+obj['metadata']['name'];current=self.objects[key]
        if self.conflict or current['metadata']['resourceVersion']!=obj['metadata']['resourceVersion']:
            raise c.Failure('kubernetes_api_rejected',409)
        current['status']=copy.deepcopy(status)
        current['metadata']['resourceVersion']=str(int(current['metadata']['resourceVersion'])+1)
        self.calls.append(('status',key,copy.deepcopy(status)))
        return self.get(key)
    def listing(self,path,query=None):
        values=[]
        for key,value in self.objects.items():
            if key.startswith(path+'/') and '/' not in key[len(path)+1:]:
                values.append(copy.deepcopy(value))
            elif path=='/api/v1/pods' and value.get('kind')=='Pod':
                values.append(copy.deepcopy(value))
        if query and 'fieldSelector' in query:
            node=query['fieldSelector'].split('=',1)[1]
            values=[v for v in values if v.get('spec',{}).get('nodeName')==node]
        return values,'1'
    def delete(self,path,uid,version=None):
        self.calls.append(('delete',path,uid,version))
        if self.before_delete:self.before_delete(path)
        obj=self.objects.get(path)
        if obj is None:return None
        if obj['metadata']['uid']!=uid or (version is not None and obj['metadata']['resourceVersion']!=version):
            raise c.Failure('kubernetes_api_rejected',409)
        del self.objects[path]
    def request(self,method,path,value=None,content_type=None):
        self.calls.append((method,path,copy.deepcopy(value)))
        if method=='PUT':
            previous=self.objects[path]
            if previous['metadata']['resourceVersion']!=value['metadata']['resourceVersion']:
                raise c.Failure('kubernetes_api_rejected',409)
            value=copy.deepcopy(value);value['metadata']['resourceVersion']=str(int(value['metadata']['resourceVersion'])+1)
            return self.put(path,value)
        raise AssertionError('unexpected mutation '+method)


class InlineThread:
    def __init__(self,target,args=(),**kwargs):self.target,self.args=target,args
    def start(self):self.target(*self.args)


class ControllerTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.root=Path(self.temp.name)
        self.cfg=json.loads((Path(__file__).resolve().parents[1]/'deploy/enrollment/config.example.json').read_text())
        self.cfg.update(secrets_encryption_verified=True,retirement_enabled=True,runtime_dir=str(self.root/'runtime'),bootstrap_dir=str(self.root/'bootstrap'),node_script=str(self.root/'k3s_node.py'))
        self.cfg['region'].update(name='us-west',cluster_id='test-cluster',server='https://10.4.0.4:6443',private_cidrs=['10.4.0.0/22'],protected_hosts=['8.8.8.8'])
        Path(self.cfg['bootstrap_dir']).mkdir(mode=0o700)
        (Path(self.cfg['bootstrap_dir'])/'agent.token').write_text('K10'+'a'*64+'::node:'+'x'*32)
        (Path(self.cfg['bootstrap_dir'])/'agent.token').chmod(0o600)
        Path(self.cfg['node_script']).write_text('# audited installer fixture');Path(self.cfg['node_script']).chmod(0o500)
        self.api=MemoryAPI();self.ssh_calls=[];self.factory_calls=0;self.live=True
        self.controller=c.Controller(self.cfg,self.api,'control-node',self.factory,lambda:self.live)
        self.task_id='a'*32;self.uid='task-uid';self.password='fixture-password-only'
        self.fp='SHA256:'+'A'*43
        self.obj={'apiVersion':c.API_VERSION,'kind':'NodeEnrollment','metadata':{'name':'enroll-'+self.task_id,'namespace':self.cfg['requests_namespace'],'uid':self.uid,'resourceVersion':'1','generation':1,'creationTimestamp':c.stamp()},
                  'spec':{'region':'us-west','host':'1.1.1.1','port':22,'action':'Scan'}}
        self.path=self.controller.enrollments+'/'+self.obj['metadata']['name'];self.api.put(self.path,self.obj)
    def tearDown(self):self.temp.cleanup()
    def current(self):return self.api.get(self.path)
    def update(self,**spec):
        obj=self.current();obj['spec'].update(spec);obj['metadata']['generation']+=1;obj['metadata']['resourceVersion']=str(int(obj['metadata']['resourceVersion'])+1)
        return self.api.put(self.path,obj)
    def factory(self,config,api,callback=None):
        parent=self;parent.factory_calls+=1
        class Engine(c.SSHCore):
            def scan(self,data):
                ident='b'*32
                value={'scan_id':ident,'host':data['host'],'port':data['port'],'region':data['region'],'expires_at':int(time.time())+600,
                       'known_host':data['host']+' ssh-ed25519 '+base64.b64encode(b'x'*48).decode(),
                       'fingerprints':[{'algorithm':'ssh-ed25519','fingerprint':parent.fp}]}
                self.scans[ident]=value
                return {k:v for k,v in value.items() if k!='known_host'}
            def ssh(self,session,script,payload,timeout):
                if not self.fence():raise c.Failure('controller_lease_lost',503)
                parent.ssh_calls.append((script,copy.deepcopy(payload)))
                parent.assertEqual(session['password'],parent.password)
                if script==c.core.PROBE:
                    return {'uid':0,'system':'Linux','arch':'x86_64','os':'debian','systemd':True,'python_version':[3,13],
                            'cpu_cores':2,'memory_mib':1900,'disk_free_gib':20,'swap':False,'existing_installation':False,
                            'addresses':[{'ip':'10.4.0.5','interface':'eth0'}],'api_reachable':True}
                parent.assertEqual(script,c.core.INSTALL)
                parent.assertIsNone(parent.api.get(parent.controller.secret_path+'/ssh-'+parent.task_id))
                parent.api.put('/api/v1/nodes/game-1-1-1-1',parent.node(ready='True'))
                return {'installed':True}
        return Engine(config,api,callback)
    def credential(self,offset=0):
        meta={'name':'ssh-'+self.task_id,'namespace':self.cfg['requests_namespace'],'uid':'secret-uid','resourceVersion':'1',
              'creationTimestamp':c.datetime.fromtimestamp(time.time()+offset,c.timezone.utc).isoformat(),
              'labels':{'nakama-agones.io/enrollment-id':self.task_id},'ownerReferences':[{
                'apiVersion':c.API_VERSION,'kind':'NodeEnrollment','name':self.obj['metadata']['name'],'uid':self.uid,'controller':False,'blockOwnerDeletion':False}]}
        value={'apiVersion':'v1','kind':'Secret','metadata':meta,'type':'Opaque','immutable':True,'data':{'password':base64.b64encode(self.password.encode()).decode()}}
        self.api.put(self.controller.secret_path+'/'+meta['name'],value)
        return value
    def prepared(self):
        self.controller.reconcile(self.current())
        secret=self.credential()
        self.update(action='Preflight',hostFingerprint=self.fp,credentialSecretRef={'name':secret['metadata']['name'],'uid':secret['metadata']['uid']})
        self.controller.reconcile(self.current())
        self.assertEqual(self.current()['status']['phase'],'AwaitingApproval')
    def node(self,ready='Unknown'):
        return {'kind':'Node','metadata':{'name':'game-1-1-1-1','uid':'node-uid','resourceVersion':'5','labels':{
            'nakama-agones.io/game-node':'true','nakama-agones.io/role':'game','nakama-agones.io/cluster':'test-cluster'}},'spec':{},
            'status':{'conditions':[{'type':'Ready','status':ready}], 'addresses':[{'type':'InternalIP','address':'10.4.0.5'},{'type':'ExternalIP','address':'1.1.1.1'}]}}
    def retirement(self):
        obj={'apiVersion':c.API_VERSION,'kind':'NodeRetirement','metadata':{'name':'retire-'+'b'*32,'namespace':self.cfg['requests_namespace'],'uid':'retire-uid','resourceVersion':'1','generation':1},'spec':{
            'region':'us-west','nodeName':'game-1-1-1-1','nodeUID':'node-uid','internalIPs':['10.4.0.5'],'externalIPs':['1.1.1.1'],
            'confirmedNodeName':'game-1-1-1-1','confirmedIP':'1.1.1.1','permanentRetirement':True}}
        path=self.controller.retirements+'/'+obj['metadata']['name'];self.api.put(path,obj)
        self.api.put('/api/v1/nodes/game-1-1-1-1',self.node());return obj,path

    def test_complete_flow_reuses_fixed_preflight_install_and_consumes_secret_before_install(self):
        self.prepared();current=self.current()
        self.update(action='Join',approvedPreflightDigest=current['status']['preflightDigest'])
        with patch.object(c.threading,'Thread',InlineThread):self.controller.reconcile(self.current())
        self.assertEqual(self.current()['status']['phase'],'Ready')
        self.assertEqual(len(self.ssh_calls),3)
        self.assertNotIn(self.password,json.dumps(self.api.objects))
        self.assertNotIn('join.token',json.dumps(self.current()))
        count=len(self.ssh_calls);self.controller.reconcile(self.current());self.assertEqual(len(self.ssh_calls),count)

    def test_wrong_owner_uid_or_secret_uid_never_sends_password(self):
        for field in ('owner','reference','expired'):
            with self.subTest(field=field):
                obj=self.current();obj['spec']['credentialSecretRef']={'name':'ssh-'+self.task_id,'uid':'secret-uid'}
                secret=self.credential(offset=-601 if field=='expired' else 0)
                if field=='owner':secret['metadata']['ownerReferences'][0]['uid']='foreign'
                if field=='reference':obj['spec']['credentialSecretRef']['uid']='foreign'
                self.api.put(self.controller.secret_path+'/ssh-'+self.task_id,secret)
                with self.assertRaises(c.Failure):self.controller.secret(obj)
                self.assertEqual(self.ssh_calls,[])

    def test_wrong_host_fingerprint_fails_without_ssh_and_deletes_secret(self):
        self.controller.reconcile(self.current());secret=self.credential()
        self.update(action='Preflight',hostFingerprint='SHA256:'+'B'*43,credentialSecretRef={'name':secret['metadata']['name'],'uid':secret['metadata']['uid']})
        self.controller.reconcile(self.current())
        self.assertEqual(self.current()['status']['phase'],'Failed');self.assertEqual(self.ssh_calls,[])
        self.assertIsNone(self.api.get(self.controller.secret_path+'/ssh-'+self.task_id))

    def test_wrong_approval_cannot_install(self):
        self.prepared();self.update(action='Join',approvedPreflightDigest='0'*64)
        self.controller.reconcile(self.current())
        self.assertEqual(self.current()['status']['phase'],'Failed');self.assertEqual(len(self.ssh_calls),1)

    def test_restarting_install_never_replays_ssh(self):
        self.prepared();obj=self.current();obj['status']['phase']='Installing';self.api.put(self.path,obj)
        count=len(self.ssh_calls);self.controller.reconcile(self.current(),startup=True)
        self.assertEqual(self.current()['status']['phase'],'NeedsReview');self.assertEqual(len(self.ssh_calls),count)
        self.assertIsNone(self.api.get(self.controller.secret_path+'/ssh-'+self.task_id))

    def test_expired_pending_approval_deletes_credentials(self):
        self.prepared();obj=self.current();obj['status']['preflight']['expires_at']=1;self.api.put(self.path,obj)
        self.controller.reconcile(self.current());self.assertEqual(self.current()['status']['phase'],'Expired')
        self.assertIsNone(self.api.get(self.controller.secret_path+'/ssh-'+self.task_id))

    def test_protected_target_reaches_terminal_failure_without_probe(self):
        self.update(host='8.8.8.8')
        self.controller.reconcile(self.current())
        self.assertEqual(self.current()['status']['phase'],'Failed')
        self.assertEqual(self.factory_calls,0)
        revision=self.current()['metadata']['resourceVersion']
        self.controller.reconcile(self.current())
        self.assertEqual(self.current()['metadata']['resourceVersion'],revision)

    def test_status_conflict_prevents_any_probe(self):
        self.api.conflict=True;self.controller.reconcile(self.current())
        self.assertEqual(self.factory_calls,0);self.assertEqual(self.ssh_calls,[])

    def test_lease_loss_fences_new_work_mutation_and_ssh(self):
        self.live=False
        for action in (lambda:self.controller.reconcile(self.current()),lambda:self.controller.write(self.controller.enrollments,self.current(),phase='Scanning'),lambda:self.controller.cleanup()):
            with self.assertRaises(c.Failure):action()
        self.assertEqual(self.api.calls,[])
        engine=self.factory(self.cfg,self.api);engine.fence=lambda:False
        with patch.object(c.core.Broker,'ssh') as ssh:
            with self.assertRaises(c.Failure):c.SSHCore.ssh(engine,{},'',{},1)
            ssh.assert_not_called()

    def test_cleanup_deletes_only_owned_expired_secret(self):
        stale=self.credential(-601)
        foreign=copy.deepcopy(stale);foreign['metadata']['name']='unrelated';foreign['metadata']['uid']='foreign'
        self.api.put(self.controller.secret_path+'/unrelated',foreign)
        self.controller.cleanup()
        self.assertIsNone(self.api.get(self.controller.secret_path+'/ssh-'+self.task_id))
        self.assertIsNotNone(self.api.get(self.controller.secret_path+'/unrelated'))

    def test_offline_retirement_uses_exact_uid_version_delete_only(self):
        obj,path=self.retirement();self.controller.retire(obj)
        self.assertEqual(self.api.get(path)['status']['phase'],'Deleted')
        deletes=[a for a in self.api.calls if a[0]=='delete'];self.assertEqual(deletes,[('delete','/api/v1/nodes/game-1-1-1-1','node-uid','5')])
        self.assertFalse(any(a[0]=='PATCH' for a in self.api.calls));self.assertEqual(self.ssh_calls,[])

    def test_retirement_rejects_ready_foreign_control_own_and_ip_drift(self):
        for variant in ('ready','foreign','control','own','ip'):
            obj,path=self.retirement();node=self.node()
            if variant=='ready':node['status']['conditions'][0]['status']='True'
            if variant=='foreign':node['metadata']['labels']['nakama-agones.io/cluster']='other'
            if variant=='control':node['metadata']['labels']['node-role.kubernetes.io/control-plane']=''
            if variant=='own':self.controller.own_node=obj['spec']['nodeName']
            if variant=='ip':node['status']['addresses'][0]['address']='10.4.0.9'
            self.api.put('/api/v1/nodes/game-1-1-1-1',node);self.api.calls=[]
            self.controller.retire(obj)
            self.assertEqual(self.api.get(path)['status']['phase'],'Blocked',variant)
            self.assertFalse(any(a[0]=='delete' for a in self.api.calls));self.controller.own_node='control-node'

    def test_game_pod_and_gameserver_remnants_are_visible_blockers(self):
        obj,path=self.retirement()
        self.api.put('/api/v1/namespaces/agones-games/pods/game',{'kind':'Pod','metadata':{'name':'game','namespace':'agones-games'},'spec':{'nodeName':'game-1-1-1-1'}})
        self.api.put('/apis/agones.dev/v1/namespaces/agones-games/gameservers/game',{'kind':'GameServer','metadata':{'name':'game'},'status':{'nodeName':'game-1-1-1-1'}})
        self.controller.retire(obj);status=self.api.get(path)['status']
        self.assertEqual(status['phase'],'Blocked');self.assertEqual({b['kind'] for b in status['blockers']},{'Pod','GameServer'})
        self.assertFalse(any(a[0]=='delete' for a in self.api.calls))

    def test_only_real_allowlisted_system_daemonset_remnant_is_allowed(self):
        for valid in (False,True):
            obj,path=self.retirement()
            self.api.put('/api/v1/namespaces/kube-system/pods/agent',{'kind':'Pod','metadata':{'name':'agent','namespace':'kube-system',
                'ownerReferences':[{'apiVersion':'apps/v1','kind':'DaemonSet','name':'agent','uid':'ds-uid','controller':True}]},'spec':{'nodeName':'game-1-1-1-1'}})
            self.api.put('/apis/apps/v1/namespaces/kube-system/daemonsets/agent',{'metadata':{'uid':'ds-uid' if valid else 'changed'}})
            self.controller.retire(obj);status=self.api.get(path)['status']
            self.assertEqual(status['phase'],'Deleted' if valid else 'Blocked')
            if valid:self.assertEqual(status['allowedSystemPods'][0]['name'],'agent')

    def test_node_revision_changes_at_delete_require_new_confirmation(self):
        obj,path=self.retirement()
        def drift(p):self.api.objects[p]['metadata']['resourceVersion']='6'
        self.api.before_delete=drift;self.controller.retire(obj)
        current=self.api.get(path);self.assertEqual(current['status']['phase'],'NeedsReview')
        count=len(self.api.calls);self.controller.retire(current);self.assertEqual(len(self.api.calls),count)
        self.assertIsNotNone(self.api.get('/api/v1/nodes/game-1-1-1-1'))

    def test_retirement_lease_loss_before_delete_prevents_side_effect(self):
        obj,path=self.retirement();original=self.controller.retirement_node;calls=[]
        def checked(spec):
            calls.append(1);node=original(spec)
            if len(calls)==2:self.live=False
            return node
        with patch.object(self.controller,'retirement_node',side_effect=checked):
            with self.assertRaises(c.Failure):self.controller.retire(obj)
        self.assertFalse(any(a[0]=='delete' for a in self.api.calls))

    def test_one_controller_lease_rejects_second_unexpired_process(self):
        lease=c.Lease(self.api,self.cfg['system_namespace'])
        self.api.put(lease.path,{'metadata':{'name':c.LEASE_NAME,'resourceVersion':'1'},'spec':{}})
        lease.renew();other=c.Lease(self.api,self.cfg['system_namespace'])
        with self.assertRaises(c.Failure):other.renew()
        lease.renew();self.assertTrue(lease.alive.is_set())


class PackagingTests(unittest.TestCase):
    def setUp(self):self.root=Path(__file__).resolve().parents[1]
    def manifests(self,name):return [json.loads(v) for v in (self.root/'deploy/enrollment'/name).read_text().split('\n---\n')]
    def test_submitter_cannot_create_pods_read_secrets_or_write_nodes(self):
        roles=self.manifests('20-rbac.yaml');role=next(v for v in roles if v['kind']=='Role' and v['metadata']['name']=='enrollment-submit')
        self.assertEqual({r for rule in role['rules'] for r in rule['resources']},{'nodeenrollments','noderetirements','secrets'})
        secrets_rule=next(r for r in role['rules'] if r['resources']==['secrets'])
        self.assertEqual(set(secrets_rule['verbs']),{'create','delete'})
        self.assertFalse(any('patch' in r['verbs'] for v in self.manifests('31-retirement-rbac.yaml') if v['kind']=='ClusterRole' for r in v['rules']))
    def test_delete_permission_separate_from_fail_closed_admission(self):
        policy=self.manifests('30-retirement-admission.yaml')[0]
        self.assertEqual(policy['spec']['failurePolicy'],'Fail')
        raw=json.dumps(policy)
        for expected in ('game-node','role','cluster','control-plane','master','False','Unknown','fleet-node-enrollment'):self.assertIn(expected,raw)
    def test_render_is_private_idempotent_pinned_and_nonroot(self):
        with tempfile.TemporaryDirectory() as d:
            cfg=json.loads((self.root/'deploy/enrollment/config.example.json').read_text());cfg['secrets_encryption_verified']=True
            config=Path(d)/'config.json';config.write_text(json.dumps(cfg));config.chmod(0o600)
            dest=Path(d)/'output';image='registry.example/controller@sha256:'+'a'*64
            count=render.render(config,image,dest);self.assertEqual(count,render.render(config,image,dest))
            self.assertEqual(dest.stat().st_mode&0o777,0o700);self.assertFalse((dest/'31-retirement-rbac.yaml').exists())
            objects=[json.loads(x) for x in (dest/'40-controller.yaml').read_text().split('\n---\n')];pod=objects[1]['spec']['template']['spec']
            self.assertEqual(pod['securityContext']['runAsUser'],10001);self.assertFalse(pod.get('hostNetwork',False))
            self.assertFalse(any('hostPath' in v for v in pod['volumes']));self.assertEqual(pod['containers'][0]['image'],image)
            self.assertTrue(any(t['key']=='CriticalAddonsOnly' for t in pod['tolerations']))
            self.assertEqual((dest/'40-controller.yaml').stat().st_mode&0o777,0o600)
            with self.assertRaises(c.Failure):render.render(config,'registry/controller:latest',dest)
            cfg['region']['name']='changed';config.write_text(json.dumps(cfg))
            with self.assertRaises(c.Failure):render.render(config,image,dest)
    def test_unverified_encryption_blocks_render(self):
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(c.Failure):render.render(self.root/'deploy/enrollment/config.example.json','registry/controller@sha256:'+'a'*64,Path(d)/'out')
    def test_isolated_python_import_and_old_server_entrypoint_disabled(self):
        p=subprocess.run([sys.executable,'-I',str(self.root/'scripts/node_enrollment_controller.py'),'--help'],capture_output=True,timeout=5)
        self.assertEqual(p.returncode,0,p.stderr.decode())
        p=subprocess.run([sys.executable,str(self.root/'scripts/node_onboarding.py')],capture_output=True,timeout=5)
        self.assertNotEqual(p.returncode,0);self.assertIn(b'internal installer module',p.stderr)


if __name__=='__main__':unittest.main()
