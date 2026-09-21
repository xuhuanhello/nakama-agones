"""Host-isolated safety checks: no Kubernetes connection, package install or root required."""
from contextlib import ExitStack, redirect_stdout
from datetime import datetime, timedelta, timezone
from functools import partial
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'scripts'))
import k3s_node as node
import issue_nakama_token as issuer


class NodeTests(unittest.TestCase):
    def setUp(self):
        self.stack=ExitStack()
        self.directory=Path(self.stack.enter_context(tempfile.TemporaryDirectory()))
        for key in ('CONFIG','MARKER','TOKEN','BIN','AGENT_SECRET','LOGDIR','LOGROTATE','REGISTRY_CONFIG','REGISTRY_CA','CACHE'):
            self.stack.enter_context(patch.object(node,key,self.directory/key.lower()))
        self.stack.enter_context(patch.object(node,'secure_read',partial(node.secure_read,expected_uid=os.getuid())))
        self.stack.enter_context(patch.object(node.shutil,'which',return_value=None))
        self.stack.enter_context(patch.object(node,'validate_host'))
        self.installer=self.stack.enter_context(patch.object(node,'install_binary_and_service'))
        self.runner=self.stack.enter_context(patch.object(node,'run'))
        self.output=self.stack.enter_context(redirect_stdout(io.StringIO()))
        self.addCleanup(self.stack.close)

    def arguments(self,role='manager',extra=()):
        fields=[role,'--cluster-id','test-us','--node-name','test-'+role,'--private-ip','10.0.0.2','--interface','eth0']
        if role=='worker':
            self.source=self.directory/'source.token'
            node.atomic_write(self.source,b'K10'+b'a'*64+b'::node:'+b'b'*48+b'\n')
            fields+=['--external-ip','8.8.8.8','--server','https://10.0.0.1:6443','--token-file',str(self.source)]
        return node.parser().parse_args(fields+list(extra))

    def test_repeat_install_does_not_restart_or_change_identity(self):
        args=self.arguments()
        node.apply(args)
        original=(node.CONFIG.read_bytes(),node.AGENT_SECRET.read_bytes())
        node.apply(args)
        self.assertEqual(original,(node.CONFIG.read_bytes(),node.AGENT_SECRET.read_bytes()))
        self.assertEqual(self.runner.call_args_list[0].args[0],['systemctl','enable','--now','k3s'])
        self.assertEqual(self.runner.call_args_list[1].args[0],['systemctl','enable','--now','k3s'])
        self.assertNotIn('restart',str(self.runner.call_args_list))

    def test_cluster_identity_drift_fails_before_install_or_write(self):
        args=self.arguments();node.apply(args)
        before=node.CONFIG.read_bytes();self.installer.reset_mock()
        args.cluster_id='another-cluster'
        with self.assertRaisesRegex(node.SetupError,'another cluster'):node.apply(args)
        self.assertEqual(before,node.CONFIG.read_bytes());self.installer.assert_not_called()

    def test_unmanaged_installation_is_not_adopted(self):
        node.BIN.write_text('unmanaged')
        with self.assertRaisesRegex(node.SetupError,'not owned'):node.apply(self.arguments())
        self.assertFalse(node.MARKER.exists());self.installer.assert_not_called()

    def test_config_drift_and_missing_managed_secret_fail_closed(self):
        args=self.arguments();node.apply(args);original=node.CONFIG.read_bytes()
        node.CONFIG.write_text('foreign configuration')
        with self.assertRaisesRegex(node.SetupError,'edited outside'):node.apply(args)
        node.CONFIG.write_bytes(original);node.AGENT_SECRET.unlink()
        with self.assertRaisesRegex(node.SetupError,'credential is missing'):node.apply(args)

    def test_join_token_must_be_private_agent_only_and_ca_pinned(self):
        args=self.arguments('worker')
        self.assertTrue(node.join_token(self.source).startswith(b'K10'))
        node.atomic_write(self.source,b'K10'+b'a'*64+b'::server:'+b'b'*48)
        with self.assertRaisesRegex(node.SetupError,'agent-only'):node.apply(args)
        node.atomic_write(self.source,b'K10'+b'a'*64+b'::node:'+b'b'*48,0o644)
        with self.assertRaisesRegex(node.SetupError,'0600'):node.apply(args)
        self.source.unlink();self.source.symlink_to(node.CONFIG)
        with self.assertRaises(OSError):node.join_token(self.source)

    def test_worker_registration_and_resource_boundaries(self):
        config=node.configuration(self.arguments('worker'))
        self.assertIn('nakama-agones.io/game-node=true',config['node-label'])
        self.assertNotIn('node-taint',config)
        self.assertEqual(config['server'],'https://10.0.0.1:6443')
        self.assertIn('container-log-max-size=10Mi',config['kubelet-arg'])
        self.assertIn('container-log-max-files=3',config['kubelet-arg'])
        self.assertIn('nakama-agones.io/game-node=false',node.configuration(self.arguments())['node-label'])

    def test_registry_plan_does_not_read_credentials(self):
        args=self.arguments(extra=['--plan','--registry-config-file','/root/missing.json','--registry-ca-file','/root/missing.crt'])
        with patch.object(node,'registry_inputs',side_effect=AssertionError('must not read')):
            node.apply(args)
        self.assertTrue(json.loads(self.output.getvalue())['private_registry']['enabled'])
        self.installer.assert_not_called();self.assertFalse(node.MARKER.exists())

    def registry_args(self):
        config=self.directory/'input-registry.json';ca=self.directory/'input-ca.crt'
        node.atomic_write(config,node.encode({'configs':{'10.0.0.3:5443':{'auth':{'username':'reader','password':'unit-test-password'},'tls':{'ca_file':str(node.REGISTRY_CA)}}}}))
        node.atomic_write(ca,b'unit-test-ca')
        return self.arguments(extra=['--registry-config-file',str(config),'--registry-ca-file',str(ca)])

    def test_registry_copied_before_service_install_and_drift_rejected(self):
        args=self.registry_args()
        def installed(*unused):
            self.assertTrue(node.REGISTRY_CONFIG.exists());self.assertTrue(node.REGISTRY_CA.exists())
            self.assertEqual(node.REGISTRY_CONFIG.stat().st_mode&0o777,0o600)
        self.installer.side_effect=installed
        with patch.object(node.ssl,'create_default_context'):
            node.apply(args);node.apply(args)
            value=json.loads(args.registry_config_file.read_bytes())
            value['configs']['10.0.0.3:5443']['auth']['password']='replacement'
            node.atomic_write(args.registry_config_file,node.encode(value))
            with self.assertRaisesRegex(node.SetupError,'registry credentials or CA differ'):node.apply(args)
        self.assertNotIn('unit-test-password',self.output.getvalue())

    def test_registry_rejects_insecure_tls_or_redirected_mirror(self):
        args=self.registry_args()
        value=json.loads(args.registry_config_file.read_bytes())
        value['configs']['10.0.0.3:5443']['tls']['insecure_skip_verify']=True
        node.atomic_write(args.registry_config_file,node.encode(value))
        with patch.object(node.ssl,'create_default_context'):
            with self.assertRaisesRegex(node.SetupError,'Registry TLS'):node.registry_inputs(args)
        del value['configs']['10.0.0.3:5443']['tls']['insecure_skip_verify']
        value['mirrors']={'10.0.0.3:5443':{'endpoint':['http://10.0.0.3:5443']}}
        node.atomic_write(args.registry_config_file,node.encode(value))
        with patch.object(node.ssl,'create_default_context'):
            with self.assertRaisesRegex(node.SetupError,'HTTPS'):node.registry_inputs(args)

    def test_worker_rerun_uses_managed_credentials_after_transfer_files_removed(self):
        registry=self.registry_args()
        args=self.arguments('worker')
        args.registry_config_file=registry.registry_config_file
        args.registry_ca_file=registry.registry_ca_file
        with patch.object(node.ssl,'create_default_context'):
            node.apply(args)
            original=(node.CONFIG.read_bytes(),node.TOKEN.read_bytes(),node.REGISTRY_CONFIG.read_bytes(),node.REGISTRY_CA.read_bytes())
            args.token_file.unlink();args.registry_config_file.unlink();args.registry_ca_file.unlink()
            args.token_file=node.TOKEN
            args.registry_config_file=node.REGISTRY_CONFIG
            args.registry_ca_file=node.REGISTRY_CA
            self.runner.reset_mock()
            node.apply(args)
        self.assertEqual(original,(node.CONFIG.read_bytes(),node.TOKEN.read_bytes(),node.REGISTRY_CONFIG.read_bytes(),node.REGISTRY_CA.read_bytes()))
        self.runner.assert_called_once_with(['systemctl','enable','--now','k3s-agent'])

    def test_checksum_failure_prevents_binary_install(self):
        # Exercise the actual function saved before the fixture replaces it.
        def download(url,path,limit):
            Path(path).write_bytes((b'0'*64+b' k3s\n') if url.endswith('.txt') else b'tampered-binary')
        with patch.object(node,'download',side_effect=download),patch.object(node.tempfile,'TemporaryDirectory',return_value=tempfile.TemporaryDirectory(dir=self.directory)):
            with self.assertRaisesRegex(node.SetupError,'checksum mismatch'):
                ORIGINAL_INSTALL(self.arguments(),None)
        self.assertFalse(node.BIN.exists());self.runner.assert_not_called()

    def test_private_api_url_and_absolute_credentials(self):
        for value in ('http://10.0.0.1:6443','https://8.8.8.8:6443','https://10.0.0.1:6443/path','https://name:password@10.0.0.1:6443'):
            with self.assertRaises(Exception):node.server_url(value)
        for value in ('relative','/root/../repo/secret'):
            with self.assertRaises(Exception):node.absolute_path(value)
        with self.assertRaisesRegex(node.SetupError,'outside HTTPS'):
            node.HTTPSOnlyRedirect().redirect_request(None,None,302,'',{},'http://example.com')


ORIGINAL_INSTALL=node.install_binary_and_service


class IssuerTests(unittest.TestCase):
    def response(self,seconds=3600,subject='system:serviceaccount:agones-control:nakama-agones'):
        self.now=datetime.now(timezone.utc).replace(microsecond=0)
        expires=self.now+timedelta(seconds=seconds)
        payload=base64.urlsafe_b64encode(json.dumps({'sub':subject,'exp':int(expires.timestamp())}).encode()).decode().rstrip('=')
        return {'status':{'token':'unitTestHeader.'+payload+'.unitTestSignature','expirationTimestamp':expires.isoformat()}}

    def test_one_hour_token_and_identity_validated(self):
        response=self.response()
        token,expiry=issuer.validate_response(response,'agones-control','nakama-agones',self.now)
        self.assertEqual(token,response['status']['token']);self.assertTrue(expiry.endswith('Z'))

    def test_long_lived_expired_wrong_identity_and_metadata_rejected(self):
        for seconds in (-1,10,24*3600):
            response=self.response(seconds)
            with self.assertRaises(node.SetupError):issuer.validate_response(response,'agones-control','nakama-agones',self.now)
        response=self.response(subject='system:serviceaccount:other:admin')
        with self.assertRaisesRegex(node.SetupError,'unexpected'):issuer.validate_response(response,'agones-control','nakama-agones',self.now)
        response=self.response();response['status']['expirationTimestamp']=(self.now+timedelta(seconds=3700)).isoformat()
        with self.assertRaisesRegex(node.SetupError,'inconsistent'):issuer.validate_response(response,'agones-control','nakama-agones',self.now)


if __name__=='__main__':unittest.main()
