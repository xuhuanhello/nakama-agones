"""Offline contracts for the isolated GameFleet/Nakama runtime harness."""

from __future__ import annotations

import importlib.util
import io
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from contextlib import redirect_stderr


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "test-gamefleet-runtime.py"
SPEC = importlib.util.spec_from_file_location("gamefleet_runtime_smoke", SCRIPT)
smoke = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(smoke)


class RuntimeHarnessContracts(unittest.TestCase):
    def test_search_protocol_is_opt_in_and_service_only(self) -> None:
        self.assertEqual(smoke.parse_args([]).search_protocol, "v1")
        self.assertEqual(smoke.parse_args(["--backend", "gamefleet-service"]).search_protocol, "v1")
        self.assertEqual(
            smoke.parse_args(["--backend", "gamefleet-service", "--search-protocol", "v2"]).search_protocol,
            "v2",
        )
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            smoke.parse_args(["--search-protocol", "v2"])

    def test_v1_matchmaker_request_shape_is_unchanged(self) -> None:
        old_protocol = smoke.SEARCH_PROTOCOL
        try:
            smoke.SEARCH_PROTOCOL = "v1"
            add = smoke.matchmaker_add_payload({"searchId": "search_example"}, "group-example")
        finally:
            smoke.SEARCH_PROTOCOL = old_protocol
        self.assertEqual(add, {
            "min_count": 2,
            "max_count": 2,
            "query": "+properties.smoke_group:group-example",
            "string_properties": {
                "smoke_group": "group-example",
                "gamefleet_protocol": "gamefleet.player-room.v1",
                "build_hash": "p4d-smoke",
                "region": "p4d-local",
                "gamefleet_search_id": "search_example",
            },
        })

    def test_v2_matchmaker_request_carries_attack_cases_for_before_hook(self) -> None:
        old_protocol = smoke.SEARCH_PROTOCOL
        try:
            smoke.SEARCH_PROTOCOL = "v2"
            add = smoke.matchmaker_add_payload({"searchId": "search_example"}, "group-example")
        finally:
            smoke.SEARCH_PROTOCOL = old_protocol
        self.assertEqual(add["query"], "* OR +properties.smoke_group:group-example")
        self.assertEqual(add["string_properties"]["gamefleet_match_pool"], "client-forged-pool")
        self.assertEqual(add["string_properties"]["gamefleet_queue_protocol"], "client-forged-protocol")
        self.assertEqual(set(add["numeric_properties"]), {
            "gamefleet_protocol", "build_hash", "region", "gamefleet_search_id",
            "gamefleet_match_pool", "gamefleet_queue_protocol",
        })

    @unittest.skipUnless(shutil.which("go"), "Go toolchain is required for the embedded fixture contract")
    def test_embedded_v2_fixture_compiles_and_keeps_routes_separate(self) -> None:
        with tempfile.TemporaryDirectory(prefix="gamefleet-fixture-contract-") as temp_name:
            temp = Path(temp_name)
            source = temp / "fixture.go"
            binary = temp / "fixture"
            source.write_text(smoke.GO_FIXTURE, encoding="utf-8")
            build_env = os.environ.copy()
            build_env.update({"GO111MODULE": "off", "CGO_ENABLED": "0"})
            subprocess.run(["go", "build", "-o", str(binary), str(source)], check=True,
                           cwd=temp, env=build_env, capture_output=True, text=True, timeout=60)
            source_text = smoke.GO_FIXTURE
            self.assertIn('beginPath = "/business/v2/service-searches"', source_text)
            self.assertIn('"/business/v1/service-searches/match"', source_text)
            self.assertIn('request.Version != matchVersion', source_text)
            self.assertIn('"version": routedServiceSearchVersion', source_text)
            self.assertIn('"matchPoolId": search.MatchPoolID', source_text)
            self.assertIn('routedCohortOrder := []string{"a1", "b1", "a2", "b2"}', source_text)

    def test_v2_player_search_rpc_rejects_pool_or_wire_version_leak(self) -> None:
        old_protocol = smoke.SEARCH_PROTOCOL
        try:
            smoke.SEARCH_PROTOCOL = "v2"
            with self.assertRaises(smoke.SmokeError):
                smoke.assert_player_search_dto({"search": {"searchId": "safe", "matchPoolId": "gfsp_" + "a" * 64}})
            with self.assertRaises(smoke.SmokeError):
                smoke.assert_player_search_dto({"version": smoke.ROUTED_SEARCH_VERSION, "search": {"searchId": "safe"}})
            smoke.assert_player_search_dto({"search": {"searchId": "safe", "state": "pending"}})
        finally:
            smoke.SEARCH_PROTOCOL = old_protocol


if __name__ == "__main__":
    unittest.main()
