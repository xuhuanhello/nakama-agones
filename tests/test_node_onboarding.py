import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request
from unittest.mock import patch
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
import node_onboarding as n


class EnrollmentTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.root=Path(self.temp.name)
        self.region={'name':'us-west','cluster_id':'example','server':'https://10.4.0.4:6443',
            'private_cidrs':['10.4.0.0/22'],'protected_hosts':['8.8.8.8'],
            'join_token_file':str(self.root/'token'),'api_token_file':str(self.root/'api-token'),'api_ca_file':str(self.root/'ca')}
        self.b=n.Broker({'state_dir':str(self.root/'state'),'runtime_dir':str(self.root/'runtime'),
                         'node_script':str(self.root/'installer.py'),'regions':[self.region]})
        self.password='fixture-only-do-not-persist'
        self.scan={'scan_id':'a'*32,'host':'1.1.1.1','port':22,'region':'us-west','expires_at':int(time.time())+600,
                   'known_host':'1.1.1.1 ssh-ed25519 Zml4dHVyZQ==','fingerprints':[{'fingerprint':'SHA256:fixture','algorithm':'ssh-ed25519'}]}
        self.b.scans[self.scan['scan_id']]=dict(self.scan)
        self.info={'uid':0,'system':'Linux','arch':'x86_64','os':'debian','systemd':True,'python_version':[3,11],
                   'cpu_cores':2,'memory_mib':1900,'disk_free_gib':25,'swap':False,'existing_installation':False,
                   'addresses':[{'ip':'10.4.0.6','interface':'eth0'}],'api_reachable':True}

    def tearDown(self):self.temp.cleanup()

    def request(self):return {'scan_id':self.scan['scan_id'],'username':'root','password':self.password,'fingerprint':'SHA256:fixture'}

    def test_scan_rejects_protected_or_nonliteral_targets_before_network(self):
        with patch.object(n.subprocess,'run') as run:
            for host in ('localhost','127.0.0.1','169.254.169.254','8.8.8.8','10.4.0.4','1.1.1.1;whoami'):
                with self.assertRaises(n.Failure):self.b.scan({'region':'us-west','host':host})
            run.assert_not_called()

    def test_wrong_fingerprint_never_discloses_password_to_ssh(self):
        data=self.request();data['fingerprint']='SHA256:wrong'
        with patch.object(self.b,'ssh') as ssh:
            with self.assertRaises(n.Failure):self.b.preflight(data)
            ssh.assert_not_called()

    def test_existing_installation_is_read_only_and_password_forgotten(self):
        self.info['existing_installation']=True
        with patch.object(self.b,'ssh',return_value=self.info),patch.object(self.b,'node',return_value=None):
            result=self.b.preflight(self.request())
        self.assertFalse(result['can_join'])
        self.assertEqual(self.b.preflights,{})
        self.assertNotIn(self.password,json.dumps(result))
        self.assertFalse(any(self.b.state.iterdir()))

    def test_ambiguous_private_address_blocks_enrollment(self):
        self.info['addresses'].append({'ip':'10.4.0.7','interface':'eth1'})
        with patch.object(self.b,'ssh',return_value=self.info),patch.object(self.b,'node',return_value=None):
            result=self.b.preflight(self.request())
        self.assertFalse(result['can_join'])
        self.assertEqual(self.b.preflights,{})

    def successful_preflight(self):
        with patch.object(self.b,'ssh',return_value=self.info),patch.object(self.b,'node',return_value=None):
            return self.b.preflight(self.request())

    def test_join_confirmation_is_idempotent_and_state_has_no_credential(self):
        result=self.successful_preflight()
        with patch.object(n.threading,'Thread') as thread:
            a=self.b.join({'preflight_id':result['preflight_id']})
            b=self.b.join({'preflight_id':result['preflight_id']})
            self.assertEqual(a['id'],b['id']);self.assertEqual(thread.call_count,1)
        state=next(self.b.state.iterdir()).read_text()
        self.assertNotIn(self.password,state);self.assertNotIn('known_host',state)
        self.assertEqual(next(self.b.state.iterdir()).stat().st_mode&0o777,0o600)

    def test_expiry_discards_unconsumed_credentials(self):
        result=self.successful_preflight()
        self.b.preflights[result['preflight_id']]['expires_at']=0
        self.b.prune()
        self.assertEqual(self.b.preflights,{})

    def test_failed_probe_never_keeps_password_or_reuses_scan(self):
        with patch.object(self.b,'ssh',side_effect=n.Failure('ssh_operation_failed',502)):
            with self.assertRaises(n.Failure):self.b.preflight(self.request())
        self.assertEqual(self.b.scans,{})
        self.assertEqual(self.b.preflights,{})

    def test_host_drift_does_not_run_installer_and_clears_credential(self):
        result=self.successful_preflight()
        with patch.object(n.threading,'Thread'):
            job=self.b.join({'preflight_id':result['preflight_id']})
        item=self.b.preflights[result['preflight_id']]
        changed=dict(self.info,existing_installation=True)
        with patch.object(self.b,'node',return_value=None),patch.object(self.b,'ssh',return_value=changed) as ssh:
            self.b.install(result['preflight_id'],item,self.b.jobs[job['id']])
            self.assertEqual(ssh.call_count,1)
        self.assertNotIn('session',item)
        self.assertEqual(self.b.jobs[job['id']]['error'],'host_changed_repeat_preflight')
        self.assertNotIn(self.password,next(self.b.state.iterdir()).read_text())

    def test_restart_marks_interrupted_jobs_for_manual_verification(self):
        result=self.successful_preflight()
        with patch.object(n.threading,'Thread'):
            job=self.b.join({'preflight_id':result['preflight_id']})
        restored=n.Broker(self.b.config)
        self.assertEqual(restored.jobs[job['id']]['state'],'failed')
        self.assertEqual(restored.preflights,{})

    def test_ssh_password_is_not_argv_and_temp_files_removed_on_timeout(self):
        session=dict(self.scan,password=self.password)
        def inspect(args,**kwargs):
            self.assertNotIn(self.password,' '.join(args))
            self.assertNotIn(self.password,str(kwargs['env']))
            self.assertIn('StrictHostKeyChecking=yes',args)
            secret=Path(kwargs['env']['FLEET_SSH_PASSWORD_FILE'])
            self.assertEqual(secret.read_text(),self.password)
            self.assertEqual(secret.stat().st_mode&0o777,0o600)
            raise subprocess.TimeoutExpired('ssh',12)
        with patch.object(n.subprocess,'run',side_effect=inspect):
            with self.assertRaises(n.Failure):self.b.ssh(session,n.PROBE,{},12)
        self.assertEqual(list(self.b.runtime.iterdir()),[])

    def test_credentials_require_regular_private_file(self):
        p=self.root/'private';p.write_text('fixture');p.chmod(0o600)
        self.assertEqual(n.private_read(p),b'fixture')
        link=self.root/'alias';link.symlink_to(p)
        with self.assertRaises(n.Failure):n.private_read(link)
        p.chmod(0o644)
        with self.assertRaises(n.Failure):n.private_read(p)

    def test_cluster_token_cannot_follow_redirect(self):
        request=urllib.request.Request('https://10.4.0.4:6443/api/v1/nodes/game',headers={'Authorization':'Bearer fixture'})
        handler=n.NoRedirect()
        for target in ('https://attacker.invalid/','http://10.4.0.4:6443/','https://10.4.0.4:6443/other'):
            self.assertIsNone(handler.redirect_request(request,None,302,'Found',{},target))

    def test_second_process_cannot_recover_or_overwrite_running_jobs(self):
        first=n.lock_process(self.b.config)
        try:
            with self.assertRaises(n.Failure):n.lock_process(self.b.config)
        finally:first.close()
        again=n.lock_process(self.b.config)
        again.close()


if __name__=='__main__':unittest.main()
