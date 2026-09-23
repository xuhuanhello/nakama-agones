"""Real check producers must fit the persisted NodeEnrollment status schema."""
import json
from pathlib import Path
import re
import unittest
from unittest.mock import patch

import test_node_enrollment_controller as fixtures


class StatusSchemaTests(unittest.TestCase):
    def setUp(self):
        docs = [json.loads(raw) for raw in (Path(__file__).resolve().parents[1] /
                'deploy/enrollment/10-crds.yaml').read_text().split('\n---\n')]
        enrollment = next(v for v in docs if v['spec']['names']['kind'] == 'NodeEnrollment')
        self.properties = enrollment['spec']['versions'][0]['schema']['openAPIV3Schema']['properties']
        self.status = self.properties['status']['properties']
        self.fixture = fixtures.ControllerTests('test_complete_flow_reuses_fixed_preflight_install_and_consumes_secret_before_install')
        self.fixture.setUp()
        self.addCleanup(self.fixture.tearDown)

    def assert_names(self, checks, schema):
        self.assertTrue(checks)
        rule = schema['properties']['checks']['items']['properties']['name']
        for check in checks:
            value = check['name']
            self.assertGreaterEqual(len(value), rule['minLength'])
            self.assertLessEqual(len(value), rule['maxLength'])
            self.assertRegex(value, rule['pattern'], value)

    def test_actual_preflight_and_install_producers_fit_status(self):
        f = self.fixture
        f.prepared()  # Calls the actual SSHCore/Broker preflight; only transport is synthetic.
        preflight = f.current()['status']['preflight']
        self.assertTrue(preflight['can_join'])
        self.assertIn('root_and_systemd', {v['name'] for v in preflight['checks']})
        self.assert_names(preflight['checks'], self.status['preflight'])
        f.update(action='Join', approvedPreflightDigest=f.current()['status']['preflightDigest'])
        with patch.object(fixtures.c.threading, 'Thread', fixtures.InlineThread):
            f.controller.reconcile(f.current())
        result = f.current()['status']
        self.assertEqual(result['phase'], 'Ready')
        self.assertIn('kubernetes_ready', {v['name'] for v in result['job']['checks']})
        self.assert_names(result['job']['checks'], self.status['job'])

    def test_existing_node_refusal_checks_fit_status_and_secret_is_cleaned(self):
        f = self.fixture
        original_factory = f.controller.engine_factory
        def existing_installation_factory(config, api, callback=None):
            engine = original_factory(config, api, callback)
            transport = engine.ssh
            def probe_existing(session, script, payload, timeout):
                result = transport(session, script, payload, timeout)
                if script == fixtures.c.core.PROBE:
                    result['existing_installation'] = True
                return result
            engine.ssh = probe_existing
            return engine
        f.controller.engine_factory = existing_installation_factory
        f.controller.reconcile(f.current())
        secret = f.credential()
        f.api.put('/api/v1/nodes/game-1-1-1-1', f.node(ready='True'))
        f.update(action='Preflight', hostFingerprint=f.fp,
                 credentialSecretRef={'name': secret['metadata']['name'], 'uid': secret['metadata']['uid']})
        f.controller.reconcile(f.current())
        status = f.current()['status']
        self.assertEqual(status['phase'], 'Failed')
        self.assertEqual(status['error'], 'preflight_checks_failed')
        self.assertFalse(status['preflight']['can_join'])
        self.assertTrue(status['preflight']['existing_installation'])
        self.assertFalse(next(v for v in status['preflight']['checks'] if v['name'] == 'fresh_node')['ok'])
        self.assert_names(status['preflight']['checks'], self.status['preflight'])
        self.assertIsNone(f.api.get(f.controller.secret_path + '/ssh-' + f.task_id))

    def test_check_names_remain_bounded_and_resource_names_keep_dns_rules(self):
        for field in ('preflight', 'job'):
            rule = self.status[field]['properties']['checks']['items']['properties']['name']
            for invalid in ('', '../secret', 'check name', 'password=secret', 'x\ny', 'a' * 64):
                self.assertFalse(len(invalid) <= rule['maxLength'] and re.fullmatch(rule['pattern'], invalid))
        for rule in (self.properties['spec']['properties']['region'],
                     self.status['preflight']['properties']['node_name']):
            self.assertIsNone(re.fullmatch(rule['pattern'], 'unsafe_node_name'))
            self.assertIsNotNone(re.fullmatch(rule['pattern'], 'safe-node-name'))


if __name__ == '__main__':
    unittest.main()
