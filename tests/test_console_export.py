import argparse
from contextlib import contextmanager, redirect_stderr, redirect_stdout
import copy
import grp
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "export_console_status.py"
SPEC = importlib.util.spec_from_file_location("export_console_status", SCRIPT)
export = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(export)
WORKER = "a" * 32
FAKE_TOKEN = "synthetic_admin_credential_not_real"


def fixture():
    return {"revision": 5, "profile": "private-profile", "leader": "private-leader", "commands": {"secret-command": {}},
            "creation_blocked_reason": "", "workers": {WORKER: {
                "id": WORKER, "provider_id": "private-provider", "boot_id": "private-boot", "region": "us-west",
                "build_hash": "dm-v1", "state": "ready", "host": "game.example.internal", "port": 20000,
                "max_rooms": 2, "player_count": 1, "ready": True, "draining": False, "created_at": 100,
                "last_heartbeat": 150, "metrics": {"memory_bytes": 12345, "frame_p99_ms": 16.6,
                                                    "simulation_pending": 1, "admin_token": "private-metric-token"}}},
            "allocations": {"active": {
                "allocation_id": "allocation-1", "room_id": "room-1", "request_key": "private-request-key",
                "worker_id": WORKER, "state": "active", "epoch": 1, "created_at": 110, "expires_at": 300,
                "user_ids": ["user-1", "user-2"], "sessions": [
                    {"reservation_id": "private-reservation", "user_id": "user-1", "seat": 0,
                     "connected": True, "ever_connected": True, "reconnect_until": 250}]}}}


@contextmanager
def server(callback):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            try:
                callback(self)
            except (BrokenPipeError, ConnectionResetError):
                pass

        def log_message(self, *args):
            pass

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    httpd.daemon_threads = True
    thread = threading.Thread(target=httpd.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
    thread.start()
    try:
        yield "http://127.0.0.1:" + str(httpd.server_port)
    finally:
        httpd.shutdown()
        httpd.server_close()
        thread.join(timeout=2)


def reply(handler, payload=None):
    body = json.dumps(fixture() if payload is None else payload).encode()
    handler.send_response(200)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


class ProjectionTests(unittest.TestCase):
    def test_exact_projection_omits_all_private_fields(self):
        output = export.project_status(fixture())
        self.assertEqual({"ok", "revision", "creation_blocked_reason", "workers", "rooms"}, set(output))
        self.assertEqual({"id", "pod", "region", "build_hash", "state", "host", "port", "max_rooms", "occupied_rooms",
                          "player_count", "ready", "draining", "created_at", "last_heartbeat", "metrics", "error"}, set(output["workers"][0]))
        self.assertEqual({"id", "allocation_id", "worker_id", "region", "state", "epoch", "created_at", "expires_at",
                          "terminal_at", "players", "error"}, set(output["rooms"][0]))
        self.assertEqual(set(export.INTEGER_METRICS + export.FLOAT_METRICS), set(output["workers"][0]["metrics"]))
        text = json.dumps(output)
        for secret in ("private-", "commands", "request_key", "reservation_id", "provider_id", "boot_id", "admin_token"):
            self.assertNotIn(secret, text)

    def test_sessions_and_missing_players_match_console_projection(self):
        output = export.project_status(fixture())
        worker, room = output["workers"][0], output["rooms"][0]
        self.assertEqual("nag-" + WORKER, worker["pod"])
        self.assertEqual(1, worker["occupied_rooms"])
        self.assertEqual("us-west", room["region"])
        self.assertEqual([{"user_id": "user-1", "seat": 0, "connected": True, "ever_connected": True, "reconnect_until": 250},
                          {"user_id": "user-2", "seat": 1, "connected": False, "ever_connected": False, "reconnect_until": 0}], room["players"])

    def test_unmeasured_metrics_are_null(self):
        value = fixture()
        value["workers"][WORKER]["last_heartbeat"] = 0
        self.assertIsNone(export.project_status(value)["workers"][0]["metrics"])

    def test_no_heartbeat_metrics_object_is_not_fabricated(self):
        value = fixture()
        value["workers"][WORKER].pop("metrics")
        with self.assertRaisesRegex(export.ExportError, "invalid_state_metrics"):
            export.project_status(value)

    def test_occupied_excludes_every_terminal_state(self):
        value = fixture()
        for index, state in enumerate(export.TERMINAL):
            room = copy.deepcopy(value["allocations"]["active"])
            room.update(state=state, created_at=1000 + index)
            value["allocations"][state] = room
        output = export.project_status(value)
        self.assertEqual(1, output["workers"][0]["occupied_rooms"])
        self.assertEqual(sorted([item["created_at"] for item in output["rooms"]], reverse=True),
                         [item["created_at"] for item in output["rooms"]])

    def test_null_map_entries_are_skipped(self):
        value = fixture()
        value["workers"]["removed"] = None
        value["allocations"]["removed"] = None
        self.assertEqual(1, len(export.project_status(value)["workers"]))
        self.assertEqual(1, len(export.project_status(value)["rooms"]))

    def test_malformed_upstream_cannot_become_fresh_empty_snapshot(self):
        for value in ({}, {"error": "unavailable"}, {"revision": True, "workers": {}, "allocations": {}},
                      {"revision": 1, "workers": [], "allocations": {}},
                      {"revision": 1, "workers": {"a": "bad"}, "allocations": {}}):
            with self.subTest(value=value), self.assertRaises(export.ExportError):
                export.project_status(value)

    def test_reject_wrong_typed_metrics_and_nested_player_fields(self):
        for key, value in (("metrics", {"memory_bytes": "secret"}), ("metrics", {"frame_p99_ms": float("nan")}), ("ready", "true")):
            state = fixture()
            state["workers"][WORKER][key] = value
            with self.subTest(key=key), self.assertRaises(export.ExportError):
                export.project_status(state)
        state = fixture()
        state["allocations"]["active"]["sessions"][0]["user_id"] = {"admin_token": FAKE_TOKEN}
        with self.assertRaises(export.ExportError):
            export.project_status(state)

    def test_all_text_fields_receive_redaction(self):
        for secret in ('password=example', 'Bearer synthetic', 'postgres://name:pass@host/db',
                       r'{\"token\":\"example\"}', 'ADMISSION_KEY=example', 'boot_id=example',
                       'prefix ' + FAKE_TOKEN + ' suffix'):
            state = fixture()
            state["creation_blocked_reason"] = secret
            state["workers"][WORKER]["region"] = secret
            state["allocations"]["active"]["sessions"][0]["user_id"] = secret
            output = export.project_status(state, (FAKE_TOKEN,))
            self.assertEqual(export.REDACTED, output["creation_blocked_reason"])
            self.assertEqual(export.REDACTED, output["workers"][0]["region"])
            self.assertEqual(export.REDACTED, output["rooms"][0]["players"][0]["user_id"])

    def test_safe_error_names_remain_visible(self):
        for value in ("invalid_bootstrap_token", "error=invalid_bootstrap_token", "token_file_unavailable"):
            self.assertEqual(value, export.string(value, (FAKE_TOKEN,)))

    def test_secret_scan_precedes_text_truncation(self):
        start = time.monotonic()
        self.assertEqual(export.REDACTED, export.string("a" * 20000 + " password=example", ()))
        self.assertTrue(export.string("a" * 20000, ()).endswith(" [truncated]"))
        # Regexes must not retry an unbounded JWT/URI prefix at every character.
        self.assertLess(time.monotonic() - start, 1)

    def test_nonempty_valid_state_can_have_no_workers(self):
        self.assertEqual({"ok": True, "revision": 1, "creation_blocked_reason": "", "workers": [], "rooms": []},
                         export.project_status({"revision": 1, "workers": None, "allocations": None}))


class HTTPTests(unittest.TestCase):
    def test_get_uses_exact_path_header_and_no_environment_proxy(self):
        seen = []
        def handler(request):
            seen.append((request.command, request.path, request.headers["Authorization"]))
            reply(request)
        with server(handler) as url, patch.dict(os.environ, {"http_proxy": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1"}):
            self.assertEqual(5, export.fetch_status(url, FAKE_TOKEN)["revision"])
        self.assertEqual([("GET", export.STATUS_PATH, "Bearer " + FAKE_TOKEN)], seen)

    def test_refuses_hostname_public_url_credentials_paths_and_query(self):
        for url in ("http://localhost:7350", "http://192.168.1.1:7350", "https://example.com", "ftp://127.0.0.1",
                    "http://user:pass@127.0.0.1", "http://127.0.0.1/path", "http://127.0.0.1?", "http://127.0.0.1#",
                    "http://127.0.0.1:0", "http://127.0.0.1:99999", "http://[::1%lo0]:7350", "http://127.0.0.1\n"):
            with self.subTest(url=url), self.assertRaisesRegex(export.ExportError, "invalid_upstream_url"):
                export.parse_url(url)
        self.assertEqual(("http", "::1", 7350), export.parse_url("http://[::1]:7350/"))
        self.assertEqual(("https", "127.0.0.1", 443), export.parse_url("https://127.0.0.1"))

    def test_redirect_never_sends_credential_to_target(self):
        seen = []
        def target(request):
            seen.append(True)
            reply(request)
        with server(target) as destination:
            def redirect(request):
                request.send_response(302)
                request.send_header("Location", destination)
                request.end_headers()
            with server(redirect) as url:
                with self.assertRaisesRegex(export.ExportError, "upstream_status_rejected"):
                    export.fetch_status(url, FAKE_TOKEN)
        self.assertEqual([], seen)

    def test_large_body_is_bounded_without_content_length(self):
        def handler(request):
            request.send_response(200)
            request.end_headers()
            request.wfile.write(b"x" * 300)
        with server(handler) as url, patch.object(export, "MAX_RESPONSE", 256):
            with self.assertRaisesRegex(export.ExportError, "response_size_limit"):
                export.fetch_status(url, FAKE_TOKEN)

    def test_oversized_declared_body_is_rejected_without_read(self):
        def handler(request):
            request.send_response(200)
            request.send_header("Content-Length", str(export.MAX_RESPONSE + 1))
            request.end_headers()
        with server(handler) as url:
            with self.assertRaisesRegex(export.ExportError, "response_size_limit"):
                export.fetch_status(url, FAKE_TOKEN)

    def test_total_deadline_stops_continuously_trickled_body(self):
        def handler(request):
            request.send_response(200)
            request.end_headers()
            for _ in range(100):
                request.wfile.write(b" ")
                request.wfile.flush()
                time.sleep(0.01)
        before = time.monotonic()
        with server(handler) as url, patch.object(export, "REQUEST_TIMEOUT", 0.12):
            with self.assertRaisesRegex(export.ExportError, "upstream_timeout"):
                export.fetch_status(url, FAKE_TOKEN)
        self.assertLess(time.monotonic() - before, 0.8)

    def test_compressed_body_rejected(self):
        def handler(request):
            request.send_response(200)
            request.send_header("Content-Encoding", "gzip")
            request.end_headers()
        with server(handler) as url:
            with self.assertRaisesRegex(export.ExportError, "upstream_encoding_rejected"):
                export.fetch_status(url, FAKE_TOKEN)

    def test_json_duplicates_nonfinite_and_invalid_json_rejected(self):
        for raw in (b'{"a":1,"a":2}', b'{"a":NaN}', b'[]', b'not-json', b'\xff'):
            with self.subTest(raw=raw), self.assertRaises(export.ExportError):
                export.decode_object(raw, 1000)


class FileTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.directory = self.root / "status"
        self.directory.mkdir(mode=0o750)
        self.directory.chmod(0o750)
        self.output = self.directory / "fleet.json"
        self.credentials = self.root / "credentials.json"
        self.credentials.write_text(json.dumps({"admin_token": FAKE_TOKEN, "database_password": "unrelated_private_value"}))
        self.credentials.chmod(0o600)
        self.gid = self.directory.stat().st_gid
        self.group = grp.getgrgid(self.gid).gr_name
        self.args = argparse.Namespace(credentials=self.credentials, output=self.output,
                                       url="http://127.0.0.1:7350", group=self.group)
        self.addCleanup(patch.stopall)
        patch.object(export, "ROOT_UID", os.geteuid()).start()

    def successful_export(self):
        with patch.object(export, "fetch_status", return_value=fixture()):
            return export.export_status(self.args)

    def old_snapshot(self):
        self.successful_export()
        os.utime(self.output, ns=(1600000000000000000, 1600000000000000000))
        return self.output.read_bytes(), self.output.stat().st_mtime_ns

    def assert_preserved(self, old):
        self.assertEqual(old, (self.output.read_bytes(), self.output.stat().st_mtime_ns))
        self.assertEqual([], list(self.directory.glob(".fleet-status-*")))

    def test_export_reads_only_token_and_publishes_expected_mode_and_group(self):
        result = self.successful_export()
        self.assertEqual(result, json.loads(self.output.read_text()))
        self.assertEqual(0o640, stat.S_IMODE(self.output.stat().st_mode))
        self.assertEqual(os.geteuid(), self.output.stat().st_uid)
        self.assertEqual(self.gid, self.output.stat().st_gid)
        self.assertNotIn(FAKE_TOKEN, self.output.read_text())
        self.assertNotIn("unrelated_private_value", self.output.read_text())
        self.assertEqual([], list(self.directory.glob(".fleet-status-*")))

    def test_successful_refresh_atomically_replaces_inode(self):
        self.successful_export()
        inode = self.output.stat().st_ino
        self.successful_export()
        self.assertNotEqual(inode, self.output.stat().st_ino)

    def test_fetch_failure_preserves_bytes_and_mtime(self):
        old = self.old_snapshot()
        with patch.object(export, "fetch_status", side_effect=export.ExportError("upstream_unavailable")):
            with self.assertRaises(export.ExportError):
                export.export_status(self.args)
        self.assert_preserved(old)

    def test_invalid_projection_preserves_bytes_and_mtime(self):
        old = self.old_snapshot()
        with patch.object(export, "fetch_status", return_value={"error": FAKE_TOKEN}):
            with self.assertRaisesRegex(export.ExportError, "invalid_state"):
                export.export_status(self.args)
        self.assert_preserved(old)

    def test_fsync_failure_preserves_bytes_and_mtime(self):
        old = self.old_snapshot()
        with patch.object(export.os, "fsync", side_effect=OSError("sensitive I/O detail")):
            with self.assertRaises(OSError):
                self.successful_export()
        self.assert_preserved(old)

    def test_atomic_replace_failure_preserves_bytes_and_mtime(self):
        old = self.old_snapshot()
        with patch.object(export.os, "replace", side_effect=OSError("sensitive I/O detail")):
            with self.assertRaises(OSError):
                self.successful_export()
        self.assert_preserved(old)

    def test_reject_credentials_symlink_wrong_mode_and_header_injection(self):
        alias = self.root / "credentials-link"
        alias.symlink_to(self.credentials)
        with self.assertRaises(export.ExportError):
            export.read_admin_token(alias)
        self.credentials.chmod(0o640)
        with self.assertRaisesRegex(export.ExportError, "unsafe_credentials_file"):
            export.read_admin_token(self.credentials)
        self.credentials.chmod(0o600)
        self.credentials.write_text(json.dumps({"admin_token": "injected\r\nHeader: value"}))
        with self.assertRaisesRegex(export.ExportError, "invalid_admin_token"):
            export.read_admin_token(self.credentials)

    def test_reject_writable_parent_directory_or_wrong_group(self):
        for mode in (0o770, 0o777, 0o755):
            self.directory.chmod(mode)
            with self.subTest(mode=mode), self.assertRaisesRegex(export.ExportError, "unsafe_output_directory"):
                self.successful_export()
        self.directory.chmod(0o750)
        with self.assertRaisesRegex(export.ExportError, "unsafe_output_directory"):
            with export.output_directory(self.output, self.gid + 1):
                self.fail("wrong group allowed")

    def test_reject_symlink_directory_and_output(self):
        alias = self.root / "status-link"
        alias.symlink_to(self.directory, target_is_directory=True)
        with self.assertRaisesRegex(export.ExportError, "output_directory_unavailable"):
            with export.output_directory(alias / "fleet.json", self.gid):
                self.fail("symlink directory allowed")
        self.output.symlink_to(self.credentials)
        with self.assertRaisesRegex(export.ExportError, "unsafe_output_file"):
            self.successful_export()
        self.assertEqual(FAKE_TOKEN, json.loads(self.credentials.read_text())["admin_token"])

    def test_root_required(self):
        with patch.object(export.os, "geteuid", return_value=export.ROOT_UID + 1):
            with self.assertRaisesRegex(export.ExportError, "root_required"):
                self.successful_export()

    def test_concurrent_export_is_refused(self):
        with export.output_directory(self.output, self.gid):
            with self.assertRaisesRegex(export.ExportError, "export_busy"):
                self.successful_export()

    def test_cli_output_never_echoes_tokens_exceptions_or_body(self):
        old = self.old_snapshot()
        arguments = ["--credentials", str(self.credentials), "--output", str(self.output), "--group", self.group]
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err), patch.object(export, "fetch_status", side_effect=OSError(FAKE_TOKEN)):
            self.assertEqual(1, export.main(arguments))
        self.assertEqual({"ok": False, "code": "export_failed"}, json.loads(err.getvalue()))
        self.assertNotIn(FAKE_TOKEN, out.getvalue() + err.getvalue())
        self.assert_preserved(old)

    def test_current_admin_token_cannot_appear_even_in_allowed_fields(self):
        value = fixture()
        value["workers"][WORKER]["error"] = "failure " + FAKE_TOKEN
        with patch.object(export, "fetch_status", return_value=value):
            export.export_status(self.args)
        self.assertNotIn(FAKE_TOKEN, self.output.read_text())
        self.assertEqual(export.REDACTED, json.loads(self.output.read_text())["workers"][0]["error"])


if __name__ == "__main__":
    unittest.main()
