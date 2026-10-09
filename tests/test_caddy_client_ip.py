"""Exercise the production Caddyfile's client-IP overwrite with real Caddy.

Run with ``python3 tests/test_caddy_client_ip.py``. The test skips when Caddy is
not installed; set CADDY_BINARY to select an installed Caddy v2 binary.
"""
from __future__ import annotations

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]
CADDYFILE = ROOT / "deploy" / "Caddyfile"


class _EchoHeaders(BaseHTTPRequestHandler):
    def do_GET(self):
        values = self.headers.get_all("X-DM-Client-IP", [])
        body = json.dumps(values).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        pass


class CaddyClientIPIntegrationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Class cleanups run even when this setup later raises, unlike
        # tearDownClass. Register once before allocating any resources.
        cls.addClassCleanup(cls._cleanup)
        cls.caddy = os.environ.get("CADDY_BINARY") or shutil.which("caddy")
        if not cls.caddy:
            raise unittest.SkipTest("install Caddy v2 or set CADDY_BINARY")

        cls.temp = tempfile.TemporaryDirectory(prefix="caddy-client-ip-")
        cls.upstream = ThreadingHTTPServer(("127.0.0.1", 0), _EchoHeaders)
        cls.upstream.daemon_threads = True
        cls.upstream_thread = threading.Thread(target=cls.upstream.serve_forever, daemon=True)
        cls.upstream_thread.start()

        cls.proxy_port = cls._unused_port()
        source = CADDYFILE.read_text(encoding="utf-8")
        if source.count("email {$ACME_EMAIL}") != 1 or source.count("{$NAKAMA_DOMAIN}") != 1:
            raise RuntimeError("production Caddyfile test anchors changed")
        if source.count("reverse_proxy nakama:7350") != 1:
            raise RuntimeError("production Caddyfile upstream anchor changed")
        candidate = source.replace(
            "reverse_proxy nakama:7350",
            f"reverse_proxy 127.0.0.1:{cls.upstream.server_port}",
        )
        # Isolate this real Caddy process: it listens only on loopback, does not
        # start the admin endpoint, and cannot attempt automatic certificate work.
        candidate = candidate.replace(
            "email {$ACME_EMAIL}",
            "email {$ACME_EMAIL}\n\tadmin off\n\tauto_https off",
        )
        config_path = Path(cls.temp.name) / "Caddyfile"
        config_path.write_text(candidate, encoding="utf-8")

        child_env = {
            "PATH": os.environ.get("PATH", ""),
            "ACME_EMAIL": "caddy-smoke@example.invalid",
            "NAKAMA_DOMAIN": f"http://127.0.0.1:{cls.proxy_port}",
            "XDG_CONFIG_HOME": str(Path(cls.temp.name) / "config"),
            "XDG_DATA_HOME": str(Path(cls.temp.name) / "data"),
        }
        cls.process = subprocess.Popen(
            [cls.caddy, "run", "--config", str(config_path), "--adapter", "caddyfile"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=child_env,
        )
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline:
            if cls.process.poll() is not None:
                cls._stop()
                raise RuntimeError("Caddy exited before opening the isolated test listener")
            try:
                with socket.create_connection(("127.0.0.1", cls.proxy_port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.05)
        else:
            cls._stop()
            raise RuntimeError("Caddy did not open the isolated test listener")

    @classmethod
    def _unused_port(cls):
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            return sock.getsockname()[1]

    @classmethod
    def _stop(cls):
        process = getattr(cls, "process", None)
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)

    @classmethod
    def _cleanup(cls):
        try:
            cls._stop()
        finally:
            cls._cleanup_upstream_and_temp()

    @classmethod
    def _cleanup_upstream_and_temp(cls):
        upstream = getattr(cls, "upstream", None)
        if upstream is not None:
            thread = getattr(cls, "upstream_thread", None)
            if thread is not None and thread.is_alive():
                upstream.shutdown()
            upstream.server_close()
            if thread is not None:
                thread.join(timeout=2)
        temp = getattr(cls, "temp", None)
        if temp is not None:
            temp.cleanup()

    def _forwarded_values(self, supplied_values):
        conn = http.client.HTTPConnection("127.0.0.1", self.proxy_port, timeout=3)
        conn.putrequest("GET", "/header-check")
        for value in supplied_values:
            conn.putheader("X-DM-Client-IP", value)
        conn.endheaders()
        try:
            response = conn.getresponse()
            self.assertEqual(response.status, 200)
            return json.loads(response.read())
        finally:
            conn.close()

    def test_client_supplied_single_or_multiple_values_are_replaced_by_peer_ip(self):
        expected = ["127.0.0.1"]
        for supplied in ([], ["203.0.113.77"], ["203.0.113.7", "198.51.100.9"]):
            with self.subTest(supplied_header_count=len(supplied)):
                self.assertEqual(self._forwarded_values(supplied), expected)


if __name__ == "__main__":
    unittest.main()
