#!/usr/bin/env python3
"""Isolated live Nakama/GameFleet plugin load check. Uses temporary containers and an ephemeral PostgreSQL database."""
from __future__ import annotations

import base64
import argparse
import http.client
import json
import os
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


REPO = Path.cwd()
RUNTIME_IMAGE = os.environ.get("GF_P4D_RUNTIME_IMAGE", "nakama-gamefleet-p4d:924ddf85")
POSTGRES_IMAGE = "postgres:16-alpine"
SYNTHETIC_KEY = "gfbiz_p4d_synthetic_only_0123456789abcdef"
APP_ID = "app_p4d_fixture"
PLACEMENT_ID = "placement_p4d_fixture"
REVISION_ID = "revision_p4d_fixture"
REGION = "p4d-local"
COMPATIBILITY = "p4d-smoke"
ROOM_VERSION = "gamefleet.player-room.v1"
EVIDENCE_PATH = Path(tempfile.gettempdir()) / "gamefleet-runtime-smoke-evidence.json"
db_password = ""

GO_FIXTURE = r'''package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
)

const (
	appID = "app_p4d_fixture"
	placementID = "placement_p4d_fixture"
	revisionID = "revision_p4d_fixture"
	region = "p4d-local"
	roomVersion = "gamefleet.player-room.v1"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func main() {
	key := os.Getenv("GF_P4D_FIXTURE_KEY")
	mode := os.Getenv("GF_P4D_FIXTURE_MODE")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/business/v1/caller", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_caller_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if mode == "deny" {
			log.Print("p4d_fixture_scope_denied")
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "fixture_denied"})
			return
		}
		log.Print("p4d_fixture_scope_ok")
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"callerId": "caller_p4d_fixture", "applicationId": appID,
				"placementId": placementID, "revisionId": revisionID,
				"region": region, "operations": []string{"reserve", "read", "cancel", "assignment", "resume"},
			},
			"requestId": "p4d-fixture-caller",
		})
	})
	mux.HandleFunc("/business/v1/reservations/current", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_current_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		var request struct {
			Version string `json:"version"`
			ParticipantID string `json:"participantId"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(body) > 4096 || json.Unmarshal(body, &request) != nil || request.Version != roomVersion || request.ParticipantID == "" {
			log.Print("p4d_fixture_bad_current_request")
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_fixture_request"})
			return
		}
		log.Printf("p4d_fixture_current_null user=%s", request.ParticipantID)
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"current": nil}, "requestId": "p4d-fixture-current"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("p4d_fixture_unexpected method=%s path=%s", r.Method, r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	})
	listener, err := net.Listen("tcp", "127.0.0.1:17682")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("p4d_fixture_ready")
	log.Fatal(http.Serve(listener, mux))
}
'''


class SmokeError(RuntimeError):
    pass


def docker(args: list[str], *, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(["docker", *args], text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        stderr, stdout = result.stderr, result.stdout
        for secret in (db_password, SYNTHETIC_KEY):
            if secret:
                replacement = "<redacted-db-password>" if secret == db_password else "<synthetic-key>"
                stderr = stderr.replace(secret, replacement)
                stdout = stdout.replace(secret, replacement)
        raise SmokeError(f"docker {args[0]} failed ({result.returncode}): {stderr[-4000:]} {stdout[-1000:]}")
    return result


def image_facts(image: str) -> dict[str, str]:
    result = docker(["image", "inspect", image])
    inspected = json.loads(result.stdout)[0]
    config = inspected.get("Config") or {}
    return {
        "id": inspected["Id"],
        "platform": inspected["Os"] + "/" + inspected["Architecture"],
        "labels": config.get("Labels") or {},
        "repo_digests": inspected.get("RepoDigests") or [],
    }


def container_state(name: str) -> tuple[str, int]:
    result = docker(["inspect", "--format", "{{.State.Status}}|{{.State.ExitCode}}", name], check=False)
    if result.returncode:
        return "missing", -1
    status, code = result.stdout.strip().split("|", 1)
    return status, int(code)


def logs(name: str) -> str:
    result = docker(["logs", name], check=False, timeout=15)
    return result.stdout + result.stderr


def wait_for_db(name: str, deadline: float) -> None:
    while time.monotonic() < deadline:
        result = docker(["exec", name, "pg_isready", "-U", "postgres", "-d", "nakama"], check=False, timeout=10)
        if result.returncode == 0:
            return
        status, _ = container_state(name)
        if status != "running":
            raise SmokeError("temporary PostgreSQL exited before becoming ready")
        time.sleep(1)
    raise SmokeError("temporary PostgreSQL readiness timed out")


def wait_for_fixture(name: str, deadline: float) -> None:
    while time.monotonic() < deadline:
        if "p4d_fixture_ready" in logs(name):
            return
        status, _ = container_state(name)
        if status != "running":
            raise SmokeError("loopback Business fixture exited before listening")
        time.sleep(0.25)
    raise SmokeError("loopback Business fixture readiness timed out")


def get_url(url: str, timeout: float = 2.0) -> tuple[int, bytes]:
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            return response.status, response.read(16384)
    except urllib.error.HTTPError as error:
        return error.code, error.read(16384)
    except (urllib.error.URLError, http.client.HTTPException, OSError, TimeoutError, socket.timeout):
        return 0, b""


def safe_log_excerpt(raw: str) -> list[str]:
    for secret, replacement in ((db_password, "<redacted-db-password>"), (SYNTHETIC_KEY, "<synthetic-key>")):
        if secret:
            raw = raw.replace(secret, replacement)
    terms = ("gamefleet", "plugin", "runtime module", "nakama", "error", "fatal")
    return [line[:1000] for line in raw.splitlines() if any(term in line.lower() for term in terms)][-30:]


def wait_health(url: str, name: str, deadline: float) -> bool:
    while time.monotonic() < deadline:
        status, _ = get_url(url)
        if status == 200:
            return True
        state, _ = container_state(name)
        if state != "running":
            return False
        time.sleep(0.5)
    return get_url(url)[0] == 200


def post_json(url: str, body: bytes, headers: dict[str, str], timeout: float = 10.0) -> tuple[int, dict[str, object]]:
    request = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", **headers}, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read(65536)
            return response.status, json.loads(raw)
    except urllib.error.HTTPError as error:
        try:
            payload = json.loads(error.read(65536))
        except Exception:
            payload = {}
        return error.code, payload


def authenticate(base_url: str, device_id: str) -> str:
    basic = base64.b64encode(b"local-server-key:").decode("ascii")
    status, response = post_json(
        base_url + "/v2/account/authenticate/device?create=true",
        json.dumps({"id": device_id}).encode("utf-8"),
        {"Authorization": "Basic " + basic},
    )
    token = response.get("token")
    if status != 200 or not isinstance(token, str) or not token:
        raise SmokeError(f"synthetic device authentication failed with HTTP {status}")
    return token


def current_rpc(base_url: str, token: str, *, expect_success: bool) -> tuple[int, dict[str, object]]:
    payload = {"version": ROOM_VERSION, "compatibility": COMPATIBILITY, "region": REGION}
    # Nakama's HTTP RPC API takes the plugin payload as a JSON string.
    wire = json.dumps(json.dumps(payload, separators=(",", ":")), separators=(",", ":")).encode("utf-8")
    status, response = post_json(
        base_url + "/v2/rpc/gamefleet_current_v1",
        wire,
        {"Authorization": "Bearer " + token},
    )
    if expect_success:
        if status != 200 or not isinstance(response.get("payload"), str):
            raise SmokeError(f"GameFleet current RPC failed with HTTP {status}")
        try:
            current = json.loads(response["payload"])
        except Exception as exc:
            raise SmokeError("GameFleet current RPC returned a non-JSON payload") from exc
        if current != {"current": None}:
            raise SmokeError("current RPC did not return the fixture's expected empty state")
    return status, response


def start_fixture(name: str, image: str, mode: str) -> None:
    docker([
        "run", "--detach", "--pull=never", "--platform", "linux/amd64", "--name", name,
        "--network", "container:" + db_container,
        "--env", "GF_P4D_FIXTURE_KEY=" + SYNTHETIC_KEY,
        "--env", "GF_P4D_FIXTURE_MODE=" + mode,
        image,
    ])
    wait_for_fixture(name, time.monotonic() + 15)


def start_nakama(name: str, config_path: Path, key_path: Path) -> None:
    dsn = f"postgres:{db_password}@127.0.0.1:5432/nakama?sslmode=disable"
    env = {
        "NAKAMA_FLEET_BACKEND": "gamefleet",
        "GAMEFLEET_BUSINESS_URL": "http://127.0.0.1:17682",
        "GAMEFLEET_BUSINESS_KEY_FILE": "/run/secrets/gamefleet-business-key",
        "GAMEFLEET_APPLICATION_ID": APP_ID,
        "GAMEFLEET_PLACEMENT_ID": PLACEMENT_ID,
        "GAMEFLEET_REVISION_ID": REVISION_ID,
        "GAMEFLEET_REGION": REGION,
        "GAMEFLEET_COMPATIBILITY": COMPATIBILITY,
        # Tripwire: GameFleet selection must bypass the legacy Agones backend.
        "AGONES_FLEET_DATABASE_URL": "intentionally-not-a-dsn",
    }
    args = [
        "run", "--detach", "--pull=never", "--platform", "linux/amd64", "--name", name,
        "--network", "container:" + db_container,
        "--mount", f"type=bind,src={config_path},dst=/nakama/data/local.yml,readonly",
        "--mount", f"type=bind,src={key_path},dst=/run/secrets/gamefleet-business-key,readonly",
    ]
    for key, value in env.items():
        args.extend(["--env", key + "=" + value])
    args.extend(["--entrypoint", "/nakama/nakama", RUNTIME_IMAGE,
                 "--config", "/nakama/data/local.yml", "--database.address", dsn])
    docker(args)


def main() -> None:
    global db_container, db_password, REPO, RUNTIME_IMAGE, EVIDENCE_PATH
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path.cwd(), help="Nakama Agones checkout (default: current directory)")
    parser.add_argument("--image", default=RUNTIME_IMAGE, help="already-built linux/amd64 runtime image tag")
    parser.add_argument("--evidence", type=Path, default=EVIDENCE_PATH, help="evidence JSON output path (contains no bearer credentials)")
    parser.add_argument("--temp-root", type=Path, default=Path(tempfile.gettempdir()), help="temporary build/configuration directory parent")
    parser.add_argument("--positive-only", action="store_true", help="stop after runtime load and two authenticated current-null RPCs")
    args = parser.parse_args()
    REPO = args.repo.resolve()
    RUNTIME_IMAGE = args.image
    EVIDENCE_PATH = args.evidence
    if not REPO.is_dir():
        raise SmokeError(f"expected checkout missing: {REPO}")
    suffix = uuid.uuid4().hex[:8]
    prefix = "gf-p4d-" + suffix
    db_container = prefix + "-db"
    network = prefix + "-net"
    fixture_image = prefix + "-fixture:local"
    fixture_container = prefix + "-fixture-ok"
    nakama_container = prefix + "-nakama-ok"
    fail_fixture_container = prefix + "-fixture-deny"
    fail_nakama_container = prefix + "-nakama-deny"
    migration_container = prefix + "-migrate"
    owned_containers = [migration_container, nakama_container, fail_nakama_container, fixture_container, fail_fixture_container, db_container]
    temp_root = Path(tempfile.mkdtemp(prefix=prefix + "-", dir=args.temp_root))
    key_path = temp_root / "business.key"
    config_path = temp_root / "nakama.local.yml"
    fixture_source = temp_root / "main.go"
    fixture_binary = temp_root / "gf-p4d-fixture"
    dockerfile = temp_root / "Dockerfile"
    evidence: dict[str, object] = {
        "status": "running",
        "scope": "isolated Nakama process load and player RPC check; no GameFleet allocation or Fixed gameplay",
        "runtime_image": RUNTIME_IMAGE,
        "prefix": prefix,
        "checks": [],
        "limits": [
            "Synthetic local Business fixture only; no real GameFleet reservation, host, endpoint, ticket, or allocation was tested.",
            "Temporary PostgreSQL database contains only this smoke's Nakama auth/state and is deleted at cleanup.",
            "Loopback fixture and Nakama API are local-only; no production credentials or service were used.",
        ],
    }
    image_created = False
    network_created = False
    try:
        runtime = image_facts(RUNTIME_IMAGE)
        if runtime["platform"] != "linux/amd64":
            raise SmokeError("runtime image is not linux/amd64")
        evidence["runtime_image_facts"] = runtime
        revision = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, text=True, capture_output=True, check=True).stdout.strip()
        dirty = subprocess.run(["git", "status", "--porcelain"], cwd=REPO, text=True, capture_output=True, check=True).stdout.strip()
        evidence["source_revision"] = revision
        evidence["source_worktree_clean"] = not bool(dirty)
        postgres = image_facts(POSTGRES_IMAGE)
        evidence["postgres_image_facts"] = postgres

        key_path.write_text(SYNTHETIC_KEY, encoding="ascii")
        key_path.chmod(0o400)
        config_path.write_bytes((REPO / "deploy/nakama.local.yml").read_bytes())
        fixture_source.write_text(GO_FIXTURE, encoding="utf-8")
        dockerfile.write_text(
            "FROM scratch\nCOPY gf-p4d-fixture /gf-p4d-fixture\nENTRYPOINT [\"/gf-p4d-fixture\"]\n",
            encoding="utf-8",
        )
        build_env = os.environ.copy()
        build_env.update({"GO111MODULE": "off", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
        subprocess.run(["go", "build", "-trimpath", "-o", str(fixture_binary), str(fixture_source)],
                       cwd=temp_root, env=build_env, check=True, capture_output=True, text=True)
        docker(["build", "--pull=false", "--network=none", "--platform", "linux/amd64", "--tag", fixture_image,
                "--file", str(dockerfile), str(temp_root)])
        image_created = True
        evidence["fixture_image_facts"] = image_facts(fixture_image)
        evidence["checks"].append("built static local-only fixture image from scratch; no base image pull")

        docker(["network", "create", network])
        network_created = True
        db_password = secrets.token_urlsafe(24)
        docker([
            "run", "--detach", "--pull=never", "--name", db_container, "--network", network,
            "--network-alias", "postgres", "--publish", "127.0.0.1::7350",
            "--tmpfs", "/var/lib/postgresql/data:rw,nosuid,nodev,size=512m",
            "--env", "POSTGRES_USER=postgres", "--env", "POSTGRES_DB=nakama",
            "--env", "POSTGRES_PASSWORD=" + db_password, POSTGRES_IMAGE,
        ])
        wait_for_db(db_container, time.monotonic() + 60)
        evidence["checks"].append("temporary PostgreSQL ready on tmpfs; database port is not published")
        port = docker(["port", db_container, "7350/tcp"]).stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        api_url = "http://127.0.0.1:" + port
        evidence["nakama_api"] = "127.0.0.1:<ephemeral> (only this temporary container mapping)"

        dsn = f"postgres:{db_password}@127.0.0.1:5432/nakama?sslmode=disable"
        docker([
            "run", "--rm", "--name", migration_container, "--pull=never", "--platform", "linux/amd64", "--network", "container:" + db_container,
            "--entrypoint", "/nakama/nakama", RUNTIME_IMAGE, "migrate", "up", "--database.address", dsn,
        ], timeout=120)
        evidence["checks"].append("official Nakama 3.41.0 runtime applied migrations to the isolated nakama database")

        start_fixture(fixture_container, fixture_image, "allow")
        start_nakama(nakama_container, config_path, key_path)
        if not wait_health(api_url + "/healthcheck", nakama_container, time.monotonic() + 90):
            evidence["nakama_initial_state"] = container_state(nakama_container)
            evidence["nakama_startup_log_excerpt"] = safe_log_excerpt(logs(nakama_container))
            raise SmokeError("Nakama did not reach /healthcheck after GameFleet initialization")
        ok_logs = logs(nakama_container)
        marker = "GameFleet pilot bridge registered; allocation and capacity are owned by GameFleet"
        if marker not in ok_logs:
            raise SmokeError("Nakama process became healthy without the GameFleet module registration log")
        if "Agones FleetManager registered" in ok_logs:
            raise SmokeError("legacy Agones FleetManager unexpectedly registered in GameFleet mode")
        evidence["checks"].append("live Nakama process loaded agones.so, ran GameFleet scope preflight, and registered the GameFleet bridge")
        evidence["checks"].append("legacy Agones FleetManager registration log absent; invalid legacy DB URL was bypassed")

        device_ids = ["gf-p4d-a-" + uuid.uuid4().hex, "gf-p4d-b-" + uuid.uuid4().hex]
        for device_id in device_ids:
            token = authenticate(api_url, device_id)
            status, _ = current_rpc(api_url, token, expect_success=True)
            if status != 200:
                raise SmokeError("synthetic authenticated current RPC did not return HTTP 200")
        ok_fixture_logs = logs(fixture_container)
        if ok_fixture_logs.count("p4d_fixture_scope_ok") != 1 or ok_fixture_logs.count("p4d_fixture_current_null user=") != 2:
            raise SmokeError("fixture did not observe one scope preflight and current-null for both synthetic devices")
        evidence["checks"].append("two synthetic Nakama devices authenticated and each received current=null through gamefleet_current_v1")
        evidence["synthetic_devices"] = 2
        evidence["business_fixture_requests"] = {"caller_scope": 1, "current_null": 2}
        print("LIVE POSITIVE CHECK COMPLETE: plugin registered; two authenticated current-null RPCs passed", flush=True)
        if args.positive_only:
            evidence["status"] = "passed-positive-only"
            evidence["scope_failure"] = "not run (--positive-only)"
            return

        docker(["stop", "--time", "10", nakama_container], check=False, timeout=20)
        docker(["stop", "--time", "5", fixture_container], check=False, timeout=15)
        start_fixture(fail_fixture_container, fixture_image, "deny")
        start_nakama(fail_nakama_container, config_path, key_path)
        fail_logs = ""
        fail_state = "running"
        fail_code = 0
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            fail_logs = logs(fail_nakama_container)
            fail_state, _ = container_state(fail_nakama_container)
            if fail_state != "running":
                _, fail_code = container_state(fail_nakama_container)
                break
            time.sleep(0.5)
        fail_logs = logs(fail_nakama_container)
        fixture_fail_logs = logs(fail_fixture_container)
        if "p4d_fixture_scope_denied" not in fixture_fail_logs:
            raise SmokeError("scope-denial fixture was not called")
        if marker in fail_logs or "Agones FleetManager registered" in fail_logs:
            raise SmokeError("scope-denial path registered a fleet backend")
        if "GameFleet business scope preflight failed" not in fail_logs:
            raise SmokeError("Nakama logs did not report the GameFleet scope preflight failure")
        if fail_state != "exited" or fail_code == 0:
            raise SmokeError(f"Nakama did not abort with a nonzero exit after scope preflight failure (state={fail_state}, code={fail_code})")
        evidence["checks"].append("Business scope denial made the Nakama process exit nonzero before readiness")
        evidence["scope_failure"] = {"fixture_status": "HTTP 403", "nakama_container_state": fail_state, "exit_code": fail_code}
        evidence["checks"].append("invalid Business scope failed closed without falling back to Agones")
        evidence["status"] = "passed"
    except Exception as exc:
        evidence["status"] = "failed"
        evidence["failure"] = str(exc)
        raise
    finally:
        for name in owned_containers:
            docker(["rm", "--force", name], check=False, timeout=20)
        if network_created:
            docker(["network", "rm", network], check=False, timeout=20)
        if image_created:
            docker(["image", "rm", "--force", fixture_image], check=False, timeout=20)
        shutil.rmtree(temp_root, ignore_errors=True)
        EVIDENCE_PATH.write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print("evidence:", EVIDENCE_PATH)
        print("status:", evidence["status"])
        for check in evidence["checks"]:
            print("PASS:", check)
        if "failure" in evidence:
            print("FAIL:", evidence["failure"])


db_container = ""

if __name__ == "__main__":
    main()
