#!/usr/bin/env python3
"""Regenerate public, credential-free manifests. Runtime does not use this file."""
import json
from pathlib import Path
ROOT = Path(__file__).resolve().parent
GROUP = 'infrastructure.nakama-agones.io'
NS = 'fleet-enrollment-requests'
SYSTEM = 'fleet-enrollment-system'
NAME = {'type':'string','minLength':1,'maxLength':63,'pattern':'^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'}
ID = {'type':'string','pattern':'^[a-f0-9]{32}$','maxLength':32}
UID = {'type':'string','minLength':1,'maxLength':128}
CODE = {'type':'string','maxLength':96,'pattern':'^[a-z0-9_]*$'}
IP = {'type':'string','minLength':3,'maxLength':45}
INTEGER = {'type':'integer','format':'int64','minimum':0}
CHECK = {'type':'object','required':['name','ok','detail'],'properties':{'name':NAME,'ok':{'type':'boolean'},'detail':{'type':'string','maxLength':512}}}
CHECKS = {'type':'array','maxItems':32,'items':CHECK}
JOB = {'type':'object','properties':{
 'id':ID,'host':IP,'region':NAME,'node_name':NAME,'node_uid':UID,
 'state':{'type':'string','maxLength':32},'stage':CODE,'error':CODE,
 'created_at':INTEGER,'updated_at':INTEGER,'checks':CHECKS}}
SCAN = {'type':'object','required':['scan_id','host','port','region','expires_at','fingerprints'],'properties':{
 'scan_id':ID,'host':IP,'port':{'type':'integer','minimum':1,'maximum':65535},'region':NAME,'expires_at':INTEGER,
 'fingerprints':{'type':'array','minItems':1,'maxItems':1,'items':{'type':'object','required':['algorithm','fingerprint'],'properties':{
 'algorithm':{'type':'string','enum':['ssh-ed25519']},'fingerprint':{'type':'string','pattern':'^SHA256:[A-Za-z0-9+/]{43}$','maxLength':50}}}}}}
PRE = {'type':'object','properties':{
 'preflight_id':ID,'expires_at':INTEGER,'host':IP,'region':NAME,'node_name':NAME,
 'private_ip':{'type':'string','maxLength':45},'external_ip':IP,'interface':{'type':'string','maxLength':32},
 'cpu_cores':{'type':'integer','minimum':0,'maximum':4096},'memory_mib':{'type':'integer','minimum':0,'maximum':100000000},
 'disk_free_gib':{'type':'number','minimum':0},'existing_installation':{'type':'boolean'},'checks':CHECKS,
 'can_join':{'type':'boolean'},'plan':{'type':'array','maxItems':16,'items':{'type':'string','maxLength':512}}}}
RESOURCES = {'type':'array','maxItems':200,'items':{'type':'object','required':['kind','namespace','name','reason'],'properties':{
 'kind':{'type':'string','enum':['Pod','GameServer']},'namespace':NAME,'name':{'type':'string','maxLength':253},'reason':CODE}}}

def output(name, objects):
    (ROOT/name).write_text('\n---\n'.join(json.dumps(v,indent=2)+'\n' for v in objects))

def crd(plural, kind, spec, phases, extra):
    status={'type':'object','properties':{'phase':{'type':'string','enum':phases},'observedGeneration':INTEGER,
       'updatedAt':{'type':'string','format':'date-time'},'error':CODE,'job':JOB,**extra}}
    return {'apiVersion':'apiextensions.k8s.io/v1','kind':'CustomResourceDefinition','metadata':{'name':plural+'.'+GROUP},'spec':{
       'group':GROUP,'scope':'Namespaced','names':{'plural':plural,'singular':kind.lower(),'kind':kind},
       'versions':[{'name':'v1alpha1','served':True,'storage':True,'subresources':{'status':{}},
        'additionalPrinterColumns':[{'name':'Phase','type':'string','jsonPath':'.status.phase'},{'name':'Region','type':'string','jsonPath':'.spec.region'}],
        'schema':{'openAPIV3Schema':{'type':'object','required':['spec'],'properties':{'spec':spec,'status':status}}}}]}}

spec={'type':'object','required':['region','host','port','action'],'properties':{
 'region':NAME,'host':{'type':'string','maxLength':15,'pattern':'^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$'},
 'port':{'type':'integer','minimum':1,'maximum':65535},'action':{'type':'string','enum':['Scan','Preflight','Join']},
 'hostFingerprint':SCAN['properties']['fingerprints']['items']['properties']['fingerprint'],
 'credentialSecretRef':{'type':'object','required':['name','uid'],'properties':{'name':{'type':'string','pattern':'^ssh-[a-f0-9]{32}$','maxLength':36},'uid':UID}},
 'approvedPreflightDigest':{'type':'string','pattern':'^[a-f0-9]{64}$','maxLength':64}},
 'x-kubernetes-validations':[
 {'rule':'self.region == oldSelf.region && self.host == oldSelf.host && self.port == oldSelf.port','message':'target and region are immutable'},
 {'rule':"self.action == oldSelf.action || (oldSelf.action == 'Scan' && self.action == 'Preflight') || (oldSelf.action == 'Preflight' && self.action == 'Join')",'message':'actions can only advance'},
 {'rule':"self.action == 'Scan' || (has(self.hostFingerprint) && has(self.credentialSecretRef))",'message':'verified host and credential reference are required'},
 {'rule':"self.action != 'Join' || has(self.approvedPreflightDigest)",'message':'join requires approval of the exact preflight'},
 {'rule':'!has(oldSelf.hostFingerprint) || (has(self.hostFingerprint) && self.hostFingerprint == oldSelf.hostFingerprint)','message':'host fingerprint is immutable once approved'},
 {'rule':'!has(oldSelf.credentialSecretRef) || (has(self.credentialSecretRef) && self.credentialSecretRef == oldSelf.credentialSecretRef)','message':'credential identity is immutable once provided'},
 {'rule':'!has(oldSelf.approvedPreflightDigest) || (has(self.approvedPreflightDigest) && self.approvedPreflightDigest == oldSelf.approvedPreflightDigest)','message':'approval cannot be replaced'}]}
retire={'type':'object','required':['region','nodeName','nodeUID','internalIPs','externalIPs','confirmedNodeName','confirmedIP','permanentRetirement'],'properties':{
 'region':NAME,'nodeName':NAME,'nodeUID':UID,'internalIPs':{'type':'array','maxItems':8,'x-kubernetes-list-type':'set','items':IP},
 'externalIPs':{'type':'array','maxItems':8,'x-kubernetes-list-type':'set','items':IP},'confirmedNodeName':NAME,'confirmedIP':IP,
 'permanentRetirement':{'type':'boolean','enum':[True]}},'x-kubernetes-validations':[
 {'rule':'self == oldSelf','message':'retirement request is immutable'},
 {'rule':'self.confirmedNodeName == self.nodeName','message':'type the exact node name'},
 {'rule':'self.confirmedIP in self.internalIPs || self.confirmedIP in self.externalIPs','message':'type a current node IP'}]}
output('10-crds.yaml',[
 crd('nodeenrollments','NodeEnrollment',spec,['Scanning','AwaitingPreflight','Preflighting','AwaitingApproval','Installing','Verifying','Ready','Failed','NeedsReview','Expired'],{
 'scan':SCAN,'preflight':PRE,'preflightDigest':{'type':'string','maxLength':64,'pattern':'^[a-f0-9]{64}$'},'sshHostKey':{'type':'string','maxLength':512}}),
 crd('noderetirements','NodeRetirement',retire,['Pending','Checking','Deleted','Blocked','NeedsReview'],{
 'blockers':RESOURCES,'allowedSystemPods':RESOURCES,'blockersTruncated':{'type':'boolean'}})])

def role(name, ns, rules, cluster=False):
 return {'apiVersion':'rbac.authorization.k8s.io/v1','kind':'ClusterRole' if cluster else 'Role','metadata':{'name':name,**({} if cluster else {'namespace':ns})},'rules':rules}
def binding(name, ns, account='fleet-node-enrollment', cluster=False):
 return {'apiVersion':'rbac.authorization.k8s.io/v1','kind':'ClusterRoleBinding' if cluster else 'RoleBinding',
 'metadata':{'name':name,**({} if cluster else {'namespace':ns})},'subjects':[{'kind':'ServiceAccount','name':account,'namespace':SYSTEM}],
 'roleRef':{'apiGroup':'rbac.authorization.k8s.io','kind':'ClusterRole' if cluster else 'Role','name':name}}
def rule(group, resources, verbs, names=None):
 return {'apiGroups':[group],'resources':resources,'verbs':verbs,**({'resourceNames':names} if names else {})}
output('00-namespaces.yaml',[
 {'apiVersion':'v1','kind':'Namespace','metadata':{'name':name,'labels':{'pod-security.kubernetes.io/enforce':'restricted','pod-security.kubernetes.io/enforce-version':'v1.35'}}} for name in (NS,SYSTEM)] + [
 {'apiVersion':'v1','kind':'ResourceQuota','metadata':{'name':'enrollment-bounds','namespace':NS},'spec':{'hard':{
 'count/nodeenrollments.'+GROUP:'100','count/noderetirements.'+GROUP:'100','secrets':'20','pods':'0','services':'0'}}}])
output('20-rbac.yaml',[
 *[{'apiVersion':'v1','kind':'ServiceAccount','metadata':{'name':name,'namespace':SYSTEM},'automountServiceAccountToken':False} for name in ('fleet-node-enrollment','fleet-enrollment-submitter')],
 role('enrollment-submit',NS,[rule(GROUP,['nodeenrollments'],['get','list','watch','create','patch']),rule(GROUP,['noderetirements'],['get','list','watch','create']),rule('',['secrets'],['create','delete'])]),
 binding('enrollment-submit',NS,'fleet-enrollment-submitter'),
 role('enrollment-reconcile',NS,[rule(GROUP,['nodeenrollments','noderetirements'],['get','list','watch','delete']),rule(GROUP,['nodeenrollments/status','noderetirements/status'],['get','update']),rule('',['secrets'],['get','list','delete'])]),
 binding('enrollment-reconcile',NS),
 role('enrollment-observe-nodes',None,[rule('',['nodes','pods'],['get','list'])],True),binding('enrollment-observe-nodes',None,cluster=True),
 role('enrollment-observe-games','agones-games',[rule('agones.dev',['gameservers'],['get','list'])]),binding('enrollment-observe-games','agones-games'),
 *[obj for ns in ('kube-system','agones-system','agones-observability') for obj in (
 role('enrollment-observe-daemonsets',ns,[rule('apps',['daemonsets'],['get'])]),binding('enrollment-observe-daemonsets',ns))],
 {'apiVersion':'coordination.k8s.io/v1','kind':'Lease','metadata':{'name':'fleet-node-enrollment','namespace':SYSTEM},'spec':{}},
 role('enrollment-controller-lease',SYSTEM,[rule('coordination.k8s.io',['leases'],['get','update'],['fleet-node-enrollment'])]),binding('enrollment-controller-lease',SYSTEM)])

subject='system:serviceaccount:'+SYSTEM+':fleet-node-enrollment'
labels="has(oldObject.metadata.labels) && 'nakama-agones.io/game-node' in oldObject.metadata.labels && oldObject.metadata.labels['nakama-agones.io/game-node'] == 'true' && 'nakama-agones.io/role' in oldObject.metadata.labels && oldObject.metadata.labels['nakama-agones.io/role'] == 'game' && 'nakama-agones.io/cluster' in oldObject.metadata.labels && oldObject.metadata.labels['nakama-agones.io/cluster'] == 'example-us' && !('node-role.kubernetes.io/control-plane' in oldObject.metadata.labels) && !('node-role.kubernetes.io/master' in oldObject.metadata.labels)"
offline="has(oldObject.status) && has(oldObject.status.conditions) && oldObject.status.conditions.filter(c, c.type == 'Ready').size() == 1 && oldObject.status.conditions.exists(c, c.type == 'Ready' && c.status in ['False', 'Unknown'])"
policy={'apiVersion':'admissionregistration.k8s.io/v1','kind':'ValidatingAdmissionPolicy','metadata':{'name':'fleet-offline-game-node-delete'},'spec':{
 'failurePolicy':'Fail','matchConstraints':{'resourceRules':[{'apiGroups':[''],'apiVersions':['v1'],'operations':['DELETE'],'resources':['nodes']}]},
 'matchConditions':[{'name':'enrollment-controller-only','expression':"request.userInfo.username == '"+subject+"'"}],
 'validations':[{'expression':labels,'message':'controller may only retire owned regional game workers'}, {'expression':offline,'message':'controller may only retire an explicitly offline node'}]}}
output('30-retirement-admission.yaml',[
 policy, {'apiVersion':'admissionregistration.k8s.io/v1','kind':'ValidatingAdmissionPolicyBinding','metadata':{'name':'fleet-offline-game-node-delete'},'spec':{'policyName':'fleet-offline-game-node-delete','validationActions':['Deny']}}])
output('31-retirement-rbac.yaml',[
 role('enrollment-delete-offline-node',None,[rule('',['nodes'],['delete'])],True),binding('enrollment-delete-offline-node',None,cluster=True)])

secretpolicy={'apiVersion':'admissionregistration.k8s.io/v1','kind':'ValidatingAdmissionPolicy','metadata':{'name':'fleet-enrollment-secret-shape'},'spec':{
 'failurePolicy':'Fail','matchConstraints':{'resourceRules':[{'apiGroups':[''],'apiVersions':['v1'],'operations':['CREATE'],'resources':['secrets']}]},
 'matchConditions':[{'name':'submitter-only','expression':"request.userInfo.username == 'system:serviceaccount:"+SYSTEM+":fleet-enrollment-submitter'"}],
 'validations':[
 {'expression':"request.namespace == '"+NS+"' && object.metadata.name.matches('^ssh-[a-f0-9]{32}$')",'message':'only request credentials may be created'},
 {'expression':"object.type == 'Opaque' && has(object.immutable) && object.immutable && has(object.data) && object.data.size() == 1 && 'password' in object.data && size(object.data['password']) <= 1368",'message':'one immutable bounded password is required'},
 {'expression':"has(object.metadata.labels) && 'nakama-agones.io/enrollment-id' in object.metadata.labels && object.metadata.name == 'ssh-' + object.metadata.labels['nakama-agones.io/enrollment-id']",'message':'credential label must match its name'},
 {'expression':"has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences.all(o, o.apiVersion == '"+GROUP+"/v1alpha1' && o.kind == 'NodeEnrollment' && o.name == 'enroll-' + object.metadata.labels['nakama-agones.io/enrollment-id'] && size(o.uid) > 0 && (!has(o.controller) || !o.controller) && (!has(o.blockOwnerDeletion) || !o.blockOwnerDeletion))",'message':'credential must be owned by one enrollment request'}]}}
output('25-secret-admission.yaml',[secretpolicy,{'apiVersion':'admissionregistration.k8s.io/v1','kind':'ValidatingAdmissionPolicyBinding','metadata':{'name':'fleet-enrollment-secret-shape'},'spec':{'policyName':'fleet-enrollment-secret-shape','validationActions':['Deny']}}])
