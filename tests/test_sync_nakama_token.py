import argparse
import base64
from contextlib import redirect_stderr, redirect_stdout
from datetime import datetime, timedelta, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "sync_nakama_token.py"
SPEC = importlib.util.spec_from_file_location("sync_nakama_token", SCRIPT)
sync = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(sync)
NOW = datetime(2026, 9, 22, 12, 0, tzinfo=timezone.utc)
API = "https://kubernetes.example.internal:6443"
NAMESPACE = "agones-control"
SERVICE_ACCOUNT = "nakama-agones"


def jwt(claims):
    def encoded(value):
        return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")
    return encoded({"alg": "RS256", "typ": "JWT"}) + "." + encoded(claims) + ".test_signature_only"


def bundle(seconds=3600, **overrides):
    expires = NOW + timedelta(seconds=seconds)
    result = {"schema": 1, "api_url": API, "namespace": NAMESPACE, "service_account": SERVICE_ACCOUNT,
              "expires_at": sync.iso(expires),
              "token": jwt({"sub": "system:serviceaccount:" + NAMESPACE + ":" + SERVICE_ACCOUNT,
                            "exp": int(expires.timestamp()), "iat": int(NOW.timestamp()) - 1})}
    result.update(overrides)
    return result


def encoded(value):
    return json.dumps(value).encode()


def validate(value, **kwargs):
    return sync.validate_bundle(encoded(value), API, NAMESPACE, SERVICE_ACCOUNT, NOW, **kwargs)


class BundleTests(unittest.TestCase):
    def test_valid_issuer_bundle(self):
        self.assertEqual(validate(bundle()), bundle())

    def test_supported_origin_canonicalization(self):
        self.assertEqual(validate(bundle(api_url=API.upper().replace("HTTPS", "https") + "/"))["api_url"], API)

    def test_expired_and_soon_expiring_tokens(self):
        for seconds in (-1, 0, 899):
            with self.subTest(seconds=seconds), self.assertRaisesRegex(sync.SyncError, "token_too_close_to_expiry"):
                validate(bundle(seconds))
        self.assertEqual(validate(bundle(900))["expires_at"], sync.iso(NOW + timedelta(seconds=900)))

    def test_reject_long_lived_token(self):
        with self.assertRaisesRegex(sync.SyncError, "token_lifetime_too_long"):
            validate(bundle(7201))

    def test_reject_identity_mismatch(self):
        for changed in ({"api_url": "https://other.example.internal:6443"}, {"namespace": "other"},
                        {"service_account": "other"}):
            with self.subTest(changed=changed), self.assertRaisesRegex(sync.SyncError, "bundle_identity_mismatch"):
                validate(bundle(**changed))

    def test_reject_schema_and_unknown_fields(self):
        for value in (bundle(schema=True), bundle(schema="1"), bundle(schema=2), bundle(extra="untrusted")):
            with self.subTest(schema=value.get("schema")), self.assertRaisesRegex(sync.SyncError, "invalid_schema"):
                validate(value)

    def test_reject_missing_field(self):
        value = bundle()
        value.pop("token")
        with self.assertRaisesRegex(sync.SyncError, "invalid_schema"):
            validate(value)

    def test_reject_duplicate_json_fields(self):
        raw = encoded(bundle()).replace(b'"schema": 1', b'"schema": 1, "schema": 1')
        with self.assertRaisesRegex(sync.SyncError, "invalid_json"):
            sync.validate_bundle(raw, API, NAMESPACE, SERVICE_ACCOUNT, NOW)

    def test_reject_malformed_and_oversized_json(self):
        for raw in (b"", b"[1]", b"not-json", b"\xff", b'{"schema": NaN}', b" " * (sync.MAX_BUNDLE_BYTES + 1)):
            with self.subTest(size=len(raw)), self.assertRaises(sync.SyncError):
                sync.validate_bundle(raw, API, NAMESPACE, SERVICE_ACCOUNT, NOW)

    def test_reject_unsafe_api_origins(self):
        for url in ("http://kubernetes.example.internal:6443", "https://user:pass@host.internal",
                    API + "/api", API + "?token=secret", API + "#secret", "https://-option", "https://host:0",
                    "https://host:99999", "https://host\n", "https://host:abc", "https://host/%20"):
            with self.subTest(url=url), self.assertRaisesRegex(sync.SyncError, "invalid_api_url"):
                validate(bundle(api_url=url))

    def test_reject_invalid_expiration(self):
        for expires in ("tomorrow", "2026-09-22T13:00:00", 123, "9" * 100):
            with self.subTest(expires=expires), self.assertRaisesRegex(sync.SyncError, "invalid_expiration"):
                validate(bundle(expires_at=expires))

    def test_reject_wrong_jwt_identity(self):
        token = jwt({"sub": "system:serviceaccount:other:admin", "exp": int(NOW.timestamp()) + 3600})
        with self.assertRaisesRegex(sync.SyncError, "token_identity_mismatch"):
            validate(bundle(token=token))

    def test_reject_jwt_expiration_inconsistency(self):
        for exp in (int(NOW.timestamp()) + 3500, True, str(int(NOW.timestamp()) + 3600)):
            token = jwt({"sub": "system:serviceaccount:" + NAMESPACE + ":" + SERVICE_ACCOUNT, "exp": exp})
            with self.subTest(exp=exp), self.assertRaisesRegex(sync.SyncError, "token_expiration_mismatch"):
                validate(bundle(token=token))

    def test_reject_future_jwt_claims(self):
        for field in ("nbf", "iat"):
            for value in (int(NOW.timestamp()) + 31, "soon", True):
                claims = {"sub": "system:serviceaccount:" + NAMESPACE + ":" + SERVICE_ACCOUNT,
                          "exp": int(NOW.timestamp()) + 3600, field: value}
                with self.subTest(field=field, value=value), self.assertRaisesRegex(sync.SyncError, "token_not_yet_valid"):
                    validate(bundle(token=jwt(claims)))

    def test_reject_invalid_jwt_syntax_and_payload(self):
        for token in ("secret", "a.b.c\n", "a.b.c;command", "a.b.c", "x" * 33000, None):
            with self.subTest(length=len(token) if token else 0), self.assertRaisesRegex(sync.SyncError, "invalid_token"):
                validate(bundle(token=token))


class LocalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.output = self.root / "credentials"
        self.output.mkdir(mode=0o700)
        self.key = self.root / "token-pull.key"
        self.key.write_text("test key fixture; never used for network\n")
        self.key.chmod(0o600)
        self.known = self.root / "known_hosts"
        self.known.write_text("manager.example.internal ssh-ed25519 TEST_ONLY\n")
        self.known.chmod(0o644)
        self.args = argparse.Namespace(ssh_host="manager.example.internal", ssh_user="root", ssh_port=22,
                                      ssh_key_file=self.key, known_hosts=self.known, expected_api_url=API,
                                      namespace=NAMESPACE, service_account=SERVICE_ACCOUNT,
                                      minimum_remaining_seconds=900, output_dir=self.output)

    def install(self, value=None, now=NOW):
        with sync.CredentialStore(self.output) as store, store.locked():
            return store.install(validate(value or bundle()), now)

    def before(self):
        return tuple((self.output / name).read_bytes() for name in (sync.TOKEN_FILE, sync.METADATA_FILE))

    def assertNoTemps(self):
        self.assertEqual([], list(self.output.glob(".token-sync-*")))

    def test_install_permissions_and_metadata(self):
        previous_umask = os.umask(0)
        try:
            status = self.install()
        finally:
            os.umask(previous_umask)
        self.assertEqual("valid", status["state"])
        token, raw_metadata = self.before()
        metadata = json.loads(raw_metadata)
        self.assertEqual(bundle()["token"] + "\n", token.decode())
        self.assertNotIn("token", metadata)
        self.assertNotIn(bundle()["token"], raw_metadata.decode())
        self.assertEqual(hashlib.sha256(token).hexdigest(), metadata["token_sha256"])
        for path in self.output.iterdir():
            self.assertEqual(0o600, stat.S_IMODE(path.stat().st_mode))
        self.assertNoTemps()

    def test_successful_rotation_is_visible_through_same_directory(self):
        self.install()
        old = self.before()
        token_inode = (self.output / sync.TOKEN_FILE).stat().st_ino
        self.install(bundle(3900))
        self.assertNotEqual(old[0], self.before()[0])
        self.assertNotEqual(token_inode, (self.output / sync.TOKEN_FILE).stat().st_ino)
        self.assertEqual(3900, sync.current_status(self.output, NOW)["remaining_seconds"])
        self.assertNoTemps()

    def test_reject_expiry_regression(self):
        self.install(bundle(4000))
        old = self.before()
        with self.assertRaisesRegex(sync.SyncError, "token_expiration_regressed"):
            self.install(bundle(3600))
        self.assertEqual(old, self.before())

    def test_rejected_bundle_preserves_old_token_and_metadata(self):
        self.install()
        old = self.before()
        for value in (bundle(-1), bundle(namespace="other"), bundle(api_url="https://other.internal:6443")):
            with self.subTest(identity=value["namespace"]), patch.object(sync, "fetch_bundle", return_value=encoded(value)):
                with self.assertRaises(sync.SyncError):
                    sync.synchronize(self.args, NOW)
            self.assertEqual(old, self.before())
        self.assertNoTemps()

    def test_transport_failure_preserves_old_files(self):
        self.install()
        old = self.before()
        with patch.object(sync, "fetch_bundle", side_effect=sync.SyncError("ssh_failed")):
            with self.assertRaisesRegex(sync.SyncError, "ssh_failed"):
                sync.synchronize(self.args, NOW)
        self.assertEqual(old, self.before())
        self.assertNoTemps()

    def test_failed_final_rename_rolls_back_metadata(self):
        self.install()
        old = self.before()
        replace = sync.CredentialStore.replace

        def fail_token(store, staged, target):
            if target == sync.TOKEN_FILE:
                raise OSError("synthetic failure with sensitive text")
            replace(store, staged, target)

        with patch.object(sync.CredentialStore, "replace", fail_token):
            with self.assertRaisesRegex(sync.SyncError, "token_replace_failed"):
                self.install(bundle(4000))
        self.assertEqual(old, self.before())
        self.assertEqual("valid", sync.current_status(self.output, NOW)["state"])
        self.assertNoTemps()

    def test_failed_first_install_restores_missing_pair(self):
        replace = sync.CredentialStore.replace

        def fail_token(store, staged, target):
            if target == sync.TOKEN_FILE:
                raise OSError("write error")
            replace(store, staged, target)

        with patch.object(sync.CredentialStore, "replace", fail_token):
            with self.assertRaisesRegex(sync.SyncError, "token_replace_failed"):
                self.install()
        self.assertEqual("missing", sync.current_status(self.output, NOW)["state"])
        self.assertNoTemps()

    def test_staging_failure_preserves_old_files(self):
        self.install()
        old = self.before()
        with patch.object(sync.os, "fsync", side_effect=OSError("disk full")):
            with self.assertRaises(OSError):
                self.install(bundle(4000))
        self.assertEqual(old, self.before())
        self.assertNoTemps()

    def test_status_detects_crash_between_metadata_and_token_rename(self):
        self.install()
        path = self.output / sync.METADATA_FILE
        metadata = json.loads(path.read_text())
        metadata["expires_at"] = sync.iso(NOW + timedelta(hours=2))
        metadata["token_sha256"] = "0" * 64
        path.write_text(json.dumps(metadata))
        self.assertEqual({"state": "metadata_mismatch"}, sync.current_status(self.output, NOW))
        self.install(bundle(3900))
        self.assertEqual("valid", sync.current_status(self.output, NOW)["state"])

    def test_status_states_without_token_disclosure(self):
        self.assertEqual({"state": "missing"}, sync.current_status(self.output, NOW))
        self.install()
        self.assertEqual("valid", sync.current_status(self.output, NOW)["state"])
        self.assertEqual("expired", sync.current_status(self.output, NOW + timedelta(hours=2))["state"])
        self.assertNotIn(bundle()["token"], json.dumps(sync.current_status(self.output, NOW)))
        (self.output / sync.METADATA_FILE).unlink()
        self.assertEqual({"state": "incomplete"}, sync.current_status(self.output, NOW))

    def test_symlink_directory_rejected(self):
        alias = self.root / "alias"
        alias.symlink_to(self.output, target_is_directory=True)
        with self.assertRaisesRegex(sync.SyncError, "credential_directory_unavailable"):
            with sync.CredentialStore(alias):
                self.fail("opened symlink")

    def test_unsafe_directory_permissions_rejected(self):
        self.output.chmod(0o755)
        with self.assertRaisesRegex(sync.SyncError, "unsafe_credential_directory"):
            self.install()

    def test_symlink_token_never_followed_or_replaced(self):
        outside = self.root / "outside"
        outside.write_text("untouched")
        outside.chmod(0o600)
        (self.output / sync.TOKEN_FILE).symlink_to(outside)
        with self.assertRaisesRegex(sync.SyncError, "credential_file_unavailable"):
            self.install()
        self.assertEqual("untouched", outside.read_text())
        self.assertTrue((self.output / sync.TOKEN_FILE).is_symlink())

    def test_world_readable_existing_token_rejected(self):
        self.install()
        (self.output / sync.TOKEN_FILE).chmod(0o644)
        with self.assertRaisesRegex(sync.SyncError, "unsafe_credential_file"):
            self.install(bundle(4000))

    def test_concurrent_sync_refused(self):
        with sync.CredentialStore(self.output) as first, first.locked():
            with sync.CredentialStore(self.output) as second:
                with self.assertRaisesRegex(sync.SyncError, "sync_busy"):
                    with second.locked():
                        self.fail("acquired a held lock")

    def test_exact_ssh_argv_disables_ambient_auth_and_shell(self):
        argv = sync.ssh_command(self.args)
        self.assertEqual(["manager.example.internal", sync.REMOTE_REQUEST], argv[-2:])
        self.assertEqual(["/usr/bin/ssh", "-F", "/dev/null", "-T"], argv[:4])
        for option in ("BatchMode=yes", "StrictHostKeyChecking=yes", "IdentityAgent=none", "IdentitiesOnly=yes",
                       "ConnectTimeout=10", "ControlPath=none", "ProxyCommand=none", "ClearAllForwardings=yes"):
            self.assertIn(option, argv)
        self.assertIn("UserKnownHostsFile=" + str(self.known), argv)

    def test_reject_ssh_argument_injection(self):
        for host in ("-oProxyCommand=evil", "root@host", "host;command", "host\ncommand", "host/path", "::1%evil"):
            self.args.ssh_host = host
            with self.subTest(host=host), self.assertRaisesRegex(sync.SyncError, "invalid_ssh_host"):
                sync.ssh_command(self.args)
        self.args.ssh_host = "manager.example.internal"
        for user in ("-root", "root;command", "root@host", "root\n"):
            self.args.ssh_user = user
            with self.subTest(user=user), self.assertRaisesRegex(sync.SyncError, "invalid_ssh_user"):
                sync.ssh_command(self.args)

    def test_reject_ssh_key_and_known_hosts_unsafe_modes(self):
        self.key.chmod(0o644)
        with self.assertRaisesRegex(sync.SyncError, "unsafe_ssh_file"):
            sync.ssh_command(self.args)
        self.key.chmod(0o600)
        self.known.chmod(0o666)
        with self.assertRaisesRegex(sync.SyncError, "unsafe_ssh_file"):
            sync.ssh_command(self.args)

    def test_reject_ssh_file_symlink_and_expansion(self):
        alias = self.root / "key-alias"
        alias.symlink_to(self.key)
        self.args.ssh_key_file = alias
        with self.assertRaisesRegex(sync.SyncError, "unsafe_ssh_file"):
            sync.ssh_command(self.args)
        self.args.ssh_key_file = self.key
        self.args.known_hosts = Path("/opt/%h/known_hosts")
        with self.assertRaisesRegex(sync.SyncError, "invalid_ssh_path"):
            sync.ssh_command(self.args)

    def local_child(self, source):
        original = subprocess.Popen

        def factory(argv, **kwargs):
            self.assertEqual(sync.REMOTE_REQUEST, argv[-1])
            self.assertFalse(kwargs["shell"])
            self.assertEqual(subprocess.DEVNULL, kwargs["stderr"])
            self.assertEqual({"PATH": "/usr/bin:/bin", "LC_ALL": "C"}, kwargs["env"])
            return original([sys.executable, "-c", source], **kwargs)
        return patch.object(sync.subprocess, "Popen", factory)

    def test_fetch_reads_only_bounded_stdout(self):
        raw = encoded(bundle())
        with self.local_child("import sys; sys.stdout.buffer.write(" + repr(raw) + ")"):
            self.assertEqual(raw, sync.fetch_bundle(self.args))

    def test_fetch_rejects_oversized_output(self):
        with self.local_child("import sys; sys.stdout.buffer.write(b'x' * 70000)"):
            with self.assertRaisesRegex(sync.SyncError, "bundle_size"):
                sync.fetch_bundle(self.args)

    def test_fetch_kills_timed_out_child(self):
        with self.local_child("import time; time.sleep(5)"):
            with self.assertRaisesRegex(sync.SyncError, "ssh_timeout"):
                sync.fetch_bundle(self.args, timeout=0.05)

    def test_fetch_rejects_nonzero_status_and_discards_stderr(self):
        with self.local_child("import sys; sys.stderr.write('SENSITIVE_REMOTE_ERROR'); sys.exit(7)"):
            with self.assertRaisesRegex(sync.SyncError, "^ssh_failed$"):
                sync.fetch_bundle(self.args)

    def test_cli_errors_never_echo_remote_payload_or_exception(self):
        self.install()
        argv = ["--output-dir", str(self.output), "--ssh-host", self.args.ssh_host, "--expected-api-url", API]
        for failure in (OSError("SENSITIVE_OS_ERROR"), None):
            stdout, stderr = io.StringIO(), io.StringIO()
            with redirect_stdout(stdout), redirect_stderr(stderr), patch.object(sync, "utc_now", return_value=NOW):
                with patch.object(sync, "fetch_bundle", side_effect=failure, return_value=b"SENSITIVE_REMOTE_PAYLOAD"):
                    self.assertEqual(1, sync.main(argv))
            output = stdout.getvalue() + stderr.getvalue()
            self.assertNotIn("SENSITIVE", output)
            self.assertNotIn(bundle()["token"], output)
            self.assertEqual("valid", json.loads(stderr.getvalue())["credential"]["state"])

    def test_status_cli_does_not_fetch_or_require_ssh_configuration(self):
        self.install()
        stdout = io.StringIO()
        with redirect_stdout(stdout), patch.object(sync, "utc_now", return_value=NOW), patch.object(sync, "fetch_bundle") as fetch:
            self.assertEqual(0, sync.main(["--status", "--output-dir", str(self.output)]))
            fetch.assert_not_called()
        self.assertNotIn(bundle()["token"], stdout.getvalue())
        self.assertEqual(3600, json.loads(stdout.getvalue())["credential"]["remaining_seconds"])

    def test_arbitrary_remote_command_option_is_rejected_without_echo(self):
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            self.assertEqual(1, sync.main(["--remote-command", "SENSITIVE_SHELL_PAYLOAD"]))
        self.assertEqual({"ok": False, "code": "invalid_arguments"}, json.loads(stderr.getvalue()))


if __name__ == "__main__":
    unittest.main()
