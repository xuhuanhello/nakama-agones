#!/usr/bin/env python3
"""Regional NodeEnrollment controller. Kubernetes API only; no inbound service."""
from __future__ import annotations

import argparse
import base64
import copy
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import signal
import ssl
import stat
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

import importlib.util
_core_spec = importlib.util.spec_from_file_location('enrollment_ssh_core', Path(__file__).resolve().with_name('node_onboarding.py'))
core = importlib.util.module_from_spec(_core_spec)
_core_spec.loader.exec_module(core)

API_VERSION = 'infrastructure.nakama-agones.io/v1alpha1'
GROUP_PATH = '/apis/' + API_VERSION
ID = re.compile(r'^[a-f0-9]{32}$')
DNS = re.compile(r'^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$')
CODE = re.compile(r'^[a-z0-9_]{1,96}$')
TERMINAL = {'Ready', 'Failed', 'NeedsReview', 'Expired'}
RETIRE_TERMINAL = {'Deleted', 'Blocked', 'NeedsReview'}
TTL = 600
MAX_OBJECT = 1 << 20
LEASE_NAME = 'fleet-node-enrollment'
Failure = core.Failure


def stamp():
    return datetime.now(timezone.utc).isoformat(timespec='seconds').replace('+00:00', 'Z')


def seconds(value):
    try:
        return datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()
    except (AttributeError, ValueError, TypeError):
        raise Failure('invalid_timestamp', 400) from None


def strict_json(raw):
    def pairs(items):
        out = {}
        for k, v in items:
            if k in out:
                raise ValueError()
            out[k] = v
        return out
    def bad(_):
        raise ValueError()
    try:
        value = json.loads(raw, object_pairs_hook=pairs, parse_constant=bad)
        if not isinstance(value, dict):
            raise ValueError()
        return value
    except (ValueError, UnicodeError, RecursionError):
        raise Failure('invalid_api_object', 502) from None


def bounded_file(path, maximum=65536):
    # Kubernetes projected files intentionally use symlinks for atomic rotation.
    # Paths come only from the root-owned deployment/config, never from a CR.
    try:
        with open(path, 'rb') as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o022:
                raise Failure('unsafe_projected_file', 503)
            raw = stream.read(maximum + 1)
        if not raw or len(raw) > maximum:
            raise Failure('invalid_projected_file', 503)
        return raw
    except OSError:
        raise Failure('projected_file_unavailable', 503) from None


def load_config(path):
    value = strict_json(bounded_file(path))
    required = ('region', 'requests_namespace', 'system_namespace', 'game_namespace',
                'api_url', 'api_ca_file', 'api_token_file', 'runtime_dir', 'bootstrap_dir',
                'node_script', 'secrets_encryption_verified', 'retirement_enabled', 'retirement_system_namespaces')
    core.object_fields(value, required, required)
    if value['secrets_encryption_verified'] is not True:
        raise Failure('secrets_encryption_verification_required', 503)
    if type(value['retirement_enabled']) is not bool:
        raise Failure('invalid_controller_configuration', 503)
    for key in ('requests_namespace', 'system_namespace', 'game_namespace'):
        if not isinstance(value[key], str) or not DNS.fullmatch(value[key]):
            raise Failure('invalid_controller_configuration', 503)
    if len({value[k] for k in ('requests_namespace', 'system_namespace', 'game_namespace')}) != 3:
        raise Failure('separate_namespaces_required', 503)
    allowed = value['retirement_system_namespaces']
    if (not isinstance(allowed, list) or not 1 <= len(allowed) <= 8
            or any(not isinstance(v, str) or not DNS.fullmatch(v) for v in allowed)
            or value['game_namespace'] in allowed or value['requests_namespace'] in allowed):
        raise Failure('invalid_system_namespace_allowlist', 503)
    region = value['region']
    fields = ('name', 'cluster_id', 'server', 'private_cidrs', 'protected_hosts', 'game_port_min', 'game_port_max')
    core.object_fields(region, fields, fields)
    if not all(isinstance(region[k], str) and DNS.fullmatch(region[k]) for k in ('name', 'cluster_id')):
        raise Failure('invalid_controller_configuration', 503)
    server = urllib.parse.urlsplit(region['server'])
    try:
        private_ip = ipaddress.IPv4Address(server.hostname or '')
        if (server.scheme != 'https' or server.port != 6443 or not private_ip.is_private
                or server.path not in ('', '/') or server.username or server.query or server.fragment):
            raise ValueError()
        if not isinstance(region['private_cidrs'], list) or not 1 <= len(region['private_cidrs']) <= 8:
            raise ValueError()
        for item in region['private_cidrs']:
            network = ipaddress.IPv4Network(item, strict=True)
            if not network.is_private or network.prefixlen < 16:
                raise ValueError()
        if not isinstance(region['protected_hosts'], list) or not 1 <= len(region['protected_hosts']) <= 32:
            raise ValueError()
        for host in region['protected_hosts']:
            core.literal_ip(host)
        for key in ('game_port_min', 'game_port_max'):
            core.bounded_int(region[key], 1024, 65535)
        if region['game_port_min'] > region['game_port_max']:
            raise ValueError()
        api = urllib.parse.urlsplit(value['api_url'])
        if (api.scheme != 'https' or not api.hostname or api.username or api.path not in ('', '/')
                or api.query or api.fragment):
            raise ValueError()
        for key in ('api_ca_file', 'api_token_file', 'runtime_dir', 'bootstrap_dir', 'node_script'):
            p = Path(value[key])
            if not p.is_absolute() or '..' in p.parts:
                raise ValueError()
    except (ValueError, TypeError):
        raise Failure('invalid_controller_configuration', 503) from None
    return value


class API:
    def __init__(self, config):
        self.url = config['api_url'].rstrip('/')
        self.token_file = config['api_token_file']
        context = ssl.create_default_context(cadata=bounded_file(config['api_ca_file']).decode())
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPSHandler(context=context), core.NoRedirect())

    def open(self, method, path, value=None, content_type='application/json', timeout=12):
        if not path.startswith(('/api/v1/', '/apis/')) or any(c in path for c in '\r\n'):
            raise Failure('invalid_api_path', 500)
        token = bounded_file(self.token_file, 16384).decode('ascii').strip()
        if not token or any(c.isspace() for c in token):
            raise Failure('invalid_api_credential', 503)
        raw = None if value is None else json.dumps(value, allow_nan=False, separators=(',', ':')).encode()
        request = urllib.request.Request(self.url + path, data=raw, method=method,
            headers={'Authorization': 'Bearer ' + token, 'Content-Type': content_type, 'Accept': 'application/json'})
        try:
            return self.opener.open(request, timeout=timeout)
        except urllib.error.HTTPError as error:
            # API bodies can include Secret contents; never propagate them.
            error.close()
            raise Failure('kubernetes_api_rejected', error.code) from None
        except (OSError, ValueError):
            raise Failure('kubernetes_api_unavailable', 503) from None

    def request(self, method, path, value=None, content_type='application/json'):
        with self.open(method, path, value, content_type) as response:
            raw = response.read(MAX_OBJECT + 1)
            if len(raw) > MAX_OBJECT:
                raise Failure('kubernetes_response_too_large', 502)
            return strict_json(raw)

    def get(self, path):
        try:
            return self.request('GET', path)
        except Failure as error:
            if error.status == 404:
                return None
            raise

    def listing(self, path, query=None):
        values, token = [], ''
        for _ in range(100):
            args = dict(query or {}, limit=100)
            if token:
                args['continue'] = token
            response = self.request('GET', path + '?' + urllib.parse.urlencode(args))
            items = response.get('items')
            if not isinstance(items, list):
                raise Failure('invalid_kubernetes_list', 502)
            values.extend(items)
            token = response.get('metadata', {}).get('continue', '')
            if not token:
                return values, response.get('metadata', {}).get('resourceVersion', '')
        raise Failure('kubernetes_list_limit', 502)

    def watch(self, path, resource_version):
        query = urllib.parse.urlencode({'watch': 'true', 'timeoutSeconds': 10,
                                        'resourceVersion': resource_version, 'allowWatchBookmarks': 'true'})
        with self.open('GET', path + '?' + query, timeout=15) as response:
            for _ in range(1000):
                raw = response.readline(MAX_OBJECT + 1)
                if not raw:
                    return
                if len(raw) > MAX_OBJECT:
                    raise Failure('kubernetes_response_too_large', 502)
                yield strict_json(raw)
        # The next list/watch cycle reconciles events beyond this bound.

    def status(self, path, obj, status):
        return self.request('PUT', path + '/' + obj['metadata']['name'] + '/status', {
            'apiVersion': obj['apiVersion'], 'kind': obj['kind'],
            'metadata': {k: obj['metadata'][k] for k in ('name', 'namespace', 'resourceVersion', 'uid')},
            'status': status})

    def delete(self, path, uid, version=None):
        conditions = {'uid': uid}
        if version is not None:
            conditions['resourceVersion'] = version
        try:
            return self.request('DELETE', path, {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': conditions})
        except Failure as error:
            if error.status == 404:
                return None
            raise


class Lease:
    """One pre-created Lease; no create or arbitrary coordination permissions."""
    def __init__(self, api, namespace):
        self.api, self.identity = api, secrets.token_hex(16)
        self.path = '/apis/coordination.k8s.io/v1/namespaces/' + namespace + '/leases/' + LEASE_NAME
        self.alive = threading.Event()

    def renew(self):
        obj = self.api.get(self.path)
        if obj is None:
            raise Failure('controller_lease_missing', 503)
        spec = obj.get('spec', {})
        holder = spec.get('holderIdentity', '')
        renewed = seconds(spec['renewTime']) if spec.get('renewTime') else 0
        if holder and holder != self.identity and renewed + 30 > time.time():
            raise Failure('another_controller_is_running', 409)
        obj['spec'] = {'holderIdentity': self.identity, 'leaseDurationSeconds': 30,
                       'renewTime': stamp(), 'leaseTransitions': spec.get('leaseTransitions', 0) + (holder != self.identity)}
        self.api.request('PUT', self.path, obj)
        self.alive.set()

    def maintain(self):
        while self.alive.wait(0) and not STOP.wait(8):
            try:
                self.renew()
            except Exception:
                self.alive.clear()
                return


class SSHCore(core.Broker):
    """Reuse only the reviewed fixed SSH installer; no root/HTTP/socket entrypoint."""
    def __init__(self, config, api, callback=None):
        self.api, self.callback = api, callback
        self.fence = lambda: True
        region = dict(config['region'])
        runtime = Path(config['runtime_dir'])
        runtime.mkdir(mode=0o700, parents=True, exist_ok=True)
        bootstrap = Path(config['bootstrap_dir'])
        if (bootstrap / 'registry.json').exists() != (bootstrap / 'registry-ca.crt').exists():
            raise Failure('registry_credentials_pair_required', 503)
        for leaf, source in (('join.token', 'agent.token'), ('registry.json', 'registry.json'), ('registry-ca.crt', 'registry-ca.crt')):
            path = Path(config['bootstrap_dir']) / source
            if source != 'agent.token' and not path.exists():
                (runtime / leaf).unlink(missing_ok=True)
                continue
            raw = bounded_file(path)
            if source == 'agent.token' and not re.fullmatch(rb'K10[a-f0-9]{64}::node:[A-Za-z0-9._~-]{16,512}\s*', raw):
                raise Failure('agent_only_join_token_required', 503)
            destination = runtime / leaf
            temporary = runtime / ('.credential-' + secrets.token_hex(8))
            fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, 'wb') as stream:
                stream.write(raw)
            os.replace(temporary, destination)
        region['join_token_file'] = str(runtime / 'join.token')
        if (runtime / 'registry.json').exists():
            region['registry_config_file'] = str(runtime / 'registry.json')
            region['registry_ca_file'] = str(runtime / 'registry-ca.crt')
        super().__init__({'regions': [region], 'runtime_dir': str(runtime / 'ssh'),
                          'state_dir': str(runtime / 'local'), 'node_script': config['node_script']})

    def scan(self, data):
        if not self.fence():
            raise Failure('controller_lease_lost', 503)
        return super().scan(data)

    def ssh(self, session, script, payload, timeout):
        if not self.fence():
            raise Failure('controller_lease_lost', 503)
        return super().ssh(session, script, payload, timeout)

    def node(self, region, name):
        if not self.fence():
            raise Failure('controller_lease_lost', 503)
        if not DNS.fullmatch(name):
            raise Failure('invalid_node_name')
        return self.api.get('/api/v1/nodes/' + name)

    def save_job(self, job, **updates):
        job.update(updates, updated_at=int(time.time()))
        if self.callback:
            self.callback(copy.deepcopy(job))


class Controller:
    def __init__(self, config, api, own_node, engine_factory=SSHCore, fence=lambda: True):
        if not own_node or not DNS.fullmatch(own_node):
            raise Failure('controller_node_identity_required', 503)
        self.config, self.api, self.own_node, self.engine_factory = config, api, own_node, engine_factory
        self.fence = fence
        self.namespace, self.region = config['requests_namespace'], config['region']
        self.enrollments = GROUP_PATH + '/namespaces/' + self.namespace + '/nodeenrollments'
        self.retirements = GROUP_PATH + '/namespaces/' + self.namespace + '/noderetirements'
        self.secret_path = '/api/v1/namespaces/' + self.namespace + '/secrets'
        self.active = set()
        self.lock = threading.RLock()

    def require_lease(self):
        if not self.fence():
            raise Failure('controller_lease_lost', 503)

    def engine(self, callback=None):
        self.require_lease()
        value = self.engine_factory(self.config, self.api, callback)
        value.fence = self.fence
        return value

    def identity(self, obj, prefix, kind):
        metadata = obj.get('metadata', {})
        name = metadata.get('name', '')
        if (obj.get('apiVersion') != API_VERSION or obj.get('kind') != kind
                or metadata.get('namespace') != self.namespace or not name.startswith(prefix)
                or not ID.fullmatch(name[len(prefix):]) or not metadata.get('uid') or not metadata.get('resourceVersion')):
            raise Failure('invalid_task_identity')
        return name[len(prefix):]

    def write(self, path, obj, **changes):
        self.require_lease()
        status = copy.deepcopy(obj.get('status', {}))
        status.update(changes, observedGeneration=obj['metadata'].get('generation', 1), updatedAt=stamp())
        return self.api.status(path, obj, status)

    def secret(self, obj, now=None):
        task_id = self.identity(obj, 'enroll-', 'NodeEnrollment')
        ref = obj['spec'].get('credentialSecretRef', {})
        if set(ref) != {'name', 'uid'} or ref['name'] != 'ssh-' + task_id or not isinstance(ref['uid'], str):
            raise Failure('invalid_credential_reference')
        value = self.api.get(self.secret_path + '/' + ref['name'])
        if value is None:
            raise Failure('credential_expired_or_unavailable', 409)
        metadata = value.get('metadata', {})
        owners = metadata.get('ownerReferences', [])
        expected = {'apiVersion': API_VERSION, 'kind': 'NodeEnrollment', 'name': obj['metadata']['name'], 'uid': obj['metadata']['uid']}
        if (metadata.get('uid') != ref['uid'] or metadata.get('namespace') != self.namespace
                or metadata.get('labels', {}).get('nakama-agones.io/enrollment-id') != task_id
                or len(owners) != 1 or any(owners[0].get(k) != v for k, v in expected.items())
                or owners[0].get('blockOwnerDeletion', False) or owners[0].get('controller', False)
                or value.get('type') != 'Opaque' or value.get('immutable') is not True
                or set(value.get('data', {})) != {'password'}):
            raise Failure('credential_identity_mismatch', 409)
        now = time.time() if now is None else now
        created = seconds(metadata.get('creationTimestamp'))
        if created > now + 5 or created + TTL <= now:
            raise Failure('credential_expired_or_unavailable', 409)
        try:
            password = base64.b64decode(value['data']['password'], validate=True).decode('utf-8')
            if not 1 <= len(password.encode()) <= 1024 or any(c in password for c in '\r\n\x00'):
                raise ValueError()
        except (ValueError, UnicodeError):
            raise Failure('invalid_ssh_password') from None
        return password, int(created + TTL)

    def clean_secret(self, obj):
        self.require_lease()
        # Delete only the exact Secret bound to this CR UID, never a name alone.
        ref = obj.get('spec', {}).get('credentialSecretRef', {})
        task_id = self.identity(obj, 'enroll-', 'NodeEnrollment')
        name = 'ssh-' + task_id
        value = self.api.get(self.secret_path + '/' + name)
        if value is None:
            return
        metadata = value.get('metadata', {})
        owners = metadata.get('ownerReferences', [])
        if (metadata.get('labels', {}).get('nakama-agones.io/enrollment-id') != task_id or len(owners) != 1
                or owners[0].get('uid') != obj['metadata']['uid'] or owners[0].get('name') != obj['metadata']['name']
                or owners[0].get('kind') != 'NodeEnrollment' or owners[0].get('apiVersion') != API_VERSION
                or (ref.get('uid') and ref['uid'] != metadata.get('uid'))):
            return
        self.require_lease()
        self.api.delete(self.secret_path + '/' + name, metadata['uid'])

    def fail(self, obj, error, phase='Failed'):
        code = error.code if isinstance(error, Failure) and CODE.fullmatch(error.code) else 'controller_operation_failed'
        task_id = self.identity(obj, 'enroll-', 'NodeEnrollment')
        now = int(time.time())
        job = dict(obj.get('status', {}).get('job', {}), id=task_id, host=obj['spec']['host'], region=self.region['name'],
                   state={'NeedsReview': 'needs_review', 'Expired': 'expired'}.get(phase, 'failed'),
                   stage='manual_verification_required' if phase == 'NeedsReview' else code, error=code, updated_at=now)
        job.setdefault('created_at', now)
        result = self.write(self.enrollments, obj, phase=phase, error=code, job=job)
        self.clean_secret(result)
        return result

    def validate_enrollment(self, obj):
        task_id = self.identity(obj, 'enroll-', 'NodeEnrollment')
        spec = obj.get('spec', {})
        core.object_fields(spec, ('region', 'host', 'port', 'action', 'hostFingerprint', 'credentialSecretRef', 'approvedPreflightDigest'),
                           ('region', 'host', 'port', 'action'))
        if spec['region'] != self.region['name'] or spec['action'] not in ('Scan', 'Preflight', 'Join'):
            raise Failure('invalid_region_or_action')
        host = core.literal_ip(spec['host'])
        if not ipaddress.IPv4Address(host).is_global or host in self.region['protected_hosts'] or host == urllib.parse.urlsplit(self.region['server']).hostname:
            raise Failure('protected_or_invalid_target')
        core.bounded_int(spec['port'], 1, 65535)
        return task_id

    def reconcile(self, obj, startup=False):
        self.require_lease()
        task_id = self.identity(obj, 'enroll-', 'NodeEnrollment')
        phase = obj.get('status', {}).get('phase', '')
        if obj['metadata'].get('deletionTimestamp') or phase in TERMINAL:
            self.clean_secret(obj)
            return
        try:
            self.validate_enrollment(obj)
        except Failure as error:
            self.fail(obj, error)
            return
        with self.lock:
            if self.active:
                return
        if phase in ('Installing', 'Verifying'):
            self.fail(obj, Failure('controller_interrupted_check_node'), 'NeedsReview')
            return
        if startup and phase == 'Preflighting':
            self.fail(obj, Failure('preflight_interrupted_repeat_scan'))
            return
        spec = obj['spec']
        try:
            if spec['action'] == 'Scan' and phase not in ('AwaitingPreflight',):
                obj = self.write(self.enrollments, obj, phase='Scanning')
                engine = self.engine()
                view = engine.scan({'region': spec['region'], 'host': spec['host'], 'port': spec['port']})
                scan = engine.scans[view['scan_id']]
                view['scan_id'] = task_id
                self.write(self.enrollments, obj, phase='AwaitingPreflight', scan=view, sshHostKey=scan['known_host'], error='')
            elif spec['action'] == 'Preflight' and phase == 'AwaitingPreflight':
                self.preflight(obj, task_id)
            elif spec['action'] == 'Join' and phase == 'AwaitingApproval':
                self.join(obj, task_id)
            elif phase == 'AwaitingApproval' and obj['status']['preflight']['expires_at'] < time.time():
                self.fail(obj, Failure('preflight_expired'), 'Expired')
            elif phase == 'AwaitingPreflight' and obj['status']['scan']['expires_at'] < time.time():
                self.fail(obj, Failure('scan_expired'), 'Expired')
            elif spec['action'] not in {'AwaitingPreflight': ('Scan',), 'AwaitingApproval': ('Preflight',)}.get(phase, ()):
                self.fail(obj, Failure('invalid_action_transition'))
        except Failure as error:
            if error.status == 409 and error.code == 'kubernetes_api_rejected':
                return # Concurrent spec/status update; next watch reads current state.
            fresh = self.api.get(self.enrollments + '/' + obj['metadata']['name'])
            if fresh and fresh['metadata']['uid'] == obj['metadata']['uid']:
                self.fail(fresh, error)

    def preflight(self, obj, task_id):
        status, spec = obj['status'], obj['spec']
        if status['scan']['expires_at'] < time.time():
            raise Failure('scan_expired', 409)
        password, expires = self.secret(obj)
        scan = dict(status['scan'], known_host=status['sshHostKey'], scan_id=task_id)
        obj = self.write(self.enrollments, obj, phase='Preflighting')
        engine = self.engine()
        engine.scans[task_id] = scan
        try:
            view = engine.preflight({'scan_id': task_id, 'username': 'root', 'password': password,
                                     'fingerprint': spec.get('hostFingerprint')})
        finally:
            password = ''
            engine.preflights.clear()
        view['preflight_id'], view['expires_at'] = task_id, min(view['expires_at'], expires)
        digest = hashlib.sha256(json.dumps(view, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()
        obj = self.write(self.enrollments, obj, phase='AwaitingApproval' if view['can_join'] else 'Failed',
                         preflight=view, preflightDigest=digest, error='' if view['can_join'] else 'preflight_checks_failed')
        if not view['can_join']:
            self.clean_secret(obj)

    def join(self, obj, task_id):
        status = obj['status']
        if (not status['preflight'].get('can_join') or obj['spec'].get('approvedPreflightDigest') != status.get('preflightDigest')):
            raise Failure('preflight_approval_mismatch', 409)
        if status['preflight']['expires_at'] <= time.time():
            raise Failure('preflight_expired', 409)
        password, _ = self.secret(obj)
        with self.lock:
            if self.active:
                # Leave approval pending; never start concurrent machine installs.
                return
            job = {'id': task_id, 'host': obj['spec']['host'], 'region': self.region['name'],
                   'node_name': status['preflight']['node_name'], 'state': 'installing',
                   'stage': 'revalidating_host', 'created_at': int(time.time()), 'updated_at': int(time.time())}
            obj = self.write(self.enrollments, obj, phase='Installing', job=job)
            # A crash beyond this durable boundary needs inspection, not replay.
            self.active.add(task_id)
        try:
            self.clean_secret(obj)
            self.require_lease()
            thread = threading.Thread(target=self.install, args=(obj, task_id, password), daemon=True)
            thread.start()
        except Exception:
            with self.lock:
                self.active.discard(task_id)
            raise

    def install(self, obj, task_id, password):
        name, uid = obj['metadata']['name'], obj['metadata']['uid']
        def update(job):
            current = self.api.get(self.enrollments + '/' + name)
            if (not current or current['metadata']['uid'] != uid
                    or current.get('status', {}).get('phase') not in ('Installing', 'Verifying')):
                raise Failure('execution_identity_changed', 409)
            phase = {'installing': 'Installing', 'verifying': 'Verifying', 'ready': 'Ready', 'failed': 'NeedsReview'}[job['state']]
            if phase == 'NeedsReview':
                job['state'] = 'needs_review'
            self.write(self.enrollments, current, phase=phase, job=job, error=job.get('error', ''))
        try:
            engine = self.engine(update)
            item = {'view': obj['status']['preflight'], 'session': dict(obj['status']['scan'],
                    known_host=obj['status']['sshHostKey'], password=password)}
            password = ''
            engine.install(task_id, item, dict(obj['status']['job']))
        except Exception:
            try:
                current = self.api.get(self.enrollments + '/' + name)
                if current and current['metadata']['uid'] == uid:
                    self.fail(current, Failure('execution_interrupted_check_node'), 'NeedsReview')
            except Exception:
                pass # The durable Installing status remains, requiring review.
        finally:
            password = ''
            with self.lock:
                self.active.discard(task_id)

    def cleanup(self):
        self.require_lease()
        objects, _ = self.api.listing(self.enrollments)
        owners = {o['metadata']['uid']: o for o in objects}
        values, _ = self.api.listing(self.secret_path, {'labelSelector': 'nakama-agones.io/enrollment-id'})
        for value in values:
            meta = value.get('metadata', {})
            task_id = meta.get('labels', {}).get('nakama-agones.io/enrollment-id', '')
            if not ID.fullmatch(task_id) or meta.get('name') != 'ssh-' + task_id:
                continue
            refs = meta.get('ownerReferences', [])
            if len(refs) != 1 or refs[0].get('apiVersion') != API_VERSION or refs[0].get('kind') != 'NodeEnrollment' or refs[0].get('name') != 'enroll-' + task_id:
                continue
            owner = owners.get(refs[0].get('uid'))
            expired = seconds(meta.get('creationTimestamp')) + TTL <= time.time()
            if owner is None or expired or owner.get('status', {}).get('phase') in TERMINAL:
                self.require_lease()
                self.api.delete(self.secret_path + '/' + meta['name'], meta['uid'])
        for obj in objects:
            if obj.get('status', {}).get('phase') in TERMINAL and seconds(obj['status']['updatedAt']) + 7 * 86400 <= time.time():
                self.require_lease()
                self.api.delete(self.enrollments + '/' + obj['metadata']['name'], obj['metadata']['uid'])

        retirements, _ = self.api.listing(self.retirements)
        for obj in retirements:
            if obj.get('status', {}).get('phase') in RETIRE_TERMINAL and seconds(obj['status']['updatedAt']) + 7 * 86400 <= time.time():
                self.require_lease()
                self.api.delete(self.retirements + '/' + obj['metadata']['name'], obj['metadata']['uid'])

    def retirement_node(self, spec):
        node = self.api.get('/api/v1/nodes/' + spec['nodeName'])
        if node is None:
            raise Failure('node_missing_needs_review', 409)
        meta, state = node.get('metadata', {}), node.get('status', {})
        labels = meta.get('labels', {})
        if (meta.get('uid') != spec['nodeUID'] or meta.get('deletionTimestamp')
                or spec['nodeName'] == self.own_node or any(k in labels for k in ('node-role.kubernetes.io/control-plane', 'node-role.kubernetes.io/master'))
                or labels.get('nakama-agones.io/game-node') != 'true' or labels.get('nakama-agones.io/role') != 'game'
                or labels.get('nakama-agones.io/cluster') != self.region['cluster_id']):
            raise Failure('node_identity_or_ownership_mismatch', 409)
        ready = [v.get('status') for v in state.get('conditions', []) if v.get('type') == 'Ready']
        if len(ready) != 1 or ready[0] not in ('False', 'Unknown'):
            raise Failure('node_must_be_offline', 409)
        for kind, key in (('InternalIP', 'internalIPs'), ('ExternalIP', 'externalIPs')):
            actual = sorted({a.get('address') for a in state.get('addresses', []) if a.get('type') == kind})
            if actual != sorted(spec[key]):
                raise Failure('node_address_changed', 409)
        return node

    def retirement_resources(self, node_name):
        blocked, allowed = [], []
        pods, _ = self.api.listing('/api/v1/pods', {'fieldSelector': 'spec.nodeName=' + node_name})
        for pod in pods:
            meta = pod.get('metadata', {})
            ns, name = meta.get('namespace', ''), meta.get('name', '')
            summary = {'kind': 'Pod', 'namespace': ns, 'name': name, 'reason': 'remaining_workload'}
            refs = [o for o in meta.get('ownerReferences', []) if o.get('controller') is True]
            system = False
            if ns != self.config['game_namespace'] and ns in self.config['retirement_system_namespaces'] and len(refs) == 1:
                owner = refs[0]
                if owner.get('apiVersion') == 'apps/v1' and owner.get('kind') == 'DaemonSet' and DNS.fullmatch(owner.get('name', '')):
                    daemon = self.api.get('/apis/apps/v1/namespaces/' + ns + '/daemonsets/' + owner['name'])
                    system = daemon is not None and daemon.get('metadata', {}).get('uid') == owner.get('uid')
            if system:
                summary['reason'] = 'system_daemonset_no_force_cleanup'
                allowed.append(summary)
            else:
                blocked.append(summary)
        games, _ = self.api.listing('/apis/agones.dev/v1/namespaces/' + self.config['game_namespace'] + '/gameservers')
        for game in games:
            if game.get('status', {}).get('nodeName') == node_name:
                blocked.append({'kind': 'GameServer', 'namespace': self.config['game_namespace'],
                    'name': game.get('metadata', {}).get('name', ''), 'reason': 'remaining_gameserver'})
        return blocked, allowed

    def retire(self, obj, startup=False):
        self.require_lease()
        task_id = self.identity(obj, 'retire-', 'NodeRetirement')
        if obj.get('status', {}).get('phase') in RETIRE_TERMINAL:
            return
        spec = obj.get('spec', {})
        if startup and obj.get('status', {}).get('phase') == 'Checking':
            job = dict(obj.get('status', {}).get('job', {}), state='needs_review', stage='controller_interrupted_verify_node_metadata', error='controller_interrupted_verify_node_metadata')
            self.write(self.retirements, obj, phase='NeedsReview', job=job, error='controller_interrupted_verify_node_metadata')
            return
        now = int(time.time())
        deletion_attempted = False
        job = {'id': task_id, 'node_name': spec.get('nodeName', ''), 'node_uid': spec.get('nodeUID', ''),
               'region': self.region['name'], 'state': 'checking', 'stage': 'validating_offline_node',
               'created_at': obj.get('status', {}).get('job', {}).get('created_at', now), 'updated_at': now}
        try:
            required = ('region', 'nodeName', 'nodeUID', 'internalIPs', 'externalIPs', 'confirmedNodeName', 'confirmedIP', 'permanentRetirement')
            core.object_fields(spec, required, required)
            if (not self.config['retirement_enabled'] or spec['region'] != self.region['name']
                    or not DNS.fullmatch(spec['nodeName']) or not isinstance(spec['nodeUID'], str) or not spec['nodeUID']
                    or spec['confirmedNodeName'] != spec['nodeName'] or spec['permanentRetirement'] is not True):
                raise Failure('retirement_not_authorized', 403)
            for key in ('internalIPs', 'externalIPs'):
                if not isinstance(spec[key], list) or len(spec[key]) > 8 or len(set(spec[key])) != len(spec[key]):
                    raise Failure('invalid_node_addresses')
                for address in spec[key]:
                    if str(ipaddress.ip_address(address)) != address:
                        raise Failure('invalid_node_addresses')
            if spec['confirmedIP'] not in spec['internalIPs'] + spec['externalIPs']:
                raise Failure('node_confirmation_mismatch')
            # A deletion request must never become an implicit uninstall/force operation.
            obj = self.write(self.retirements, obj, phase='Checking', job=job)
            self.retirement_node(spec)
            blocked, allowed = self.retirement_resources(spec['nodeName'])
            if blocked:
                job.update(state='blocked', stage='node_has_remaining_workloads', error='node_has_remaining_workloads', updated_at=int(time.time()))
                self.write(self.retirements, obj, phase='Blocked', job=job, error='node_has_remaining_workloads',
                           blockers=blocked[:200], allowedSystemPods=allowed[:200], blockersTruncated=len(blocked) > 200)
                return
            # Offline metadata cleanup only. No cordon/patch, eviction or Pod DELETE.
            # Recheck identity after inventory; DELETE rejects a changed node revision.
            node = self.retirement_node(spec)
            self.require_lease()
            deletion_attempted = True
            self.api.delete('/api/v1/nodes/' + spec['nodeName'], node['metadata']['uid'], node['metadata']['resourceVersion'])
            job.update(state='deleted', stage='node_metadata_removed_vps_unchanged', updated_at=int(time.time()))
            self.write(self.retirements, obj, phase='Deleted', job=job, error='', blockers=[], allowedSystemPods=allowed[:200])
        except (Failure, ValueError, TypeError, KeyError) as error:
            code = error.code if isinstance(error, Failure) and CODE.fullmatch(error.code) else 'invalid_retirement_request'
            if code == 'kubernetes_api_rejected' and error.status == 409 and not deletion_attempted:
                return
            if code == 'kubernetes_api_rejected' and error.status == 409:
                code = 'node_changed_reconfirm_retirement'
            fresh = self.api.get(self.retirements + '/' + obj['metadata']['name'])
            if fresh and fresh['metadata']['uid'] == obj['metadata']['uid'] and fresh.get('status', {}).get('phase') not in RETIRE_TERMINAL:
                # A timeout/deletion ambiguity is never turned into another delete.
                review = deletion_attempted or code in ('kubernetes_api_unavailable', 'node_missing_needs_review')
                job.update(state='needs_review' if review else 'blocked', stage=code, error=code, updated_at=int(time.time()))
                self.write(self.retirements, fresh, phase='NeedsReview' if review else 'Blocked', job=job, error=code)


STOP = threading.Event()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True)
    args = parser.parse_args()
    if os.geteuid() == 0:
        raise Failure('nonroot_controller_required', 503)
    config = load_config(args.config)
    api = API(config)
    lease = Lease(api, config['system_namespace'])
    lease.renew()
    threading.Thread(target=lease.maintain, daemon=True).start()
    controller = Controller(config, api, os.environ.get('CONTROLLER_NODE_NAME', ''), fence=lease.alive.is_set)
    controller.engine() # Validate fixed projected bootstrap files and initialize private tmpfs.
    controller.cleanup()
    (Path(config['runtime_dir']) / 'healthy').write_text(str(int(time.time())))
    first = True
    signal.signal(signal.SIGTERM, lambda *_: STOP.set())
    signal.signal(signal.SIGINT, lambda *_: STOP.set())
    while not STOP.is_set() and lease.alive.is_set():
        try:
            for path, handler in ((controller.enrollments, controller.reconcile), (controller.retirements, controller.retire)):
                values, version = api.listing(path)
                for obj in values:
                    controller.require_lease()
                    if STOP.is_set():
                        break
                    try:
                        handler(obj, startup=first)
                    except Failure as error:
                        if error.code == 'controller_lease_lost':
                            raise
                        # A malformed/contended task must not starve other tasks.
                for event in api.watch(path, version):
                    if not lease.alive.is_set() or STOP.is_set():
                        break
                    if event.get('type') in ('ADDED', 'MODIFIED'):
                        try:
                            handler(event['object'])
                        except Failure as error:
                            if error.code == 'controller_lease_lost':
                                raise
            first = False
            controller.cleanup()
            (Path(config['runtime_dir']) / 'healthy').write_text(str(int(time.time())))
        except Exception:
            # Do not log API/SSH exceptions: their payloads may contain credentials.
            STOP.wait(2)
    if not lease.alive.is_set():
        raise Failure('controller_lease_lost', 503)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        code = error.code if isinstance(error, Failure) and CODE.fullmatch(error.code) else 'controller_start_failed'
        raise SystemExit('node-enrollment: ' + code)
