from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import gamefleet_service as service


APP_IMAGE = "registry.example.test/nakama-fixed@sha256:" + "a" * 64
PUBLISHED_IMAGE = "registry.example.test/nakama-fixed-service@sha256:" + "b" * 64


class GameFleetServiceConfigTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.env = self.root / "service.env"
        self.key = self.root / "service.key"
        self.key.write_text("synthetic-key-content-must-not-be-read", encoding="utf-8")
        self.key.chmod(0o400)
        self.write_env()

    def tearDown(self):
        self.tmp.cleanup()

    def write_env(self, **overrides):
        values = {
            "NAKAMA_FLEET_BACKEND": "gamefleet-service",
            "GAMEFLEET_SERVICE_URL": "http://127.0.0.1:17682",
            "GAMEFLEET_SERVICE_KEY_FILE": "/run/secrets/gamefleet-service-key",
            "GAMEFLEET_SERVICE_ID": "service-production",
            "GAMEFLEET_SERVICE_APPLICATION_ID": "application-production",
            "GAMEFLEET_SERVICE_IDENTITY_ISSUER": "nakama-production",
            "GAMEFLEET_SERVICE_REGION": "us-west",
            "GAMEFLEET_SERVICE_COMPATIBILITY": "fixed-v6",
            "GAMEFLEET_ARCHIVE_URL": "",
            "GAMEFLEET_ARCHIVE_KEY_FILE": "",
            "GAMEFLEET_ARCHIVE_APPLICATION_ID": "",
            "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER": "",
            "GAMEFLEET_ARCHIVE_REGION": "",
            "GAMEFLEET_ARCHIVE_COMPATIBILITY": "",
            "GAMEFLEET_ARCHIVE_SERVICE_ID": "",
        }
        values.update(overrides)
        self.env.write_text("\n".join(f"{name}={value}" for name, value in values.items()) + "\n", encoding="utf-8")

    def validate(self):
        return service.validate_config(self.env, self.key)

    def test_example_template_has_placeholders_and_never_contains_a_credential(self):
        template = service.TEMPLATE.read_text(encoding="utf-8")
        self.assertIn("GAMEFLEET_SERVICE_ID=service_replace_me", template)
        self.assertIn("GAMEFLEET_SERVICE_KEY_FILE=/run/secrets/gamefleet-service-key", template)
        self.assertNotIn("gfsvc_fake", template)
        self.assertNotIn("GAMEFLEET_SERVICE_KEY=", template)

    def test_exact_seven_fields_and_private_key_metadata_are_required(self):
        validated = self.validate()
        self.assertEqual(len(service.SERVICE_KEYS), 7)
        self.assertEqual(validated.key_source, self.key.resolve())

        self.write_env(GAMEFLEET_SERVICE_COMPATIBILITY="")
        with self.assertRaisesRegex(service.ConfigError, "all seven"):
            self.validate()

    def test_validation_does_not_open_or_read_key_file(self):
        original_read_text = Path.read_text

        def guarded_read_text(path, *args, **kwargs):
            if path == self.key:
                raise AssertionError("key file content was read")
            return original_read_text(path, *args, **kwargs)

        with patch.object(Path, "read_text", guarded_read_text):
            self.validate()

    def test_key_mode_symlink_and_non_regular_files_are_rejected(self):
        self.key.chmod(0o644)
        with self.assertRaisesRegex(service.ConfigError, "0400 or 0600"):
            self.validate()
        self.key.chmod(0o400)

        link = self.root / "link.key"
        link.symlink_to(self.key)
        with self.assertRaisesRegex(service.ConfigError, "non-symlink"):
            service.validate_config(self.env, link)
        with self.assertRaisesRegex(service.ConfigError, "non-symlink"):
            service.validate_config(self.env, self.root)

    def test_non_loopback_or_non_origin_url_is_rejected(self):
        for bad_url in ("http://gamefleet.internal:17682", "https://127.0.0.1:17682",
                        "http://127.0.0.1", "http://127.0.0.1:17682/private",
                        "http://127.0.0.1:17682?token=x", "http://user@127.0.0.1:17682"):
            with self.subTest(url=bad_url):
                self.write_env(GAMEFLEET_SERVICE_URL=bad_url)
                with self.assertRaisesRegex(service.ConfigError, "loopback HTTP origin"):
                    self.validate()

    def test_wrong_backend_placeholders_and_chat_style_secret_setting_fail_safely(self):
        self.write_env(NAKAMA_FLEET_BACKEND="gamefleet")
        with self.assertRaisesRegex(service.ConfigError, "must be gamefleet-service"):
            self.validate()
        self.write_env(GAMEFLEET_SERVICE_ID="service_replace_me")
        with self.assertRaisesRegex(service.ConfigError, "exact configured value"):
            self.validate()
        self.env.write_text(self.env.read_text() + "GAMEFLEET_SERVICE_KEY=do-not-send-in-chat\n", encoding="utf-8")
        with self.assertRaises(service.ConfigError) as raised:
            self.validate()
        self.assertNotIn("do-not-send-in-chat", str(raised.exception))

    def test_archive_configuration_is_all_or_none_and_uses_a_separate_key(self):
        self.write_env(GAMEFLEET_ARCHIVE_URL="http://127.0.0.1:17682")
        with self.assertRaisesRegex(service.ConfigError, "all configured or all empty"):
            self.validate()

        archive_key = self.root / "archive.key"
        archive_key.write_text("another synthetic credential", encoding="utf-8")
        archive_key.chmod(0o600)
        archive = {
            "GAMEFLEET_ARCHIVE_URL": "http://127.0.0.1:17682",
            "GAMEFLEET_ARCHIVE_KEY_FILE": "/run/secrets/archive-key",
            "GAMEFLEET_ARCHIVE_APPLICATION_ID": "application-old",
            "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER": "nakama-old",
            "GAMEFLEET_ARCHIVE_REGION": "us-west",
            "GAMEFLEET_ARCHIVE_COMPATIBILITY": "fixed-v5",
            "GAMEFLEET_ARCHIVE_SERVICE_ID": "service-history",
        }
        self.write_env(**archive)
        with self.assertRaisesRegex(service.ConfigError, "requires --archive-key-source"):
            self.validate()
        validated = service.validate_config(self.env, self.key, archive_key)
        self.assertEqual(validated.archive_key_source, archive_key.resolve())
        self.assertIn("GAMEFLEET_ARCHIVE_KEY_FILE", service.compose_override(validated, PUBLISHED_IMAGE))

    def test_render_is_pinned_preserves_full_image_and_adds_no_ports_or_network_mode(self):
        rendered = service.compose_override(self.validate(), PUBLISHED_IMAGE)
        self.assertIn(f'image: "{PUBLISHED_IMAGE}"', rendered)
        self.assertIn('NAKAMA_FLEET_BACKEND: "gamefleet-service"', rendered)
        for name in service.SERVICE_KEYS:
            self.assertIn(f"{name}:", rendered)
        self.assertIn(f'source: "{self.key.resolve()}"', rendered)
        self.assertIn('read_only: true', rendered)
        self.assertNotIn("ports:", rendered)
        self.assertNotIn("network_mode:", rendered)
        self.assertNotIn("synthetic-key-content-must-not-be-read", rendered)
        with self.assertRaisesRegex(service.ConfigError, "pinned by an OCI sha256 digest"):
            service.compose_override(self.validate(), "registry.example.test/latest")

    def test_init_is_secret_free_and_does_not_overwrite(self):
        output = self.root / "nested" / "gamefleet.env"
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            self.assertEqual(service.main(["init", "--output", str(output)]), 0)
        self.assertIn("did not deploy", stdout.getvalue())
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        with self.assertRaisesRegex(service.ConfigError, "already exists"):
            service.command_init(type("Args", (), {"output": output})())

    def test_render_writes_explicit_not_deployed_manifest_and_status_does_not_probe(self):
        output = self.root / "rendered"
        self.assertEqual(service.main([
            "render", "--env", str(self.env), "--key-source", str(self.key),
            "--runtime-image", PUBLISHED_IMAGE, "--output-dir", str(output),
        ]), 0)
        manifest = json.loads((output / "render-manifest.json").read_text(encoding="utf-8"))
        self.assertFalse(manifest["deployed"])
        self.assertFalse(manifest["remote_status_checked"])
        stdout = io.StringIO()
        with patch.object(service.subprocess, "run", side_effect=AssertionError("status must not call subprocess")):
            with contextlib.redirect_stdout(stdout):
                self.assertEqual(service.main(["status", "--directory", str(output)]), 0)
        self.assertIn("Deployment status: not checked", stdout.getvalue())

    def test_build_uses_pinned_abi_and_extends_application_image_without_push(self):
        output = self.root / "build"
        captured = {}

        def fake_run(command, **kwargs):
            captured["command"] = command
            return type("Result", (), {"stdout": "", "returncode": 0})()

        stdout = io.StringIO()
        with patch.object(service, "_source_revision", return_value=("abc123", False)):
            with patch.object(service.subprocess, "run", side_effect=fake_run):
                with contextlib.redirect_stdout(stdout):
                    self.assertEqual(service.main([
                        "build", "--env", str(self.env), "--key-source", str(self.key),
                        "--application-image", APP_IMAGE, "--output-image", "nakama-service:local",
                        "--output-dir", str(output),
                    ]), 0)
        command = captured["command"]
        self.assertIn("--target", command)
        self.assertEqual(command[command.index("--target") + 1], "service-runtime")
        self.assertIn(f"APPLICATION_RUNTIME_IMAGE={APP_IMAGE}", command)
        compatibility = service._compatibility_inputs()
        self.assertIn(f"NAKAMA_IMAGE={compatibility['NAKAMA_IMAGE']}", command)
        self.assertIn(f"PLUGIN_BUILDER_IMAGE={compatibility['PLUGIN_BUILDER_IMAGE']}", command)
        self.assertIn("--load", command)
        self.assertNotIn("--push", command)
        metadata = json.loads((output / "build-inputs.json").read_text(encoding="utf-8"))
        self.assertTrue(metadata["local_build_completed"])
        self.assertFalse(metadata["push_or_deploy_performed"])
        self.assertEqual(metadata["application_runtime_image"], APP_IMAGE)


if __name__ == "__main__":
    unittest.main()
