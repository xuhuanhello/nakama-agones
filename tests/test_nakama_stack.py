from __future__ import annotations

import contextlib
import io
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from scripts import nakama_stack as stack


DIGEST = "sha256:" + "a" * 64
RUNTIME = "sha256:" + "b" * 64


class NakamaStackTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name) / "nakama"
        with contextlib.redirect_stdout(io.StringIO()):
            with patch.object(stack.os, "geteuid", return_value=0):
                self.assertEqual(stack.main(["init", "--directory", str(self.root)]), 0)
        self.env = self.root / ".env"
        self.service_env = self.root / "private" / "gamefleet-service.env"
        self.service_key = self.root / "private" / "gamefleet-service-key"
        self.tls_key = self.root / "private" / "gamefleet-client.key"
        self.tls_cert = self.root / "private" / "gamefleet-client.crt"
        for path in (self.service_key, self.tls_key, self.tls_cert):
            path.chmod(0o600)
        ca=self.root / "private" / "gamefleet-ca.crt"
        ca.chmod(0o600);ca.write_text("synthetic CA");ca.chmod(0o400)
        self.service_key.write_text("gfsvc-test-key\n", encoding="utf-8")
        self.tls_key.write_text("synthetic ssh key material\n", encoding="utf-8")
        self.tls_cert.write_text("gamefleet-admin.example.com ssh-ed25519 synthetic-public-key\n", encoding="utf-8")
        for path in (self.service_key, self.tls_key, self.tls_cert):
            path.chmod(0o400)
        self._update_env({
            "ACME_EMAIL": "ops@acme-check.io",
            "NAKAMA_DOMAIN": "pool.example.net",
            "NAKAMA_RUNTIME_IMAGE": RUNTIME,
            "POSTGRES_IMAGE": f"docker.io/library/postgres:17.6-bookworm@{DIGEST}",
            "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}",
            "ALPINE_IMAGE": f"docker.io/library/alpine:3.23.5@{DIGEST}",
            "GAMEFLEET_SSH_TARGET": "tunnel@gamefleet-admin.example.net",
            "APPLICATION_REQUIRED_MODULES": "account.so",
        })
        self._update_service_env({
            "NAKAMA_FLEET_BACKEND": "gamefleet-service",
            "GAMEFLEET_SERVICE_URL": "https://business.example.net:17682",
            "GAMEFLEET_SERVICE_KEY_FILE": "/run/secrets/gamefleet-service-key",
            "GAMEFLEET_SERVICE_ID": "service-prod-1",
            "GAMEFLEET_SERVICE_APPLICATION_ID": "pool-app",
            "GAMEFLEET_SERVICE_IDENTITY_ISSUER": "nakama-prod",
            "GAMEFLEET_SERVICE_REGION": "us-west",
            "GAMEFLEET_SERVICE_COMPATIBILITY": "fixed-v7",
            "GAMEFLEET_ARCHIVE_URL": "",
            "GAMEFLEET_ARCHIVE_KEY_FILE": "",
            "GAMEFLEET_ARCHIVE_APPLICATION_ID": "",
            "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER": "",
            "GAMEFLEET_ARCHIVE_REGION": "",
            "GAMEFLEET_ARCHIVE_COMPATIBILITY": "",
            "GAMEFLEET_ARCHIVE_SERVICE_ID": "",
        })

    def tearDown(self):
        self.tmp.cleanup()

    def _update_env(self, changes):
        values = stack._read_env(self.env) if self.env.exists() else {}
        values.update(changes)
        self.env.write_text("\n".join(f"{key}={value}" for key, value in values.items()) + "\n", encoding="utf-8")
        self.env.chmod(0o600)

    def _update_service_env(self, changes):
        values = stack._read_env(self.service_env) if self.service_env.exists() else {}
        values.update(changes)
        self.service_env.write_text("\n".join(f"{key}={value}" for key, value in values.items()) + "\n", encoding="utf-8")
        self.service_env.chmod(0o600)

    def validate(self, **kwargs):
        original = Path.lstat
        compose_config = kwargs.pop("compose_config", False)
        owner_uid = kwargs.pop("synthetic_tunnel_uid", 0)

        def synthetic_root_owned(path, *args, **kwargs):
            info = original(path, *args, **kwargs)
            root_paths = (self.tls_key.resolve(), self.tls_cert.resolve())
            if path.resolve() in root_paths:
                fields = {name: getattr(info, name) for name in dir(info) if name.startswith("st_")}
                fields["st_uid"] = owner_uid
                return SimpleNamespace(**fields)
            return info

        with patch.object(Path, "lstat", synthetic_root_owned):
            return stack.validate_project(self.root, compose_config=compose_config, **kwargs)

    def test_route_derives_hostname_and_preserves_unrelated_settings(self):
        before = stack._read_env(self.env)
        with patch.object(stack.os, "geteuid", return_value=0), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(stack.main(["route", "--directory", str(self.root), "--ip", "10.4.0.4"]), 0)
        after = stack._read_env(self.env)
        self.assertEqual(after["GAMEFLEET_SERVICE_HOST"], "business.example.net")
        self.assertEqual(after["GAMEFLEET_SERVICE_HOST_IP"], "10.4.0.4")
        for key, value in before.items():
            if key not in ("GAMEFLEET_SERVICE_HOST", "GAMEFLEET_SERVICE_HOST_IP"):
                self.assertEqual(after[key], value)
        self.validate(check_image=False, compose_config=True)
        with patch.object(stack.os, "geteuid", return_value=0), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(stack.main(["route", "--directory", str(self.root), "--clear"]), 0)
        self.validate(check_image=False)
        self.assertEqual(stack._read_env(self.env)["GAMEFLEET_SERVICE_HOST"], "")

    def test_route_rejects_ambiguous_hosts_and_unusable_ips(self):
        for value in ("127.0.0.1", "0.0.0.0", "224.0.0.1", "169.254.1.1", "::1", "fe80::1%en0", "host.example.net", "10.4.0.4,other=1.2.3.4"):
            with self.subTest(ip=value), self.assertRaises(stack.ConfigError):
                stack._route_ip(value)
        for value in ("http://business.test", "https://127.0.0.1", "https://a..test", "https://-bad.test", "https://a.test/path"):
            with self.subTest(url=value), self.assertRaises(stack.ConfigError):
                stack._route_host(value)
        self._update_env({"GAMEFLEET_SERVICE_HOST": "other.test", "GAMEFLEET_SERVICE_HOST_IP": "10.4.0.4"})
        with self.assertRaisesRegex(stack.ConfigError, "exactly match"):
            self.validate(check_image=False)
        self._update_env({"GAMEFLEET_SERVICE_HOST": "", "GAMEFLEET_SERVICE_HOST_IP": "10.4.0.4"})
        with self.assertRaisesRegex(stack.ConfigError, "exactly match"):
            self.validate(check_image=False)

    def test_init_creates_secret_free_template_and_private_random_secrets(self):
        output = io.StringIO()
        error = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error), patch.object(stack.os, "geteuid", return_value=0):
            self.assertEqual(stack.main(["init", "--directory", str(self.root)]), 2)
        self.assertIn("never overwrites", error.getvalue())

        for name in stack.GENERATED_SECRETS:
            path = self.root / "private" / name
            self.assertEqual(path.stat().st_mode & 0o777, 0o400)
            value = path.read_text(encoding="ascii").strip()
            self.assertGreaterEqual(len(value), 32)
            self.assertNotIn(value, self.env.read_text(encoding="utf-8"))
            self.assertNotIn(value, (self.root / "compose.yaml").read_text(encoding="utf-8"))
        self.assertEqual((self.root / "private").stat().st_mode & 0o777, 0o700)

    def test_seven_service_fields_and_file_metadata_are_required_without_reading_key(self):
        valid = self.validate(check_image=False)
        self.assertEqual(valid["_APPLICATION_REQUIRED_MODULES"], "account.so")

        original = Path.read_text

        def guarded(path, *args, **kwargs):
            if path == self.service_key or path == self.tls_key:
                raise AssertionError("private key content must not be read")
            return original(path, *args, **kwargs)

        with patch.object(Path, "read_text", guarded):
            self.validate(check_image=False)

        self._update_service_env({"GAMEFLEET_SERVICE_REGION": ""})
        with self.assertRaisesRegex(stack.ConfigError, "all seven"):
            self.validate(check_image=False)

        self._update_service_env({"GAMEFLEET_SERVICE_REGION": "us-west"})
        self.service_key.chmod(0o644)
        with self.assertRaisesRegex(stack.ConfigError, "0400 or 0600"):
            self.validate(check_image=False)

    def test_acme_email_is_required_validated_and_never_echoed(self):
        self.assertEqual(stack._read_env(stack.ENV_TEMPLATE)["ACME_EMAIL"], "replace_me@example.invalid")
        self.validate(check_image=False)

        def set_email(value):
            lines = self.env.read_text(encoding="utf-8").splitlines()
            replaced = [f"ACME_EMAIL={value}" if line.startswith("ACME_EMAIL=") else line for line in lines]
            self.env.write_text("\n".join(replaced) + "\n", encoding="utf-8")
            self.env.chmod(0o600)

        for value in ("", "replace_me@example.invalid", "bad address", "user@host", "a..b@acme-check.io",
                      "user@-bad.example", "user@example.net", "user@acme-check.test"):
            with self.subTest(value=value):
                set_email(value)
                with self.assertRaises(stack.ConfigError) as caught:
                    self.validate(check_image=False)
                if value:
                    self.assertNotIn(value, str(caught.exception))

        contact = "ops@acme-check.io"
        set_email(contact)
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr), \
             patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value={"_APPLICATION_REQUIRED_MODULES": ""}):
            self.assertEqual(stack.main(["validate", "--directory", str(self.root), "--skip-image-check"]), 0)
        self.assertNotIn(contact, stdout.getvalue())
        self.assertNotIn(contact, stderr.getvalue())

    def test_loopback_network_ssh_and_digest_inputs_are_strict(self):
        for bad_url in ("http://gamefleet.example.net:17682", "http://127.0.0.1:17682",
                        "http://127.0.0.1:17683", "http://127.0.0.1:17682/path"):
            with self.subTest(url=bad_url):
                self._update_service_env({"GAMEFLEET_SERVICE_URL": bad_url})
                with self.assertRaisesRegex(stack.ConfigError, "HTTPS business origin"):
                    self.validate(check_image=False)
        self._update_service_env({"GAMEFLEET_SERVICE_URL": "https://business.example.net:17682"})

        self._update_env({"POSTGRES_IMAGE": "postgres:latest"})
        with self.assertRaisesRegex(stack.ConfigError, "immutable OCI sha256 digest"):
            self.validate(check_image=False)
        self._update_env({"POSTGRES_IMAGE": f"docker.io/library/postgres:17.6@{DIGEST}",
                          "DATABASE_SUBNET": "172.29.240.128/25"})
        with self.assertRaisesRegex(stack.ConfigError, "must not overlap"):
            self.validate(check_image=False)

    def test_tls_files_must_be_private(self):
        self.tls_key.chmod(0o644)
        with self.assertRaisesRegex(stack.ConfigError, "0400 or 0600"):
            self.validate(check_image=False)

    def test_init_rejects_non_root_without_creating_private_files(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "new-stack"
            output, error = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error), patch.object(stack.os, "geteuid", return_value=501):
                result = stack.main(["init", "--directory", str(target)])
            self.assertEqual(result, 2)
            self.assertIn("run init with sudo", error.getvalue())
            self.assertFalse(target.exists())

    def test_fixed_account_proxy_cidr_must_match_exact_caddy_ip(self):
        account_env = self.root / "private" / "account.env"
        account_env.write_text("", encoding="utf-8")
        account_env.chmod(0o600)
        account_env.write_text("DM_ACCOUNT_TRUSTED_PROXY_CIDRS=172.29.240.3/32\n", encoding="utf-8")
        account_env.chmod(0o400)
        with self.assertRaisesRegex(stack.ConfigError, "exact Caddy gateway IPv4 /32"):
            self.validate(check_image=False)
        account_env.chmod(0o600)
        account_env.write_text("DM_ACCOUNT_TRUSTED_PROXY_CIDRS=172.29.240.2/32\n", encoding="utf-8")
        account_env.chmod(0o400)
        self.validate(check_image=False)

    def test_runtime_image_is_exact_and_modules_are_checked_without_network(self):
        captured = []

        def fake_run(command, **kwargs):
            captured.append(command)
            if command[1:3] == ["image", "inspect"]:
                return type("Result", (), {"stdout": f"{RUNTIME}|[]\n"})()
            return type("Result", (), {"stdout": ""})()

        with patch.object(stack.subprocess, "run", side_effect=fake_run):
            stack._check_runtime_image(RUNTIME, ["account.so"])
        check = captured[1]
        self.assertIn("--network", check)
        self.assertEqual(check[check.index("--network") + 1], "none")
        self.assertEqual(check[-2:], ["agones.so", "account.so"])
        self.assertIn("pull_policy", (self.root / "compose.yaml").read_text(encoding="utf-8"))

        self._update_env({"APPLICATION_REQUIRED_MODULES": "account.so;touch"})
        with self.assertRaisesRegex(stack.ConfigError, "comma-separated list"):
            self.validate(check_image=False)

    def test_image_validation_rejects_mismatched_local_id(self):
        def fake_run(command, **kwargs):
            return type("Result", (), {"stdout": f"{DIGEST}|[]\n"})()

        with patch.object(stack.subprocess, "run", side_effect=fake_run):
            with self.assertRaisesRegex(stack.ConfigError, "does not match"):
                stack._check_runtime_image(RUNTIME, [])

    @unittest.skipUnless(shutil.which("docker"), "Docker CLI is not installed")
    def test_compose_model_parses_locally_without_running_containers(self):
        try:
            subprocess.run(["docker", "compose", "version"], check=True, capture_output=True)
        except (OSError, subprocess.SubprocessError):
            self.skipTest("Docker Compose plugin is not available")
        self.validate(check_image=False, compose_config=True)

    def test_compose_keeps_private_routes_unpublished_and_replaces_client_ip(self):
        compose = (self.root / "compose.yaml").read_text(encoding="utf-8")
        caddyfile = (self.root / "deploy" / "Caddyfile").read_text(encoding="utf-8")
        nakama_entrypoint = (self.root / "deploy" / "nakama-entrypoint.sh").read_text(encoding="utf-8")
        self.assertNotIn("network_mode: service:nakama", compose)
        self.assertIn('"127.0.0.1:17351:7351/tcp"', compose)
        self.assertNotIn('"17682:', compose)
        self.assertNotIn('"7350:', compose)
        self.assertNotIn("gamefleet-tunnel", compose)
        self.assertNotIn("wait_for_local_tunnel", nakama_entrypoint)
        self.assertIn("gamefleet-client.key", compose)
        self.assertNotIn("cap_add:", compose)
        self.assertIn("header_up -X-DM-Client-IP", caddyfile)
        self.assertIn("header_up X-DM-Client-IP {remote_host}", caddyfile)
        self.assertIn("ACME_EMAIL: ${ACME_EMAIL:?", compose)
        self.assertIn("email {$ACME_EMAIL}", caddyfile)


if __name__ == "__main__":
    unittest.main()
