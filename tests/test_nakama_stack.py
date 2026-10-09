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

    def _root_owned_stat(self, files=()):
        original_lstat = Path.lstat
        root_owned_paths = {self.root.resolve(), (self.root / "deploy").resolve()}
        root_owned_paths.update(Path(path).resolve() for path in files)

        def synthetic_root_owned(path, *args, **kwargs):
            info = original_lstat(path, *args, **kwargs)
            if Path(path).resolve() in root_owned_paths:
                fields = {name: getattr(info, name) for name in dir(info) if name.startswith("st_")}
                fields["st_uid"] = 0
                return SimpleNamespace(**fields)
            return info

        return synthetic_root_owned

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

    def test_validate_gateway_runs_only_an_isolated_pinned_caddy_container(self):
        candidate = self.root / "deploy" / "Caddyfile.candidate"
        candidate.write_text("{$NAKAMA_DOMAIN} {}\n", encoding="utf-8")
        candidate.chmod(0o600)
        contact = "ops@acme-check.io"
        caddy_env = {"ACME_EMAIL": contact, "NAKAMA_DOMAIN": "pool.example.net",
                     "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}"}
        observed = {}

        def fake_run(command, **kwargs):
            observed["command"] = command
            observed["kwargs"] = kwargs
            return subprocess.CompletedProcess(command, 0, stdout=f"validated {contact}", stderr=f"contact {contact}")

        output, error = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error), \
             patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value=caddy_env) as validate, \
             patch.object(stack.Path, "lstat", self._root_owned_stat([candidate])), \
             patch.object(stack.subprocess, "run", side_effect=fake_run), \
             patch.dict(os.environ, {"UNRELATED_SYNTHETIC_SECRET": "do-not-pass"}):
            self.assertEqual(stack.main(["validate-gateway", "--directory", str(self.root), "--caddyfile", str(candidate)]), 0, error.getvalue())

        validate.assert_called_once_with(self.root.resolve(), check_image=False, compose_config=True)
        command = observed["command"]
        joined = " ".join(command)
        for required in ("unix:///var/run/docker.sock", "run", "--rm", "--pull=never", "--network=none",
                         "--read-only", "--platform", "linux/amd64", "--tmpfs", "--mount", "--env", "NAKAMA_DOMAIN",
                         "ACME_EMAIL", "--entrypoint", "caddy", caddy_env["CADDY_IMAGE"], "validate", "/etc/caddy/Caddyfile"):
            self.assertIn(required, command if required != "/etc/caddy/Caddyfile" else joined)
        self.assertEqual(command.count("--mount"), 1)
        mount = command[command.index("--mount") + 1]
        self.assertEqual(mount, f"type=bind,src={candidate.resolve()},dst=/etc/caddy/Caddyfile,readonly")
        self.assertIn("/data:rw,nosuid,nodev,noexec,size=16m", command)
        self.assertIn("/config:rw,nosuid,nodev,noexec,size=16m", command)
        self.assertIn("/tmp:rw,nosuid,nodev,noexec,size=16m", command)
        self.assertNotIn(contact, command)
        self.assertNotIn(caddy_env["NAKAMA_DOMAIN"], command)
        self.assertNotIn("--publish", command)
        self.assertNotIn("-p", command)
        run_env = observed["kwargs"]["env"]
        self.assertEqual(set(run_env), {"PATH", "DOCKER_CONFIG", "NAKAMA_DOMAIN", "ACME_EMAIL"})
        self.assertEqual(run_env["ACME_EMAIL"], contact)
        self.assertEqual(run_env["NAKAMA_DOMAIN"], caddy_env["NAKAMA_DOMAIN"])
        self.assertNotIn("UNRELATED_SYNTHETIC_SECRET", run_env)
        self.assertNotIn("do-not-pass", run_env.values())
        self.assertTrue(observed["kwargs"]["capture_output"])
        self.assertTrue(observed["kwargs"]["text"])
        self.assertEqual(observed["kwargs"]["timeout"], 30)
        self.assertIn("isolated pinned-image container", output.getvalue())
        self.assertNotIn(contact, output.getvalue() + error.getvalue())
        self.assertNotIn(f"validated {contact}", output.getvalue())
        self.assertFalse(Path(run_env["DOCKER_CONFIG"]).exists())

    def test_validate_gateway_failure_discards_caddy_diagnostics(self):
        caddyfile = self.root / "deploy" / "Caddyfile"
        contact = "ops@acme-check.io"
        caddy_env = {"ACME_EMAIL": contact, "NAKAMA_DOMAIN": "pool.example.net",
                     "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}"}

        output, error = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error), \
             patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value=caddy_env), \
             patch.object(stack.Path, "lstat", self._root_owned_stat([caddyfile])), \
             patch.object(stack.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, contact, contact)):
            self.assertEqual(stack.main(["validate-gateway", "--directory", str(self.root)]), 2)

        self.assertIn("Caddy gateway configuration failed isolated validation", error.getvalue())
        self.assertNotIn(contact, output.getvalue() + error.getvalue())
        self.assertNotIn("validated", output.getvalue())

    def test_validate_gateway_container_start_failure_is_distinct_and_redacted(self):
        caddyfile = self.root / "deploy" / "Caddyfile"
        contact = "ops@acme-check.io"
        caddy_env = {"ACME_EMAIL": contact, "NAKAMA_DOMAIN": "pool.example.net",
                     "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}"}

        output, error = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error), \
             patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value=caddy_env), \
             patch.object(stack.Path, "lstat", self._root_owned_stat([caddyfile])), \
             patch.object(stack.subprocess, "run", return_value=subprocess.CompletedProcess([], 125, contact, contact)):
            self.assertEqual(stack.main(["validate-gateway", "--directory", str(self.root)]), 2)

        self.assertIn("validation container could not start", error.getvalue())
        self.assertNotIn(contact, output.getvalue() + error.getvalue())
        self.assertNotIn("validated", output.getvalue())

    def test_validate_gateway_timeout_attempts_local_container_cleanup(self):
        caddyfile = self.root / "deploy" / "Caddyfile"
        contact = "ops@acme-check.io"
        caddy_env = {"ACME_EMAIL": contact, "NAKAMA_DOMAIN": "pool.example.net",
                     "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}"}
        error = io.StringIO()
        timed_out = subprocess.TimeoutExpired(["docker"], 30)
        cleanup_result = subprocess.CompletedProcess(["docker"], 0, "", "")
        with contextlib.redirect_stderr(error), patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value=caddy_env), \
             patch.object(stack.Path, "lstat", self._root_owned_stat([caddyfile])), \
             patch.object(stack.subprocess, "run", side_effect=[timed_out, cleanup_result]) as run:
            self.assertEqual(stack.main(["validate-gateway", "--directory", str(self.root)]), 2)

        self.assertIn("gateway validation timed out", error.getvalue())
        self.assertNotIn(contact, error.getvalue())
        self.assertEqual(run.call_count, 2)
        command = run.call_args_list[0].args[0]
        cleanup = run.call_args_list[1].args[0]
        cleanup_env = run.call_args_list[1].kwargs["env"]
        self.assertIn("--network=none", command)
        self.assertIn("--name", command)
        self.assertEqual(cleanup[-2:], ["--force", command[command.index("--name") + 1]])
        self.assertNotIn(contact, command + cleanup)
        self.assertNotIn(caddy_env["NAKAMA_DOMAIN"], command + cleanup)
        self.assertEqual(set(cleanup_env), {"PATH", "DOCKER_CONFIG"})

    def test_validate_gateway_rejects_symlink_candidate_before_container_start(self):
        candidate = self.root / "deploy" / "Caddyfile.symlink"
        candidate.symlink_to(self.root / "deploy" / "Caddyfile")
        caddy_env = {"ACME_EMAIL": "ops@acme-check.io", "NAKAMA_DOMAIN": "pool.example.net",
                     "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}"}
        error = io.StringIO()
        with contextlib.redirect_stderr(error), patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value=caddy_env), \
             patch.object(stack.subprocess, "run") as run:
            self.assertEqual(stack.main(["validate-gateway", "--directory", str(self.root), "--caddyfile", str(candidate)]), 2)
        run.assert_not_called()
        self.assertIn("Caddyfile", error.getvalue())

    def test_validate_gateway_rejects_group_or_world_writable_candidate(self):
        candidate = self.root / "deploy" / "Caddyfile.writable"
        candidate.write_text("{$NAKAMA_DOMAIN} {}\n", encoding="utf-8")
        candidate.chmod(0o666)
        caddy_env = {"ACME_EMAIL": "ops@acme-check.io", "NAKAMA_DOMAIN": "pool.example.net",
                     "CADDY_IMAGE": f"docker.io/library/caddy:2.11.4-alpine@{DIGEST}"}
        error = io.StringIO()
        with contextlib.redirect_stderr(error), patch.object(stack.os, "geteuid", return_value=0), \
             patch.object(stack, "validate_project", return_value=caddy_env), \
             patch.object(stack.Path, "lstat", self._root_owned_stat([candidate])), \
             patch.object(stack.subprocess, "run") as run:
            self.assertEqual(stack.main(["validate-gateway", "--directory", str(self.root), "--caddyfile", str(candidate)]), 2)
        run.assert_not_called()
        self.assertIn("not writable by group or others", error.getvalue())

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
        # reverse_proxy applies its Delete operations after Set; a same-block
        # delete would remove the trusted replacement value.
        self.assertNotIn("header_up -X-DM-Client-IP", caddyfile)
        self.assertIn("header_up X-DM-Client-IP {remote_host}", caddyfile)
        self.assertIn("ACME_EMAIL: ${ACME_EMAIL:?", compose)
        self.assertIn("email {$ACME_EMAIL}", caddyfile)


if __name__ == "__main__":
    unittest.main()
