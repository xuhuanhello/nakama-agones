"""Migration audit must reject unsafe retirement and never disclose identities."""
from contextlib import redirect_stdout, redirect_stderr
import copy
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'scripts'))
import audit_cutover as audit


class CutoverAuditTests(unittest.TestCase):
    def state(self):
        return {'workers':{'private-worker':{'state':'stopped','provider_id':'private-provider-id','metrics':{},'player_count':0}},
                'allocations':{'private-allocation':{'state':'completed','user_ids':['private-user']}},'commands':{}}

    def config(self):
        return {'nakama_database_addresses':['user:private-password@unchanged-postgres:5432/nakama?sslmode=disable'],
            'fleet_database_url':'postgres://fleet:private-password@unchanged-postgres:5432/fleet?sslmode=disable',
            'control_url':'https://api.example.com','nakama_server_key':'private-server-key',
            'session_encryption_key':'private-session-key','refresh_encryption_key':'private-refresh-key'}

    def test_stopped_history_is_retired_but_does_not_claim_provider_verification(self):
        result=audit.inspect_state(self.state(),1000)
        self.assertTrue(result['retirement_state_complete'])
        self.assertFalse(result['provider_inventory_verified'])
        self.assertFalse(result['old_controller_shutdown_verified'])
        self.assertNotIn('private-',json.dumps(result))

    def test_unknown_lost_or_running_worker_blocks_cutover(self):
        for phase in ('unknown','lost','ready','stopping','unexpected-private-content'):
            snapshot=self.state();snapshot['workers']['private-worker']['state']=phase
            result=audit.inspect_state(snapshot,1000)
            self.assertFalse(result['retirement_state_complete'])
            self.assertNotIn('unexpected-private-content',json.dumps(result))

    def test_failed_worker_with_provider_id_is_not_proven_retired(self):
        snapshot=self.state();snapshot['workers']['private-worker']['state']='failed'
        self.assertFalse(audit.inspect_state(snapshot,1000)['retirement_state_complete'])
        snapshot['workers']['private-worker']['provider_id']=''
        self.assertTrue(audit.inspect_state(snapshot,1000)['retirement_state_complete'])

    def test_nonterminal_allocation_and_actionable_command_block(self):
        snapshot=self.state();snapshot['allocations']['private-allocation']['state']='active'
        self.assertFalse(audit.inspect_state(snapshot,1000)['retirement_state_complete'])
        snapshot=self.state();snapshot['commands']['private-command']={'done':False,'expires_at':1001}
        self.assertFalse(audit.inspect_state(snapshot,1000)['retirement_state_complete'])
        self.assertTrue(audit.inspect_state(snapshot,1002)['retirement_state_complete'])

    def test_configuration_compares_dsn_and_identity_exactly_without_normalization(self):
        before=self.config();after=copy.deepcopy(before)
        self.assertTrue(all(audit.compare_config(before,after).values()))
        for name in before:
            after=copy.deepcopy(before)
            if isinstance(after[name],list):after[name][0]+=' '
            else:after[name]+=' '
            result=audit.compare_config(before,after)
            self.assertFalse(result[name]);self.assertNotIn('private-',json.dumps(result))

    def test_incomplete_or_malformed_state_does_not_succeed(self):
        for snapshot in ({},[],{'workers':{},'allocations':{},'commands':[]}, {'workers':{'x':None},'allocations':{},'commands':{}}):
            with self.assertRaises(audit.AuditError):audit.inspect_state(snapshot,1000)
        with self.assertRaises(audit.AuditError):audit.compare_config({},self.config())

    def test_cli_private_files_and_secret_free_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            for name,value in (('state',self.state()),('before',self.config()),('after',self.config())):
                (root/name).write_text(json.dumps(value));(root/name).chmod(0o600)
            output=io.StringIO();error=io.StringIO()
            argv=['audit','--state-file',str(root/'state'),'--before-config',str(root/'before'),'--after-config',str(root/'after')]
            with patch.object(sys,'argv',argv),redirect_stdout(output),redirect_stderr(error):
                self.assertEqual(audit.main(),0)
            self.assertNotIn('private-',output.getvalue()+error.getvalue())
            (root/'before').chmod(0o644)
            with patch.object(sys,'argv',argv),redirect_stdout(output),redirect_stderr(error):
                self.assertEqual(audit.main(),1)
            self.assertNotIn('private-',output.getvalue()+error.getvalue())


if __name__=='__main__':unittest.main()
