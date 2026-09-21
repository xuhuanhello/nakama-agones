#!/usr/bin/env python3
"""Validate public log manifests; --images also runs isolated pinned-image checks."""
import argparse
import base64
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time
import unittest
import uuid

import yaml


ROOT = Path(__file__).resolve().parent
DOCUMENTS = [document for path in sorted(ROOT.glob("*.yaml"))
             for document in yaml.safe_load_all(path.read_text()) if document]


def resource(kind, name, namespace="agones-observability"):
    return next(document for document in DOCUMENTS if document["kind"] == kind
                and document["metadata"]["name"] == name and document["metadata"].get("namespace") == namespace)


def config(name, key):
    return resource("ConfigMap", name)["data"][key]


def deployment(name):
    return resource("Deployment", name)


class ManifestTests(unittest.TestCase):
    def test_unique_resources_and_expected_kinds(self):
        identities = [(item["kind"], item["metadata"].get("namespace"), item["metadata"]["name"])
                      for item in DOCUMENTS]
        self.assertEqual(len(identities), len(set(identities)))
        self.assertEqual({"Namespace", "ServiceAccount", "PersistentVolumeClaim", "ConfigMap", "Deployment",
                          "Service", "Role", "RoleBinding", "NetworkPolicy"}, {item["kind"] for item in DOCUMENTS})

    def test_one_local_storage_writer_and_collector_on_control_node(self):
        for name in ("fleet-loki", "fleet-alloy"):
            spec = deployment(name)["spec"]
            self.assertEqual(1, spec["replicas"])
            self.assertEqual("Recreate", spec["strategy"]["type"])
            pod = spec["template"]["spec"]
            self.assertEqual("control", pod["nodeSelector"]["nakama-agones.io/role"])
            self.assertIn({"key": "CriticalAddonsOnly", "operator": "Equal", "value": "true", "effect": "NoExecute"},
                          pod["tolerations"])
            self.assertNotIn("hostNetwork", pod)
            self.assertTrue(all("hostPath" not in volume for volume in pod["volumes"]))

    def test_pinned_images_and_restricted_security(self):
        for name in ("fleet-loki", "fleet-alloy"):
            pod = deployment(name)["spec"]["template"]["spec"]
            self.assertTrue(pod["securityContext"]["runAsNonRoot"])
            self.assertEqual("RuntimeDefault", pod["securityContext"]["seccompProfile"]["type"])
            container = pod["containers"][0]
            self.assertRegex(container["image"], r"^grafana/[a-z]+:v?[0-9.]+@sha256:[a-f0-9]{64}$")
            self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
            self.assertFalse(container["securityContext"]["allowPrivilegeEscalation"])
            self.assertEqual(["ALL"], container["securityContext"]["capabilities"]["drop"])

    def test_combined_resource_budget(self):
        total = {"cpu_request": 0, "cpu_limit": 0, "memory_request": 0, "memory_limit": 0}
        for name in ("fleet-loki", "fleet-alloy"):
            spec = deployment(name)["spec"]["template"]["spec"]["containers"][0]["resources"]
            for mode, suffix in (("requests", "request"), ("limits", "limit")):
                total["cpu_" + suffix] += int(spec[mode]["cpu"].removesuffix("m"))
                total["memory_" + suffix] += int(spec[mode]["memory"].removesuffix("Mi"))
        self.assertEqual({"cpu_request": 150, "cpu_limit": 750, "memory_request": 512, "memory_limit": 1024}, total)

    def test_no_public_listener_or_loki_kubernetes_token(self):
        services = [item for item in DOCUMENTS if item["kind"] == "Service"]
        self.assertEqual(1, len(services))
        self.assertEqual("ClusterIP", services[0]["spec"]["type"])
        self.assertEqual([{"name": "http", "port": 3100, "targetPort": "http"}], services[0]["spec"]["ports"])
        self.assertFalse(resource("ServiceAccount", "fleet-loki")["automountServiceAccountToken"])
        self.assertFalse(deployment("fleet-loki")["spec"]["template"]["spec"]["automountServiceAccountToken"])

    def test_collector_roles_only_read_pods_and_logs_in_two_namespaces(self):
        roles = [item for item in DOCUMENTS if item["kind"] == "Role"
                 and item["metadata"]["name"] == "fleet-alloy-pod-logs"]
        self.assertEqual({"agones-games", "agones-system"}, {item["metadata"]["namespace"] for item in roles})
        expected = [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list", "watch"]},
                    {"apiGroups": [""], "resources": ["pods/log"], "verbs": ["get"]}]
        for role in roles:
            self.assertEqual(expected, role["rules"])
            binding = resource("RoleBinding", "fleet-alloy-pod-logs", role["metadata"]["namespace"])
            self.assertEqual([{"kind": "ServiceAccount", "name": "fleet-alloy", "namespace": "agones-observability"}],
                             binding["subjects"])

    def test_console_role_is_exact_service_proxy_get(self):
        self.assertEqual([{"apiGroups": [""], "resources": ["services/proxy"], "resourceNames": ["http:fleet-loki:http"],
                           "verbs": ["get"]}], resource("Role", "fleet-console-loki-read")["rules"])
        self.assertEqual([{"kind": "ServiceAccount", "name": "fleet-console", "namespace": "agones-control"}],
                         resource("RoleBinding", "fleet-console-loki-read")["subjects"])

    def test_retention_schema_and_query_window(self):
        loki = yaml.safe_load(config("fleet-loki-config", "loki.yaml"))
        self.assertEqual("168h", loki["limits_config"]["retention_period"])
        self.assertEqual("168h", loki["limits_config"]["max_query_lookback"])
        self.assertEqual("168h", loki["limits_config"]["max_query_length"])
        schema = loki["schema_config"]["configs"][0]
        self.assertEqual(("tsdb", "v13", "filesystem", "24h"),
                         (schema["store"], schema["schema"], schema["object_store"], schema["index"]["period"]))
        self.assertTrue(loki["compactor"]["retention_enabled"])
        self.assertEqual("filesystem", loki["compactor"]["delete_request_store"])
        self.assertEqual("2h", loki["compactor"]["retention_delete_delay"])

    def test_markers_wal_indexes_and_chunks_share_persistent_volume(self):
        loki = yaml.safe_load(config("fleet-loki-config", "loki.yaml"))
        paths = [loki["compactor"]["working_directory"], loki["ingester"]["wal"]["dir"],
                 *loki["common"]["storage"]["filesystem"].values(),
                 loki["storage_config"]["tsdb_shipper"]["active_index_directory"],
                 loki["storage_config"]["tsdb_shipper"]["cache_location"]]
        self.assertTrue(all(path.startswith("/var/loki/") for path in paths))
        pod = deployment("fleet-loki")["spec"]["template"]["spec"]
        self.assertIn({"name": "data", "mountPath": "/var/loki"}, pod["containers"][0]["volumeMounts"])
        self.assertIn({"name": "data", "persistentVolumeClaim": {"claimName": "fleet-loki-data"}}, pod["volumes"])
        self.assertEqual("10Gi", resource("PersistentVolumeClaim", "fleet-loki-data")["spec"]["resources"]["requests"]["storage"])

    def test_alloy_positions_are_persistent_and_not_loki_storage(self):
        pod = deployment("fleet-alloy")["spec"]["template"]["spec"]
        self.assertIn({"name": "positions", "persistentVolumeClaim": {"claimName": "fleet-alloy-positions"}}, pod["volumes"])
        self.assertIn({"name": "positions", "mountPath": "/var/lib/alloy"}, pod["containers"][0]["volumeMounts"])
        self.assertNotIn("fleet-loki-data", json.dumps(pod["volumes"]))

    def test_discovery_and_exported_labels_are_bounded(self):
        alloy = config("fleet-alloy-config", "config.alloy")
        self.assertEqual(["agones-games", "agones-system"], re.findall(r'names = \["([^"]+)"\]', alloy))
        self.assertIn('label = "app.kubernetes.io/managed-by=nakama-agones"', alloy)
        self.assertIn('label = "app=agones,agones.dev/role in (controller,extensions)"', alloy)
        self.assertIn('values = ["cluster", "namespace", "pod", "container"]', alloy)
        self.assertNotIn("insecure_skip_verify", alloy)
        self.assertNotIn("loki.source.file", alloy)
        self.assertNotIn("stage.labels", alloy)
        self.assertNotIn("loki.secretfilter", alloy)

    def test_only_alloy_pods_can_initiate_loki_ingress(self):
        ingress = resource("NetworkPolicy", "fleet-loki-ingress")["spec"]["ingress"]
        self.assertEqual([{"from": [{"podSelector": {"matchLabels": {"app.kubernetes.io/name": "fleet-alloy"}}}],
                           "ports": [{"protocol": "TCP", "port": 3100}]}], ingress)
        self.assertEqual([], resource("NetworkPolicy", "fleet-alloy-ingress")["spec"]["ingress"])


def image_for(name):
    return deployment(name)["spec"]["template"]["spec"]["containers"][0]["image"]


def run(command, timeout=60):
    return subprocess.run(command, check=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                          text=True, timeout=timeout).stdout


def verify_images():
    """Use real parsers and processing stages; no Kubernetes context or credentials."""
    with tempfile.TemporaryDirectory(prefix="nakama-agones-observability-") as directory:
        temp = Path(directory)
        temp.chmod(0o755)
        (temp / "loki.yaml").write_text(config("fleet-loki-config", "loki.yaml"))
        alloy = config("fleet-alloy-config", "config.alloy")
        (temp / "config.alloy").write_text(alloy)
        base = ["docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "-v", str(temp) + ":/fixtures:ro"]
        run(base + [image_for("fleet-loki"), "-config.file=/fixtures/loki.yaml", "-verify-config"])
        print("Pinned Loki image: configuration valid.")
        run(base + ["-e", "FLEET_LOG_CLUSTER=validation", image_for("fleet-alloy"), "validate", "/fixtures/config.alloy"])
        print("Pinned Alloy image: configuration valid.")

        # Test the production sanitizer block itself with synthetic file input;
        # only the source/sink change. No network, API token or live log is used.
        sanitizer = alloy[alloy.index('loki.process "sanitize"'):alloy.index('loki.write "local"')]
        sanitizer = sanitizer.replace("loki.write.local.receiver", "loki.echo.verify.receiver")
        fixture_config = '''logging { level = "info" format = "json" }
loki.source.file "fixtures" {
  targets = [{__path__ = "/fixtures/events.log", cluster = "validation", namespace = "agones-games", pod = "fixture", container = "game", room = "HIGH_CARDINALITY_MUST_DISAPPEAR", user = "HIGH_CARDINALITY_MUST_DISAPPEAR"}]
  forward_to = [loki.process.sanitize.receiver]
}
''' + sanitizer + '\nloki.echo "verify" {}\n'
        # Attribute separators are newlines, including the small logging block.
        fixture_config = fixture_config.replace('logging { level = "info" format = "json" }',
                                                'logging {\n  level = "info"\n  format = "json"\n}')
        (temp / "filter.alloy").write_text(fixture_config)
        good = ["DM_FLEET_READY worker=fixture rooms=2", "safe token_file_unavailable operation=list",
                "error=invalid_bootstrap_token operation=heartbeat", "SAFE_END"]
        # Build a deliberately unsigned synthetic JWT; no real credential fixture
        # is stored in the public source tree.
        synthetic_jwt = ".".join(base64.urlsafe_b64encode(part).decode().rstrip("=") for part in (
            b'{"alg":"HS256"}', b'{"sub":"synthetic"}', b"synthetic_signature"))
        bad = [
            'password=FAKE_PASSWORD', '{"token":"FAKE_TOKEN"}', r'{\"secret\":\"FAKE_SECRET\"}',
            'AGONES_FLEET_ADMISSION_KEY=FAKE_ADMISSION_KEY', 'Authorization: Bearer FAKE_BEARER',
            'credential = "FAKE_CREDENTIAL"', 'dsn=FAKE_DSN', 'postgresql://test:FAKE_PASS@db.invalid/game',
            'prefix ' + synthetic_jwt + ' suffix',
            'prefix pfclient_SYNTHETIC_ONLY_123456789 suffix', '-----BEGIN PRIVATE KEY-----',
            'Q' * 64, '\x1b[31mpassword\x1b[0m=FAKE_ANSI_SECRET', 'OVERSIZED_' + 'x' * 17000,
        ]
        (temp / "events.log").write_text("\n".join([good[0], *bad, *good[1:]]) + "\n")
        name = "nakama-agones-logs-check-" + uuid.uuid4().hex[:12]
        command = ["docker", "run", "--detach", "--name", name, "--network", "none", "--read-only",
                   "--memory", "256m", "--cpus", "0.25", "--user", "473:473", "--cap-drop", "ALL",
                   "--security-opt", "no-new-privileges", "--tmpfs", "/var/lib/alloy:rw,uid=473,gid=473,size=32m",
                   "--tmpfs", "/tmp:rw,uid=473,gid=473,size=16m", "-v", str(temp) + ":/fixtures:ro",
                   "-e", "GOMEMLIMIT=200MiB", "-e", "GOMAXPROCS=1", image_for("fleet-alloy"), "run",
                   "--storage.path=/var/lib/alloy", "--disable-reporting", "/fixtures/filter.alloy"]
        try:
            run(command)
            deadline = time.monotonic() + 20
            entries = []
            while time.monotonic() < deadline:
                output = run(["docker", "logs", name])
                rows = []
                for line in output.splitlines():
                    try:
                        row = json.loads(line)
                    except ValueError:
                        continue
                    if row.get("component_id") == "loki.echo.verify" and "entry" in row:
                        rows.append(row)
                entries = [row["entry"] for row in rows]
                if "SAFE_END" in entries:
                    break
                if run(["docker", "inspect", "--format", "{{.State.Running}}", name]).strip() != "true":
                    raise RuntimeError("Alloy filter-check container exited before processing fixtures")
                time.sleep(0.5)
            if sorted(entries) != sorted(good):
                raise RuntimeError("Alloy sanitizer did not preserve exactly the safe fixtures")
            for row in rows:
                names = set(re.findall(r'([a-zA-Z_][a-zA-Z0-9_]*)=', row["labels"]))
                if names != {"cluster", "namespace", "pod", "container"}:
                    raise RuntimeError("Alloy sanitizer exported unexpected labels")
            print("Pinned Alloy pipeline: 14 sensitive/oversized fixtures dropped; 4 safe fixtures retained; only 4 labels exported.")
        finally:
            subprocess.run(["docker", "rm", "--force", name], stdout=subprocess.DEVNULL,
                           stderr=subprocess.DEVNULL, timeout=15)


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("--images", action="store_true", help="Also run the pinned images locally in isolated containers.")
    args = cli.parse_args()
    result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(ManifestTests))
    if not result.wasSuccessful():
        return 1
    if args.images:
        try:
            verify_images()
        except (subprocess.SubprocessError, OSError, RuntimeError) as error:
            if isinstance(error, subprocess.CalledProcessError):
                print(error.stdout)
            else:
                print(str(error))
            return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
