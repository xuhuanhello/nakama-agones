import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("console_query", Path(__file__).resolve().parents[1]/"scripts/console_query.py")
query = importlib.util.module_from_spec(spec)
spec.loader.exec_module(query)
TOKEN = "fcro_" + "x" * 43
BASE = "http://127.0.0.1:17365/fleet-admin/"


class Response:
    def __init__(self, data): self.data = data
    def __enter__(self): return self
    def __exit__(self, *unused): return False
    def read(self, maximum): return self.data[:maximum]


class Opener:
    def __init__(self, data): self.data = data; self.request = None
    def open(self, request, timeout):
        self.request = request
        return Response(self.data)


class ConsoleQueryTests(unittest.TestCase):
    def test_private_file_and_tunnel_only(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"credentials.json"
            path.write_text(json.dumps({"base_url": BASE, "token": TOKEN}));path.chmod(0o600)
            self.assertEqual((BASE, TOKEN), query.read_credentials(path))
            for url in ["http://public.example/fleet-admin/", "http://127.0.0.1:1/", BASE+"?token=x",
                        "http://user@127.0.0.1:1/fleet-admin/"]:
                path.write_text(json.dumps({"base_url": url, "token": TOKEN}))
                with self.assertRaises(query.QueryError): query.read_credentials(path)
            path.write_text(json.dumps({"base_url": BASE, "token": TOKEN}));path.chmod(0o644)
            if os.name != "nt":
                with self.assertRaises(query.QueryError): query.read_credentials(path)

    def test_cannot_redirect_credentials(self):
        self.assertIsNone(query.NoRedirect().redirect_request(None, None, 302, "", {}, "https://elsewhere.invalid/"))

    def test_only_fixed_read_routes_and_encoded_filters(self):
        result = {"api_version": "v1", "read_only": True, "data": []}
        op = Opener(json.dumps(result).encode())
        with patch.object(query.urllib.request, "build_opener", return_value=op) as build:
            self.assertEqual(result, query.query(BASE, TOKEN, "logs", {"search": "a&b?secret=no"}))
        self.assertEqual("GET", op.request.method)
        self.assertEqual("Bearer "+TOKEN, op.request.get_header("Authorization"))
        self.assertNotIn(TOKEN, op.request.full_url)
        self.assertIn("search=a%26b%3Fsecret%3Dno", op.request.full_url)
        self.assertEqual({}, build.call_args.args[0].proxies)
        with self.assertRaises(query.QueryError): query.query(BASE, TOKEN, "drain", {})
        with self.assertRaises(query.QueryError): query.query(BASE, TOKEN, "rooms", {"arbitrary": "value"})

    def test_credential_echo_and_bad_contract_are_not_printed(self):
        for value in [{"api_version": "v1", "read_only": True, "data": TOKEN},
                      {"api_version": "v1", "read_only": False}, {"api_version": "other"}]:
            with patch.object(query.urllib.request, "build_opener", return_value=Opener(json.dumps(value).encode())):
                with self.assertRaises(query.QueryError) as error:
                    query.query(BASE, TOKEN, "overview", {})
                self.assertNotIn(TOKEN, str(error.exception))


if __name__ == "__main__": unittest.main()
