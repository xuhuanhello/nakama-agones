"""Projection tests: optional timing data must not create fake zero measurements."""
import json
import unittest

from test_console_export import export, fixture, WORKER


def window(count=10):
    return {"window_seconds": 60, "count": count, "p50": 10, "p95": 15,
            "p99": 20, "max": 25, "last_sample_age_seconds": 3}


class TimingProjectionTests(unittest.TestCase):
    def test_optional_windows_preserve_nulls_and_reject_unknown_private_fields(self):
        state = fixture()
        metrics = state["workers"][WORKER]["metrics"]
        metrics["client_presentation_to_ready_ms"] = window()
        metrics["client_presentation_to_ready_ms"]["secret"] = "must-not-leak"
        metrics["simulation_queue_ms"] = dict(window_seconds=60, count=0, p50=None,
                                            p95=None, p99=None, max=None, last_sample_age_seconds=None)
        metrics["simulation_workers"] = 2
        result = export.project_status(state)["workers"][0]["metrics"]
        self.assertEqual(15, result["client_presentation_to_ready_ms"]["p95"])
        self.assertIsNone(result["simulation_queue_ms"]["p95"])
        self.assertNotIn("client_presentation_to_settlement_ms", result)
        self.assertEqual(2, result["simulation_workers"])
        self.assertNotIn("must-not-leak", json.dumps(result))

    def test_malformed_samples_fail_closed(self):
        for field, value in (("count", 100001), ("count", True), ("p95", None),
                             ("p95", float("inf")), ("p95", 1), ("max", -1),
                             ("last_sample_age_seconds", 61), ("window_seconds", 59)):
            state = fixture()
            state["workers"][WORKER]["metrics"]["client_presentation_to_ready_ms"] = sample = window()
            sample[field] = value
            with self.subTest(field=field), self.assertRaises(export.ExportError):
                export.project_status(state)
        state = fixture()
        state["workers"][WORKER]["metrics"]["client_presentation_to_ready_ms"] = window(0)
        with self.assertRaises(export.ExportError):
            export.project_status(state)

    def test_policy_projection_has_no_runtime_credentials(self):
        state = fixture()
        state["capacity_policy"] = {"revision": 2, "rooms_per_instance": 4,
            "cpu_request_millicores": 1500, "cpu_limit_millicores": 1500,
            "updated_at": 1, "updated_by": "admin", "admin_token": "must-not-leak"}
        result = export.project_status(state)["capacity_policy"]
        self.assertEqual(1500, result["cpu_request_millicores"])
        self.assertNotIn("admin_token", result)


if __name__ == "__main__":
    unittest.main()
