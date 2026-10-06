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
        self.ssh_key = self.root / "private" / "platform-ssh-key"
        self.known_hosts = self.root / "private" / "platform-known-hosts"
        for path in (self.service_key, self.ssh_key, self.known_hosts):
            path.chmod(0o600)
        self.service_key.write_text("gfsvc-test-key\n", encoding="utf-8")
        self.ssh_key.write_text("synthetic ssh key material\n", encoding="utf-8")
        self.known_hosts.write_text("gamefleet-admin.example.com ssh-ed25519 synthetic-public-key\n", encoding="utf-8")
        for path in (self.service_key, self.ssh_key, self.known_hosts):
            path.chmod(0o400)
        self._update_env({
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
            "GAMEFLEET_SERVICE_URL": "http://127.0.0.1:17682",
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
            root_paths = (self.ssh_key.resolve(), self.known_hosts.resolve())
            if path.resolve() in root_paths:
                fields = {name: getattr(info, name) for name in dir(info) if name.startswith("st_")}
                fields["st_uid"] = owner_uid
                return SimpleNamespace(**fields)
            return info

        with patch.object(Path, "lstat", synthetic_root_owned):
            return stack.validate_project(self.root, compose_config=compose_config, **kwargs)

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
            if path == self.service_key or path == self.ssh_key:
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

    def test_loopback_network_ssh_and_digest_inputs_are_strict(self):
        for bad_url in ("http://gamefleet.example.net:17682", "https://127.0.0.1:17682",
                        "http://127.0.0.1:17683", "http://127.0.0.1:17682/path"):
            with self.subTest(url=bad_url):
                self._update_service_env({"GAMEFLEET_SERVICE_URL": bad_url})
                with self.assertRaisesRegex(stack.ConfigError, "loopback tunnel"):
                    self.validate(check_image=False)
        self._update_service_env({"GAMEFLEET_SERVICE_URL": "http://127.0.0.1:17682"})

        self._update_env({"POSTGRES_IMAGE": "postgres:latest"})
        with self.assertRaisesRegex(stack.ConfigError, "immutable OCI sha256 digest"):
            self.validate(check_image=False)
        self._update_env({"POSTGRES_IMAGE": f"docker.io/library/postgres:17.6@{DIGEST}",
                          "DATABASE_SUBNET": "172.29.240.128/25"})
        with self.assertRaisesRegex(stack.ConfigError, "must not overlap"):
            self.validate(check_image=False)

    def test_tunnel_host_keys_must_be_root_owned(self):
        with self.assertRaisesRegex(stack.ConfigError, "owned by UID 0"):
            self.validate(check_image=False, synthetic_tunnel_uid=501)

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
        tunnel = (self.root / "deploy" / "tunnel" / "entrypoint.sh").read_text(encoding="utf-8")
        nakama_entrypoint = (self.root / "deploy" / "nakama-entrypoint.sh").read_text(encoding="utf-8")
        self.assertIn("network_mode: service:nakama", compose)
        self.assertIn('"127.0.0.1:17351:7351/tcp"', compose)
        self.assertNotIn('"17682:', compose)
        self.assertNotIn('"7350:', compose)
        self.assertIn("ssh -F /dev/null -N -T", tunnel)
        self.assertNotIn("ClearAllForwardings", tunnel)
        self.assertIn("StrictHostKeyChecking=yes", tunnel)
        self.assertIn("-L 127.0.0.1:17682:127.0.0.1:17682", tunnel)
        self.assertIn(f'$2 == "0100007F:{17682:04X}" && $4 == "0A"', nakama_entrypoint)
        self.assertIn("cap_drop:\n      - ALL", compose)
        self.assertNotIn("cap_add:", compose)
        self.assertIn("header_up -X-DM-Client-IP", caddyfile)
        self.assertIn("header_up X-DM-Client-IP {remote_host}", caddyfile)


if __name__ == "__main__":
    unittest.main()
